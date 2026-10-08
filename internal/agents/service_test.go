package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"

	"github.com/giantswarm/agent-manager/internal/chart"
	"github.com/giantswarm/agent-manager/internal/identity"
	"github.com/giantswarm/agent-manager/internal/kube"
)

const kagentAPI = "api.kagent.dev/v1alpha3"

var (
	hrGVR      = schema.GroupVersionResource{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"}
	ociGVR     = schema.GroupVersionResource{Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "ocirepositories"}
	agentGVR   = schema.GroupVersionResource{Group: "api.kagent.dev", Version: "v1alpha3", Resource: "agents"}
	serverGVR  = schema.GroupVersionResource{Group: "api.kagent.dev", Version: "v1alpha3", Resource: "remotemcpservers"}
	harnessGVR = schema.GroupVersionResource{Group: "api.kagent.dev", Version: "v1alpha3", Resource: "harnesses"}
	mcGVR      = schema.GroupVersionResource{Group: "api.kagent.dev", Version: "v1alpha3", Resource: "modelconfigs"}
	listKinds  = map[schema.GroupVersionResource]string{
		hrGVR: "HelmReleaseList", ociGVR: "OCIRepositoryList", agentGVR: "AgentList",
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

// harness is a Harness of ns whose spec carries the runtime discriminator
// (kagent, claude, codex, byo) next to its workload.
func harness(ns, name, runtime string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": kagentAPI, "kind": "Harness",
		"metadata": map[string]any{"name": name, "namespace": ns},
		"spec": map[string]any{
			runtime:    map[string]any{},
			"workload": map[string]any{"image": "ghcr.io/example/" + runtime + ":1"},
		},
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

// agentStatusObj is an Agent's status as kagent writes it.
func agentStatusObj(observed int64, desired, latest string, ready bool, failing string) map[string]any {
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
	st := map[string]any{"observedGeneration": observed, "desiredRevision": desired, "conditions": conds}
	if latest != "" {
		st["latestSuccessfulRevision"] = latest
	}
	return st
}

// agentObject is what the chart renders for hrName (the owning release in
// hrNs; "" for a bare Agent): the template inline, the kagent Harness by
// name, Ready when ready. It binds its own RemoteMCPServer (the toolset
// carrier) unless bound is false.
func agentObject(ns, name, hrName, hrNs string, ready bool, bound bool) *unstructured.Unstructured {
	labels := map[string]any{}
	if hrName != "" {
		labels[HelmReleaseNameLabel] = hrName
		labels[HelmReleaseNamespaceLabel] = hrNs
	}
	tools := []any{}
	if bound {
		tools = append(tools, map[string]any{"mcp": map[string]any{"server": map[string]any{"kind": kindRemoteMCPServer, "name": name}}})
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": kagentAPI, "kind": "Agent",
		"metadata": map[string]any{"name": name, "namespace": ns, "generation": int64(1), "labels": labels, "annotations": map[string]any{DisplayNameAnnotation: "Display " + name, IconURLAnnotation: "https://avatars.example/" + name + ".png"}},
		"spec": map[string]any{
			"harnessRef": map[string]any{"name": "kagent"},
			"template": map[string]any{
				"description": "desc " + name, "modelConfig": map[string]any{"name": "default-model-config"}, "systemPrompt": "You are " + name,
				"skills": []any{map[string]any{"name": "a", "source": map[string]any{"git": map[string]any{"url": skillsRepo, "commit": mainHead}, "path": "a"}}},
				"tools":  tools,
			},
		},
		"status": agentStatusObj(1, "rev-1", map[bool]string{true: "rev-1", false: ""}[ready], ready, ""),
	}}
}

// onHarness names another Harness in the Agent's spec.harnessRef.
func onHarness(obj *unstructured.Unstructured, name string) *unstructured.Unstructured {
	_ = unstructured.SetNestedField(obj.Object, name, "spec", "harnessRef", "name")
	return obj
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
		ociRepository("kagent", "2.x"),
		helmRelease("kagent", "verifier", verifierValues, true, nil),
		agentObject("kagent", "verifier", "verifier", "kagent", true, true),
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
	obj, server := loadFixture(t, "agent-full.yaml"), loadFixture(t, "remotemcpserver.yaml")
	a := agentFromObject(obj, server)
	assert.Equal(t, "SRE Assistant", a.DisplayName, "display name from the annotation")
	assert.Equal(t, "https://avatars.example/v1/sre.png", a.IconURL, "icon URL from the annotation")
	assert.Equal(t, "helps", a.Description)
	assert.Equal(t, "default-model-config", a.ModelConfig)
	assert.Equal(t, "Be brief.", a.SystemMessage)
	assert.Equal(t, "claude", a.Harness, "the Harness is spec.harnessRef.name")
	assert.Equal(t, Skills{
		{Name: "runbooks", Path: "nested/runbooks", Git: &GitSkill{URL: skillsRepo, Commit: mainHead}},
		{Name: "kubectl", OCI: kubectlRef + "@" + kubectlSum},
	}, a.Skills, "skills are reported as the pins the template carries")
	assert.Equal(t, Plugins{
		{Path: "bundles/sre", Git: &GitSkill{URL: skillsRepo, Commit: tagHead}, Skills: []string{"triage", "postmortem"}},
		{OCI: kubectlRef + "@" + kubectlSum, Skills: []string{"k8s"}},
	}, a.Plugins, "plugins are reported as {source, skills}")
	assert.Equal(t, []string{"preset:read-only", "workflow:incident-triage"}, a.Toolset, "the toolset is the header of the agent's own RemoteMCPServer")
	assert.False(t, a.ImplicitFullAccess)
	assert.Equal(t, []ToolBinding{{Server: "sre"}, {Server: "github", Tools: []string{"get_issue", "list_issues"}}}, a.Tools)
	require.NotNil(t, a.Ready)
	assert.True(t, *a.Ready)
	require.NotNil(t, a.Status)
	assert.Equal(t, "claude", a.Status.Harness)
	assert.Equal(t, "sre-a1b2c3", a.Status.LatestSuccessfulRevision)
	assert.Equal(t, []string{"tools narrowed for server github could not be verified: discovery disabled"}, a.Status.Warnings)
	assert.Equal(t, ManagedNone, a.Managed, "before the owning release is folded in")

	// A carrier without the header is implicit full access; no carrier bound
	// at all (preset:none) is neither.
	implicit := agentFromObject(obj, remoteMCPServer("kagent", "sre"))
	assert.Nil(t, implicit.Toolset)
	assert.True(t, implicit.ImplicitFullAccess)
	none := agentFromObject(agentObject("kagent", "quiet", "quiet", "kagent", true, false), nil)
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
	// A HelmRelease that has not rendered its Agent yet (GitOps-owned), a
	// bare Agent with a toolset on its carrier, and a scoped agent.
	pending := map[string]any{"agent": map[string]any{"name": "pending", "displayName": "Pending", "harness": "claude"}, "modelConfig": map[string]any{"name": "qwen3-8-27b"},
		"skills":  []any{map[string]any{"name": "runbooks", "git": map[string]any{"url": skillsRepo, "commit": tagHead}}},
		"plugins": []any{map[string]any{"oci": kubectlRef + "@" + kubectlSum, "skills": []any{"k8s"}}}}
	mustCreate(t, f, hrGVR, helmRelease("kagent", "pending", pending, false, map[string]any{KustomizationNameLabel: "flux-system"}))
	mustCreate(t, f, agentGVR, agentObject("kagent", "bare", "", "", true, true))
	mustCreate(t, f, serverGVR, remoteMCPServer("kagent", "bare", "preset:read-only", "workflow:incident-triage"))
	scoped := map[string]any{"agent": map[string]any{"name": "scoped"}, "modelConfig": map[string]any{"name": "qwen3-8-27b"}, ToolsetValuesKey: []any{"preset:infrastructure"}}
	mustCreate(t, f, hrGVR, helmRelease("kagent", "scoped", scoped, true, nil))
	mustCreate(t, f, agentGVR, agentObject("kagent", "scoped", "scoped", "kagent", true, true))
	mustCreate(t, f, serverGVR, remoteMCPServer("kagent", "scoped", "preset:infrastructure"))

	list, err := f.svc.List(ctx, In(""))
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
	assert.True(t, p.ImplicitFullAccess, "reported before the Agent is rendered")
	assert.Equal(t, Skills{{Name: "runbooks", Git: &GitSkill{URL: skillsRepo, Commit: tagHead}}}, p.Skills, "skills from the values while nothing is rendered")
	assert.Equal(t, Plugins{{OCI: kubectlRef + "@" + kubectlSum, Skills: []string{"k8s"}}}, p.Plugins, "plugins from the values too")
	assert.Equal(t, "claude", p.Harness)

	b := byName["bare"]
	assert.Equal(t, ManagedNone, b.Managed)
	assert.Nil(t, b.HelmRelease)
	assert.Equal(t, []string{"preset:read-only", "workflow:incident-triage"}, b.Toolset, "a bare Agent reports its carrier's header")
	assert.False(t, b.ImplicitFullAccess)

	sc := byName["scoped"]
	assert.Equal(t, []string{"preset:infrastructure"}, sc.Toolset)
	assert.False(t, sc.ImplicitFullAccess)
	assert.Equal(t, []any{"preset:infrastructure"}, sc.Values[ToolsetValuesKey])

	got, err := f.svc.Get(ctx, In(""), "scoped")
	require.NoError(t, err)
	assert.Equal(t, []string{"preset:infrastructure"}, got.Toolset)
	got, err = f.svc.Get(ctx, In(""), "verifier")
	require.NoError(t, err)
	assert.True(t, got.ImplicitFullAccess)

	_, err = f.svc.List(ctx, In("other"))
	assert.ErrorIs(t, err, ErrInvalid, "unmanaged namespaces are refused")
}

// TestGitOpsOwnedReleaseInAnotherNamespace is the fleet's sre-agent shape:
// HelmRelease + OCIRepository in flux-giantswarm, targetNamespace kagent. The
// Agent's provenance labels lead to the release; it is read-only.
func TestGitOpsOwnedReleaseInAnotherNamespace(t *testing.T) {
	f := seeded(t)
	ctx := context.Background()
	values := map[string]any{"agent": map[string]any{"name": "sre-agent"}, "modelConfig": map[string]any{"name": "default-model-config"}, ToolsetValuesKey: []any{"preset:infrastructure"}}
	hr := helmRelease("flux-giantswarm", "sre-agent", values, true, map[string]any{KustomizationNameLabel: "flux-giantswarm"})
	require.NoError(t, unstructured.SetNestedField(hr.Object, "kagent", "spec", "targetNamespace"))
	mustCreate(t, f, hrGVR, hr)
	mustCreate(t, f, ociGVR, ociRepository("flux-giantswarm", ">=0.2.1 <1.0.0"))
	mustCreate(t, f, agentGVR, agentObject("kagent", "sre-agent", "sre-agent", "flux-giantswarm", true, true))
	mustCreate(t, f, serverGVR, remoteMCPServer("kagent", "sre-agent", "preset:infrastructure"))

	got, err := f.svc.Get(ctx, In(""), "sre-agent")
	require.NoError(t, err)
	assert.Equal(t, ManagedGitOps, got.Managed)
	require.NotNil(t, got.HelmRelease)
	assert.Equal(t, "flux-giantswarm", got.HelmRelease.Namespace)
	assert.True(t, got.HelmRelease.GitOpsOwned)
	assert.Equal(t, []string{"preset:infrastructure"}, got.Toolset)

	list, err := f.svc.List(ctx, In(""))
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
	require.ErrorIs(t, err, ErrGitOpsOwned)
	assert.Contains(t, err.Error(), "flux-giantswarm")
	assert.Contains(t, err.Error(), "mode commit")
	_, err = f.svc.Delete(ctx, In(""), "sre-agent", false, WriteOptions{})
	assert.ErrorIs(t, err, ErrGitOpsOwned)
	_, err = f.svc.Delete(ctx, In(""), "sre-agent", true, WriteOptions{})
	assert.ErrorIs(t, err, ErrGitOpsOwned, "force never writes a GitOps-owned release live")

	st, err := f.svc.Status(ctx, In(""), "sre-agent")
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
	assert.Contains(t, res.Manifests.OCIRepository, "semver: 2.x")
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
	// get_agent reports the pins before the Agent exists, too.
	got, err := f.svc.Get(ctx, In(""), "sre")
	require.NoError(t, err)
	require.Len(t, got.Skills, 3)
	assert.Equal(t, mainHead, got.Skills[0].Git.Commit)
	assert.Equal(t, kubectlRef+"@"+kubectlSum, got.Skills[2].OCI)

	// A namespace without a chart source gets one, tracking 2.x.
	mustCreate(t, f, mcGVR, modelConfig("tenant", "mc", "Ollama", "qwen3"))
	res, err = f.svc.Create(ctx, Spec{Location: In("tenant"), Name: "t1", ModelConfig: "mc", Toolset: []string{"preset:none"}})
	require.NoError(t, err)
	assert.True(t, res.Created.OCIRepository)
	repo, err := f.dyn.Resource(ociGVR).Namespace("tenant").Get(ctx, "agent", metav1.GetOptions{})
	require.NoError(t, err)
	url, _, _ := unstructured.NestedString(repo.Object, "spec", "url")
	assert.Equal(t, DefaultChartOCIURL, url)
	semver, _, _ := unstructured.NestedString(repo.Object, "spec", "ref", "semver")
	assert.Equal(t, "2.x", semver)
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

func TestValidateCreateChecksTheName(t *testing.T) {
	f := seeded(t)
	ctx := t.Context()
	spec := Spec{Name: "verifier", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"}}

	taken, err := f.svc.ValidateCreate(ctx, spec)
	require.NoError(t, err)
	assert.False(t, taken.Valid)
	assert.Contains(t, taken.Errors, "HelmRelease kagent/verifier already exists; use update_agent to change it", "the review page names the clash before Deploy")
	assert.Empty(t, taken.Notes)

	spec.Name = "fresh"
	free, err := f.svc.ValidateCreate(ctx, spec)
	require.NoError(t, err)
	assert.True(t, free.Valid, free.Errors)
	assert.Empty(t, free.Notes)

	// A viewer who may not read the namespace's releases still gets the
	// manifests, with a note that the name went unchecked.
	f.dyn.PrependReactor("get", "helmreleases", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(hrGVR.GroupResource(), "verifier", errors.New("viewer may not get helmreleases"))
	})
	spec.Name = "verifier"
	unchecked, err := f.svc.ValidateCreate(ctx, spec)
	require.NoError(t, err)
	assert.True(t, unchecked.Valid, unchecked.Errors)
	require.Len(t, unchecked.Notes, 1)
	assert.Contains(t, unchecked.Notes[0], `the name "verifier" could not be checked for a clash`)
	assert.Contains(t, unchecked.Notes[0], "forbidden")
	assert.Contains(t, unchecked.Manifests.HelmRelease, "kind: HelmRelease")
}

func TestCreateAndUpdateRefuseATooLongSystemMessage(t *testing.T) {
	f := seeded(t)
	ctx := t.Context()
	tooLong := strings.Repeat("é", MaxSystemMessageLength+1)
	want := "systemMessage is 20001 characters; the agent chart accepts at most 20000. Move long reference material into a skill"

	_, err := f.svc.Create(ctx, Spec{Name: "long", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"}, SystemMessage: tooLong})
	require.ErrorIs(t, err, ErrInvalid)
	require.ErrorContains(t, err, want)
	dry, err := f.svc.ValidateCreate(ctx, Spec{Name: "long", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"}, SystemMessage: tooLong})
	require.NoError(t, err)
	require.False(t, dry.Valid)
	require.Contains(t, dry.Errors, want)

	_, err = f.svc.Update(ctx, Update{Name: "verifier", SystemMessage: &tooLong})
	require.ErrorIs(t, err, ErrInvalid)
	require.ErrorContains(t, err, want)
	dry, err = f.svc.ValidateUpdate(ctx, Update{Name: "verifier", SystemMessage: &tooLong})
	require.NoError(t, err)
	require.False(t, dry.Valid)
	require.Contains(t, dry.Errors, want)

	atCap := strings.Repeat("é", MaxSystemMessageLength)
	_, err = f.svc.Update(ctx, Update{Name: "verifier", SystemMessage: &atCap})
	require.NoError(t, err)
}

func TestUpdateMergesIntoValuesAndHonorsOwnership(t *testing.T) {
	f := seeded(t)
	ctx := context.Background()

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

	// GitOps-owned: refused in apply mode, force or not; commit mode is the
	// way (not offered by this service).
	gitops := helmRelease("kagent", "gitops", map[string]any{"agent": map[string]any{"name": "gitops"}, "modelConfig": map[string]any{"name": "default-model-config"}}, true, map[string]any{KustomizationNameLabel: "flux-system"})
	mustCreate(t, f, hrGVR, gitops)
	_, err = f.svc.Update(ctx, Update{Name: "gitops", DisplayName: str("x")})
	require.ErrorIs(t, err, ErrGitOpsOwned)
	assert.Contains(t, err.Error(), "flux-system")
	_, err = f.svc.Update(ctx, Update{Name: "gitops", DisplayName: str("x"), Force: true})
	require.ErrorIs(t, err, ErrGitOpsOwned)
	_, err = f.svc.Update(ctx, Update{Name: "gitops", DisplayName: str("x"), WriteOptions: WriteOptions{Mode: ModeCommit}})
	require.ErrorIs(t, err, ErrUnsupported)
	// The dry run answers as the write does.
	_, err = f.svc.ValidateUpdate(ctx, Update{Name: "gitops", DisplayName: str("x")})
	require.ErrorIs(t, err, ErrGitOpsOwned)
	_, err = f.svc.ValidateUpdate(ctx, Update{Name: "gitops", DisplayName: str("x"), Force: true})
	require.ErrorIs(t, err, ErrGitOpsOwned)
	_, err = f.svc.ValidateUpdate(ctx, Update{Name: "gitops", DisplayName: str("x"), WriteOptions: WriteOptions{Mode: ModeCommit}})
	require.ErrorIs(t, err, ErrUnsupported)

	// A bare Agent has nothing to write to.
	mustCreate(t, f, agentGVR, agentObject("kagent", "bare", "", "", true, true))
	_, err = f.svc.Update(ctx, Update{Name: "bare", DisplayName: str("x")})
	require.ErrorIs(t, err, ErrConflict)
	assert.Contains(t, err.Error(), "bare Agent")
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

	res, err := f.svc.Delete(ctx, In(""), "sre", false, WriteOptions{})
	require.NoError(t, err)
	assert.True(t, res.HelmReleaseDeleted)
	assert.False(t, res.OCIRepositoryDeleted)
	assert.Contains(t, res.OCIRepositoryKept, "verifier")
	_, err = f.dyn.Resource(ociGVR).Namespace("kagent").Get(ctx, "agent", metav1.GetOptions{})
	require.NoError(t, err, "the shared source stays while verifier references it")

	res, err = f.svc.Delete(ctx, In(""), "verifier", false, WriteOptions{})
	require.NoError(t, err)
	assert.True(t, res.HelmReleaseDeleted)
	assert.True(t, res.OCIRepositoryDeleted)
	_, err = f.dyn.Resource(ociGVR).Namespace("kagent").Get(ctx, "agent", metav1.GetOptions{})
	assert.Error(t, err, "the last release takes the source with it")

	_, err = f.svc.Delete(ctx, In(""), "verifier", false, WriteOptions{})
	assert.ErrorIs(t, err, ErrConflict, "the Agent is still there (the fake has no helm-controller): a bare Agent now")
	_, err = f.svc.Delete(ctx, In(""), "nothing", false, WriteOptions{})
	assert.ErrorIs(t, err, ErrNotFound)

	// Bare Agent: force deletes the object itself.
	res, err = f.svc.Delete(ctx, In(""), "verifier", true, WriteOptions{})
	require.NoError(t, err)
	assert.True(t, res.AgentDeleted)
	assert.False(t, res.HelmReleaseDeleted)
	assert.False(t, res.RemoteMCPServerDeleted, "a bare Agent's server is not the release's to remove")
	_, err = f.dyn.Resource(agentGVR).Namespace("kagent").Get(ctx, "verifier", metav1.GetOptions{})
	assert.Error(t, err)

	// Suspended: refused without force; with force the rendered objects go
	// too (Flux would leave them behind).
	suspended := helmRelease("kagent", "susp", map[string]any{"agent": map[string]any{"name": "susp"}, "modelConfig": map[string]any{"name": "default-model-config"}}, true, nil)
	require.NoError(t, unstructured.SetNestedField(suspended.Object, true, "spec", "suspend"))
	mustCreate(t, f, hrGVR, suspended)
	mustCreate(t, f, agentGVR, agentObject("kagent", "susp", "susp", "kagent", true, true))
	mustCreate(t, f, serverGVR, remoteMCPServer("kagent", "susp", "preset:none"))
	_, err = f.svc.Delete(ctx, In(""), "susp", false, WriteOptions{})
	assert.ErrorIs(t, err, ErrConflict)
	res, err = f.svc.Delete(ctx, In(""), "susp", true, WriteOptions{})
	require.NoError(t, err)
	assert.True(t, res.HelmReleaseDeleted)
	assert.True(t, res.AgentDeleted)
	assert.True(t, res.RemoteMCPServerDeleted)
	_, err = f.dyn.Resource(serverGVR).Namespace("kagent").Get(ctx, "susp", metav1.GetOptions{})
	assert.Error(t, err)
}

func TestStatusVerdictsComeFromTheAgentObject(t *testing.T) {
	ctx := t.Context()
	withStatus := func(obj *unstructured.Unstructured, st map[string]any) *unstructured.Unstructured {
		obj.Object["status"] = st
		return obj
	}
	release := func(name string) *unstructured.Unstructured {
		return helmRelease("kagent", name, map[string]any{"agent": map[string]any{"name": name}, "modelConfig": map[string]any{"name": "default-model-config"}}, true, nil)
	}

	// Ready: Ready=True and the desired revision is the latest successful one.
	f := seeded(t)
	st, err := f.svc.Status(ctx, In(""), "verifier")
	require.NoError(t, err)
	assert.Equal(t, VerdictReady, st.Verdict, st.Summary)
	assert.Contains(t, st.Summary, "Harness kagent")
	assert.Contains(t, st.Summary, "rev-1")
	require.Len(t, st.HelmRelease.History, 1)
	assert.Equal(t, "1.0.0", st.HelmRelease.History[0].ChartVersion)
	require.NotNil(t, st.Agent)
	assert.True(t, st.Agent.Exists)
	assert.Equal(t, "kagent", st.Agent.Harness)
	assert.True(t, *st.Agent.Ready)
	assert.True(t, *st.Agent.Accepted)
	assert.Nil(t, st.Events)

	// Progressing: a new revision compiling (desired != latest successful),
	// even though Ready still reads True for the previous one.
	f = newFixture(t, []runtime.Object{release("sre"), withStatus(agentObject("kagent", "sre", "sre", "kagent", true, true), agentStatusObj(2, "rev-2", "rev-1", true, ""))})
	st, err = f.svc.Status(ctx, In(""), "sre")
	require.NoError(t, err)
	assert.Equal(t, VerdictProgressing, st.Verdict, st.Summary)
	assert.Contains(t, st.Summary, "rev-2")
	// Progressing: first revision, Ready not yet True.
	f = newFixture(t, []runtime.Object{release("sre"), withStatus(agentObject("kagent", "sre", "sre", "kagent", false, true), agentStatusObj(1, "rev-1", "", false, ""))})
	st, err = f.svc.Status(ctx, In(""), "sre")
	require.NoError(t, err)
	assert.Equal(t, VerdictProgressing, st.Verdict, st.Summary)

	// Failed: Ready=False with a reason other than the pending one is a
	// failure the controller will not get past (agentlab's terminal rule).
	snapshotFailed := agentStatusObj(1, "rev-1", "", false, "")
	for _, c := range snapshotFailed["conditions"].([]any) {
		if c.(map[string]any)["type"] == "Ready" {
			c.(map[string]any)["reason"] = "SnapshotFailed"
			c.(map[string]any)["message"] = "actor exited before the snapshot"
		}
	}
	f = newFixture(t, []runtime.Object{release("sre"), withStatus(agentObject("kagent", "sre", "sre", "kagent", false, true), snapshotFailed)})
	st, err = f.svc.Status(ctx, In(""), "sre")
	require.NoError(t, err)
	assert.Equal(t, VerdictFailed, st.Verdict, st.Summary)
	assert.Contains(t, st.Summary, "SnapshotFailed: actor exited before the snapshot")

	// Failed: Accepted=False / Compatible=False / ResolvedRefs=False with the
	// condition's message (a Harness that does not exist is ResolvedRefs).
	for _, failing := range []string{"Accepted", "Compatible", "ResolvedRefs"} {
		f = newFixture(t, []runtime.Object{release("sre"), withStatus(agentObject("kagent", "sre", "sre", "kagent", false, true), agentStatusObj(1, "rev-1", "", false, failing))})
		st, err = f.svc.Status(ctx, In(""), "sre")
		require.NoError(t, err)
		assert.Equal(t, VerdictFailed, st.Verdict, st.Summary)
		assert.Contains(t, st.Summary, failing+" is False")
		assert.Contains(t, st.Summary, failing+" rejected the template")
	}

	// A coding agent names its own Harness: the summary names it, and the
	// list reads the same conditions.
	coding := onHarness(withStatus(agentObject("kagent", "coder", "coder", "kagent", true, true), agentStatusObj(1, "rev-1", "rev-1", true, "")), "claude")
	f = newFixture(t, []runtime.Object{release("coder"), coding})
	st, err = f.svc.Status(ctx, In(""), "coder")
	require.NoError(t, err)
	assert.Equal(t, VerdictReady, st.Verdict, st.Summary)
	assert.Contains(t, st.Summary, "Harness claude")
	listed, err := f.svc.List(ctx, In(""))
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.NotNil(t, listed[0].Ready)
	assert.True(t, *listed[0].Ready, "the list reads the Agent's conditions too")
	assert.Equal(t, "claude", listed[0].Harness)
	// kagent has not observed the Agent yet.
	f = newFixture(t, []runtime.Object{release("sre"), withStatus(agentObject("kagent", "sre", "sre", "kagent", false, true), map[string]any{})})
	st, err = f.svc.Status(ctx, In(""), "sre")
	require.NoError(t, err)
	assert.Equal(t, VerdictProgressing, st.Verdict, st.Summary)
	assert.Contains(t, st.Summary, "not reported on the Agent yet")

	// Failed: the HelmRelease itself failed (nothing rendered); a Warning
	// event on the agent is reported.
	f = newFixture(t, []runtime.Object{helmRelease("kagent", "broken", map[string]any{"agent": map[string]any{"name": "broken"}}, false, nil)},
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "ev1", Namespace: "kagent"}, Type: "Warning", Reason: "InstallFailed", Message: "values don't meet the specifications of the schema(s)", InvolvedObject: corev1.ObjectReference{Kind: "HelmRelease", Name: "broken"}},
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "ev2", Namespace: "kagent"}, Type: "Warning", Reason: "Other", Message: "somebody else's", InvolvedObject: corev1.ObjectReference{Kind: "HelmRelease", Name: "other"}})
	st, err = f.svc.Status(ctx, In(""), "broken")
	require.NoError(t, err)
	assert.Equal(t, VerdictFailed, st.Verdict)
	assert.Contains(t, st.Summary, "InstallFailed")
	assert.False(t, st.Agent.Exists)
	require.Len(t, st.Events, 1)
	assert.Equal(t, "HelmRelease/broken", st.Events[0].Object)

	// Progressing: fresh HelmRelease without status.
	fresh := helmRelease("kagent", "fresh", map[string]any{"agent": map[string]any{"name": "fresh"}}, true, nil)
	delete(fresh.Object, "status")
	f = newFixture(t, []runtime.Object{fresh})
	st, err = f.svc.Status(ctx, In(""), "fresh")
	require.NoError(t, err)
	assert.Equal(t, VerdictProgressing, st.Verdict)

	_, err = f.svc.Status(ctx, In(""), "absent")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestListModelConfigsAndInfo(t *testing.T) {
	f := seeded(t)
	ctx := context.Background()
	list, err := f.svc.ListModelConfigs(ctx, In(""))
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
	assert.Equal(t, "api.kagent.dev/v1alpha3", info.APIVersions.Agent)
	assert.Equal(t, "api.kagent.dev/v1alpha3", info.APIVersions.AgentTemplate)
	assert.Equal(t, "api.kagent.dev/v1alpha3", info.APIVersions.Harness)
	assert.Equal(t, "api.kagent.dev/v1alpha3", info.APIVersions.RemoteMCPServer)
	assert.Equal(t, "api.kagent.dev/v1alpha3", info.APIVersions.ModelConfig)
	assert.Equal(t, "helm.toolkit.fluxcd.io/v2", info.APIVersions.HelmRelease)
	assert.Equal(t, "kagent", info.Harness.Name)
	assert.Equal(t, "http://muster.agent-platform.svc.cluster.local:8090/mcp", info.Muster.URL)
	assert.Equal(t, DefaultChartOCIURL, info.Chart.OCIURL)
	assert.Equal(t, "2.x", info.Chart.Semver)

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

	deleted, err := f.svc.Delete(ctx, In(""), "sre", false, WriteOptions{})
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
	_, err = callerOnly.List(context.Background(), In(""))
	assert.ErrorIs(t, err, ErrUnauthenticated)
	_, err = callerOnly.Create(context.Background(), Spec{Name: "sre", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"}})
	assert.ErrorIs(t, err, ErrUnauthenticated)
}

// The write is a read-modify-write that races helm-controller: it writes the
// release's status after every change, so an update following another closely
// carries a stale resourceVersion. The API server's Conflict is retried on a
// fresh read — both calls of the lab's agents-test proof (a description, then
// refreshSkills) succeed — and a Conflict that outlasts the attempts is still
// reported as one.
func TestUpdateRetriesTheWriteWhenTheReleaseMovedUnderneath(t *testing.T) {
	f := seeded(t)
	ctx := context.Background()
	conflict := apierrors.NewConflict(hrGVR.GroupResource(), "verifier", errors.New("the object has been modified; please apply your changes to the latest version and try again"))

	// Between the read and the write another writer renamed the agent and
	// helm-controller wrote the status: the first Update answers Conflict, the
	// second attempt merges into the re-read release.
	updates := 0
	f.dyn.PrependReactor("update", "helmreleases", func(k8stesting.Action) (bool, runtime.Object, error) {
		updates++
		if updates > 1 {
			return false, nil, nil
		}
		stored, err := f.dyn.Tracker().Get(hrGVR, "kagent", "verifier")
		require.NoError(t, err)
		moved := stored.(*unstructured.Unstructured)
		require.NoError(t, unstructured.SetNestedField(moved.Object, "Renamed elsewhere", "spec", "values", "agent", "displayName"))
		require.NoError(t, unstructured.SetNestedField(moved.Object, int64(7), "status", "observedGeneration"))
		require.NoError(t, f.dyn.Tracker().Update(hrGVR, moved, "kagent"))
		return true, nil, conflict
	})
	res, err := f.svc.Update(ctx, Update{Name: "verifier", Description: str("a new description")})
	require.NoError(t, err)
	assert.Equal(t, 2, updates, "one Conflict, one write")
	assert.Equal(t, []string{"agent.description"}, res.Changed)
	assert.Equal(t, "Renamed elsewhere", res.Before["agent"].(map[string]any)["displayName"], "the second attempt read the moved release")
	hr, err := f.dyn.Resource(hrGVR).Namespace("kagent").Get(ctx, "verifier", metav1.GetOptions{})
	require.NoError(t, err)
	agent := mustValues(hr)["agent"].(map[string]any)
	assert.Equal(t, "a new description", agent["description"])
	assert.Equal(t, "Renamed elsewhere", agent["displayName"], "the concurrent change survived the retry")
	generation, _, _ := unstructured.NestedInt64(hr.Object, "status", "observedGeneration")
	assert.Equal(t, int64(7), generation)

	// A Conflict on every attempt is reported as one, and nothing is written.
	updates = 0
	f.dyn.PrependReactor("update", "helmreleases", func(k8stesting.Action) (bool, runtime.Object, error) {
		updates++
		return true, nil, conflict
	})
	_, err = f.svc.Update(ctx, Update{Name: "verifier", Description: str("never lands")})
	require.ErrorIs(t, err, ErrConflict)
	assert.True(t, apierrors.IsConflict(err), "the API server's error stays in the chain")
	assert.Greater(t, updates, 1, "retried before giving up")
	hr, err = f.dyn.Resource(hrGVR).Namespace("kagent").Get(ctx, "verifier", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "a new description", mustValues(hr)["agent"].(map[string]any)["description"])
}

func TestCreateOnTheHarnessTheCallerNames(t *testing.T) {
	f := seeded(t)
	mustCreate(t, f, f.svc.harnessGVR(), harness("kagent", "claude", "claude"))
	ctx := t.Context()
	readOnly := []string{"preset:read-only"}

	dry, err := f.svc.ValidateCreate(ctx, Spec{Name: "coder", ModelConfig: "default-model-config", Toolset: readOnly, Harness: "claude"})
	require.NoError(t, err)
	require.True(t, dry.Valid, dry.Errors)
	assert.Equal(t, "claude", dry.Manifests.Values["agent"].(map[string]any)["harness"])

	dry, err = f.svc.ValidateCreate(ctx, Spec{Name: "coder", ModelConfig: "default-model-config", Toolset: readOnly, Harness: "codex"})
	require.NoError(t, err)
	assert.False(t, dry.Valid)
	assert.Contains(t, dry.Errors, `harness "codex" does not exist in namespace kagent; valid: claude, kagent`)

	_, err = f.svc.Create(ctx, Spec{Name: "coder", ModelConfig: "default-model-config", Toolset: readOnly, Harness: "codex"})
	require.ErrorIs(t, err, ErrInvalid)

	_, err = f.svc.Create(ctx, Spec{Name: "coder", ModelConfig: "default-model-config", Toolset: readOnly, Harness: "claude"})
	require.NoError(t, err)
	hr, err := f.dyn.Resource(hrGVR).Namespace("kagent").Get(ctx, "coder", metav1.GetOptions{})
	require.NoError(t, err)
	got, _, _ := unstructured.NestedString(hr.Object, "spec", "values", "agent", "harness")
	assert.Equal(t, "claude", got)

	plain, err := f.svc.ValidateCreate(ctx, Spec{Name: "plain", ModelConfig: "default-model-config", Toolset: readOnly})
	require.NoError(t, err)
	assert.Equal(t, DefaultHarnessName, plain.Manifests.Values["agent"].(map[string]any)["harness"], "no harness is the platform Harness")

	// The platform Harness may be absent (the connectivity chart provisions
	// it); a named one may not.
	empty := newFixture(t, []runtime.Object{modelConfig("kagent", "default-model-config", "Anthropic", "claude-sonnet-4-6")})
	dry, err = empty.svc.ValidateCreate(ctx, Spec{Name: "plain", ModelConfig: "default-model-config", Toolset: readOnly})
	require.NoError(t, err)
	assert.True(t, dry.Valid, dry.Errors)
	dry, err = empty.svc.ValidateCreate(ctx, Spec{Name: "plain", ModelConfig: "default-model-config", Toolset: readOnly, Harness: "claude"})
	require.NoError(t, err)
	assert.Contains(t, dry.Errors, `harness "claude" does not exist in namespace kagent (no Harness there)`)
}

func TestPluginsComePinnedAndSelectSkills(t *testing.T) {
	f := seeded(t)
	f.svc.cfg.Compose.SkillsGitAuthSecretName = "kagent-skills-token"
	ctx := t.Context()
	readOnly := []string{"preset:read-only"}
	pinnedGit := Plugin{Path: "bundles/sre", Git: &GitSkill{URL: skillsRepo, Commit: tagHead}, Skills: []string{"triage", "postmortem"}}
	pinnedOCI := Plugin{OCI: kubectlRef + "@" + kubectlSum, Skills: []string{"k8s"}}

	dry, err := f.svc.ValidateCreate(ctx, Spec{Name: "sre", ModelConfig: "default-model-config", Toolset: readOnly, Plugins: Plugins{pinnedGit, pinnedOCI}})
	require.NoError(t, err)
	require.True(t, dry.Valid, dry.Errors)
	assert.Equal(t, []any{
		map[string]any{"git": map[string]any{"url": skillsRepo, "commit": tagHead}, "path": "bundles/sre", "skills": []any{"triage", "postmortem"}},
		map[string]any{"oci": kubectlRef + "@" + kubectlSum, "skills": []any{"k8s"}},
	}, dry.Manifests.Values["plugins"], "the chart's plugins list")
	assert.Equal(t, map[string]any{"name": "kagent-skills-token"}, dry.Manifests.Values[SkillsGitAuthValuesKey], "a git plugin gets the skills credential")
	assert.Empty(t, f.pinner.calls, "a plugin is never resolved")

	for name, tc := range map[string]struct {
		plugin Plugin
		want   string
	}{
		"no source":     {Plugin{Skills: []string{"a"}}, "no source"},
		"both sources":  {Plugin{Git: pinnedGit.Git, OCI: pinnedOCI.OCI, Skills: []string{"a"}}, "both git and oci"},
		"a ref":         {Plugin{Git: &GitSkill{URL: skillsRepo, Ref: "main"}, Skills: []string{"a"}}, "git.ref is not accepted"},
		"short sha":     {Plugin{Git: &GitSkill{URL: skillsRepo, Commit: "abc123"}, Skills: []string{"a"}}, "not a full commit id"},
		"an image tag":  {Plugin{OCI: kubectlRef + ":1.4.0", Skills: []string{"a"}}, "not digest-pinned"},
		"no skill":      {Plugin{OCI: pinnedOCI.OCI}, "selects no skill"},
		"twice":         {Plugin{OCI: pinnedOCI.OCI, Skills: []string{"a", "a"}}, `names "a" twice`},
		"climbing path": {Plugin{OCI: pinnedOCI.OCI, Path: "../x", Skills: []string{"a"}}, `".."`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.svc.Create(ctx, Spec{Name: "sre", ModelConfig: "default-model-config", Toolset: readOnly, Plugins: Plugins{tc.plugin}})
			require.ErrorIs(t, err, ErrInvalid)
			assert.Contains(t, err.Error(), tc.want)
			assert.Contains(t, err.Error(), "plugins[0]")
		})
	}

	// An update replaces the list; [] clears it and drops the credential
	// with the last git source.
	res, err := f.svc.Update(ctx, Update{Name: "verifier", Plugins: &Plugins{pinnedGit}})
	require.NoError(t, err)
	assert.Equal(t, []string{"plugins", "skillsGitAuthSecretRef.name"}, res.Changed)
	assert.Equal(t, []any{map[string]any{"git": map[string]any{"url": skillsRepo, "commit": tagHead}, "path": "bundles/sre", "skills": []any{"triage", "postmortem"}}}, res.After["plugins"])
	res, err = f.svc.Update(ctx, Update{Name: "verifier", Plugins: &Plugins{}})
	require.NoError(t, err)
	assert.Equal(t, []string{"plugins", "skillsGitAuthSecretRef.name"}, res.Changed)
	_, hasPlugins := res.After["plugins"]
	assert.False(t, hasPlugins)
	_, err = f.svc.Update(ctx, Update{Name: "verifier", Plugins: &Plugins{{OCI: kubectlRef + ":1.4.0", Skills: []string{"a"}}}})
	require.ErrorIs(t, err, ErrInvalid)
}

func TestUpdateKeepsTheSkillsCredentialInStep(t *testing.T) {
	f := seeded(t)
	f.svc.cfg.Compose.SkillsGitAuthSecretName = "kagent-skills-token"
	ctx := context.Background()
	credential := func(res *UpdateResult) any { return res.After[SkillsGitAuthValuesKey] }

	// A git skill gets the installation's credential.
	res, err := f.svc.Update(ctx, Update{Name: "verifier", Skills: &Skills{{Git: &GitSkill{URL: skillsRepo, Ref: "feature"}, Path: "runbooks"}}})
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"name": "kagent-skills-token"}, credential(res))
	assert.Contains(t, res.Changed, "skillsGitAuthSecretRef.name")

	// The agent's own wins, "" goes back to the installation's.
	res, err = f.svc.Update(ctx, Update{Name: "verifier", GitAuthSecretName: str("team-token")})
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"name": "team-token"}, credential(res))
	assert.Equal(t, []string{"skillsGitAuthSecretRef.name"}, res.Changed)
	res, err = f.svc.Update(ctx, Update{Name: "verifier", Description: str("unrelated")})
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"name": "team-token"}, credential(res), "an unrelated update keeps the agent's own")
	res, err = f.svc.Update(ctx, Update{Name: "verifier", GitAuthSecretName: str("")})
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"name": "kagent-skills-token"}, credential(res))

	// Without a git skill the credential is dropped.
	res, err = f.svc.Update(ctx, Update{Name: "verifier", Skills: &Skills{{OCI: kubectlRef + ":1.4.0"}}})
	require.NoError(t, err)
	assert.Nil(t, credential(res))
}

func TestCreateRefusesSkillsFromARepositoryTheCallerCannotRead(t *testing.T) {
	f := seeded(t)
	private := Skills{{Name: "runbooks", Path: "runbooks", Git: &GitSkill{URL: privateSkillsRepo, Commit: mainHead}}}
	spec := func(name string) Spec {
		return Spec{Name: name, ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"}, Skills: private}
	}
	asGitHub := func(login string) context.Context {
		return identity.ContextWithGitHub(context.Background(), &identity.GitHub{Login: login, Token: "ghu_" + login})
	}

	for name, ctx := range map[string]context.Context{"john": asGitHub("john"), "unknown": context.Background()} {
		_, err := f.svc.Create(ctx, spec("refused-"+name))
		require.ErrorIs(t, err, ErrForbidden, name)
		assert.Contains(t, err.Error(), privateSkillsRepo)
		_, getErr := f.dyn.Resource(hrGVR).Namespace("kagent").Get(ctx, "refused-"+name, metav1.GetOptions{})
		assert.True(t, apierrors.IsNotFound(getErr), "a refusal writes nothing")
		dry, err := f.svc.ValidateCreate(ctx, spec("refused-"+name))
		require.NoError(t, err)
		assert.False(t, dry.Valid)
		assert.Contains(t, strings.Join(dry.Errors, "\n"), privateSkillsRepo)
	}

	_, err := f.svc.Update(asGitHub("john"), Update{Name: "verifier", Skills: &private})
	require.ErrorIs(t, err, ErrForbidden, "an update cannot add what the caller cannot read either")

	res, err := f.svc.Create(asGitHub("jane"), spec("private-analyst"))
	require.NoError(t, err)
	assert.Equal(t, "private-analyst", res.Agent.Name)
}

func TestCreateRefusesPluginsFromARepositoryTheCallerCannotRead(t *testing.T) {
	f := seeded(t)
	private := Plugins{{Path: "bundle", Git: &GitSkill{URL: privateSkillsRepo, Commit: mainHead}, Skills: []string{"runbooks"}}}
	spec := func(name string) Spec {
		return Spec{Name: name, ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"}, Plugins: private}
	}
	asGitHub := func(login string) context.Context {
		return identity.ContextWithGitHub(t.Context(), &identity.GitHub{Login: login, Token: "ghu_" + login})
	}

	for name, ctx := range map[string]context.Context{"john": asGitHub("john"), "unknown": t.Context()} {
		_, err := f.svc.Create(ctx, spec("refused-"+name))
		require.ErrorIs(t, err, ErrForbidden, name)
		assert.Contains(t, err.Error(), "plugins[0]")
		assert.Contains(t, err.Error(), privateSkillsRepo)
		_, getErr := f.dyn.Resource(hrGVR).Namespace("kagent").Get(ctx, "refused-"+name, metav1.GetOptions{})
		assert.True(t, apierrors.IsNotFound(getErr), "a refusal writes nothing")
		dry, err := f.svc.ValidateCreate(ctx, spec("refused-"+name))
		require.NoError(t, err)
		assert.False(t, dry.Valid)
		assert.Contains(t, strings.Join(dry.Errors, "\n"), privateSkillsRepo)
	}

	_, err := f.svc.Update(asGitHub("john"), Update{Name: "verifier", Plugins: &private})
	require.ErrorIs(t, err, ErrForbidden, "an update cannot add what the caller cannot read either")

	res, err := f.svc.Create(asGitHub("jane"), spec("private-bundle"))
	require.NoError(t, err)
	assert.Equal(t, "private-bundle", res.Agent.Name)
}

func TestStatusVerdictOfAHelmReleaseThatIsNotReady(t *testing.T) {
	cond := func(typ, status, reason, message string) map[string]any {
		return map[string]any{"type": typ, "status": status, "reason": reason, "message": message}
	}
	release := func(conds ...any) *unstructured.Unstructured {
		hr := helmRelease("kagent", "fresh", map[string]any{"agent": map[string]any{"name": "fresh"}}, true, nil)
		require.NoError(t, unstructured.SetNestedSlice(hr.Object, conds, "status", "conditions"))
		return hr
	}
	source := func(conds ...any) *unstructured.Unstructured {
		src := ociRepository("kagent", "1.x")
		src.SetName("agent")
		src.SetNamespace("kagent")
		if len(conds) > 0 {
			require.NoError(t, unstructured.SetNestedSlice(src.Object, conds, "status", "conditions"))
		}
		return src
	}
	waiting := cond("Ready", "False", "SourceNotReady", "OCIRepository 'kagent/agent' is not ready: latest generation of object has not been reconciled")

	tests := []struct {
		name        string
		objects     []runtime.Object
		wantVerdict string
		wantSummary string
	}{
		{
			name:        "source not reconciled yet, no source status",
			objects:     []runtime.Object{release(waiting), source()},
			wantVerdict: VerdictProgressing,
			wantSummary: "latest generation of object has not been reconciled",
		},
		{
			name:        "source recreated and reconciling",
			objects:     []runtime.Object{release(waiting), source(cond("Ready", "Unknown", "Progressing", "reconciliation in progress"))},
			wantVerdict: VerdictProgressing,
			wantSummary: "SourceNotReady",
		},
		{
			name:        "source deleted and not recreated yet",
			objects:     []runtime.Object{release(waiting)},
			wantVerdict: VerdictProgressing,
			wantSummary: "SourceNotReady",
		},
		{
			name:        "source reports a real failure",
			objects:     []runtime.Object{release(waiting), source(cond("Ready", "False", "AuthenticationFailed", "unauthorized: authentication required"))},
			wantVerdict: VerdictFailed,
			wantSummary: "AuthenticationFailed: unauthorized: authentication required",
		},
		{
			name:        "dependency not ready",
			objects:     []runtime.Object{release(cond("Ready", "False", "DependencyNotReady", "dependency 'kagent/crds' is not ready"))},
			wantVerdict: VerdictProgressing,
			wantSummary: "dependency 'kagent/crds' is not ready",
		},
		{
			name:        "install failed",
			objects:     []runtime.Object{release(cond("Ready", "False", "InstallFailed", "Helm install failed"))},
			wantVerdict: VerdictFailed,
			wantSummary: "InstallFailed",
		},
		{
			name:        "upgrade failed",
			objects:     []runtime.Object{release(cond("Ready", "False", "UpgradeFailed", "Helm upgrade failed"))},
			wantVerdict: VerdictFailed,
			wantSummary: "UpgradeFailed",
		},
		{
			name:        "artifact failed",
			objects:     []runtime.Object{release(cond("Ready", "False", "ArtifactFailed", "could not load chart"))},
			wantVerdict: VerdictFailed,
			wantSummary: "ArtifactFailed",
		},
		{
			name:        "stalled wins over a waiting reason",
			objects:     []runtime.Object{release(cond("Ready", "False", "SourceNotReady", "waiting"), cond("Stalled", "True", "RetriesExceeded", "Failed to install after 3 attempt(s)"))},
			wantVerdict: VerdictFailed,
			wantSummary: "RetriesExceeded",
		},
		{
			name:        "stalled with a failed reason",
			objects:     []runtime.Object{release(cond("Ready", "False", "InstallFailed", "Helm install failed"), cond("Stalled", "True", "RetriesExceeded", "Failed to install after 3 attempt(s)"))},
			wantVerdict: VerdictFailed,
			wantSummary: "RetriesExceeded",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, tt.objects)
			st, err := f.svc.Status(t.Context(), In(""), "fresh")
			require.NoError(t, err)
			require.Equal(t, tt.wantVerdict, st.Verdict, st.Summary)
			require.Contains(t, st.Summary, tt.wantSummary)
		})
	}
}
