package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/giantswarm/agent-manager/internal/identity"
	"github.com/giantswarm/agent-manager/internal/kube"
)

var (
	tplGVR    = schema.GroupVersionResource{Group: "kagent.dev", Version: "v1alpha3", Resource: "agenttemplates"}
	rmsGVR    = schema.GroupVersionResource{Group: "kagent.dev", Version: "v1alpha3", Resource: "remotemcpservers"}
	hGVR      = schema.GroupVersionResource{Group: "kagent.dev", Version: "v1alpha3", Resource: "harnesses"}
	mcGVR     = schema.GroupVersionResource{Group: "kagent.dev", Version: "v1alpha3", Resource: "modelconfigs"}
	listKinds = map[schema.GroupVersionResource]string{
		tplGVR: "AgentTemplateList", rmsGVR: "RemoteMCPServerList", hGVR: "HarnessList", mcGVR: "ModelConfigList",
	}
)

type fixture struct {
	dyn *dynamicfake.FakeDynamicClient
	svc *Service
}

func newFixture(t *testing.T, objs ...runtime.Object) *fixture {
	t.Helper()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, objs...)
	client := kube.FromInterfaces(dyn, kubefake.NewClientset().Discovery())
	svc := New(kube.NewServiceAccountProvider(client), nil, Config{DefaultNamespace: "kagent", ManagedNamespaces: []string{"tenant"}, Version: "test"}, nil)
	return &fixture{dyn: dyn, svc: svc}
}

// writes lists the fake's write actions on a resource, e.g. "create", "update".
func (f *fixture) writes(resource string) []string {
	var out []string
	for _, a := range f.dyn.Actions() {
		if a.GetResource().Resource != resource {
			continue
		}
		switch a.GetVerb() {
		case "create", "update", "delete", "patch":
			out = append(out, a.GetVerb())
		}
	}
	return out
}

func (f *fixture) get(t *testing.T, gvr schema.GroupVersionResource, ns, name string) *unstructured.Unstructured {
	t.Helper()
	obj, err := f.dyn.Resource(gvr).Namespace(ns).Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	return obj
}

func (f *fixture) exists(gvr schema.GroupVersionResource, ns, name string) bool {
	_, err := f.dyn.Resource(gvr).Namespace(ns).Get(context.Background(), name, metav1.GetOptions{})
	return err == nil
}

func modelConfig(ns, name, provider, model string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": DefaultAPIVersion, "kind": "ModelConfig",
		"metadata": map[string]any{"name": name, "namespace": ns},
		"spec":     map[string]any{"provider": provider, "model": model},
		"status":   map[string]any{"conditions": []any{map[string]any{"type": "Accepted", "status": "True", "reason": "ModelConfigReconciled"}}},
	}}
}

// harness is a Harness admitting templates labelled kagent.dev/harness=<name>
// (the platform's shape); selector overrides the matchLabels.
func harness(ns, name string, selector map[string]any) *unstructured.Unstructured {
	if selector == nil {
		selector = map[string]any{HarnessLabel: name}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": DefaultAPIVersion, "kind": "Harness",
		"metadata": map[string]any{"name": name, "namespace": ns},
		"spec": map[string]any{
			name:       map[string]any{},
			"workload": map[string]any{"image": "localhost:5001/kagent-dev/kagent/golang-adk@" + testDigest},
			"substrate": map[string]any{"workerPoolRef": map[string]any{"name": "kagent-default"}, "snapshotPolicy": map[string]any{"location": "s3://ate-snapshots/" + name}},
			"allowedAgentTemplates": map[string]any{"selector": map[string]any{"matchLabels": selector}},
		},
	}}
}

// harnessEntry is one status.harnesses[] entry as kagent reports it.
func harnessEntry(name string, generation int64, ready *bool, failing string, warnings ...string) map[string]any {
	cond := func(typ, status, reason, message string) map[string]any {
		return map[string]any{"type": typ, "status": status, "reason": reason, "message": message, "observedGeneration": generation, "lastTransitionTime": "2026-09-09T15:49:49Z"}
	}
	conds := []any{cond("Accepted", "True", "Accepted", "Harness admission selector matches the AgentTemplate")}
	switch failing {
	case "ResolvedRefs":
		conds = append(conds, cond("ResolvedRefs", "False", "WorkerPoolNotFound", `WorkerPool "kagent/kagent-default" not found`))
	case "Compatible":
		conds = append(conds, cond("ResolvedRefs", "True", "Resolved", "All runtime references resolved"), cond("Compatible", "False", "ActorTemplateInvalid", "tool binding needs discovery"))
	default:
		conds = append(conds, cond("ResolvedRefs", "True", "Resolved", "All runtime references resolved"), cond("Compatible", "True", "Compatible", "Resolved configuration is compatible with the Harness"))
		if ready != nil && *ready {
			conds = append(conds, cond("Ready", "True", "Ready", "ActorTemplate golden snapshot is ready"))
		} else if ready != nil {
			conds = append(conds, cond("Ready", "False", "ActorTemplateFailed", "snapshot failed"))
		}
	}
	entry := map[string]any{"harness": name, "desiredRevision": "3bd7156d4194f4580b49a4a46001e59c8550fa3a270740d00afa012883eedcb1", "conditions": conds}
	if ready != nil && *ready {
		entry["latestSuccessfulRevision"] = entry["desiredRevision"]
	}
	if len(warnings) > 0 {
		entry["warnings"] = toAnySlice(warnings)
	}
	return entry
}

// template is an agent-manager-managed AgentTemplate binding its carrier
// (toolset != nil) or the platform server (bindsPlatform) with the given
// status entries; extra labels are merged in.
func template(ns, name string, toolset []string, bindsPlatform bool, labels map[string]string, entries ...map[string]any) *unstructured.Unstructured {
	d := declaration{Spec: Spec{Namespace: ns, Name: name, DisplayName: "Display " + name, Description: "desc " + name, SystemMessage: "You are " + name,
		ModelConfig: "default-model-config", Toolset: toolset, Labels: labels,
		Skills: &Skills{GitRefs: []SkillGitRef{{URL: "https://github.com/giantswarm/agent-skills", Path: "a", Ref: testCommit}}}}, bindsPlatformServer: bindsPlatform}
	tpl, err := BuildAgentTemplate(d, testCompose, "seed@lab.local")
	if err != nil {
		panic(err)
	}
	tpl.SetGeneration(1)
	if len(entries) > 0 {
		list := make([]any, 0, len(entries))
		for _, e := range entries {
			list = append(list, e)
		}
		tpl.Object["status"] = map[string]any{"observedGeneration": int64(1), "harnesses": list}
	}
	return tpl
}

func carrier(ns, agent string, toolset ...string) *unstructured.Unstructured {
	c, _ := BuildToolsetCarrier(agent, toolset, platformServer(ns), testCompose, "seed@lab.local")
	c.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Accepted", "status": "True", "reason": "DiscoveryDisabled", "message": "Tool discovery is disabled"}}}
	return c
}

// seeded is the platform namespace as the connectivity chart and kagent leave
// it — two ModelConfigs, the two Harnesses, the muster server — plus the agent
// `verifier`, created here earlier and Ready on the kagent Harness.
func seeded(t *testing.T, more ...runtime.Object) *fixture {
	objs := []runtime.Object{
		modelConfig("kagent", "default-model-config", "Anthropic", "claude-sonnet-4-6"),
		modelConfig("kagent", "qwen3-8-27b", "OpenAI", "qwen3-8-27b"),
		harness("kagent", "kagent", nil),
		harness("kagent", "claude", nil),
		platformServer("kagent"),
		template("kagent", "verifier", []string{"preset:read-only"}, false, nil, harnessEntry("kagent", 1, boolPtr(true), "")),
		carrier("kagent", "verifier", "preset:read-only"),
	}
	return newFixture(t, append(objs, more...)...)
}

func TestListAndGetReadTemplatesAndCarriers(t *testing.T) {
	ctx := context.Background()
	// Written by hand (the lab's smoke template): binds muster directly.
	smoke := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": DefaultAPIVersion, "kind": kindAgentTemplate,
		"metadata": map[string]any{"name": "smoke", "namespace": "kagent", "labels": map[string]any{HarnessLabel: "kagent", "kagent.dev/e2e-runtime": "kagent"}},
		"spec": map[string]any{"description": "Minimal fixture", "modelConfig": map[string]any{"name": "default-model-config"}, "systemPrompt": "Reply briefly.",
			"tools": []any{mcpBinding("muster")}},
	}}
	// Applied from git: no tools, not ready yet.
	gitops := template("kagent", "gitops", nil, false, map[string]string{KustomizationNameLabel: "flux-system"}, harnessEntry("kagent", 1, nil, ""))
	// Ours, but the carrier is gone.
	orphan := template("kagent", "orphan", []string{"preset:none"}, false, nil)
	f := seeded(t, smoke, gitops, orphan)

	list, err := f.svc.List(ctx, "")
	require.NoError(t, err)
	require.Len(t, list, 4)
	byName := map[string]Agent{}
	for _, a := range list {
		byName[a.Name] = a
	}

	v := byName["verifier"]
	assert.Equal(t, "Display verifier", v.DisplayName)
	assert.Equal(t, "desc verifier", v.Description)
	assert.Equal(t, "You are verifier", v.SystemMessage)
	assert.Equal(t, "default-model-config", v.ModelConfig)
	assert.Equal(t, "kagent", v.Harness)
	assert.Equal(t, []string{"preset:read-only"}, v.Toolset)
	assert.False(t, v.ImplicitFullAccess)
	require.NotNil(t, v.ToolsetCarrier)
	assert.True(t, v.ToolsetCarrier.Exists)
	assert.Equal(t, "preset:read-only", v.ToolsetCarrier.Header)
	assert.True(t, *v.ToolsetCarrier.Accepted)
	require.NotNil(t, v.Skills)
	assert.Equal(t, SkillGitRef{URL: "https://github.com/giantswarm/agent-skills", Path: "a", Ref: testCommit, Name: "a"}, v.Skills.GitRefs[0])
	assert.Equal(t, ManagedAgentManager, v.Managed)
	require.NotNil(t, v.Ready)
	assert.True(t, *v.Ready)
	require.Len(t, v.Harnesses, 1)
	assert.Equal(t, "kagent", v.Harnesses[0].Harness)
	assert.True(t, *v.Harnesses[0].Ready)
	assert.NotEmpty(t, v.Harnesses[0].LatestSuccessfulRevision)
	assert.Equal(t, "default-model-config", v.Spec["modelConfig"].(map[string]any)["name"], "the kagent contract is served as is")
	assert.Empty(t, v.UnmanagedFields)

	s := byName["smoke"]
	assert.Equal(t, ManagedExternal, s.Managed)
	assert.True(t, s.ImplicitFullAccess, "a template binding the platform server has every tool the gateway exposes")
	assert.Nil(t, s.Toolset)
	assert.Nil(t, s.ToolsetCarrier)
	assert.Nil(t, s.Ready, "no Harness has reported")
	assert.Equal(t, "Minimal fixture", s.Description)
	assert.Empty(t, s.DisplayName)

	g := byName["gitops"]
	assert.Equal(t, ManagedGitOps, g.Managed)
	assert.Nil(t, g.Toolset)
	assert.False(t, g.ImplicitFullAccess, "no tools at all is neither a toolset nor full access")
	require.NotNil(t, g.Ready)
	assert.False(t, *g.Ready, "reported but not Ready yet")

	o := byName["orphan"]
	assert.Nil(t, o.Toolset)
	require.NotNil(t, o.ToolsetCarrier)
	assert.False(t, o.ToolsetCarrier.Exists)
	assert.Equal(t, "muster-orphan", o.ToolsetCarrier.Name)
	require.Len(t, o.UnmanagedFields, 1)
	assert.Contains(t, o.UnmanagedFields[0], "does not exist")

	got, err := f.svc.Get(ctx, "", "verifier")
	require.NoError(t, err)
	assert.Equal(t, []string{"preset:read-only"}, got.Toolset)
	_, err = f.svc.Get(ctx, "", "nothing")
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = f.svc.List(ctx, "other")
	assert.ErrorIs(t, err, ErrInvalid, "unmanaged namespaces are refused")
}

func TestCreateChecksEverythingThenWritesCarrierAndTemplate(t *testing.T) {
	f := seeded(t)
	ctx := context.Background()
	readOnly := []string{"preset:read-only"}
	base := func() Spec { return Spec{Name: "sre", ModelConfig: "default-model-config", Toolset: readOnly} }

	for name, tc := range map[string]struct {
		mutate  func(*Spec)
		wantErr error
		want    []string
	}{
		"bad name":            {func(s *Spec) { s.Name = "Bad" }, ErrInvalid, []string{"DNS-1123"}},
		"no toolset":          {func(s *Spec) { s.Toolset = nil }, ErrInvalid, []string{"toolset is required", "preset:read-only", "preset:none", "preset:infrastructure", "preset:agent-platform", "preset:full"}},
		"empty toolset":       {func(s *Spec) { s.Toolset = []string{} }, ErrInvalid, []string{"preset:none"}},
		"reserved selector":   {func(s *Spec) { s.Toolset = []string{"toolset:shared"} }, ErrInvalid, []string{"reserved"}},
		"toolNames removed":   {func(s *Spec) { s.RemovedToolNames = json.RawMessage(`["x_a_b"]`) }, ErrInvalid, []string{"toolNames never narrowed anything against muster"}},
		"runtime removed":     {func(s *Spec) { s.RemovedRuntime = json.RawMessage(`"python"`) }, ErrInvalid, []string{"runtime is gone", "harness"}},
		"iconUrl removed":     {func(s *Spec) { s.RemovedIconURL = json.RawMessage(`"https://x"`) }, ErrInvalid, []string{"iconUrl is gone"}},
		"gitAuthSecret":       {func(s *Spec) { s.Skills = &Skills{RemovedGitAuthSecretName: json.RawMessage(`"x"`)} }, ErrInvalid, []string{"gitAuthSecretName is gone"}},
		"unknown modelConfig": {func(s *Spec) { s.ModelConfig = "nope" }, ErrInvalid, []string{"default-model-config, qwen3-8-27b"}},
		"unknown harness":     {func(s *Spec) { s.Harness = "codex" }, ErrInvalid, []string{"no Harness in namespace kagent admits", "kagent.dev/harness=codex", "claude (admits kagent.dev/harness=claude)", "kagent (admits kagent.dev/harness=kagent)"}},
		"harness label":       {func(s *Spec) { s.Labels = map[string]string{HarnessLabel: "claude"} }, ErrInvalid, []string{"conflicts with harness"}},
		"mutable skill":       {func(s *Spec) { s.Skills = &Skills{GitRefs: []SkillGitRef{{URL: "https://github.com/o/r", Ref: "main"}}} }, ErrInvalid, []string{"full git commit id", "list_skills reports each skill's commit"}},
		"tagged oci skill":    {func(s *Spec) { s.Skills = &Skills{Refs: []string{"ghcr.io/o/s:1"}} }, ErrInvalid, []string{"digest-pinned"}},
		"exists":              {func(s *Spec) { s.Name = "verifier" }, ErrConflict, []string{"already exists", "update_agent"}},
	} {
		t.Run(name, func(t *testing.T) {
			spec := base()
			tc.mutate(&spec)
			_, err := f.svc.Create(ctx, spec)
			require.ErrorIs(t, err, tc.wantErr)
			for _, want := range tc.want {
				assert.Contains(t, err.Error(), want)
			}
		})
	}
	// A carrier of the agent's name blocks the create too.
	_, err := f.dyn.Resource(rmsGVR).Namespace("kagent").Create(ctx, carrier("kagent", "taken", "preset:full"), metav1.CreateOptions{})
	require.NoError(t, err)
	spec := base()
	spec.Name = "taken"
	_, err = f.svc.Create(ctx, spec)
	require.ErrorIs(t, err, ErrConflict)
	assert.Contains(t, err.Error(), "muster-taken")

	// Nothing was written by the failures.
	assert.Empty(t, f.writes("agenttemplates"))
	assert.Equal(t, []string{"create"}, f.writes("remotemcpservers"), "only the test's own seed")
	f.dyn.ClearActions()

	res, err := f.svc.Create(ctx, Spec{
		Name: "sre", DisplayName: "SRE", Description: "helps", SystemMessage: "Be brief.", ModelConfig: "qwen3-8-27b", Harness: "claude",
		Skills:  &Skills{GitRefs: []SkillGitRef{{URL: "https://github.com/giantswarm/agent-skills", Path: "runbooks", Ref: testCommit}}},
		Toolset: []string{"preset:read-only", "workflow:incident-triage"},
		Labels:  map[string]string{"tenant": "sre"},
	})
	require.NoError(t, err)
	assert.True(t, res.Created.AgentTemplate)
	assert.True(t, res.Created.ToolsetCarrier)
	assert.Equal(t, []string{"create"}, f.writes("remotemcpservers"))
	assert.Equal(t, []string{"create"}, f.writes("agenttemplates"))
	assert.Equal(t, "sre", res.Agent.Name)
	assert.Equal(t, "claude", res.Agent.Harness)
	assert.Equal(t, []string{"preset:read-only", "workflow:incident-triage"}, res.Agent.Toolset)
	assert.False(t, res.Agent.ImplicitFullAccess)
	assert.Equal(t, ManagedAgentManager, res.Agent.Managed)
	assert.Nil(t, res.Agent.Ready, "kagent has not reported yet")
	assert.Contains(t, res.Manifests.AgentTemplate, "kind: AgentTemplate")
	assert.Contains(t, res.Manifests.ToolsetCarrier, "kind: RemoteMCPServer")
	require.NotNil(t, res.Status)
	assert.Equal(t, VerdictProgressing, res.Status.Verdict, res.Status.Summary)

	tpl := f.get(t, tplGVR, "kagent", "sre")
	assert.Equal(t, map[string]string{ManagedByLabel: ManagedByValue, HarnessLabel: "claude", "tenant": "sre"}, tpl.GetLabels())
	assert.Equal(t, "SRE", tpl.GetAnnotations()[DisplayNameAnnotation])
	assert.Equal(t, map[string]any{
		"description":  "helps",
		"systemPrompt": "Be brief.",
		"modelConfig":  map[string]any{"name": "qwen3-8-27b"},
		"skills":       []any{map[string]any{"name": "runbooks", "source": map[string]any{"git": map[string]any{"url": "https://github.com/giantswarm/agent-skills", "commit": testCommit}, "path": "runbooks"}}},
		"tools":        []any{mcpBinding("muster-sre")},
	}, tpl.Object["spec"], "the template binds the carrier, not the platform server")

	c := f.get(t, rmsGVR, "kagent", "muster-sre")
	assert.Equal(t, "preset:read-only,workflow:incident-triage", carrierHeader(c))
	assert.Equal(t, "disabled", c.GetLabels()[DiscoveryLabel])
	assert.Equal(t, "sre", c.GetLabels()[AgentLabel])
	assert.Equal(t, ManagedByValue, c.GetLabels()[ManagedByLabel])
	url, _, _ := unstructured.NestedString(c.Object, "spec", "url")
	assert.Equal(t, "http://muster.agent-platform.svc.cluster.local:8090/mcp", url)
	headers, _, _ := unstructured.NestedSlice(c.Object, "spec", "headersFrom")
	assert.Len(t, headers, 1, "never an Authorization header: the caller's bearer is the only one")

	// Another managed namespace: it needs its own platform server.
	_, err = f.dyn.Resource(mcGVR).Namespace("tenant").Create(ctx, modelConfig("tenant", "mc", "Ollama", "qwen3"), metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = f.dyn.Resource(hGVR).Namespace("tenant").Create(ctx, harness("tenant", "kagent", nil), metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = f.svc.Create(ctx, Spec{Namespace: "tenant", Name: "t1", ModelConfig: "mc", Toolset: []string{"preset:none"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "platform RemoteMCPServer tenant/muster does not exist")
	assert.False(t, f.exists(tplGVR, "tenant", "t1"))
	_, err = f.dyn.Resource(rmsGVR).Namespace("tenant").Create(ctx, platformServer("tenant"), metav1.CreateOptions{})
	require.NoError(t, err)
	res, err = f.svc.Create(ctx, Spec{Namespace: "tenant", Name: "t1", ModelConfig: "mc", Toolset: []string{"preset:none"}})
	require.NoError(t, err)
	assert.Equal(t, "kagent", res.Agent.Harness, "the default Harness")
	assert.True(t, f.exists(rmsGVR, "tenant", "muster-t1"))
}

func TestCreateRemovesTheCarrierWhenTheTemplateFails(t *testing.T) {
	f := seeded(t)
	f.dyn.PrependReactor("create", "agenttemplates", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("admission webhook denied the request")
	})
	_, err := f.svc.Create(context.Background(), Spec{Name: "sre", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "create AgentTemplate kagent/sre")
	assert.False(t, f.exists(rmsGVR, "kagent", "muster-sre"), "the carrier does not outlive the failed template")
	assert.Equal(t, []string{"create", "delete"}, f.writes("remotemcpservers"))
}

func TestValidateCreateIsADryRun(t *testing.T) {
	f := seeded(t)
	ctx := context.Background()
	res, err := f.svc.ValidateCreate(ctx, Spec{Name: "verifier", ModelConfig: "nope", Harness: "codex", Skills: &Skills{Refs: []string{"ghcr.io/o/s:1"}}})
	require.NoError(t, err)
	assert.False(t, res.Valid)
	assert.Equal(t, "create", res.Mode)
	joined := strings.Join(res.Errors, "\n")
	assert.Contains(t, joined, "already exists")
	assert.Contains(t, joined, "does not exist")
	assert.Contains(t, joined, "no Harness in namespace kagent admits")
	assert.Contains(t, joined, "toolset is required")
	assert.Contains(t, joined, "digest-pinned")
	assert.Empty(t, res.Manifests.AgentTemplate, "no template for skills that cannot be expressed")

	// The removed arguments are a hard refusal, not a listed violation.
	_, err = f.svc.ValidateCreate(ctx, Spec{Name: "fresh", ModelConfig: "default-model-config", Toolset: []string{"preset:full"}, RemovedToolNames: json.RawMessage(`[]`)})
	require.ErrorIs(t, err, ErrInvalid)

	ok, err := f.svc.ValidateCreate(ctx, Spec{Name: "fresh", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"}})
	require.NoError(t, err)
	assert.True(t, ok.Valid, ok.Errors)
	assert.Contains(t, ok.Manifests.AgentTemplate, "name: muster-fresh")
	assert.Contains(t, ok.Manifests.ToolsetCarrier, "value: preset:read-only")
	assert.Empty(t, f.writes("agenttemplates"), "validate writes nothing")
	assert.Empty(t, f.writes("remotemcpservers"))
}

func TestUpdateWritesTheCarrierForAToolsetAndTheTemplateForTheRest(t *testing.T) {
	f := seeded(t)
	ctx := context.Background()
	str := func(s string) *string { return &s }

	// A description change: the template changes (a new revision), the
	// carrier does not.
	res, err := f.svc.Update(ctx, Update{Name: "verifier", Description: str("now with a description"), DisplayName: str("Verifier 2")})
	require.NoError(t, err)
	assert.Equal(t, []string{"description", "displayName"}, res.Changed)
	assert.Equal(t, "desc verifier", res.Before["description"])
	assert.Equal(t, "now with a description", res.After["description"])
	assert.Equal(t, revisionNote, res.Note)
	assert.Equal(t, []string{"update"}, f.writes("agenttemplates"))
	assert.Empty(t, f.writes("remotemcpservers"))
	tpl := f.get(t, tplGVR, "kagent", "verifier")
	assert.Equal(t, "now with a description", tpl.Object["spec"].(map[string]any)["description"])
	assert.Equal(t, "Verifier 2", tpl.GetAnnotations()[DisplayNameAnnotation])
	assert.Equal(t, []any{mcpBinding("muster-verifier")}, tpl.Object["spec"].(map[string]any)["tools"], "the binding is kept")
	f.dyn.ClearActions()

	// A toolset change: the carrier changes, the template does not — no
	// new revision.
	res, err = f.svc.Update(ctx, Update{Name: "verifier", Toolset: &[]string{"preset:read-only", "server:github"}})
	require.NoError(t, err)
	assert.Equal(t, []string{"toolset"}, res.Changed)
	assert.Empty(t, res.Note)
	assert.Equal(t, []any{"preset:read-only", "server:github"}, res.After["toolset"])
	assert.Equal(t, []string{"preset:read-only", "server:github"}, res.Agent.Toolset)
	assert.Empty(t, f.writes("agenttemplates"), "a toolset change never touches the template")
	assert.Equal(t, []string{"update"}, f.writes("remotemcpservers"))
	assert.Equal(t, "preset:read-only,server:github", carrierHeader(f.get(t, rmsGVR, "kagent", "muster-verifier")))
	f.dyn.ClearActions()

	// The toolset is replaced as a whole and never cleared; the grammar holds.
	_, err = f.svc.Update(ctx, Update{Name: "verifier", Toolset: &[]string{}})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "preset:none")
	_, err = f.svc.Update(ctx, Update{Name: "verifier", Toolset: &[]string{"toolset:shared"}})
	require.ErrorIs(t, err, ErrInvalid)
	_, err = f.svc.Update(ctx, Update{Name: "verifier", RemovedToolNames: json.RawMessage(`["x_a_b"]`)})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "toolNames never narrowed")
	_, err = f.svc.Update(ctx, Update{Name: "verifier", RemovedRuntime: json.RawMessage(`"go"`)})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "runtime is gone")
	_, err = f.svc.Update(ctx, Update{Name: "verifier", ModelConfig: str("nope")})
	assert.ErrorIs(t, err, ErrInvalid)
	_, err = f.svc.Update(ctx, Update{Name: "verifier", Harness: str("codex")})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "no Harness in namespace kagent admits")
	_, err = f.svc.Update(ctx, Update{Name: "verifier", Skills: &Skills{GitRefs: []SkillGitRef{{URL: "https://github.com/o/r", Ref: "main"}}}})
	require.ErrorIs(t, err, ErrInvalid)
	_, err = f.svc.Update(ctx, Update{Name: "missing", DisplayName: str("x")})
	assert.ErrorIs(t, err, ErrNotFound)
	dry, err := f.svc.ValidateUpdate(ctx, Update{Name: "verifier", Toolset: &[]string{"label:x=y"}})
	require.NoError(t, err)
	assert.False(t, dry.Valid)
	assert.Contains(t, strings.Join(dry.Errors, "\n"), "presets only")
	assert.Empty(t, f.writes("agenttemplates"), "refusals and dry runs write nothing")
	assert.Empty(t, f.writes("remotemcpservers"))

	// Moving to another Harness relabels the template; clearing a field
	// drops it; skills replace the block.
	res, err = f.svc.Update(ctx, Update{Name: "verifier", Harness: str("claude"), Description: str(""), Skills: &Skills{Refs: []string{"ghcr.io/o/kubectl@" + testDigest}}})
	require.NoError(t, err)
	assert.Equal(t, []string{"description", "harness", "skills.gitRefs", "skills.refs"}, res.Changed)
	tpl = f.get(t, tplGVR, "kagent", "verifier")
	assert.Equal(t, "claude", tpl.GetLabels()[HarnessLabel])
	_, hasDesc := tpl.Object["spec"].(map[string]any)["description"]
	assert.False(t, hasDesc)
	assert.Equal(t, []any{map[string]any{"name": "kubectl", "source": map[string]any{"oci": "ghcr.io/o/kubectl@" + testDigest}}}, tpl.Object["spec"].(map[string]any)["skills"])
	assert.Equal(t, "claude", res.Agent.Harness)
	f.dyn.ClearActions()

	// No change is a no-op.
	res, err = f.svc.Update(ctx, Update{Name: "verifier"})
	require.NoError(t, err)
	assert.Empty(t, res.Changed)
	assert.Empty(t, f.writes("agenttemplates"))
}

func TestUpdateHonorsOwnershipAndUnmanagedFields(t *testing.T) {
	ctx := context.Background()
	str := func(s string) *string { return &s }
	// Applied from git.
	gitops := template("kagent", "gitops", []string{"preset:none"}, false, map[string]string{KustomizationNameLabel: "flux-system"})
	// Written by hand, binding the platform server: implicit full access.
	external := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": DefaultAPIVersion, "kind": kindAgentTemplate,
		"metadata": map[string]any{"name": "smoke", "namespace": "kagent", "labels": map[string]any{HarnessLabel: "kagent"}},
		"spec":     map[string]any{"description": "fixture", "modelConfig": map[string]any{"name": "default-model-config"}, "tools": []any{mcpBinding("muster")}},
	}}
	// Ours, but carrying a plugin bundle agent-manager does not compose.
	plugged := template("kagent", "plugged", []string{"preset:none"}, false, nil)
	spec := plugged.Object["spec"].(map[string]any)
	spec["plugins"] = []any{map[string]any{"source": map[string]any{"oci": "ghcr.io/o/p@" + testDigest}}}
	f := seeded(t, gitops, external, carrier("kagent", "gitops", "preset:none"), plugged, carrier("kagent", "plugged", "preset:none"))

	_, err := f.svc.Update(ctx, Update{Name: "gitops", DisplayName: str("x")})
	require.ErrorIs(t, err, ErrConflict)
	assert.Contains(t, err.Error(), "flux-system")
	res, err := f.svc.Update(ctx, Update{Name: "gitops", DisplayName: str("x"), Force: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"displayName"}, res.Changed)
	assert.Equal(t, "flux-system", f.get(t, tplGVR, "kagent", "gitops").GetLabels()[KustomizationNameLabel], "tooling labels survive the write")

	_, err = f.svc.Update(ctx, Update{Name: "smoke", Toolset: &[]string{"preset:read-only"}})
	require.ErrorIs(t, err, ErrConflict)
	assert.Contains(t, err.Error(), "not created by agent-manager")
	// Taking it over assigns the toolset: the carrier appears and the
	// template is rebound from the platform server to it (a new revision).
	res, err = f.svc.Update(ctx, Update{Name: "smoke", Toolset: &[]string{"preset:read-only"}, Force: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"toolset"}, res.Changed)
	assert.Equal(t, revisionNote, res.Note, "the binding moved, so the template changed")
	assert.False(t, res.Agent.ImplicitFullAccess)
	assert.Equal(t, []string{"preset:read-only"}, res.Agent.Toolset)
	assert.Equal(t, ManagedAgentManager, res.Agent.Managed, "taken over")
	tpl := f.get(t, tplGVR, "kagent", "smoke")
	assert.Equal(t, []any{mcpBinding("muster-smoke")}, tpl.Object["spec"].(map[string]any)["tools"])
	assert.Equal(t, "preset:read-only", carrierHeader(f.get(t, rmsGVR, "kagent", "muster-smoke")))

	_, err = f.svc.Update(ctx, Update{Name: "plugged", DisplayName: str("x")})
	require.ErrorIs(t, err, ErrConflict)
	assert.Contains(t, err.Error(), "plugins")
	res, err = f.svc.Update(ctx, Update{Name: "plugged", DisplayName: str("x"), Force: true})
	require.NoError(t, err)
	_, hasPlugins := f.get(t, tplGVR, "kagent", "plugged").Object["spec"].(map[string]any)["plugins"]
	assert.False(t, hasPlugins, "force overwrites what agent-manager does not compose")
	assert.Empty(t, res.Agent.UnmanagedFields)

	// Delete honours the same ownership, not the unmanaged fields.
	_, err = f.svc.Delete(ctx, "", "gitops", false)
	assert.ErrorIs(t, err, ErrConflict)
	del, err := f.svc.Delete(ctx, "", "gitops", true)
	require.NoError(t, err)
	assert.True(t, del.AgentTemplateDeleted)
	assert.True(t, del.ToolsetCarrierDeleted)
}

func TestDeleteRemovesTemplateAndCarrier(t *testing.T) {
	f := seeded(t)
	ctx := context.Background()

	res, err := f.svc.Delete(ctx, "", "verifier", false)
	require.NoError(t, err)
	assert.True(t, res.AgentTemplateDeleted)
	assert.True(t, res.ToolsetCarrierDeleted)
	assert.Empty(t, res.ToolsetCarrierKept)
	assert.False(t, f.exists(tplGVR, "kagent", "verifier"))
	assert.False(t, f.exists(rmsGVR, "kagent", "muster-verifier"))
	_, err = f.svc.Delete(ctx, "", "verifier", false)
	assert.ErrorIs(t, err, ErrNotFound)

	// A carrier somebody else made under the agent's name is kept.
	_, err = f.svc.Create(ctx, Spec{Name: "sre", ModelConfig: "default-model-config", Toolset: []string{"preset:none"}})
	require.NoError(t, err)
	foreign := f.get(t, rmsGVR, "kagent", "muster-sre")
	foreign.SetLabels(map[string]string{DiscoveryLabel: "disabled"})
	_, err = f.dyn.Resource(rmsGVR).Namespace("kagent").Update(ctx, foreign, metav1.UpdateOptions{})
	require.NoError(t, err)
	res, err = f.svc.Delete(ctx, "", "sre", false)
	require.NoError(t, err)
	assert.True(t, res.AgentTemplateDeleted)
	assert.False(t, res.ToolsetCarrierDeleted)
	assert.Contains(t, res.ToolsetCarrierKept, "not created by agent-manager")
	assert.True(t, f.exists(rmsGVR, "kagent", "muster-sre"))

	// An externally written template needs force.
	_, err = f.dyn.Resource(tplGVR).Namespace("kagent").Create(ctx, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": DefaultAPIVersion, "kind": kindAgentTemplate,
		"metadata": map[string]any{"name": "smoke", "namespace": "kagent"},
		"spec":     map[string]any{"modelConfig": map[string]any{"name": "default-model-config"}},
	}}, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = f.svc.Delete(ctx, "", "smoke", false)
	assert.ErrorIs(t, err, ErrConflict)
	res, err = f.svc.Delete(ctx, "", "smoke", true)
	require.NoError(t, err)
	assert.True(t, res.AgentTemplateDeleted)
	assert.False(t, res.ToolsetCarrierDeleted, "there was none")
}

func TestStatusVerdicts(t *testing.T) {
	ctx := context.Background()
	ro := []string{"preset:read-only"}
	cases := []struct {
		name    string
		tpl     *unstructured.Unstructured
		carrier *unstructured.Unstructured
		verdict string
		summary string
	}{
		{"ready on one harness", template("kagent", "a", ro, false, nil, harnessEntry("kagent", 1, boolPtr(true), "")), carrier("kagent", "a", ro...), VerdictReady, "Ready on Harness kagent"},
		{"ready with warnings", template("kagent", "a", ro, false, nil, harnessEntry("kagent", 1, boolPtr(true), "", "tool selection downgraded")), carrier("kagent", "a", ro...), VerdictReady, "1 compile warning"},
		{"compiling", template("kagent", "a", ro, false, nil, harnessEntry("kagent", 1, nil, "")), carrier("kagent", "a", ro...), VerdictProgressing, "compiling the revision for Harness kagent"},
		{"not observed yet", template("kagent", "a", ro, false, nil), carrier("kagent", "a", ro...), VerdictProgressing, "has not reported on the AgentTemplate yet"},
		{"generation behind", func() *unstructured.Unstructured {
			tpl := template("kagent", "a", ro, false, nil, harnessEntry("kagent", 1, nil, ""))
			tpl.SetGeneration(2)
			return tpl
		}(), carrier("kagent", "a", ro...), VerdictProgressing, "not observed generation 2"},
		{"no harness admits", func() *unstructured.Unstructured {
			tpl := template("kagent", "a", ro, false, nil)
			tpl.Object["status"] = map[string]any{"observedGeneration": int64(1)}
			return tpl
		}(), carrier("kagent", "a", ro...), VerdictFailed, "no Harness admits"},
		{"a failing stage", template("kagent", "a", ro, false, nil, harnessEntry("kagent", 1, nil, "ResolvedRefs")), carrier("kagent", "a", ro...), VerdictFailed, "ResolvedRefs is False (WorkerPoolNotFound"},
		{"a failed snapshot", template("kagent", "a", ro, false, nil, harnessEntry("kagent", 1, boolPtr(false), "")), carrier("kagent", "a", ro...), VerdictFailed, "Ready is False (ActorTemplateFailed"},
		{"stale ready is progressing", func() *unstructured.Unstructured {
			tpl := template("kagent", "a", ro, false, nil, harnessEntry("kagent", 1, boolPtr(true), ""))
			tpl.SetGeneration(2)
			return tpl
		}(), carrier("kagent", "a", ro...), VerdictProgressing, "not observed generation 2"},
		{"carrier missing", template("kagent", "a", ro, false, nil, harnessEntry("kagent", 1, boolPtr(true), "")), nil, VerdictFailed, "muster-a, which does not exist"},
		{"carrier rejected", template("kagent", "a", ro, false, nil, harnessEntry("kagent", 1, boolPtr(true), "")), func() *unstructured.Unstructured {
			c := carrier("kagent", "a", ro...)
			c.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Accepted", "status": "False", "reason": "ReconcileFailed", "message": "401 Unauthorized"}}}
			return c
		}(), VerdictFailed, "rejected the toolset carrier"},
		{"implicit full access ready", template("kagent", "a", nil, true, nil, harnessEntry("kagent", 1, boolPtr(true), "")), nil, VerdictReady, "Ready on Harness kagent"},
		{"two harnesses, one ready", template("kagent", "a", nil, true, nil, harnessEntry("claude", 1, nil, "Compatible"), harnessEntry("kagent", 1, boolPtr(true), "")), nil, VerdictFailed, "Harness claude: Compatible is False"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := statusOf(tc.tpl, tc.carrier, testCompose)
			assert.Equal(t, tc.verdict, st.Verdict, st.Summary)
			assert.Contains(t, st.Summary, tc.summary)
		})
	}

	f := seeded(t)
	st, err := f.svc.Status(ctx, "", "verifier")
	require.NoError(t, err)
	assert.Equal(t, VerdictReady, st.Verdict)
	require.NotNil(t, st.Template)
	require.Len(t, st.Template.Harnesses, 1)
	assert.Equal(t, int64(1), st.Template.ObservedGeneration)
	require.NotNil(t, st.ToolsetCarrier)
	assert.True(t, st.ToolsetCarrier.Exists)
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
	assert.True(t, info.Capabilities["harnesses"])
	assert.True(t, info.Capabilities["toolsetCarrier"])
	assert.False(t, info.Capabilities["skills"], "no repositories configured")
	assert.False(t, info.Capabilities["writesAsCaller"])
	for _, gone := range []string{"perToolSelection", "runtime", "iconUrl", "skillGitAuthSecret", "mutableSkillRefs"} {
		v, ok := info.Capabilities[gone]
		assert.True(t, ok, gone)
		assert.False(t, v, gone)
	}
	assert.Equal(t, "kagent.dev/v1alpha3", info.APIVersions.AgentTemplate)
	assert.Equal(t, "kagent.dev/v1alpha3", info.APIVersions.RemoteMCPServer)
	assert.Equal(t, "kagent.dev/v1alpha3", info.APIVersions.Harness)
	assert.Equal(t, "muster", info.Kagent.MusterServer)
	assert.Equal(t, "kagent", info.Kagent.DefaultHarness)
	assert.Equal(t, ToolsetHeader, info.Kagent.ToolsetHeader)

	_, err = f.svc.ListSkills(ctx, "", "", false)
	assert.ErrorIs(t, err, ErrUnsupported)
}

// TestMutationsCarryTheCaller: with OAuth in front, every write is attributed
// to the authenticated caller — on the result (requestedBy), on the objects
// and in the log — and a caller-only server refuses to act for a request
// without a token.
func TestMutationsCarryTheCaller(t *testing.T) {
	f := seeded(t)
	ctx := identity.ContextWith(context.Background(), &identity.Identity{Subject: "sub-1", Email: "admin@lab.local", Source: identity.SourceSSO})

	created, err := f.svc.Create(ctx, Spec{Name: "sre", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"}})
	require.NoError(t, err)
	assert.Equal(t, "admin@lab.local", created.RequestedBy)
	assert.Equal(t, "admin@lab.local", f.get(t, tplGVR, "kagent", "sre").GetAnnotations()[RequestedByAnnotation])
	assert.Equal(t, "admin@lab.local", f.get(t, rmsGVR, "kagent", "muster-sre").GetAnnotations()[RequestedByAnnotation])

	desc := "on call"
	updated, err := f.svc.Update(identity.ContextWith(context.Background(), &identity.Identity{Email: "dev@lab.local"}), Update{Name: "sre", Description: &desc})
	require.NoError(t, err)
	assert.Equal(t, "dev@lab.local", updated.RequestedBy)
	assert.Equal(t, "dev@lab.local", f.get(t, tplGVR, "kagent", "sre").GetAnnotations()[RequestedByAnnotation], "the last writer")

	deleted, err := f.svc.Delete(ctx, "", "sre", false)
	require.NoError(t, err)
	assert.Equal(t, "admin@lab.local", deleted.RequestedBy)

	anonymous, err := f.svc.Create(context.Background(), Spec{Name: "sre", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"}})
	require.NoError(t, err)
	assert.Empty(t, anonymous.RequestedBy, "without OAuth there is no caller to report")
	_, has := f.get(t, tplGVR, "kagent", "sre").GetAnnotations()[RequestedByAnnotation]
	assert.False(t, has)

	// The caller provider (downstream OAuth): no token on the request, no
	// Kubernetes call — 401, not a ServiceAccount fallback.
	callerOnly := New(kube.NewCallerProvider(kube.FromInterfaces(f.dyn, kubefake.NewClientset().Discovery()), nil), nil, Config{DefaultNamespace: "kagent", Version: "test"}, nil)
	assert.True(t, callerOnly.Info(context.Background()).Capabilities["writesAsCaller"])
	assert.Equal(t, kube.IdentityCaller, callerOnly.Info(context.Background()).Identity)
	_, err = callerOnly.List(context.Background(), "")
	assert.ErrorIs(t, err, ErrUnauthenticated)
	_, err = callerOnly.Create(context.Background(), Spec{Name: "sre", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"}})
	assert.ErrorIs(t, err, ErrUnauthenticated)
}

func TestHarnessAdmissionMatchesLikeTheController(t *testing.T) {
	harnesses := []harnessAdmission{
		harnessAdmissionOf(harness("kagent", "kagent", nil)),
		harnessAdmissionOf(harness("kagent", "e2e", map[string]any{"kagent.dev/e2e-runtime": "kagent"})),
		harnessAdmissionOf(&unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "closed"}, "spec": map[string]any{}}}),
	}
	assert.NoError(t, requireHarnessAdmission(harnesses, map[string]string{HarnessLabel: "kagent"}, "kagent"))
	assert.NoError(t, requireHarnessAdmission(harnesses, map[string]string{HarnessLabel: "codex", "kagent.dev/e2e-runtime": "kagent"}, "kagent"), "a foreign selector is matched through the request's labels")
	err := requireHarnessAdmission(harnesses, map[string]string{HarnessLabel: "codex"}, "kagent")
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "closed (admits nothing: no allowedAgentTemplates)")
	assert.Contains(t, err.Error(), "e2e (admits kagent.dev/e2e-runtime=kagent)")
	err = requireHarnessAdmission(nil, map[string]string{HarnessLabel: "kagent"}, "kagent")
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "no Harness exists in namespace kagent")
	assert.Equal(t, "kagent (admits kagent.dev/harness=kagent)", fmt.Sprint(harnesses[0]))
}
