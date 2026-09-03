package agents

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/giantswarm/agent-manager/internal/kube"
)

var (
	hrGVR     = schema.GroupVersionResource{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"}
	ociGVR    = schema.GroupVersionResource{Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "ocirepositories"}
	agGVR     = schema.GroupVersionResource{Group: "kagent.dev", Version: "v1alpha2", Resource: "agents"}
	mcGVR     = schema.GroupVersionResource{Group: "kagent.dev", Version: "v1alpha2", Resource: "modelconfigs"}
	listKinds = map[schema.GroupVersionResource]string{
		hrGVR: "HelmReleaseList", ociGVR: "OCIRepositoryList", agGVR: "AgentList", mcGVR: "ModelConfigList",
	}
)

type fixture struct {
	dyn   *dynamicfake.FakeDynamicClient
	typed *kubefake.Clientset
	svc   *Service
}

func newFixture(t *testing.T, dynObjs []runtime.Object, typedObjs ...runtime.Object) *fixture {
	t.Helper()
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, dynObjs...)
	typed := kubefake.NewClientset(typedObjs...)
	client := kube.FromInterfaces(dyn, typed, typed.Discovery())
	svc := New(kube.NewServiceAccountProvider(client), embeddedChart{}, nil, Config{DefaultNamespace: "kagent", ManagedNamespaces: []string{"tenant"}, Version: "test"}, nil)
	return &fixture{dyn: dyn, typed: typed, svc: svc}
}

func modelConfig(ns, name, provider, model string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kagent.dev/v1alpha2", "kind": "ModelConfig",
		"metadata": map[string]any{"name": name, "namespace": ns},
		"spec":     map[string]any{"provider": provider, "model": model},
		"status":   map[string]any{"conditions": []any{map[string]any{"type": "Accepted", "status": "True", "reason": "ModelConfigReconciled"}}},
	}}
}

func ociRepository(ns string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "source.toolkit.fluxcd.io/v1", "kind": "OCIRepository",
		"metadata": map[string]any{"name": "agent", "namespace": ns},
		"spec":     map[string]any{"interval": "30m", "url": DefaultChartOCIURL, "ref": map[string]any{"semver": "x.x.x"}},
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
			"history":               []any{map[string]any{"version": int64(1), "chartVersion": "0.5.2+abc", "status": "deployed", "lastDeployed": "2026-09-01T10:00:00Z"}},
			"lastAttemptedRevision": "0.5.2+abc",
		},
	}}
}

func agentCR(ns, name, hrName string, ready bool) *unstructured.Unstructured {
	labels := map[string]any{}
	if hrName != "" {
		labels[HelmReleaseNameLabel] = hrName
		labels[HelmReleaseNamespaceLabel] = ns
	}
	readyStatus := "True"
	if !ready {
		readyStatus = "False"
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kagent.dev/v1alpha2", "kind": "Agent",
		"metadata": map[string]any{"name": name, "namespace": ns, "labels": labels, "annotations": map[string]any{DisplayNameAnnotation: "Display " + name}},
		"spec": map[string]any{
			"type": "Declarative", "description": "desc " + name,
			"skills": map[string]any{"gitRefs": []any{map[string]any{"url": "https://github.com/giantswarm/agent-skills", "path": "a", "ref": "main", "name": "a"}}},
			"declarative": map[string]any{
				"runtime": "go", "modelConfig": "default-model-config", "systemMessage": "You are " + name,
				"tools": []any{map[string]any{"type": "McpServer", "mcpServer": map[string]any{"kind": "RemoteMCPServer", "name": "muster", "toolNames": []any{"x_a_b"}}}},
			},
		},
		"status": map[string]any{"conditions": []any{
			map[string]any{"type": "Accepted", "status": "True", "reason": "Reconciled"},
			map[string]any{"type": "Ready", "status": readyStatus, "reason": "DeploymentReady", "message": "Deployment is ready"},
		}},
	}}
}

func seeded(t *testing.T, typedObjs ...runtime.Object) *fixture {
	verifierValues := map[string]any{"agent": map[string]any{"name": "verifier", "displayName": "Verifier"}, "modelConfig": map[string]any{"name": "default-model-config"}}
	return newFixture(t, []runtime.Object{
		modelConfig("kagent", "default-model-config", "Anthropic", "claude-sonnet-4-6"),
		modelConfig("kagent", "qwen3-8-27b", "OpenAI", "qwen3-8-27b"),
		ociRepository("kagent"),
		helmRelease("kagent", "verifier", verifierValues, true, nil),
		agentCR("kagent", "verifier", "verifier", true),
	}, typedObjs...)
}

func TestListMergesAgentsAndHelmReleases(t *testing.T) {
	f := seeded(t)
	ctx := context.Background()
	// A HelmRelease that has not rendered its Agent yet, and a bare Agent.
	pending := map[string]any{"agent": map[string]any{"name": "pending", "displayName": "Pending"}, "modelConfig": map[string]any{"name": "qwen3-8-27b"}}
	_, err := f.dyn.Resource(hrGVR).Namespace("kagent").Create(ctx, helmRelease("kagent", "pending", pending, false, map[string]any{KustomizationNameLabel: "flux-system"}), metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = f.dyn.Resource(agGVR).Namespace("kagent").Create(ctx, agentCR("kagent", "bare", "", true), metav1.CreateOptions{})
	require.NoError(t, err)

	list, err := f.svc.List(ctx, "")
	require.NoError(t, err)
	require.Len(t, list, 3)
	byName := map[string]Agent{}
	for _, a := range list {
		byName[a.Name] = a
	}

	v := byName["verifier"]
	assert.True(t, v.Exists)
	assert.Equal(t, "Display verifier", v.DisplayName)
	assert.Equal(t, "default-model-config", v.ModelConfig)
	assert.Equal(t, []string{"x_a_b"}, v.ToolNames)
	require.NotNil(t, v.Skills)
	assert.Equal(t, "a", v.Skills.GitRefs[0].Path)
	assert.Equal(t, ManagedHelmRelease, v.Managed)
	require.NotNil(t, v.Ready)
	assert.True(t, *v.Ready)
	require.NotNil(t, v.HelmRelease)
	assert.Equal(t, "0.5.2+abc", v.HelmRelease.ChartVersion)
	assert.True(t, *v.HelmRelease.Ready)

	p := byName["pending"]
	assert.False(t, p.Exists, "listed from its HelmRelease alone")
	assert.Equal(t, "Pending", p.DisplayName)
	assert.Equal(t, "qwen3-8-27b", p.ModelConfig)
	assert.Equal(t, ManagedGitOps, p.Managed)
	assert.Nil(t, p.Ready)
	assert.False(t, *p.HelmRelease.Ready)

	b := byName["bare"]
	assert.Equal(t, ManagedNone, b.Managed)
	assert.Nil(t, b.HelmRelease)

	_, err = f.svc.List(ctx, "other")
	assert.ErrorIs(t, err, ErrInvalid, "unmanaged namespaces are refused")
}

func TestCreateValidatesThenAppliesBothObjects(t *testing.T) {
	f := seeded(t)
	ctx := context.Background()

	_, err := f.svc.Create(ctx, Spec{Name: "Bad", ModelConfig: "default-model-config"})
	assert.ErrorIs(t, err, ErrInvalid)

	_, err = f.svc.Create(ctx, Spec{Name: "sre", ModelConfig: "nope"})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "default-model-config, qwen3-8-27b", "the valid model configs are listed")

	long := make([]byte, 64)
	for i := range long {
		long[i] = 'x'
	}
	_, err = f.svc.Create(ctx, Spec{Name: "sre", ModelConfig: "default-model-config", DisplayName: string(long)})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "displayName")

	_, err = f.svc.Create(ctx, Spec{Name: "verifier", ModelConfig: "default-model-config"})
	assert.ErrorIs(t, err, ErrConflict)

	// Nothing was written by the failures.
	hrs, err := f.dyn.Resource(hrGVR).Namespace("kagent").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, hrs.Items, 1)

	res, err := f.svc.Create(ctx, Spec{
		Name: "sre", DisplayName: "SRE", Description: "helps", SystemMessage: "Be brief.", ModelConfig: "qwen3-8-27b",
		Skills:    &Skills{GitRefs: []SkillGitRef{{URL: "https://github.com/giantswarm/agent-skills", Path: "runbooks", Ref: "main"}}},
		ToolNames: []string{"x_mcp-kubernetes_get_pods"},
	})
	require.NoError(t, err)
	assert.False(t, res.Created.OCIRepository, "the namespace already had the chart source")
	assert.True(t, res.Created.HelmRelease)
	assert.Equal(t, "sre", res.Agent.Name)
	assert.False(t, res.Agent.Exists)
	assert.Equal(t, ManagedHelmRelease, res.Agent.Managed)
	assert.Contains(t, res.Manifests.HelmRelease, "kind: HelmRelease")
	require.NotNil(t, res.Status)
	assert.Equal(t, VerdictProgressing, res.Status.Verdict)

	hr, err := f.dyn.Resource(hrGVR).Namespace("kagent").Get(ctx, "sre", metav1.GetOptions{})
	require.NoError(t, err)
	values, _, _ := unstructured.NestedMap(hr.Object, "spec", "values")
	assert.Equal(t, map[string]any{
		"agent":       map[string]any{"name": "sre", "displayName": "SRE", "description": "helps", "systemMessage": "Be brief."},
		"modelConfig": map[string]any{"name": "qwen3-8-27b"},
		"skills":      map[string]any{"gitRefs": []any{map[string]any{"url": "https://github.com/giantswarm/agent-skills", "path": "runbooks", "ref": "main", "name": "runbooks"}}},
		"muster":      map[string]any{"toolNames": []any{"x_mcp-kubernetes_get_pods"}},
	}, values)
	assert.Equal(t, ManagedByValue, hr.GetLabels()[ManagedByLabel])

	// A namespace without a chart source gets one.
	_, err = f.dyn.Resource(mcGVR).Namespace("tenant").Create(ctx, modelConfig("tenant", "mc", "Ollama", "qwen3"), metav1.CreateOptions{})
	require.NoError(t, err)
	res, err = f.svc.Create(ctx, Spec{Namespace: "tenant", Name: "t1", ModelConfig: "mc"})
	require.NoError(t, err)
	assert.True(t, res.Created.OCIRepository)
	repo, err := f.dyn.Resource(ociGVR).Namespace("tenant").Get(ctx, "agent", metav1.GetOptions{})
	require.NoError(t, err)
	url, _, _ := unstructured.NestedString(repo.Object, "spec", "url")
	assert.Equal(t, DefaultChartOCIURL, url)
}

func TestValidateCreateIsADryRun(t *testing.T) {
	f := seeded(t)
	ctx := context.Background()
	res, err := f.svc.ValidateCreate(ctx, Spec{Name: "verifier", ModelConfig: "nope", Runtime: "rust"})
	require.NoError(t, err)
	assert.False(t, res.Valid)
	assert.Equal(t, "create", res.Mode)
	joined := ""
	for _, e := range res.Errors {
		joined += e + "\n"
	}
	assert.Contains(t, joined, "already exists")
	assert.Contains(t, joined, "does not exist")
	assert.Contains(t, joined, "/agent/runtime")
	assert.Equal(t, "0.5.2", res.SchemaVersion)

	ok, err := f.svc.ValidateCreate(ctx, Spec{Name: "fresh", ModelConfig: "default-model-config"})
	require.NoError(t, err)
	assert.True(t, ok.Valid, ok.Errors)
	assert.Contains(t, ok.Manifests.OCIRepository, "kind: OCIRepository")
	hrs, _ := f.dyn.Resource(hrGVR).Namespace("kagent").List(ctx, metav1.ListOptions{})
	assert.Len(t, hrs.Items, 1, "validate writes nothing")
}

func TestUpdateMergesIntoValuesAndHonorsOwnership(t *testing.T) {
	f := seeded(t)
	ctx := context.Background()
	str := func(s string) *string { return &s }

	res, err := f.svc.Update(ctx, Update{Name: "verifier", DisplayName: str("Verifier 2"), Description: str("now with a description"), ToolNames: &[]string{"x_a_b"}, ModelConfig: str("qwen3-8-27b")})
	require.NoError(t, err)
	assert.Equal(t, []string{"agent.description", "agent.displayName", "modelConfig.name", "muster.toolNames"}, res.Changed)
	assert.Equal(t, "Verifier", res.Before["agent"].(map[string]any)["displayName"])
	assert.Equal(t, "Verifier 2", res.After["agent"].(map[string]any)["displayName"])
	hr, err := f.dyn.Resource(hrGVR).Namespace("kagent").Get(ctx, "verifier", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, res.After, mustValues(hr))

	// Clearing a field drops the key; skills replace the block; empty tool
	// names drop the muster block.
	res, err = f.svc.Update(ctx, Update{Name: "verifier", Description: str(""), Skills: &Skills{Refs: []string{"reg/skill:1"}}, ToolNames: &[]string{}})
	require.NoError(t, err)
	after := res.After
	_, hasDesc := after["agent"].(map[string]any)["description"]
	assert.False(t, hasDesc)
	assert.Equal(t, map[string]any{"refs": []any{"reg/skill:1"}}, after["skills"])
	_, hasMuster := after["muster"]
	assert.False(t, hasMuster)

	// No change is a no-op.
	res, err = f.svc.Update(ctx, Update{Name: "verifier"})
	require.NoError(t, err)
	assert.Empty(t, res.Changed)

	_, err = f.svc.Update(ctx, Update{Name: "verifier", ModelConfig: str("nope")})
	assert.ErrorIs(t, err, ErrInvalid)
	_, err = f.svc.Update(ctx, Update{Name: "verifier", Runtime: str("rust")})
	assert.ErrorIs(t, err, ErrInvalid)
	_, err = f.svc.Update(ctx, Update{Name: "missing", DisplayName: str("x")})
	assert.ErrorIs(t, err, ErrNotFound)

	// GitOps-owned: refused without force.
	gitops := helmRelease("kagent", "gitops", map[string]any{"agent": map[string]any{"name": "gitops"}, "modelConfig": map[string]any{"name": "default-model-config"}}, true, map[string]any{KustomizationNameLabel: "flux-system"})
	_, err = f.dyn.Resource(hrGVR).Namespace("kagent").Create(ctx, gitops, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = f.svc.Update(ctx, Update{Name: "gitops", DisplayName: str("x")})
	require.ErrorIs(t, err, ErrConflict)
	assert.Contains(t, err.Error(), "flux-system")
	res, err = f.svc.Update(ctx, Update{Name: "gitops", DisplayName: str("x"), Force: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"agent.displayName"}, res.Changed)

	// A bare Agent CR has nothing to write to.
	_, err = f.dyn.Resource(agGVR).Namespace("kagent").Create(ctx, agentCR("kagent", "bare", "", true), metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = f.svc.Update(ctx, Update{Name: "bare", DisplayName: str("x")})
	assert.ErrorIs(t, err, ErrConflict)
}

func mustValues(hr *unstructured.Unstructured) map[string]any {
	v, _, _ := unstructured.NestedMap(hr.Object, "spec", "values")
	return v
}

func TestDeleteRemovesTheReleaseAndTheSourceOnlyWhenUnreferenced(t *testing.T) {
	f := seeded(t)
	ctx := context.Background()
	_, err := f.svc.Create(ctx, Spec{Name: "sre", ModelConfig: "default-model-config"})
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
	assert.ErrorIs(t, err, ErrConflict, "the Agent CR is still there (the fake has no helm-controller): a bare CR now")
	_, err = f.svc.Delete(ctx, "", "nothing", false)
	assert.ErrorIs(t, err, ErrNotFound)

	// Bare CR: force deletes the CR itself.
	res, err = f.svc.Delete(ctx, "", "verifier", true)
	require.NoError(t, err)
	assert.True(t, res.AgentDeleted)
	assert.False(t, res.HelmReleaseDeleted)
	_, err = f.dyn.Resource(agGVR).Namespace("kagent").Get(ctx, "verifier", metav1.GetOptions{})
	assert.Error(t, err)

	// Suspended: refused without force; with force the Agent goes too (Flux
	// would leave it behind).
	suspended := helmRelease("kagent", "susp", map[string]any{"agent": map[string]any{"name": "susp"}, "modelConfig": map[string]any{"name": "default-model-config"}}, true, nil)
	require.NoError(t, unstructured.SetNestedField(suspended.Object, true, "spec", "suspend"))
	_, err = f.dyn.Resource(hrGVR).Namespace("kagent").Create(ctx, suspended, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = f.dyn.Resource(agGVR).Namespace("kagent").Create(ctx, agentCR("kagent", "susp", "susp", true), metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = f.svc.Delete(ctx, "", "susp", false)
	assert.ErrorIs(t, err, ErrConflict)
	res, err = f.svc.Delete(ctx, "", "susp", true)
	require.NoError(t, err)
	assert.True(t, res.HelmReleaseDeleted)
	assert.True(t, res.AgentDeleted)
}

func TestStatusVerdicts(t *testing.T) {
	ctx := context.Background()
	deployment := func(name string, available int32) *appsv1.Deployment {
		return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kagent"}, Status: appsv1.DeploymentStatus{Replicas: 1, AvailableReplicas: available, ReadyReplicas: available}}
	}
	pod := func(name, agent string, waiting string) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kagent", Labels: map[string]string{"kagent": agent, "app": "kagent"}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
		cs := corev1.ContainerStatus{Name: "kagent", RestartCount: 7}
		if waiting != "" {
			cs.State.Waiting = &corev1.ContainerStateWaiting{Reason: waiting, Message: "back-off 5m0s restarting failed container\nmore"}
		} else {
			cs.State.Running = &corev1.ContainerStateRunning{}
			cs.Ready = true
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		}
		p.Status.ContainerStatuses = []corev1.ContainerStatus{cs}
		return p
	}

	// Ready: Agent Ready + Deployment available.
	f := seeded(t, deployment("verifier", 1), pod("verifier-abc", "verifier", ""))
	st, err := f.svc.Status(ctx, "", "verifier")
	require.NoError(t, err)
	assert.Equal(t, VerdictReady, st.Verdict, st.Summary)
	assert.True(t, st.Deployment.Exists)
	require.Len(t, st.Pods, 1)
	assert.True(t, st.Pods[0].Ready)
	assert.Equal(t, int32(7), st.Pods[0].Restarts)
	require.Len(t, st.HelmRelease.History, 1)
	assert.Equal(t, "0.5.2+abc", st.HelmRelease.History[0].ChartVersion)

	// Failed: a crash-looping pod (phase Running!) beats a Ready condition.
	f = seeded(t, deployment("verifier", 0), pod("verifier-abc", "verifier", "CrashLoopBackOff"),
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "ev1", Namespace: "kagent"}, Type: "Warning", Reason: "BackOff", Message: "Back-off restarting failed container", InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "verifier-abc"}})
	st, err = f.svc.Status(ctx, "", "verifier")
	require.NoError(t, err)
	assert.Equal(t, VerdictFailed, st.Verdict)
	assert.Contains(t, st.Summary, "CrashLoopBackOff")
	assert.NotContains(t, st.Summary, "more", "the summary keeps the first line")
	require.Len(t, st.Events, 1)
	assert.Equal(t, "BackOff", st.Events[0].Reason)

	// Failed: the HelmRelease itself failed (no Agent rendered).
	f = newFixture(t, []runtime.Object{helmRelease("kagent", "broken", map[string]any{"agent": map[string]any{"name": "broken"}}, false, nil)})
	st, err = f.svc.Status(ctx, "", "broken")
	require.NoError(t, err)
	assert.Equal(t, VerdictFailed, st.Verdict)
	assert.Contains(t, st.Summary, "InstallFailed")
	assert.False(t, st.Agent.Exists)

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
	assert.Equal(t, "kagent.dev/v1alpha2", info.APIVersions.Agent)
	assert.Equal(t, DefaultChartOCIURL, info.Chart.OCIURL)

	_, err = f.svc.ListSkills(ctx, "", "", false)
	assert.ErrorIs(t, err, ErrUnsupported)
}
