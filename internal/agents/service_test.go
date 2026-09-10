package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"

	"github.com/giantswarm/agent-manager/internal/chart"
	"github.com/giantswarm/agent-manager/internal/identity"
	"github.com/giantswarm/agent-manager/internal/kube"
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
		hrGVR: "HelmReleaseList", ociGVR: "OCIRepositoryList", tplGVR: "AgentTemplateList",
		serverGVR: "RemoteMCPServerList", harnessGVR: "HarnessList", mcGVR: "ModelConfigList",
	}
)

type fixture struct {
	dyn    *dynamicfake.FakeDynamicClient
	typed  *kubefake.Clientset
	pinner *fakePinner
	svc    *Service
}

func newFixture(t *testing.T, dynObjs []runtime.Object, typedObjs ...runtime.Object) *fixture {
	t.Helper()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, dynObjs...)
	typed := kubefake.NewClientset(typedObjs...)
	client := kube.FromInterfaces(dyn, typed, typed.Discovery())
	pinner := &fakePinner{}
	svc := New(kube.NewServiceAccountProvider(client), embeddedChart{}, nil, pinner, Config{
		DefaultNamespace: "kagent", ManagedNamespaces: []string{"tenant"}, Version: "test",
		Compose: ComposeConfig{MusterURL: "http://muster.agent-platform.svc.cluster.local:8090/mcp"},
	}, nil)
	return &fixture{dyn: dyn, typed: typed, pinner: pinner, svc: svc}
}

func modelConfig(ns, name, provider, model string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": kagentAPI, "kind": "ModelConfig",
		"metadata": map[string]any{"name": name, "namespace": ns},
		"spec":     map[string]any{"provider": provider, "model": model},
		"status":   map[string]any{"conditions": []any{map[string]any{"type": "Accepted", "status": "True", "reason": "ModelConfigReconciled"}}},
	}}
}

func harness(ns, name, label string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": kagentAPI, "kind": "Harness",
		"metadata": map[string]any{"name": name, "namespace": ns},
		"spec": map[string]any{"allowedAgentTemplates": map[string]any{"selector": map[string]any{"matchLabels": map[string]any{
			"agent-platform.giantswarm.io/harness": label,
		}}}},
	}}
}

func ociRepository(ns, semver string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "source.toolkit.fluxcd.io/v1", "kind": "OCIRepository",
		"metadata": map[string]any{"name": "agent", "namespace": ns},
		"spec":     map[string]any{"interval": "30m", "url": DefaultChartOCIURL, "ref": map[string]any{"semver": semver}},
	}}
}

func helmRelease(ns, name string, values map[string]any, ready bool, labels map[string]any) *unstructured.Unstructured {
	status := "True"
	reason := "InstallSucceeded"
	if !ready {
		status, reason = "False", "InstallFailed"
	}
	if labels == nil {
		labels = map[string]any{}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "helm.toolkit.fluxcd.io/v2", "kind": "HelmRelease",
		"metadata": map[string]any{"name": name, "namespace": ns, "labels": labels},
		"spec": map[string]any{
			"interval": "10m",
			"chartRef": map[string]any{"kind": "OCIRepository", "name": "agent", "namespace": ns},
			"values":   values,
		},
		"status": map[string]any{
			"conditions":            []any{map[string]any{"type": "Ready", "status": status, "reason": reason, "message": "Helm install " + reason}},
			"history":               []any{map[string]any{"version": int64(1), "chartVersion": "1.0.0", "status": "deployed", "lastDeployed": "2026-09-11T10:00:00Z"}},
			"lastAttemptedRevision": "1.0.0",
		},
	}}
}

// harnessEntryObj is one status.harnesses[] entry as kagent writes it.
func harnessEntryObj(name, desired, latest string, ready bool, failing string) map[string]any {
	conds := []any{
		map[string]any{"type": "Accepted", "status": "True", "reason": "Accepted"},
		map[string]any{"type": "ResolvedRefs", "status": "True", "reason": "ResolvedRefs"},
		map[string]any{"type": "Compatible", "status": "True", "reason": "Compatible"},
	}
	if failing != "" {
		for _, c := range conds {
			if c.(map[string]any)["type"] == failing {
				c.(map[string]any)["status"] = "False"
				c.(map[string]any)["reason"] = "Invalid"
				c.(map[string]any)["message"] = failing + " rejected the template"
			}
		}
	}
	readyCond := map[string]any{"type": "Ready", "status": "False", "reason": "ActorTemplatePending", "message": "waiting for the ActorTemplate golden snapshot"}
	if ready {
		readyCond = map[string]any{"type": "Ready", "status": "True", "reason": "Ready", "message": "golden snapshot ready"}
	}
	conds = append(conds, readyCond)
	return map[string]any{"harness": name, "desiredRevision": desired, "latestSuccessfulRevision": latest, "conditions": conds}
}

// agentTemplate is what the chart renders for hrName (the owning release in
// hrNs; "" for a bare template), admitted and ready on the kagent Harness when
// ready. It binds its own RemoteMCPServer (the toolset carrier) unless bound
// is false.
func agentTemplate(ns, name, hrName, hrNs string, ready bool, bound bool) *unstructured.Unstructured {
	labels := map[string]any{"agent-platform.giantswarm.io/harness": "kagent"}
	if hrName != "" {
		labels[HelmReleaseNameLabel] = hrName
		labels[HelmReleaseNamespaceLabel] = hrNs
	}
	tools := []any{}
	if bound {
		tools = append(tools, map[string]any{"mcp": map[string]any{"server": map[string]any{"kind": kindRemoteMCPServer, "name": name}}})
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": kagentAPI, "kind": "AgentTemplate",
		"metadata": map[string]any{"name": name, "namespace": ns, "generation": int64(1), "labels": labels, "annotations": map[string]any{DisplayNameAnnotation: "Display " + name, IconURLAnnotation: "https://avatars.example/" + name + ".png"}},
		"spec": map[string]any{
			"description": "desc " + name, "modelConfig": map[string]any{"name": "default-model-config"}, "systemPrompt": "You are " + name,
			"skills": []any{map[string]any{"name": "a", "source": map[string]any{"git": map[string]any{"url": skillsRepo, "commit": mainHead}, "path": "a"}}},
			"tools":  tools,
		},
		"status": map[string]any{"observedGeneration": int64(1), "harnesses": []any{harnessEntryObj("kagent", "rev-1", map[bool]string{true: "rev-1", false: ""}[ready], ready, "")}},
	}}
}

// remoteMCPServer is the agent's toolset carrier; no toolset means no header
// (implicit full access).
func remoteMCPServer(ns, name string, toolset ...string) *unstructured.Unstructured {
	spec := map[string]any{"protocol": "STREAMABLE_HTTP", "url": "http://muster.agent-platform.svc.cluster.local:8090/mcp"}
	if len(toolset) > 0 {
		spec["headersFrom"] = []any{map[string]any{"name": ToolsetHeader, "value": strings.Join(toolset, ",")}}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": kagentAPI, "kind": "RemoteMCPServer",
		"metadata": map[string]any{"name": name, "namespace": ns, "labels": map[string]any{HelmReleaseNameLabel: name, HelmReleaseNamespaceLabel: ns}},
		"spec":     spec,
	}}
}

func seeded(t *testing.T, typedObjs ...runtime.Object) *fixture {
	verifierValues := map[string]any{"agent": map[string]any{"name": "verifier", "displayName": "Verifier"}, "modelConfig": map[string]any{"name": "default-model-config"}}
	return newFixture(t, []runtime.Object{
		modelConfig("kagent", "default-model-config", "Anthropic", "claude-sonnet-4-6"),
		modelConfig("kagent", "qwen3-8-27b", "OpenAI", "qwen3-8-27b"),
		harness("kagent", "kagent", "kagent"),
		ociRepository("kagent", "1.x"),
		helmRelease("kagent", "verifier", verifierValues, true, nil),
		agentTemplate("kagent", "verifier", "verifier", "kagent", true, true),
		remoteMCPServer("kagent", "verifier"),
	}, typedObjs...)
}

func mustCreate(t *testing.T, f *fixture, gvr schema.GroupVersionResource, obj *unstructured.Unstructured) {
	t.Helper()
	_, err := f.dyn.Resource(gvr).Namespace(obj.GetNamespace()).Create(context.Background(), obj, metav1.CreateOptions{})
	require.NoError(t, err)
}

func loadFixture(t *testing.T, name string) *unstructured.Unstructured {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(filepath.Join("testdata", name)))
	require.NoError(t, err)
	var obj map[string]any
	require.NoError(t, yaml.Unmarshal(raw, &obj))
	return &unstructured.Unstructured{Object: obj}
}

func TestReadModelFromTheRenderedObjects(t *testing.T) {
	f := seeded(t)
	tpl, server := loadFixture(t, "agenttemplate-full.yaml"), loadFixture(t, "remotemcpserver.yaml")
	a := f.svc.agentFromTemplate(tpl, server)
	assert.Equal(t, "SRE Assistant", a.DisplayName, "display name from the annotation")
	assert.Equal(t, "https://avatars.example/v1/sre.png", a.IconURL, "icon URL from the annotation")
	assert.Equal(t, "helps", a.Description)
	assert.Equal(t, "default-model-config", a.ModelConfig)
	assert.Equal(t, "Be brief.", a.SystemMessage)
	assert.Equal(t, Skills{
		{Name: "runbooks", Path: "nested/runbooks", Git: &GitSkill{URL: skillsRepo, Commit: mainHead}},
		{Name: "kubectl", OCI: kubectlRef + "@" + kubectlSum},
	}, a.Skills, "skills are reported as the pins the template carries")
	assert.Equal(t, []string{"preset:read-only", "workflow:incident-triage"}, a.Toolset, "the toolset is the header of the agent's own RemoteMCPServer")
	assert.False(t, a.ImplicitFullAccess)
	assert.Equal(t, []ToolBinding{{Server: "sre"}, {Server: "github", Tools: []string{"get_issue", "list_issues"}}}, a.Tools)
	require.NotNil(t, a.Ready)
	assert.True(t, *a.Ready)
	require.Len(t, a.Harnesses, 1)
	assert.Equal(t, "kagent", a.Harnesses[0].Harness)
	assert.Equal(t, []string{"tools narrowed for server github could not be verified: discovery disabled"}, a.Harnesses[0].Warnings)
	assert.Equal(t, ManagedNone, a.Managed, "before the owning release is folded in")

	// A carrier without the header is implicit full access; no carrier bound
	// at all (preset:none) is neither.
	implicit := f.svc.agentFromTemplate(tpl, remoteMCPServer("kagent", "sre"))
	assert.Nil(t, implicit.Toolset)
	assert.True(t, implicit.ImplicitFullAccess)
	none := f.svc.agentFromTemplate(agentTemplate("kagent", "quiet", "quiet", "kagent", true, false), nil)
	assert.Nil(t, none.Toolset)
	assert.False(t, none.ImplicitFullAccess)

	// The owning release wins on the declaration and folds in the ownership.
	hr := helmRelease("kagent", "sre", map[string]any{"agent": map[string]any{"name": "sre"}, "modelConfig": map[string]any{"name": "default-model-config"}, ToolsetValuesKey: []any{"preset:read-only"}}, true, map[string]any{KustomizationNameLabel: "flux-giantswarm"})
	applyHelmRelease(&a, hr)
	assert.Equal(t, ManagedGitOps, a.Managed)
	assert.Equal(t, []string{"preset:read-only"}, a.Toolset)
	assert.Equal(t, "1.0.0", a.HelmRelease.ChartVersion)
}

func TestListMergesTemplatesAndHelmReleases(t *testing.T) {
	f := seeded(t)
	ctx := context.Background()
	// A HelmRelease that has not rendered its template yet (GitOps-owned), a
	// bare template with a toolset on its carrier, and a scoped agent.
	pending := map[string]any{"agent": map[string]any{"name": "pending", "displayName": "Pending"}, "modelConfig": map[string]any{"name": "qwen3-8-27b"},
		"skills": []any{map[string]any{"name": "runbooks", "git": map[string]any{"url": skillsRepo, "commit": tagHead}}}}
	mustCreate(t, f, hrGVR, helmRelease("kagent", "pending", pending, false, map[string]any{KustomizationNameLabel: "flux-system"}))
	mustCreate(t, f, tplGVR, agentTemplate("kagent", "bare", "", "", true, true))
	mustCreate(t, f, serverGVR, remoteMCPServer("kagent", "bare", "preset:read-only", "workflow:incident-triage"))
	scoped := map[string]any{"agent": map[string]any{"name": "scoped"}, "modelConfig": map[string]any{"name": "qwen3-8-27b"}, ToolsetValuesKey: []any{"preset:infrastructure"}}
	mustCreate(t, f, hrGVR, helmRelease("kagent", "scoped", scoped, true, nil))
	mustCreate(t, f, tplGVR, agentTemplate("kagent", "scoped", "scoped", "kagent", true, true))
	mustCreate(t, f, serverGVR, remoteMCPServer("kagent", "scoped", "preset:infrastructure"))

	list, err := f.svc.List(ctx, "")
	require.NoError(t, err)
	require.Len(t, list, 4)
	byName := map[string]Agent{}
	for _, a := range list {
		byName[a.Name] = a
	}

	v := byName["verifier"]
	assert.True(t, v.Exists)
	assert.Equal(t, "Display verifier", v.DisplayName)
	assert.Equal(t, "https://avatars.example/verifier.png", v.IconURL)
	assert.Equal(t, "default-model-config", v.ModelConfig)
	assert.Nil(t, v.Toolset, "the release declares no toolset")
	assert.True(t, v.ImplicitFullAccess, "no toolset on the release = implicit full access, whatever the carrier says")
	require.Len(t, v.Skills, 1)
	assert.Equal(t, mainHead, v.Skills[0].Git.Commit)
	assert.Equal(t, ManagedHelmRelease, v.Managed)
	require.NotNil(t, v.Ready)
	assert.True(t, *v.Ready)
	require.NotNil(t, v.HelmRelease)
	assert.Equal(t, "1.0.0", v.HelmRelease.ChartVersion)

	p := byName["pending"]
	assert.False(t, p.Exists, "listed from its HelmRelease alone")
	assert.Equal(t, "Pending", p.DisplayName)
	assert.Equal(t, "qwen3-8-27b", p.ModelConfig)
	assert.Equal(t, ManagedGitOps, p.Managed)
	assert.Nil(t, p.Ready)
	assert.False(t, *p.HelmRelease.Ready)
	assert.True(t, p.ImplicitFullAccess, "reported before the template is rendered")
	assert.Equal(t, Skills{{Name: "runbooks", Git: &GitSkill{URL: skillsRepo, Commit: tagHead}}}, p.Skills, "skills from the values while nothing is rendered")

	b := byName["bare"]
	assert.Equal(t, ManagedNone, b.Managed)
	assert.Nil(t, b.HelmRelease)
	assert.Equal(t, []string{"preset:read-only", "workflow:incident-triage"}, b.Toolset, "a bare template reports its carrier's header")
	assert.False(t, b.ImplicitFullAccess)

	sc := byName["scoped"]
	assert.Equal(t, []string{"preset:infrastructure"}, sc.Toolset)
	assert.False(t, sc.ImplicitFullAccess)
	assert.Equal(t, []any{"preset:infrastructure"}, sc.Values[ToolsetValuesKey])

	got, err := f.svc.Get(ctx, "", "scoped")
	require.NoError(t, err)
	assert.Equal(t, []string{"preset:infrastructure"}, got.Toolset)
	got, err = f.svc.Get(ctx, "", "verifier")
	require.NoError(t, err)
	assert.True(t, got.ImplicitFullAccess)

	_, err = f.svc.List(ctx, "other")
	assert.ErrorIs(t, err, ErrInvalid, "unmanaged namespaces are refused")
}

// TestGitOpsOwnedReleaseInAnotherNamespace is the fleet's sre-agent shape:
// HelmRelease + OCIRepository in flux-giantswarm, targetNamespace kagent. The
// template's provenance labels lead to the release; it is read-only.
func TestGitOpsOwnedReleaseInAnotherNamespace(t *testing.T) {
	f := seeded(t)
	ctx := context.Background()
	values := map[string]any{"agent": map[string]any{"name": "sre-agent"}, "modelConfig": map[string]any{"name": "default-model-config"}, ToolsetValuesKey: []any{"preset:infrastructure"}}
	hr := helmRelease("flux-giantswarm", "sre-agent", values, true, map[string]any{KustomizationNameLabel: "flux-giantswarm"})
	require.NoError(t, unstructured.SetNestedField(hr.Object, "kagent", "spec", "targetNamespace"))
	mustCreate(t, f, hrGVR, hr)
	mustCreate(t, f, ociGVR, ociRepository("flux-giantswarm", ">=0.2.1 <1.0.0"))
	mustCreate(t, f, tplGVR, agentTemplate("kagent", "sre-agent", "sre-agent", "flux-giantswarm", true, true))
	mustCreate(t, f, serverGVR, remoteMCPServer("kagent", "sre-agent", "preset:infrastructure"))

	got, err := f.svc.Get(ctx, "", "sre-agent")
	require.NoError(t, err)
	assert.Equal(t, ManagedGitOps, got.Managed)
	require.NotNil(t, got.HelmRelease)
	assert.Equal(t, "flux-giantswarm", got.HelmRelease.Namespace)
	assert.True(t, got.HelmRelease.GitOpsOwned)
	assert.Equal(t, []string{"preset:infrastructure"}, got.Toolset)

	list, err := f.svc.List(ctx, "")
	require.NoError(t, err)
	var listed *Agent
	for i := range list {
		if list[i].Name == "sre-agent" {
			listed = &list[i]
		}
	}
	require.NotNil(t, listed)
	assert.Equal(t, ManagedGitOps, listed.Managed)

	desc := "x"
	_, err = f.svc.Update(ctx, Update{Name: "sre-agent", Description: &desc})
	require.ErrorIs(t, err, ErrConflict)
	assert.Contains(t, err.Error(), "flux-giantswarm")
	_, err = f.svc.Delete(ctx, "", "sre-agent", false)
	assert.ErrorIs(t, err, ErrConflict)

	st, err := f.svc.Status(ctx, "", "sre-agent")
	require.NoError(t, err)
	assert.Equal(t, VerdictReady, st.Verdict, st.Summary)
	assert.True(t, st.HelmRelease.GitOpsOwned)
}

func TestCreateValidatesPinsThenAppliesBothObjects(t *testing.T) {
	f := seeded(t)
	ctx := context.Background()

	readOnly := []string{"preset:read-only"}
	_, err := f.svc.Create(ctx, Spec{Name: "Bad", ModelConfig: "default-model-config", Toolset: readOnly})
	assert.ErrorIs(t, err, ErrInvalid)

	// No toolset: refused before anything is read, naming the presets.
	_, err = f.svc.Create(ctx, Spec{Name: "sre", ModelConfig: "default-model-config"})
	require.ErrorIs(t, err, ErrInvalid)
	for _, want := range []string{"toolset is required", "preset:read-only", "preset:none", "preset:infrastructure", "preset:agent-platform", "preset:full"} {
		assert.Contains(t, err.Error(), want)
	}
	_, err = f.svc.Create(ctx, Spec{Name: "sre", ModelConfig: "default-model-config", Toolset: []string{}})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "preset:none")
	// The removed arguments are explained, not silently dropped.
	_, err = f.svc.Create(ctx, Spec{Name: "sre", ModelConfig: "default-model-config", Toolset: readOnly, RemovedToolNames: json.RawMessage(`["x_a_b"]`)})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "toolNames never narrowed anything against muster")
	_, err = f.svc.Create(ctx, Spec{Name: "sre", ModelConfig: "default-model-config", Toolset: readOnly, RemovedRuntime: json.RawMessage(`"python"`)})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "runtime is gone")

	_, err = f.svc.Create(ctx, Spec{Name: "sre", ModelConfig: "nope", Toolset: readOnly})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "default-model-config, qwen3-8-27b", "the valid model configs are listed")

	_, err = f.svc.Create(ctx, Spec{Name: "sre", ModelConfig: "default-model-config", DisplayName: strings.Repeat("x", 64), Toolset: readOnly})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "displayName")

	// A skill nobody can resolve names the repository; a short SHA is refused
	// before any lookup.
	_, err = f.svc.Create(ctx, Spec{Name: "sre", ModelConfig: "default-model-config", Toolset: readOnly, Skills: Skills{{Git: &GitSkill{URL: "https://github.com/giantswarm/private-skills", Ref: "main"}}}})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "https://github.com/giantswarm/private-skills")
	_, err = f.svc.Create(ctx, Spec{Name: "sre", ModelConfig: "default-model-config", Toolset: readOnly, Skills: Skills{{Git: &GitSkill{URL: skillsRepo, Commit: "abc1234"}}}})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "not a full commit id")

	_, err = f.svc.Create(ctx, Spec{Name: "verifier", ModelConfig: "default-model-config", Toolset: readOnly})
	assert.ErrorIs(t, err, ErrConflict)

	// Nothing was written by the failures.
	hrs, err := f.dyn.Resource(hrGVR).Namespace("kagent").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, hrs.Items, 1)

	res, err := f.svc.Create(ctx, Spec{
		Name: "sre", DisplayName: "SRE", Description: "helps", SystemMessage: "Be brief.", ModelConfig: "qwen3-8-27b", IconURL: "https://avatars.example/v1/sre.png",
		Skills: Skills{
			{Git: &GitSkill{URL: skillsRepo, Ref: "main"}, Path: "runbooks"},
			{Git: &GitSkill{URL: skillsRepo, Ref: "v1.2.0"}, Path: "nested/kube", Name: "kube"},
			{OCI: kubectlRef + ":1.4.0"},
		},
		Toolset: []string{"preset:read-only", "workflow:incident-triage"},
	})
	require.NoError(t, err)
	assert.False(t, res.Created.OCIRepository, "the namespace already had the chart source")
	assert.True(t, res.Created.HelmRelease)
	assert.Equal(t, "sre", res.Agent.Name)
	assert.False(t, res.Agent.Exists)
	assert.Equal(t, []string{"preset:read-only", "workflow:incident-triage"}, res.Agent.Toolset)
	assert.False(t, res.Agent.ImplicitFullAccess)
	assert.Equal(t, ManagedHelmRelease, res.Agent.Managed)
	assert.Contains(t, res.Manifests.HelmRelease, "kind: HelmRelease")
	assert.Contains(t, res.Manifests.OCIRepository, "semver: 1.x")
	require.NotNil(t, res.Status)
	assert.Equal(t, VerdictProgressing, res.Status.Verdict)

	hr, err := f.dyn.Resource(hrGVR).Namespace("kagent").Get(ctx, "sre", metav1.GetOptions{})
	require.NoError(t, err)
	values, _, _ := unstructured.NestedMap(hr.Object, "spec", "values")
	assert.Equal(t, map[string]any{
		"agent":       map[string]any{"name": "sre", "displayName": "SRE", "description": "helps", "systemMessage": "Be brief.", "iconUrl": "https://avatars.example/v1/sre.png", "harness": "kagent"},
		"modelConfig": map[string]any{"name": "qwen3-8-27b"},
		"skills": []any{
			map[string]any{"name": "runbooks", "path": "runbooks", "git": map[string]any{"url": skillsRepo, "commit": mainHead}},
			map[string]any{"name": "kube", "path": "nested/kube", "git": map[string]any{"url": skillsRepo, "commit": tagHead}},
			map[string]any{"name": "kubectl", "oci": kubectlRef + "@" + kubectlSum},
		},
		"toolset": []any{"preset:read-only", "workflow:incident-triage"},
		"muster":  map[string]any{"url": "http://muster.agent-platform.svc.cluster.local:8090/mcp"},
	}, values, "branches and tags are written as pins; the toolset is the chart's top-level value; muster.url is composed when configured")
	assert.Equal(t, ManagedByValue, hr.GetLabels()[ManagedByLabel])
	// get_agent reports the pins before the template exists, too.
	got, err := f.svc.Get(ctx, "", "sre")
	require.NoError(t, err)
	require.Len(t, got.Skills, 3)
	assert.Equal(t, mainHead, got.Skills[0].Git.Commit)
	assert.Equal(t, kubectlRef+"@"+kubectlSum, got.Skills[2].OCI)

	// A namespace without a chart source gets one, tracking 1.x.
	mustCreate(t, f, mcGVR, modelConfig("tenant", "mc", "Ollama", "qwen3"))
	res, err = f.svc.Create(ctx, Spec{Namespace: "tenant", Name: "t1", ModelConfig: "mc", Toolset: []string{"preset:none"}})
	require.NoError(t, err)
	assert.True(t, res.Created.OCIRepository)
	repo, err := f.dyn.Resource(ociGVR).Namespace("tenant").Get(ctx, "agent", metav1.GetOptions{})
	require.NoError(t, err)
	url, _, _ := unstructured.NestedString(repo.Object, "spec", "url")
	assert.Equal(t, DefaultChartOCIURL, url)
	semver, _, _ := unstructured.NestedString(repo.Object, "spec", "ref", "semver")
	assert.Equal(t, "1.x", semver)
}

func TestValidateCreateIsADryRun(t *testing.T) {
	f := seeded(t)
	ctx := context.Background()
	res, err := f.svc.ValidateCreate(ctx, Spec{Name: "verifier", ModelConfig: "nope", Skills: Skills{{Git: &GitSkill{URL: "https://github.com/giantswarm/private-skills"}}}})
	require.NoError(t, err)
	assert.False(t, res.Valid)
	assert.Equal(t, "create", res.Mode)
	joined := strings.Join(res.Errors, "\n")
	assert.Contains(t, joined, "already exists")
	assert.Contains(t, joined, "does not exist")
	assert.Contains(t, joined, "toolset is required", "a create without a toolset is invalid")
	assert.Contains(t, joined, "preset:none")
	assert.Contains(t, joined, "https://github.com/giantswarm/private-skills", "an unresolvable skill is a listed violation")
	assert.Equal(t, chart.EmbeddedSchemaVersion, res.SchemaVersion)

	// The toolset grammar is judged in the dry run too.
	many := make([]string, MaxToolsetSelectors+1)
	for i := range many {
		many[i] = fmt.Sprintf("tool:x_a_%d", i)
	}
	for toolset, want := range map[*[]string]string{
		{}:                                  "preset:none",
		&many:                               "define a preset",
		{"toolset:shared"}:                  "reserved",
		{"label:tool-group=infrastructure"}: "presets only",
	} {
		r, err := f.svc.ValidateCreate(ctx, Spec{Name: "fresh", ModelConfig: "default-model-config", Toolset: *toolset})
		require.NoError(t, err)
		assert.False(t, r.Valid)
		assert.Contains(t, strings.Join(r.Errors, "\n"), want)
	}
	_, err = f.svc.ValidateCreate(ctx, Spec{Name: "fresh", ModelConfig: "default-model-config", Toolset: []string{"preset:full"}, RemovedToolNames: json.RawMessage(`[]`)})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "toolNames never narrowed")
	_, err = f.svc.ValidateCreate(ctx, Spec{Name: "fresh", ModelConfig: "default-model-config", Toolset: []string{"preset:full"}, RemovedRuntime: json.RawMessage(`"go"`)})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "runtime is gone", "validate_agent answers the same as create_agent")

	ok, err := f.svc.ValidateCreate(ctx, Spec{Name: "fresh", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"}, Skills: Skills{{Git: &GitSkill{URL: skillsRepo, Ref: "feature"}, Path: "runbooks"}}})
	require.NoError(t, err)
	assert.True(t, ok.Valid, ok.Errors)
	assert.Equal(t, []any{"preset:read-only"}, ok.Manifests.Values[ToolsetValuesKey])
	assert.Equal(t, []any{map[string]any{"name": "runbooks", "path": "runbooks", "git": map[string]any{"url": skillsRepo, "commit": featureHead}}}, ok.Manifests.Values["skills"], "the dry run shows the pin that would be written")
	assert.Contains(t, ok.Manifests.OCIRepository, "kind: OCIRepository")
	hrs, _ := f.dyn.Resource(hrGVR).Namespace("kagent").List(ctx, metav1.ListOptions{})
	assert.Len(t, hrs.Items, 1, "validate writes nothing")
}

func TestUpdateMergesIntoValuesAndHonorsOwnership(t *testing.T) {
	f := seeded(t)
	ctx := context.Background()
	str := func(s string) *string { return &s }

	// Assigning a toolset to an agent that had none. The verifier release
	// predates agent.harness: an update leaves the values it does not touch.
	res, err := f.svc.Update(ctx, Update{Name: "verifier", DisplayName: str("Verifier 2"), Description: str("now with a description"), IconURL: str("https://avatars.example/v.png"), Toolset: &[]string{"preset:read-only", "server:github"}, ModelConfig: str("qwen3-8-27b")})
	require.NoError(t, err)
	assert.Equal(t, []string{"agent.description", "agent.displayName", "agent.iconUrl", "modelConfig.name", "toolset"}, res.Changed)
	assert.Equal(t, "Verifier", res.Before["agent"].(map[string]any)["displayName"])
	assert.Equal(t, "Verifier 2", res.After["agent"].(map[string]any)["displayName"])
	assert.Equal(t, []any{"preset:read-only", "server:github"}, res.After[ToolsetValuesKey])
	assert.Equal(t, []string{"preset:read-only", "server:github"}, res.Agent.Toolset)
	assert.False(t, res.Agent.ImplicitFullAccess)
	hr, err := f.dyn.Resource(hrGVR).Namespace("kagent").Get(ctx, "verifier", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, res.After, mustValues(hr))

	// The toolset is replaced as a whole, never merged.
	res, err = f.svc.Update(ctx, Update{Name: "verifier", Toolset: &[]string{"preset:none"}})
	require.NoError(t, err)
	assert.Equal(t, []string{"toolset"}, res.Changed)
	assert.Equal(t, []any{"preset:none"}, res.After[ToolsetValuesKey])

	// It cannot be cleared back to implicit full access, and the grammar holds.
	_, err = f.svc.Update(ctx, Update{Name: "verifier", Toolset: &[]string{}})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "preset:none")
	_, err = f.svc.Update(ctx, Update{Name: "verifier", Toolset: &[]string{"toolset:shared"}})
	require.ErrorIs(t, err, ErrInvalid)
	_, err = f.svc.Update(ctx, Update{Name: "verifier", RemovedToolNames: json.RawMessage(`["x_a_b"]`)})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "toolNames never narrowed")
	_, err = f.svc.Update(ctx, Update{Name: "verifier", RemovedRuntime: json.RawMessage(`"python"`)})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "runtime is gone")
	dry, err := f.svc.ValidateUpdate(ctx, Update{Name: "verifier", Toolset: &[]string{"label:x=y"}})
	require.NoError(t, err)
	assert.False(t, dry.Valid)
	assert.Contains(t, strings.Join(dry.Errors, "\n"), "presets only")
	hr, err = f.dyn.Resource(hrGVR).Namespace("kagent").Get(ctx, "verifier", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, []any{"preset:none"}, mustValues(hr)[ToolsetValuesKey], "refusals write nothing")

	// Clearing a field drops the key; skills replace the list and are pinned.
	res, err = f.svc.Update(ctx, Update{Name: "verifier", Description: str(""), Skills: &Skills{{OCI: kubectlRef + ":1.4.0"}, {Git: &GitSkill{URL: skillsRepo, Ref: "feature"}, Path: "runbooks"}}})
	require.NoError(t, err)
	after := res.After
	_, hasDesc := after["agent"].(map[string]any)["description"]
	assert.False(t, hasDesc)
	assert.Equal(t, []any{
		map[string]any{"name": "kubectl", "oci": kubectlRef + "@" + kubectlSum},
		map[string]any{"name": "runbooks", "path": "runbooks", "git": map[string]any{"url": skillsRepo, "commit": featureHead}},
	}, after["skills"])
	assert.Equal(t, []any{"preset:none"}, after[ToolsetValuesKey], "an update without toolset leaves it")
	assert.Equal(t, []string{"agent.description", "skills"}, res.Changed)

	// refreshSkills alone: every git skill to its default-branch head, nothing else.
	f.pinner.calls = nil
	res, err = f.svc.Update(ctx, Update{Name: "verifier", RefreshSkills: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"skills"}, res.Changed, "only the skills moved")
	assert.Equal(t, []any{
		map[string]any{"name": "kubectl", "oci": kubectlRef + "@" + kubectlSum},
		map[string]any{"name": "runbooks", "path": "runbooks", "git": map[string]any{"url": skillsRepo, "commit": mainHead}},
	}, res.After["skills"], "the branch pin moved to the default-branch head; the OCI pin stayed")
	assert.Equal(t, []string{"git " + skillsRepo + "@"}, f.pinner.calls, "the default branch is what a refresh follows")
	assert.Equal(t, res.Before[ToolsetValuesKey], res.After[ToolsetValuesKey])
	assert.Equal(t, res.Before["agent"], res.After["agent"])
	assert.Equal(t, res.Before["modelConfig"], res.After["modelConfig"])
	// Already at the head: a no-op.
	res, err = f.svc.Update(ctx, Update{Name: "verifier", RefreshSkills: true})
	require.NoError(t, err)
	assert.Empty(t, res.Changed)
	// refreshSkills with a skill passed with ref: that ref's head.
	res, err = f.svc.Update(ctx, Update{Name: "verifier", RefreshSkills: true, Skills: &Skills{{Git: &GitSkill{URL: skillsRepo, Ref: "feature"}, Path: "runbooks"}, {Git: &GitSkill{URL: skillsRepo, Commit: tagHead}, Path: "other"}}})
	require.NoError(t, err)
	assert.Equal(t, []any{
		map[string]any{"name": "runbooks", "path": "runbooks", "git": map[string]any{"url": skillsRepo, "commit": featureHead}},
		map[string]any{"name": "other", "path": "other", "git": map[string]any{"url": skillsRepo, "commit": mainHead}},
	}, res.After["skills"], "ref wins; a commit passed with refreshSkills is replaced by the default-branch head")
	// An empty list clears the skills.
	res, err = f.svc.Update(ctx, Update{Name: "verifier", Skills: &Skills{}})
	require.NoError(t, err)
	_, hasSkills := res.After["skills"]
	assert.False(t, hasSkills)

	// No change is a no-op.
	res, err = f.svc.Update(ctx, Update{Name: "verifier"})
	require.NoError(t, err)
	assert.Empty(t, res.Changed)

	_, err = f.svc.Update(ctx, Update{Name: "verifier", ModelConfig: str("nope")})
	assert.ErrorIs(t, err, ErrInvalid)
	_, err = f.svc.Update(ctx, Update{Name: "verifier", Skills: &Skills{{Git: &GitSkill{URL: "https://github.com/giantswarm/private-skills"}}}})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "private-skills")
	_, err = f.svc.Update(ctx, Update{Name: "missing", DisplayName: str("x")})
	assert.ErrorIs(t, err, ErrNotFound)

	// GitOps-owned: refused without force.
	gitops := helmRelease("kagent", "gitops", map[string]any{"agent": map[string]any{"name": "gitops"}, "modelConfig": map[string]any{"name": "default-model-config"}}, true, map[string]any{KustomizationNameLabel: "flux-system"})
	mustCreate(t, f, hrGVR, gitops)
	_, err = f.svc.Update(ctx, Update{Name: "gitops", DisplayName: str("x")})
	require.ErrorIs(t, err, ErrConflict)
	assert.Contains(t, err.Error(), "flux-system")
	res, err = f.svc.Update(ctx, Update{Name: "gitops", DisplayName: str("x"), Force: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"agent.displayName"}, res.Changed)

	// A bare AgentTemplate has nothing to write to.
	mustCreate(t, f, tplGVR, agentTemplate("kagent", "bare", "", "", true, true))
	_, err = f.svc.Update(ctx, Update{Name: "bare", DisplayName: str("x")})
	require.ErrorIs(t, err, ErrConflict)
	assert.Contains(t, err.Error(), "bare template")
}

func mustValues(hr *unstructured.Unstructured) map[string]any {
	v, _, _ := unstructured.NestedMap(hr.Object, "spec", "values")
	return v
}

func TestDeleteRemovesTheReleaseAndTheSourceOnlyWhenUnreferenced(t *testing.T) {
	f := seeded(t)
	ctx := context.Background()
	_, err := f.svc.Create(ctx, Spec{Name: "sre", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"}})
	require.NoError(t, err)

	res, err := f.svc.Delete(ctx, "", "sre", false)
	require.NoError(t, err)
	assert.True(t, res.HelmReleaseDeleted)
	assert.False(t, res.OCIRepositoryDeleted)
	assert.Contains(t, res.OCIRepositoryKept, "verifier")
	_, err = f.dyn.Resource(ociGVR).Namespace("kagent").Get(ctx, "agent", metav1.GetOptions{})
	require.NoError(t, err, "the shared source stays while verifier references it")

	res, err = f.svc.Delete(ctx, "", "verifier", false)
	require.NoError(t, err)
	assert.True(t, res.HelmReleaseDeleted)
	assert.True(t, res.OCIRepositoryDeleted)
	_, err = f.dyn.Resource(ociGVR).Namespace("kagent").Get(ctx, "agent", metav1.GetOptions{})
	assert.Error(t, err, "the last release takes the source with it")

	_, err = f.svc.Delete(ctx, "", "verifier", false)
	assert.ErrorIs(t, err, ErrConflict, "the AgentTemplate is still there (the fake has no helm-controller): a bare template now")
	_, err = f.svc.Delete(ctx, "", "nothing", false)
	assert.ErrorIs(t, err, ErrNotFound)

	// Bare template: force deletes the template itself.
	res, err = f.svc.Delete(ctx, "", "verifier", true)
	require.NoError(t, err)
	assert.True(t, res.AgentTemplateDeleted)
	assert.False(t, res.HelmReleaseDeleted)
	assert.False(t, res.RemoteMCPServerDeleted, "a bare template's server is not the release's to remove")
	_, err = f.dyn.Resource(tplGVR).Namespace("kagent").Get(ctx, "verifier", metav1.GetOptions{})
	assert.Error(t, err)

	// Suspended: refused without force; with force the rendered objects go
	// too (Flux would leave them behind).
	suspended := helmRelease("kagent", "susp", map[string]any{"agent": map[string]any{"name": "susp"}, "modelConfig": map[string]any{"name": "default-model-config"}}, true, nil)
	require.NoError(t, unstructured.SetNestedField(suspended.Object, true, "spec", "suspend"))
	mustCreate(t, f, hrGVR, suspended)
	mustCreate(t, f, tplGVR, agentTemplate("kagent", "susp", "susp", "kagent", true, true))
	mustCreate(t, f, serverGVR, remoteMCPServer("kagent", "susp", "preset:none"))
	_, err = f.svc.Delete(ctx, "", "susp", false)
	assert.ErrorIs(t, err, ErrConflict)
	res, err = f.svc.Delete(ctx, "", "susp", true)
	require.NoError(t, err)
	assert.True(t, res.HelmReleaseDeleted)
	assert.True(t, res.AgentTemplateDeleted)
	assert.True(t, res.RemoteMCPServerDeleted)
	_, err = f.dyn.Resource(serverGVR).Namespace("kagent").Get(ctx, "susp", metav1.GetOptions{})
	assert.Error(t, err)
}

func TestStatusVerdictsComeFromThePlatformHarness(t *testing.T) {
	ctx := context.Background()
	withStatus := func(tpl *unstructured.Unstructured, observed int64, entries ...map[string]any) *unstructured.Unstructured {
		list := make([]any, 0, len(entries))
		for _, e := range entries {
			list = append(list, e)
		}
		tpl.Object["status"] = map[string]any{"observedGeneration": observed, "harnesses": list}
		return tpl
	}
	release := func(name string) *unstructured.Unstructured {
		return helmRelease("kagent", name, map[string]any{"agent": map[string]any{"name": name}, "modelConfig": map[string]any{"name": "default-model-config"}}, true, nil)
	}

	// Ready: Ready=True and the desired revision is the latest successful one.
	f := seeded(t)
	st, err := f.svc.Status(ctx, "", "verifier")
	require.NoError(t, err)
	assert.Equal(t, VerdictReady, st.Verdict, st.Summary)
	assert.Contains(t, st.Summary, "Harness kagent")
	assert.Contains(t, st.Summary, "rev-1")
	require.Len(t, st.HelmRelease.History, 1)
	assert.Equal(t, "1.0.0", st.HelmRelease.History[0].ChartVersion)
	require.Len(t, st.Template.Harnesses, 1)
	assert.True(t, *st.Template.Harnesses[0].Ready)
	assert.Nil(t, st.Events)

	// Progressing: a new revision compiling (desired != latest successful),
	// even though Ready still reads True for the previous one.
	f = newFixture(t, []runtime.Object{release("sre"), withStatus(agentTemplate("kagent", "sre", "sre", "kagent", true, true), 2, harnessEntryObj("kagent", "rev-2", "rev-1", true, ""))})
	st, err = f.svc.Status(ctx, "", "sre")
	require.NoError(t, err)
	assert.Equal(t, VerdictProgressing, st.Verdict, st.Summary)
	assert.Contains(t, st.Summary, "rev-2")
	// Progressing: first revision, Ready not yet True.
	f = newFixture(t, []runtime.Object{release("sre"), withStatus(agentTemplate("kagent", "sre", "sre", "kagent", false, true), 1, harnessEntryObj("kagent", "rev-1", "", false, ""))})
	st, err = f.svc.Status(ctx, "", "sre")
	require.NoError(t, err)
	assert.Equal(t, VerdictProgressing, st.Verdict, st.Summary)

	// Failed: Ready=False with a reason other than the pending one is a
	// failure the controller will not get past (agentlab's terminal rule).
	snapshotFailed := harnessEntryObj("kagent", "rev-1", "", false, "")
	for _, c := range snapshotFailed["conditions"].([]any) {
		if c.(map[string]any)["type"] == "Ready" {
			c.(map[string]any)["reason"] = "SnapshotFailed"
			c.(map[string]any)["message"] = "actor exited before the snapshot"
		}
	}
	f = newFixture(t, []runtime.Object{release("sre"), withStatus(agentTemplate("kagent", "sre", "sre", "kagent", false, true), 1, snapshotFailed)})
	st, err = f.svc.Status(ctx, "", "sre")
	require.NoError(t, err)
	assert.Equal(t, VerdictFailed, st.Verdict, st.Summary)
	assert.Contains(t, st.Summary, "SnapshotFailed: actor exited before the snapshot")

	// Failed: Accepted=False / Compatible=False with the condition's message.
	for _, failing := range []string{"Accepted", "Compatible", "ResolvedRefs"} {
		f = newFixture(t, []runtime.Object{release("sre"), withStatus(agentTemplate("kagent", "sre", "sre", "kagent", false, true), 1, harnessEntryObj("kagent", "rev-1", "", false, failing))})
		st, err = f.svc.Status(ctx, "", "sre")
		require.NoError(t, err)
		assert.Equal(t, VerdictFailed, st.Verdict, st.Summary)
		assert.Contains(t, st.Summary, failing+" is False")
		assert.Contains(t, st.Summary, failing+" rejected the template")
	}

	// Not admitted: observedGeneration caught up, harnesses[] empty — with the
	// Harnesses of the namespace and what they admit.
	unadmitted := withStatus(agentTemplate("kagent", "sre", "sre", "kagent", false, true), 1)
	unadmitted.SetLabels(map[string]string{HelmReleaseNameLabel: "sre", HelmReleaseNamespaceLabel: "kagent", "kagent.dev/harness": "kagent"})
	f = newFixture(t, []runtime.Object{release("sre"), unadmitted, harness("kagent", "kagent", "kagent")})
	st, err = f.svc.Status(ctx, "", "sre")
	require.NoError(t, err)
	assert.Equal(t, VerdictFailed, st.Verdict, st.Summary)
	assert.Contains(t, st.Summary, "no Harness admits the AgentTemplate")
	assert.Contains(t, st.Summary, "kagent (admits agent-platform.giantswarm.io/harness=kagent)")
	assert.Contains(t, st.Summary, "kagent.dev/harness=kagent")
	// Another Harness admits it, the platform one does not.
	f = newFixture(t, []runtime.Object{release("sre"), withStatus(agentTemplate("kagent", "sre", "sre", "kagent", true, true), 1, harnessEntryObj("claude", "rev-1", "rev-1", true, ""))})
	st, err = f.svc.Status(ctx, "", "sre")
	require.NoError(t, err)
	assert.Equal(t, VerdictFailed, st.Verdict, st.Summary)
	assert.Contains(t, st.Summary, `"kagent" does not admit`)
	assert.Contains(t, st.Summary, "claude")
	// kagent has not observed the template yet.
	f = newFixture(t, []runtime.Object{release("sre"), withStatus(agentTemplate("kagent", "sre", "sre", "kagent", false, true), 0)})
	st, err = f.svc.Status(ctx, "", "sre")
	require.NoError(t, err)
	assert.Equal(t, VerdictProgressing, st.Verdict, st.Summary)

	// Failed: the HelmRelease itself failed (nothing rendered); a Warning
	// event on the agent is reported.
	f = newFixture(t, []runtime.Object{helmRelease("kagent", "broken", map[string]any{"agent": map[string]any{"name": "broken"}}, false, nil)},
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "ev1", Namespace: "kagent"}, Type: "Warning", Reason: "InstallFailed", Message: "values don't meet the specifications of the schema(s)", InvolvedObject: corev1.ObjectReference{Kind: "HelmRelease", Name: "broken"}},
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "ev2", Namespace: "kagent"}, Type: "Warning", Reason: "Other", Message: "somebody else's", InvolvedObject: corev1.ObjectReference{Kind: "HelmRelease", Name: "other"}})
	st, err = f.svc.Status(ctx, "", "broken")
	require.NoError(t, err)
	assert.Equal(t, VerdictFailed, st.Verdict)
	assert.Contains(t, st.Summary, "InstallFailed")
	assert.False(t, st.Template.Exists)
	require.Len(t, st.Events, 1)
	assert.Equal(t, "HelmRelease/broken", st.Events[0].Object)

	// Progressing: fresh HelmRelease without status.
	fresh := helmRelease("kagent", "fresh", map[string]any{"agent": map[string]any{"name": "fresh"}}, true, nil)
	delete(fresh.Object, "status")
	f = newFixture(t, []runtime.Object{fresh})
	st, err = f.svc.Status(ctx, "", "fresh")
	require.NoError(t, err)
	assert.Equal(t, VerdictProgressing, st.Verdict)

	_, err = f.svc.Status(ctx, "", "absent")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestListModelConfigsAndInfo(t *testing.T) {
	f := seeded(t)
	ctx := context.Background()
	list, err := f.svc.ListModelConfigs(ctx, "")
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "default-model-config", list[0].Name)
	assert.Equal(t, "Anthropic", list[0].Provider)
	assert.Equal(t, "claude-sonnet-4-6", list[0].Model)
	require.NotNil(t, list[0].Accepted)
	assert.True(t, *list[0].Accepted)

	info := f.svc.Info(ctx)
	assert.Equal(t, "test", info.Version)
	assert.Equal(t, []string{"kagent", "tenant"}, info.Namespaces.Managed)
	assert.Equal(t, "serviceAccount", info.Identity)
	assert.True(t, info.Capabilities["create"])
	assert.False(t, info.Capabilities["skills"], "no repositories configured")
	assert.False(t, info.Capabilities["writesAsCaller"])
	commit, declared := info.Capabilities["commit"]
	assert.True(t, declared)
	assert.False(t, commit, "the commit mode is not implemented")
	assert.Equal(t, "kagent.dev/v1alpha3", info.APIVersions.AgentTemplate)
	assert.Equal(t, "kagent.dev/v1alpha3", info.APIVersions.Harness)
	assert.Equal(t, "kagent.dev/v1alpha3", info.APIVersions.RemoteMCPServer)
	assert.Equal(t, "kagent.dev/v1alpha3", info.APIVersions.ModelConfig)
	assert.Equal(t, "helm.toolkit.fluxcd.io/v2", info.APIVersions.HelmRelease)
	assert.Equal(t, "kagent", info.Harness.Name)
	assert.Equal(t, "http://muster.agent-platform.svc.cluster.local:8090/mcp", info.Muster.URL)
	assert.Equal(t, DefaultChartOCIURL, info.Chart.OCIURL)
	assert.Equal(t, "1.x", info.Chart.Semver)

	_, err = f.svc.ListSkills(ctx, "", "", false)
	assert.ErrorIs(t, err, ErrUnsupported)
}

// TestMutationsCarryTheCaller: with OAuth in front, every write is attributed
// to the authenticated caller — on the result (requestedBy) and in the log —
// and a caller-only server refuses to act for a request without a token.
func TestMutationsCarryTheCaller(t *testing.T) {
	f := seeded(t)
	ctx := identity.ContextWith(context.Background(), &identity.Identity{Subject: "sub-1", Email: "admin@lab.local", Source: identity.SourceSSO})

	created, err := f.svc.Create(ctx, Spec{Name: "sre", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"}})
	require.NoError(t, err)
	assert.Equal(t, "admin@lab.local", created.RequestedBy)

	desc := "on call"
	updated, err := f.svc.Update(ctx, Update{Name: "sre", Description: &desc})
	require.NoError(t, err)
	assert.Equal(t, "admin@lab.local", updated.RequestedBy)

	deleted, err := f.svc.Delete(ctx, "", "sre", false)
	require.NoError(t, err)
	assert.Equal(t, "admin@lab.local", deleted.RequestedBy)

	anonymous, err := f.svc.Create(context.Background(), Spec{Name: "sre", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"}})
	require.NoError(t, err)
	assert.Empty(t, anonymous.RequestedBy, "without OAuth there is no caller to report")

	// The caller provider (downstream OAuth): no token on the request, no
	// Kubernetes call — 401, not a ServiceAccount fallback.
	callerOnly := New(kube.NewCallerProvider(kube.FromInterfaces(f.dyn, f.typed, f.typed.Discovery()), nil), embeddedChart{}, nil, nil, Config{DefaultNamespace: "kagent", Version: "test"}, nil)
	assert.True(t, callerOnly.Info(context.Background()).Capabilities["writesAsCaller"])
	assert.Equal(t, kube.IdentityCaller, callerOnly.Info(context.Background()).Identity)
	_, err = callerOnly.List(context.Background(), "")
	assert.ErrorIs(t, err, ErrUnauthenticated)
	_, err = callerOnly.Create(context.Background(), Spec{Name: "sre", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"}})
	assert.ErrorIs(t, err, ErrUnauthenticated)
}
