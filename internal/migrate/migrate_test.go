package migrate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/chart"
	"github.com/giantswarm/agent-manager/internal/kube"
	"github.com/giantswarm/agent-manager/internal/skills"
)

// The commits the fake GitHub API resolves refs to, and the images the fake
// registry pins.
const (
	mainHead    = "0123456789abcdef0123456789abcdef01234567"
	featureHead = "89abcdef0123456789abcdef0123456789abcdef"
	trunkHead   = "fedcba9876543210fedcba9876543210fedcba98"
	skillsRepo  = "https://github.com/giantswarm/agent-skills"
	privateRepo = "https://github.com/giantswarm/private-skills"
	kubectlTag  = "ghcr.io/giantswarm/skills/kubectl:1.4.0"
	kubectlPin  = "ghcr.io/giantswarm/skills/kubectl@sha256:5b0bcabd1ed22e9fb1310cf6c2dec7cdef19f0ad69efa1f392e94a4333501270"
	legacyRange = ">=0.2.1 <1.0.0"
	target      = "1.x"
)

const kagentAPI = "kagent.dev/v1alpha3"

var (
	hrGVR      = schema.GroupVersionResource{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"}
	ociGVR     = schema.GroupVersionResource{Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "ocirepositories"}
	tplGVR     = schema.GroupVersionResource{Group: "kagent.dev", Version: "v1alpha3", Resource: "agenttemplates"}
	serverGVR  = schema.GroupVersionResource{Group: "kagent.dev", Version: "v1alpha3", Resource: "remotemcpservers"}
	harnessGVR = schema.GroupVersionResource{Group: "kagent.dev", Version: "v1alpha3", Resource: "harnesses"}
	mcGVR      = schema.GroupVersionResource{Group: "kagent.dev", Version: "v1alpha3", Resource: "modelconfigs"}
	listKinds  = map[schema.GroupVersionResource]string{
		hrGVR: "HelmReleaseList", ociGVR: "OCIRepositoryList", tplGVR: "AgentTemplateList", serverGVR: "RemoteMCPServerList",
		harnessGVR: "HarnessList", mcGVR: "ModelConfigList", legacyAgentGVR: "AgentList", crdGVR: "CustomResourceDefinitionList",
	}
)

// ---- fakes ----------------------------------------------------------------------

// fakeGitHub is the commits API: agent-skills is public, private-skills
// answers 404 to an anonymous caller (a private repository looks like a
// missing one without a token).
func fakeGitHub(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/giantswarm/agent-skills", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"default_branch": "main"})
	})
	mux.HandleFunc("/repos/giantswarm/agent-skills/commits/", func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.URL.Path, "/repos/giantswarm/agent-skills/commits/") {
		case "main":
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": mainHead})
		case "feature":
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": featureHead})
		default:
			http.Error(w, `{"message":"No commit found"}`, http.StatusUnprocessableEntity)
		}
	})
	private := func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		if r.Header.Get("Authorization") == "" {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(body)
	}
	mux.HandleFunc("/repos/giantswarm/private-skills", func(w http.ResponseWriter, r *http.Request) {
		private(w, r, map[string]any{"default_branch": "trunk"})
	})
	mux.HandleFunc("/repos/giantswarm/private-skills/commits/trunk", func(w http.ResponseWriter, r *http.Request) {
		private(w, r, map[string]any{"sha": trunkHead})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

// pinner resolves git refs through the real resolver against the fake GitHub
// API and fakes the registry for the one skill image.
type pinner struct{ git *skills.Resolver }

func (p pinner) GitHead(ctx context.Context, repoURL, ref string) (string, error) {
	return p.git.GitHead(ctx, repoURL, ref)
}

func (p pinner) OCIDigest(_ context.Context, ref string) (string, error) {
	if strings.HasPrefix(ref, "ghcr.io/giantswarm/skills/kubectl") {
		return kubectlPin, nil
	}
	return "", fmt.Errorf("%w: OCI skill %s could not be resolved to a digest", skills.ErrUnresolvable, ref)
}

// fakeChart is the agent chart as the registry reports it: the 1.x schema
// compiled in, and latest the newest version in the target range ("" when
// none is published yet).
type fakeChart struct{ latest string }

func (fakeChart) Schema(context.Context) chart.Schema { return chart.EmbeddedSchema() }
func (c fakeChart) Info(context.Context) chart.Info {
	info := chart.Info{OCIURL: agents.DefaultChartOCIURL, Semver: target, LatestVersion: c.latest, SchemaVersion: chart.EmbeddedSchemaVersion, SchemaSource: chart.SourceEmbedded}
	if c.latest == "" {
		info.Error = "no chart version satisfies \"1.x\""
	}
	return info
}
func (fakeChart) Name() string        { return "agent" }
func (fakeChart) OCIURL() string      { return agents.DefaultChartOCIURL }
func (fakeChart) SemverRange() string { return target }

type cluster struct {
	dyn    *dynamicfake.FakeDynamicClient
	typed  *kubefake.Clientset
	client kube.Client
}

func newCluster(objs ...runtime.Object) *cluster {
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, objs...)
	typed := kubefake.NewClientset()
	return &cluster{dyn: dyn, typed: typed, client: kube.FromInterfaces(dyn, typed, typed.Discovery())}
}

// runner builds the command's wiring over the fake cluster: the fake chart,
// the pinner, and the service as the status reader.
func (c *cluster) runner(t *testing.T, opts Options, latest, token string) *Runner {
	t.Helper()
	ch := fakeChart{latest: latest}
	p := pinner{git: skills.NewResolver(fakeGitHub(t).URL, token, nil, nil)}
	svc := agents.New(kube.NewServiceAccountProvider(c.client), ch, nil, p, agents.Config{
		DefaultNamespace: opts.Namespaces[0], ManagedNamespaces: opts.Namespaces, Compose: agents.ComposeConfig{HarnessName: "kagent"}, KagentAPIVersion: "v1alpha3",
	}, nil)
	opts.Version = "test"
	return New(c.client, ch, p, svc, opts, nil)
}

func (c *cluster) get(t *testing.T, gvr schema.GroupVersionResource, ns, name string) *unstructured.Unstructured {
	t.Helper()
	obj, err := c.dyn.Resource(gvr).Namespace(ns).Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err, "%s %s/%s", gvr.Resource, ns, name)
	return obj
}

func (c *cluster) exists(gvr schema.GroupVersionResource, ns, name string) bool {
	_, err := c.dyn.Resource(gvr).Namespace(ns).Get(context.Background(), name, metav1.GetOptions{})
	return !apierrors.IsNotFound(err)
}

func (c *cluster) values(t *testing.T, ns, name string) map[string]any {
	t.Helper()
	values, _, _ := unstructured.NestedMap(c.get(t, hrGVR, ns, name).Object, "spec", "values")
	return values
}

func (c *cluster) semver(t *testing.T, ns, name string) string {
	t.Helper()
	v, _, _ := unstructured.NestedString(c.get(t, ociGVR, ns, name).Object, "spec", "ref", "semver")
	return v
}

func (c *cluster) reportConfigMap(t *testing.T, ns string) map[string]string {
	t.Helper()
	cm, err := c.typed.CoreV1().ConfigMaps(ns).Get(context.Background(), DefaultReportConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	require.NoError(t, err)
	return cm.Data
}

func (c *cluster) create(t *testing.T, gvr schema.GroupVersionResource, obj *unstructured.Unstructured) {
	t.Helper()
	_, err := c.dyn.Resource(gvr).Namespace(obj.GetNamespace()).Create(context.Background(), obj, metav1.CreateOptions{})
	require.NoError(t, err)
}

func (c *cluster) update(t *testing.T, gvr schema.GroupVersionResource, obj *unstructured.Unstructured) {
	t.Helper()
	_, err := c.dyn.Resource(gvr).Namespace(obj.GetNamespace()).Update(context.Background(), obj, metav1.UpdateOptions{})
	require.NoError(t, err)
}

func (c *cluster) delete(t *testing.T, gvr schema.GroupVersionResource, ns, name string) {
	t.Helper()
	require.NoError(t, c.dyn.Resource(gvr).Namespace(ns).Delete(context.Background(), name, metav1.DeleteOptions{}))
}

// ---- objects ----------------------------------------------------------------------

func load(t *testing.T, name string) *unstructured.Unstructured {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(filepath.Join("testdata", name)))
	require.NoError(t, err)
	var obj map[string]any
	require.NoError(t, yaml.Unmarshal(raw, &obj))
	return &unstructured.Unstructured{Object: obj}
}

func ociRepository(ns, semver string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "source.toolkit.fluxcd.io/v1", "kind": "OCIRepository",
		"metadata": map[string]any{"name": "agent", "namespace": ns},
		"spec":     map[string]any{"interval": "30m", "url": agents.DefaultChartOCIURL, "ref": map[string]any{"semver": semver}},
	}}
}

// helmRelease is a portal-created release of the agent chart deployed at
// chartVersion, Ready.
func helmRelease(ns, name string, values map[string]any, chartVersion string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "helm.toolkit.fluxcd.io/v2", "kind": "HelmRelease",
		"metadata": map[string]any{"name": name, "namespace": ns, "labels": map[string]any{agents.ManagedByLabel: agents.ManagedByValue}},
		"spec": map[string]any{
			"interval": "10m",
			"chartRef": map[string]any{"kind": "OCIRepository", "name": "agent", "namespace": ns},
			"values":   values,
		},
		"status": map[string]any{
			"conditions":            []any{map[string]any{"type": "Ready", "status": "True", "reason": "UpgradeSucceeded", "message": "Helm upgrade succeeded"}},
			"history":               []any{map[string]any{"version": int64(3), "chartVersion": chartVersion, "status": "deployed", "lastDeployed": "2026-09-11T10:00:00Z"}},
			"lastAttemptedRevision": chartVersion,
		},
	}}
}

// kagentRelease is the kagent chart's own release and source: the owner of a
// bundled example agent, not a Generic-chart release.
func kagentRelease() []runtime.Object {
	return []runtime.Object{
		&unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "helm.toolkit.fluxcd.io/v2", "kind": "HelmRelease",
			"metadata": map[string]any{"name": "kagent", "namespace": "agent-platform"},
			"spec":     map[string]any{"interval": "10m", "chartRef": map[string]any{"kind": "OCIRepository", "name": "kagent", "namespace": "agent-platform"}, "targetNamespace": "kagent"},
		}},
		&unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "source.toolkit.fluxcd.io/v1", "kind": "OCIRepository",
			"metadata": map[string]any{"name": "kagent", "namespace": "agent-platform"},
			"spec":     map[string]any{"interval": "10m", "url": "oci://ghcr.io/giantswarm/kagent/helm/kagent", "ref": map[string]any{"semver": ">=0.11.0-gs.1 <0.11.1-0"}},
		}},
	}
}

func harness(ns string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": kagentAPI, "kind": "Harness",
		"metadata": map[string]any{"name": "kagent", "namespace": ns},
		"spec":     map[string]any{"allowedAgentTemplates": map[string]any{"selector": map[string]any{"matchLabels": map[string]any{"agent-platform.giantswarm.io/harness": "kagent"}}}},
	}}
}

// legacyAgent is a minimal kagent.dev/v1alpha2 Agent rendered by hrNs/hrName.
func legacyAgentObj(ns, name, hrName, hrNs string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kagent.dev/v1alpha2", "kind": "Agent",
		"metadata": map[string]any{"name": name, "namespace": ns, "labels": map[string]any{agents.HelmReleaseNameLabel: hrName, agents.HelmReleaseNamespaceLabel: hrNs}},
		"spec":     map[string]any{"type": "Declarative", "declarative": map[string]any{"runtime": "go", "modelConfig": "default-model-config"}},
	}}
}

// agentTemplate is what chart 1.x renders for hrNs/hrName, on the kagent
// Harness, Ready or still compiling.
func agentTemplate(ns, name, hrName, hrNs string, ready bool) *unstructured.Unstructured {
	readyCond := map[string]any{"type": "Ready", "status": "False", "reason": "ActorTemplatePending", "message": "waiting for the golden snapshot"}
	latest := ""
	if ready {
		readyCond = map[string]any{"type": "Ready", "status": "True", "reason": "Ready"}
		latest = "rev-1"
	}
	entry := map[string]any{"harness": "kagent", "desiredRevision": "rev-1", "latestSuccessfulRevision": latest, "conditions": []any{
		map[string]any{"type": "Accepted", "status": "True", "reason": "Accepted"},
		map[string]any{"type": "ResolvedRefs", "status": "True", "reason": "ResolvedRefs"},
		map[string]any{"type": "Compatible", "status": "True", "reason": "Compatible"},
		readyCond,
	}}
	labels := map[string]any{"agent-platform.giantswarm.io/harness": "kagent"}
	if hrName != "" {
		labels[agents.HelmReleaseNameLabel] = hrName
		labels[agents.HelmReleaseNamespaceLabel] = hrNs
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": kagentAPI, "kind": "AgentTemplate",
		"metadata": map[string]any{"name": name, "namespace": ns, "generation": int64(1), "labels": labels},
		"spec":     map[string]any{"description": name, "modelConfig": map[string]any{"name": "default-model-config"}},
		"status":   map[string]any{"observedGeneration": int64(1), "harnesses": []any{entry}},
	}}
}

func crd(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
		"metadata": map[string]any{"name": name, "annotations": map[string]any{"helm.sh/resource-policy": "keep"}},
	}}
}

func removedCRDs() []runtime.Object {
	var out []runtime.Object
	for _, name := range RemovedCRDs {
		out = append(out, crd(name))
	}
	return out
}

// portalValues are the 0.x values a portal-created release with git skills
// on branches carries (what the 0.244 portal and the 0.4 agent-manager
// composed, plus the chart-level keys a hand edit may have added).
func portalValues() map[string]any {
	return map[string]any{
		"agent":       map[string]any{"name": "sre", "displayName": "SRE Assistant", "description": "helps", "iconUrl": "https://avatars.example/v1/sre.png", "systemMessage": "Be brief.", "runtime": "go"},
		"modelConfig": map[string]any{"name": "default-model-config"},
		"skills": map[string]any{
			"gitRefs": []any{
				map[string]any{"url": skillsRepo, "ref": "main", "path": "nested/runbooks", "name": "runbooks"},
				map[string]any{"url": skillsRepo, "ref": "feature", "path": "agent-self-awareness"},
			},
			"refs":             []any{kubectlTag},
			"gitAuthSecretRef": map[string]any{"name": "kagent-skills-token"},
		},
		"toolset":     []any{"preset:read-only", "workflow:incident-triage"},
		"labels":      map[string]any{"team": "sre"},
		"annotations": map[string]any{"owner": "sre@example.com"},
		"extraTools":  []any{map[string]any{"mcp": map[string]any{"server": map[string]any{"kind": "RemoteMCPServer", "name": "github"}}}},
		"replicas":    int64(2),
		"resources":   map[string]any{"requests": map[string]any{"cpu": "100m"}},
	}
}

// portalRewritten is what portalValues become: the removed keys gone, every
// skill pinned, everything else kept.
func portalRewritten() map[string]any {
	return map[string]any{
		"agent":       map[string]any{"name": "sre", "displayName": "SRE Assistant", "description": "helps", "iconUrl": "https://avatars.example/v1/sre.png", "systemMessage": "Be brief."},
		"modelConfig": map[string]any{"name": "default-model-config"},
		"skills": []any{
			map[string]any{"name": "runbooks", "path": "nested/runbooks", "git": map[string]any{"url": skillsRepo, "commit": mainHead}},
			map[string]any{"name": "agent-self-awareness", "path": "agent-self-awareness", "git": map[string]any{"url": skillsRepo, "commit": featureHead}},
			map[string]any{"name": "kubectl", "oci": kubectlPin},
		},
		"toolset":     []any{"preset:read-only", "workflow:incident-triage"},
		"labels":      map[string]any{"team": "sre"},
		"annotations": map[string]any{"owner": "sre@example.com"},
		"extraTools":  []any{map[string]any{"mcp": map[string]any{"server": map[string]any{"kind": "RemoteMCPServer", "name": "github"}}}},
	}
}

// narrowValues is a release narrowing muster's meta-tools with toolNames.
func narrowValues() map[string]any {
	return map[string]any{
		"agent":       map[string]any{"name": "narrow", "displayName": "Narrow", "runtime": "python"},
		"modelConfig": map[string]any{"name": "default-model-config"},
		"muster":      map[string]any{"serverRef": map[string]any{"namespace": "agent-platform"}, "allowedHeaders": []any{"authorization"}, "toolNames": []any{"list_tools", "call_tool"}},
		"toolset":     []any{"preset:infrastructure"},
	}
}

func narrowRewritten() map[string]any {
	return map[string]any{
		"agent":       map[string]any{"name": "narrow", "displayName": "Narrow"},
		"modelConfig": map[string]any{"name": "default-model-config"},
		"muster":      map[string]any{"tools": []any{"list_tools", "call_tool"}},
		"toolset":     []any{"preset:infrastructure"},
	}
}

// sreAgentRewritten is the fleet's GitOps-owned release after the rewrite.
func sreAgentRewritten() map[string]any {
	return map[string]any{
		"agent":  map[string]any{"displayName": "SRE Agent", "description": "Giant Swarm SRE agent", "systemMessage": "You are a Giant Swarm SRE agent. Prefer read-only diagnosis.\n"},
		"muster": map[string]any{"tools": []any{"list_tools", "describe_tool", "filter_tools", "call_tool"}},
	}
}

// fleet seeds the four fleet shapes in kagent plus an agent in a second
// managed namespace: (1) sre, portal-created with git skills on branches,
// (2) narrow with muster.toolNames, (3) the GitOps-owned sre-agent in
// flux-giantswarm reached through its Agent's provenance labels, (4) the
// bundled k8s-agent rendered by the kagent chart, and tenant/tenant-bot.
func fleet(t *testing.T) []runtime.Object {
	t.Helper()
	objs := []runtime.Object{
		ociRepository("kagent", legacyRange),
		helmRelease("kagent", "sre", portalValues(), "0.6.1"), load(t, "agent-v1alpha2-portal.yaml"),
		helmRelease("kagent", "narrow", narrowValues(), "0.6.1"), legacyAgentObj("kagent", "narrow", "narrow", "kagent"),
		load(t, "helmrelease-gitops-sre-agent.yaml"), load(t, "ocirepository-gitops.yaml"), load(t, "agent-v1alpha2-gitops.yaml"),
		load(t, "agent-v1alpha2-bundled.yaml"),
		ociRepository("tenant", "x.x.x"),
		helmRelease("tenant", "tenant-bot", map[string]any{"agent": map[string]any{"name": "tenant-bot", "runtime": "go"}, "modelConfig": map[string]any{"name": "default-model-config"}}, "0.6.0"),
		legacyAgentObj("tenant", "tenant-bot", "tenant-bot", "tenant"),
		harness("kagent"), harness("tenant"),
	}
	objs = append(objs, kagentRelease()...)
	return append(objs, removedCRDs()...)
}

func byRelease(r *Report) map[string]ReleaseReport {
	out := map[string]ReleaseReport{}
	for _, rel := range r.Releases {
		out[rel.Namespace+"/"+rel.Name] = rel
	}
	return out
}

func bySource(r *Report) map[string]SourceReport {
	out := map[string]SourceReport{}
	for _, s := range r.Sources {
		out[s.Namespace+"/"+s.Name] = s
	}
	return out
}

func byAgent(r *Report) map[string]AgentReport {
	out := map[string]AgentReport{}
	for _, a := range r.Agents {
		out[a.Name] = a
	}
	return out
}

var twoNamespaces = Options{Namespaces: []string{"kagent", "tenant"}}

// ---- expand ---------------------------------------------------------------------

func TestExpandRewritesTheFourFleetShapes(t *testing.T) {
	ctx := context.Background()
	c := newCluster(fleet(t)...)
	gitopsBefore := c.get(t, hrGVR, "flux-giantswarm", "sre-agent")
	gitopsSourceBefore := c.get(t, ociGVR, "flux-giantswarm", "agent")
	r := c.runner(t, twoNamespaces, "1.0.0", "secret")

	res, err := r.Run(ctx)
	require.NoError(t, err)
	require.Len(t, res.Reports, 2)
	kagent, tenant := res.Reports[0], res.Reports[1]
	releases, sources, legacy := byRelease(kagent), bySource(kagent), byAgent(kagent)

	// (1) portal-created: removed keys gone, toolNames untouched (none), every
	// skill pinned to a full commit or a digest, iconUrl/toolset/labels/
	// annotations/extraTools kept; written and valid against the 1.x schema.
	sre := releases["kagent/sre"]
	assert.Equal(t, ActionRewritten, sre.Action)
	assert.Equal(t, OwnershipHelmRelease, sre.Ownership)
	require.NotNil(t, sre.Changes)
	assert.Equal(t, []string{"agent.runtime", "replicas", "resources", "skills.gitAuthSecretRef"}, sre.Changes.Removed)
	assert.Empty(t, sre.Changes.Renamed)
	assert.Equal(t, []SkillPin{
		{Name: "runbooks", Source: skillsRepo + "@main path=nested/runbooks", Pinned: mainHead},
		{Name: "agent-self-awareness", Source: skillsRepo + "@feature path=agent-self-awareness", Pinned: featureHead},
		{Name: "kubectl", Source: kubectlTag, Pinned: kubectlPin},
	}, sre.Changes.Skills)
	written := c.values(t, "kagent", "sre")
	assert.Equal(t, portalRewritten(), written)
	_, violations := agents.ValidateValues(ctx, fakeChart{}, written)
	assert.Empty(t, violations, "the written values satisfy the 1.x schema")

	// (2) muster.toolNames becomes muster.tools; the serverRef block goes.
	narrow := releases["kagent/narrow"]
	assert.Equal(t, ActionRewritten, narrow.Action)
	assert.Equal(t, []string{"agent.runtime", "muster.serverRef", "muster.allowedHeaders"}, narrow.Changes.Removed)
	assert.Equal(t, []string{"muster.toolNames -> muster.tools"}, narrow.Changes.Renamed)
	assert.Equal(t, narrowRewritten(), c.values(t, "kagent", "narrow"))

	// (3) GitOps-owned in another namespace, found through the Agent's
	// provenance labels: never written, the diff equals the rewrite.
	gitops := releases["flux-giantswarm/sre-agent"]
	assert.Equal(t, ActionDiff, gitops.Action)
	assert.Equal(t, OwnershipExternal, gitops.Ownership)
	assert.Equal(t, "kagent", gitops.TargetNamespace)
	assert.Equal(t, []string{"agent.runtime", "muster.serverRef"}, gitops.Changes.Removed)
	assert.Equal(t, []string{"muster.toolNames -> muster.tools"}, gitops.Changes.Renamed)
	assert.Equal(t, []string{"/spec/declarative/deployment/podSecurityContext", "/spec/declarative/deployment/securityContext"}, gitops.Changes.DriftIgnoreRemoved)
	assert.Equal(t, gitopsBefore, c.get(t, hrGVR, "flux-giantswarm", "sre-agent"), "the GitOps-owned release is not written")
	expected := &unstructured.Unstructured{Object: runtime.DeepCopyJSON(gitopsBefore.Object)}
	require.NoError(t, unstructured.SetNestedMap(expected.Object, sreAgentRewritten(), "spec", "values"))
	unstructured.RemoveNestedField(expected.Object, "spec", "driftDetection", "ignore")
	assert.Equal(t, manifestDiff(gitopsBefore, expected), gitops.Diff, "the emitted diff is the rewrite")
	for _, line := range []string{"-      runtime: python", "-      toolNames:", "+      tools:", "-      - /spec/declarative/deployment/podSecurityContext", "     mode: enabled"} {
		assert.Contains(t, gitops.Diff, line+"\n")
	}
	assert.NotContains(t, gitops.Diff, "-    mode: enabled", "drift detection stays enabled")
	assert.NotContains(t, gitops.Diff, "kustomize.toolkit.fluxcd.io", "Flux's provenance labels are not part of the manifest in git")
	assert.NotContains(t, gitops.Diff, "resourceVersion")
	gitopsSource := sources["flux-giantswarm/agent"]
	assert.Equal(t, ActionDiff, gitopsSource.Action)
	assert.Equal(t, OwnershipExternal, gitopsSource.Ownership)
	assert.Contains(t, gitopsSource.Diff, "-    semver: '>=0.2.1 <1.0.0'\n")
	assert.Contains(t, gitopsSource.Diff, "+    semver: 1.x\n")
	assert.Equal(t, gitopsSourceBefore, c.get(t, ociGVR, "flux-giantswarm", "agent"), "the GitOps-owned source is not written")

	// (4) the bundled example agent has no Generic-chart release: not
	// migratable here, listed for the contract phase.
	bundled := legacy["k8s-agent"]
	assert.Equal(t, AgentNotMigratable, bundled.Action)
	assert.Equal(t, "agent-platform/kagent", bundled.Owner)
	assert.Contains(t, bundled.Reason, "not a Generic-chart release")
	assert.Equal(t, AgentAwaitingUpgrade, legacy["sre"].Action, "the portal agent's Agent goes with its release's upgrade")
	assert.Equal(t, AgentAwaitingUpgrade, legacy["sre-agent"].Action)
	assert.Equal(t, "flux-giantswarm/sre-agent", legacy["sre-agent"].Owner)
	assert.True(t, c.exists(legacyAgentGVR, "kagent", "k8s-agent"), "nothing is deleted before the contract phase")

	// The namespace's source moves last, once every release it serves is on
	// 1.x values.
	assert.Equal(t, SourceMoved, sources["kagent/agent"].Action)
	assert.Equal(t, legacyRange, sources["kagent/agent"].From)
	assert.Equal(t, target, c.semver(t, "kagent", "agent"))

	// The GitOps-owned release keeps the namespace in the expand phase until
	// its diff is merged; the report is in the namespace.
	assert.Equal(t, PhaseExpand, kagent.Phase)
	assert.True(t, kagent.Changed)
	require.Len(t, kagent.Pending, 1)
	assert.Contains(t, kagent.Pending[0], "release flux-giantswarm/sre-agent")
	cm := c.reportConfigMap(t, "kagent")
	require.NotNil(t, cm)
	assert.Equal(t, PhaseExpand, cm[ReportKeyPhase])
	assert.Contains(t, cm[ReportKeyReport], "name: sre-agent")
	assert.Contains(t, cm[ReportKeyReport], "latestVersion: 1.0.0")

	// The agent in the other managed namespace: rewritten, its own source
	// moved from the open range; Flux has not upgraded it yet, so it waits.
	bot := byRelease(tenant)["tenant/tenant-bot"]
	assert.Equal(t, ActionRewritten, bot.Action)
	assert.Equal(t, []string{"agent.runtime"}, bot.Changes.Removed)
	assert.Equal(t, target, c.semver(t, "tenant", "agent"))
	assert.Equal(t, PhaseWait, tenant.Phase)
	assert.Equal(t, PhaseWait, c.reportConfigMap(t, "tenant")[ReportKeyPhase])
	require.Len(t, tenant.Pending, 2)
	assert.Contains(t, tenant.Pending[0], `release tenant/tenant-bot is deployed from chart "0.6.0"`)
	assert.Contains(t, tenant.Pending[1], "Agent tenant/tenant-bot is still rendered by release tenant/tenant-bot")

	// A second run changes nothing and says so.
	sreAfter, narrowAfter := c.get(t, hrGVR, "kagent", "sre"), c.get(t, hrGVR, "kagent", "narrow")
	res, err = r.Run(ctx)
	require.NoError(t, err)
	again := res.Reports[0]
	assert.False(t, again.Changed)
	assert.Equal(t, ActionUnchanged, byRelease(again)["kagent/sre"].Action)
	assert.Equal(t, ActionUnchanged, byRelease(again)["kagent/narrow"].Action)
	assert.Equal(t, ActionUnchanged, bySource(again)["kagent/agent"].Action)
	assert.Equal(t, gitops.Diff, byRelease(again)["flux-giantswarm/sre-agent"].Diff, "the diff stays until it is applied in git")
	assert.Equal(t, sreAfter, c.get(t, hrGVR, "kagent", "sre"))
	assert.Equal(t, narrowAfter, c.get(t, hrGVR, "kagent", "narrow"))
	assert.False(t, res.Reports[1].Changed)
}

func TestPrivateSkillRepositoryWithoutTokenIsPendingNotACrash(t *testing.T) {
	ctx := context.Background()
	private := map[string]any{"agent": map[string]any{"name": "private"}, "modelConfig": map[string]any{"name": "default-model-config"},
		"skills": map[string]any{"gitRefs": []any{map[string]any{"url": privateRepo, "ref": "trunk", "path": "secret-runbooks"}}, "gitAuthSecretRef": map[string]any{"name": "kagent-skills-token"}}}
	c := newCluster(ociRepository("kagent", legacyRange), helmRelease("kagent", "sre", portalValues(), "0.6.1"), helmRelease("kagent", "private", private, "0.6.1"), harness("kagent"))
	one := Options{Namespaces: []string{"kagent"}}

	res, err := c.runner(t, one, "1.0.0", "").Run(ctx)
	require.NoError(t, err, "an unresolvable reference is reported, not an error")
	rep := res.Reports[0]
	pend := byRelease(rep)["kagent/private"]
	assert.Equal(t, ActionPending, pend.Action)
	assert.Contains(t, pend.Reason, "GITHUB_TOKEN")
	assert.Contains(t, pend.Reason, privateRepo)
	assert.Equal(t, private, c.values(t, "kagent", "private"), "left as it is")
	assert.Equal(t, ActionRewritten, byRelease(rep)["kagent/sre"].Action, "the public one is migrated")
	src := bySource(rep)["kagent/agent"]
	assert.Equal(t, SourceNotMoved, src.Action)
	assert.Contains(t, src.Reason, "kagent/private (pending)")
	assert.Equal(t, legacyRange, c.semver(t, "kagent", "agent"), "a pending release leaves the range untouched")
	assert.Equal(t, PhaseExpand, rep.Phase)

	// With a token that reads the repository the ref resolves and the range
	// moves.
	res, err = c.runner(t, one, "1.0.0", "secret").Run(ctx)
	require.NoError(t, err)
	rep = res.Reports[0]
	done := byRelease(rep)["kagent/private"]
	assert.Equal(t, ActionRewritten, done.Action)
	assert.Equal(t, []SkillPin{{Name: "secret-runbooks", Source: privateRepo + "@trunk path=secret-runbooks", Pinned: trunkHead}}, done.Changes.Skills)
	assert.Equal(t, SourceMoved, bySource(rep)["kagent/agent"].Action)
	assert.Equal(t, target, c.semver(t, "kagent", "agent"))
}

func TestNoTargetChartInTheRegistryWritesNothing(t *testing.T) {
	c := newCluster(ociRepository("kagent", legacyRange), helmRelease("kagent", "sre", portalValues(), "0.6.1"), harness("kagent"))
	res, err := c.runner(t, Options{Namespaces: []string{"kagent"}}, "", "secret").Run(context.Background())
	require.NoError(t, err)
	rep := res.Reports[0]
	sre := byRelease(rep)["kagent/sre"]
	assert.Equal(t, ActionPending, sre.Action)
	assert.Contains(t, sre.Reason, "no version of the chart in 1.x")
	assert.NotNil(t, sre.Changes, "the report still says what the rewrite will do")
	assert.Equal(t, portalValues(), c.values(t, "kagent", "sre"))
	assert.Equal(t, SourceNotMoved, bySource(rep)["kagent/agent"].Action)
	assert.Equal(t, legacyRange, c.semver(t, "kagent", "agent"))
	assert.False(t, rep.Changed)
	assert.Empty(t, rep.Run.Chart.LatestVersion)
}

func TestDryRunPrintsTheReportAndWritesNothing(t *testing.T) {
	ctx := context.Background()
	c := newCluster(fleet(t)...)
	opts := twoNamespaces
	opts.DryRun = true
	res, err := c.runner(t, opts, "1.0.0", "secret").Run(ctx)
	require.NoError(t, err)
	rep := res.Reports[0]
	assert.True(t, rep.Run.DryRun)
	assert.False(t, rep.Changed)
	sre := byRelease(rep)["kagent/sre"]
	assert.Equal(t, ActionRewritten, sre.Action)
	assert.Equal(t, "dry run: would be written", sre.Reason)
	require.Len(t, sre.Changes.Skills, 3, "three skills would be pinned")
	assert.Equal(t, portalValues(), c.values(t, "kagent", "sre"), "nothing is written")
	assert.Equal(t, SourceMoved, bySource(rep)["kagent/agent"].Action)
	assert.Equal(t, "dry run: would be moved", bySource(rep)["kagent/agent"].Reason)
	assert.Equal(t, legacyRange, c.semver(t, "kagent", "agent"))
	assert.Contains(t, byRelease(rep)["flux-giantswarm/sre-agent"].Diff, "+      tools:", "the diffs are in the report")
	assert.Nil(t, c.reportConfigMap(t, "kagent"), "no report ConfigMap either")
	assert.Nil(t, c.reportConfigMap(t, "tenant"))
	assert.Contains(t, rep.String(), "dry run: nothing was written")
}

// ---- wait and contract -------------------------------------------------------------

// migrated is a namespace after the expand phase: values on 1.x, the source
// on the target range, sre already upgraded by Flux and Ready, narrow still
// on the 0.x chart with its v1alpha2 Agent, the bundled agent left behind.
func migrated(t *testing.T) []runtime.Object {
	t.Helper()
	objs := []runtime.Object{
		ociRepository("kagent", target),
		helmRelease("kagent", "sre", portalRewritten(), "1.0.0"), agentTemplate("kagent", "sre", "sre", "kagent", true),
		helmRelease("kagent", "narrow", narrowRewritten(), "0.6.1"), legacyAgentObj("kagent", "narrow", "narrow", "kagent"),
		load(t, "agent-v1alpha2-bundled.yaml"),
		harness("kagent"),
	}
	objs = append(objs, kagentRelease()...)
	return append(objs, removedCRDs()...)
}

func TestWaitGatesTheContractAndContractRemovesTheOldWorld(t *testing.T) {
	ctx := context.Background()
	c := newCluster(migrated(t)...)
	r := c.runner(t, Options{Namespaces: []string{"kagent"}}, "1.0.0", "secret")

	// Flux has not upgraded narrow: wait, nothing deleted.
	res, err := r.Run(ctx)
	require.NoError(t, err)
	rep := res.Reports[0]
	assert.Equal(t, PhaseWait, rep.Phase)
	assert.False(t, rep.Changed)
	require.Len(t, rep.Pending, 2)
	assert.Contains(t, rep.Pending[0], `release kagent/narrow is deployed from chart "0.6.1"`)
	assert.Contains(t, rep.Pending[1], "Agent kagent/narrow is still rendered by release kagent/narrow")
	assert.Nil(t, rep.Contract)
	assert.True(t, c.exists(legacyAgentGVR, "kagent", "k8s-agent"))
	assert.True(t, c.exists(crdGVR, "", "agents.kagent.dev"))
	assert.Equal(t, agents.VerdictReady, byRelease(rep)["kagent/sre"].Template.Verdict)
	assert.Equal(t, PhaseWait, c.reportConfigMap(t, "kagent")[ReportKeyPhase])

	// Flux upgrades narrow and Helm replaces its Agent with a template that
	// is still compiling; a bare template of the namespace compiles too.
	narrow := c.get(t, hrGVR, "kagent", "narrow")
	require.NoError(t, unstructured.SetNestedSlice(narrow.Object, []any{map[string]any{"version": int64(4), "chartVersion": "1.0.0", "status": "deployed"}}, "status", "history"))
	c.update(t, hrGVR, narrow)
	c.delete(t, legacyAgentGVR, "kagent", "narrow")
	c.create(t, tplGVR, agentTemplate("kagent", "narrow", "narrow", "kagent", false))
	c.create(t, tplGVR, agentTemplate("kagent", "bare", "", "", false))
	res, err = r.Run(ctx)
	require.NoError(t, err)
	rep = res.Reports[0]
	assert.Equal(t, PhaseWait, rep.Phase)
	require.Len(t, rep.Pending, 2)
	assert.Contains(t, rep.Pending[0], "AgentTemplate kagent/bare is progressing")
	assert.Contains(t, rep.Pending[1], "AgentTemplate kagent/narrow is progressing")
	assert.Equal(t, agents.VerdictProgressing, byRelease(rep)["kagent/narrow"].Template.Verdict)
	assert.True(t, c.exists(legacyAgentGVR, "kagent", "k8s-agent"), "the gate holds the contract")

	// Everything Ready: the leftover Agent and the five CRDs go.
	for _, name := range []string{"narrow", "bare"} {
		tpl := c.get(t, tplGVR, "kagent", name)
		entries, _, _ := unstructured.NestedSlice(tpl.Object, "status", "harnesses")
		entry := entries[0].(map[string]any)
		entry["latestSuccessfulRevision"] = "rev-1"
		entry["conditions"].([]any)[3] = map[string]any{"type": "Ready", "status": "True", "reason": "Ready"}
		require.NoError(t, unstructured.SetNestedSlice(tpl.Object, entries, "status", "harnesses"))
		c.update(t, tplGVR, tpl)
	}
	res, err = r.Run(ctx)
	require.NoError(t, err)
	rep = res.Reports[0]
	assert.Equal(t, PhaseContract, rep.Phase)
	assert.True(t, rep.Changed)
	assert.Empty(t, rep.Pending)
	require.NotNil(t, rep.Contract)
	assert.Equal(t, []string{"k8s-agent"}, rep.Contract.AgentsDeleted)
	assert.Equal(t, AgentDeleted, byAgent(rep)["k8s-agent"].Action)
	require.Len(t, rep.Contract.CRDs, len(RemovedCRDs))
	for i, crdRep := range rep.Contract.CRDs {
		assert.Equal(t, RemovedCRDs[i], crdRep.Name)
		assert.Equal(t, CRDDeleted, crdRep.Action)
		assert.False(t, c.exists(crdGVR, "", crdRep.Name))
	}
	assert.False(t, c.exists(legacyAgentGVR, "kagent", "k8s-agent"))
	assert.Contains(t, rep.Summary, "1 kagent.dev/v1alpha2 Agent object(s) deleted")
	assert.Equal(t, PhaseContract, c.reportConfigMap(t, "kagent")[ReportKeyPhase])

	// Afterwards there is nothing left, and a run says so without changing
	// anything (the v1alpha2 CRD is gone, so its list answers not found).
	res, err = r.Run(ctx)
	require.NoError(t, err)
	rep = res.Reports[0]
	assert.Equal(t, PhaseComplete, rep.Phase)
	assert.False(t, rep.Changed)
	assert.Contains(t, rep.Summary, "nothing left to migrate")
	assert.Empty(t, rep.Agents)
	for _, crdRep := range rep.Contract.CRDs {
		assert.Equal(t, CRDAbsent, crdRep.Action)
	}
}

func TestContractDryRunDeletesNothing(t *testing.T) {
	ctx := context.Background()
	c := newCluster(
		ociRepository("kagent", target), helmRelease("kagent", "sre", portalRewritten(), "1.0.0"), agentTemplate("kagent", "sre", "sre", "kagent", true),
		load(t, "agent-v1alpha2-bundled.yaml"), harness("kagent"), crd("agents.kagent.dev"),
	)
	res, err := c.runner(t, Options{Namespaces: []string{"kagent"}, DryRun: true}, "1.0.0", "").Run(ctx)
	require.NoError(t, err)
	rep := res.Reports[0]
	assert.Equal(t, PhaseContract, rep.Phase)
	assert.False(t, rep.Changed)
	assert.Equal(t, []string{"k8s-agent"}, rep.Contract.AgentsDeleted)
	assert.Contains(t, byAgent(rep)["k8s-agent"].Reason, "dry run")
	assert.Equal(t, CRDDeleted, rep.Contract.CRDs[0].Action)
	assert.Contains(t, rep.Contract.CRDs[0].Reason, "dry run")
	assert.Equal(t, CRDAbsent, rep.Contract.CRDs[1].Action)
	assert.True(t, c.exists(legacyAgentGVR, "kagent", "k8s-agent"))
	assert.True(t, c.exists(crdGVR, "", "agents.kagent.dev"))
	assert.Nil(t, c.reportConfigMap(t, "kagent"))
}

func TestCRDsStayWhileAgentsExistOutsideTheManagedNamespaces(t *testing.T) {
	c := newCluster(harness("kagent"), legacyAgentObj("elsewhere", "stray", "stray", "elsewhere"), crd("agents.kagent.dev"))
	res, err := c.runner(t, Options{Namespaces: []string{"kagent"}}, "1.0.0", "").Run(context.Background())
	require.NoError(t, err)
	rep := res.Reports[0]
	assert.Equal(t, PhaseContract, rep.Phase)
	assert.Equal(t, CRDKept, rep.Contract.CRDs[0].Action)
	assert.Contains(t, rep.Contract.CRDs[0].Reason, "elsewhere/stray")
	assert.True(t, c.exists(crdGVR, "", "agents.kagent.dev"))
}

// TestNothingToMigrateIsComplete is a namespace on the v2 stack only (the
// lab, a fresh installation): no 0.x release, no v1alpha2 CRD — complete,
// whatever the templates there are doing.
func TestNothingToMigrateIsComplete(t *testing.T) {
	fresh := map[string]any{"agent": map[string]any{"name": "fresh", "harness": "kagent"}, "modelConfig": map[string]any{"name": "default-model-config"}, "toolset": []any{"preset:none"}}
	c := newCluster(harness("kagent"), agentTemplate("kagent", "probe", "", "", false), ociRepository("kagent", target), helmRelease("kagent", "fresh", fresh, "1.0.0"), agentTemplate("kagent", "fresh", "fresh", "kagent", true))
	res, err := c.runner(t, Options{Namespaces: []string{"kagent"}}, "1.0.0", "").Run(context.Background())
	require.NoError(t, err)
	rep := res.Reports[0]
	assert.Equal(t, PhaseComplete, rep.Phase)
	assert.False(t, rep.Changed)
	assert.Contains(t, rep.Summary, "nothing left to migrate")
	assert.Contains(t, rep.Summary, "1 item(s) not Ready yet")
	assert.Equal(t, ActionUnchanged, byRelease(rep)["kagent/fresh"].Action)
	assert.Empty(t, rep.Agents)
	assert.Empty(t, rep.Pending, "readiness gates nothing when there is nothing to contract")
	require.Len(t, rep.Warnings, 1)
	assert.Contains(t, rep.Warnings[0], "AgentTemplate kagent/probe is progressing")
	assert.Equal(t, PhaseComplete, c.reportConfigMap(t, "kagent")[ReportKeyPhase])
}

// ---- the rewrite alone --------------------------------------------------------------

func TestRewriteValues(t *testing.T) {
	ctx := context.Background()
	p := pinner{git: skills.NewResolver(fakeGitHub(t).URL, "", nil, nil)}
	cases := map[string]struct {
		values  map[string]any
		harness string
		want    map[string]any
		changes ValueChanges
	}{
		"1.x values pass through": {
			values: portalRewritten(), want: portalRewritten(),
		},
		"the platform Harness is composed when it is not the chart default": {
			values: map[string]any{"agent": map[string]any{"name": "a"}}, harness: "claude",
			want:    map[string]any{"agent": map[string]any{"name": "a", "harness": "claude"}},
			changes: ValueChanges{Set: []string{"agent.harness=claude"}},
		},
		"the default Harness adds nothing": {
			values: map[string]any{"agent": map[string]any{"name": "a"}}, harness: "kagent",
			want: map[string]any{"agent": map[string]any{"name": "a"}},
		},
		"an empty 0.x skills object goes": {
			values: map[string]any{"skills": map[string]any{"refs": []any{}, "gitRefs": []any{}}},
			want:   map[string]any{},
		},
		"a commit given as the 0.x ref is the pin, no lookup": {
			values:  map[string]any{"skills": map[string]any{"gitRefs": []any{map[string]any{"url": privateRepo, "ref": trunkHead, "path": "x"}}}},
			want:    map[string]any{"skills": []any{map[string]any{"name": "x", "path": "x", "git": map[string]any{"url": privateRepo, "commit": trunkHead}}}},
			changes: ValueChanges{Skills: []SkillPin{{Name: "x", Source: privateRepo + "@" + trunkHead + " path=x", Pinned: trunkHead}}},
		},
		"a 1.x list entry with a ref is resolved": {
			values:  map[string]any{"skills": []any{map[string]any{"name": "rb", "git": map[string]any{"url": skillsRepo, "ref": "main"}}}},
			want:    map[string]any{"skills": []any{map[string]any{"name": "rb", "git": map[string]any{"url": skillsRepo, "commit": mainHead}}}},
			changes: ValueChanges{Skills: []SkillPin{{Name: "rb", Source: skillsRepo + "@main", Pinned: mainHead}}},
		},
		"muster becomes empty and goes": {
			values:  map[string]any{"muster": map[string]any{"serverRef": map[string]any{"namespace": "agent-platform"}, "stsWellKnownUri": ""}},
			want:    map[string]any{},
			changes: ValueChanges{Removed: []string{"muster.serverRef", "muster.stsWellKnownUri"}},
		},
		"muster.enabled and a 1.x muster.tools are kept": {
			values:  map[string]any{"muster": map[string]any{"enabled": false, "tools": []any{"call_tool"}, "toolNames": []any{"list_tools"}}},
			want:    map[string]any{"muster": map[string]any{"enabled": false, "tools": []any{"call_tool"}}},
			changes: ValueChanges{Renamed: []string{"muster.toolNames -> muster.tools"}},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, ch, err := rewriteValues(ctx, tc.values, tc.harness, p)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.changes, *ch)
		})
	}

	t.Run("an unknown ref is an invalid request", func(t *testing.T) {
		_, _, err := rewriteValues(ctx, map[string]any{"skills": map[string]any{"gitRefs": []any{map[string]any{"url": skillsRepo, "ref": "nope"}}}}, "", p)
		assert.ErrorIs(t, err, agents.ErrInvalid)
		assert.ErrorIs(t, err, skills.ErrUnresolvable)
	})
	t.Run("the input is never modified", func(t *testing.T) {
		in := portalValues()
		_, _, err := rewriteValues(ctx, in, "", p)
		require.NoError(t, err)
		assert.Equal(t, portalValues(), in)
	})
}

func TestRemoveDriftIgnoreKeepsOtherPaths(t *testing.T) {
	hr := load(t, "helmrelease-gitops-sre-agent.yaml")
	ignore := []any{
		map[string]any{"paths": []any{"/spec/declarative/deployment/podSecurityContext", "/metadata/annotations/example"}},
		map[string]any{"paths": []any{"/spec/declarative/deployment/securityContext"}, "target": map[string]any{"kind": "Agent"}},
	}
	require.NoError(t, unstructured.SetNestedSlice(hr.Object, ignore, "spec", "driftDetection", "ignore"))
	removed := removeDriftIgnore(hr)
	assert.Equal(t, []string{"/spec/declarative/deployment/podSecurityContext", "/spec/declarative/deployment/securityContext"}, removed)
	kept, _, _ := unstructured.NestedSlice(hr.Object, "spec", "driftDetection", "ignore")
	assert.Equal(t, []any{map[string]any{"paths": []any{"/metadata/annotations/example"}}}, kept)
	mode, _, _ := unstructured.NestedString(hr.Object, "spec", "driftDetection", "mode")
	assert.Equal(t, "enabled", mode, "drift detection itself stays")
	assert.Nil(t, removeDriftIgnore(hr), "idempotent")
}

func TestVersionInRange(t *testing.T) {
	assert.True(t, versionInRange("1.0.0", target))
	assert.True(t, versionInRange("1.2.3+abcdef", target))
	assert.True(t, versionInRange("1.0.1@sha256:0f1e", target))
	assert.False(t, versionInRange("0.6.1", target))
	assert.False(t, versionInRange("1.0.0-dev.branch.h123", target), "a pre-release build never satisfies 1.x")
	assert.False(t, versionInRange("", target))
}
