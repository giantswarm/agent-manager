package tenants

import (
	"context"
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

	"github.com/giantswarm/agent-manager/internal/kube"
)

// keySecret names the default ModelConfig's key Secret.
const keySecret = "kagent-anthropic" // #nosec G101 -- a Secret name, not a credential

var (
	hrGVR      = schema.GroupVersionResource{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"}
	ociGVR     = schema.GroupVersionResource{Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "ocirepositories"}
	agentGVR   = schema.GroupVersionResource{Group: "api.kagent.dev", Version: "v1alpha3", Resource: "agents"}
	harnessGVR = schema.GroupVersionResource{Group: "api.kagent.dev", Version: "v1alpha3", Resource: "harnesses"}
	mcGVR      = schema.GroupVersionResource{Group: "api.kagent.dev", Version: "v1alpha3", Resource: "modelconfigs"}
	poolGVR    = schema.GroupVersionResource{Group: "ate.dev", Version: "v1alpha1", Resource: "workerpools"}
	cnpGVR     = schema.GroupVersionResource{Group: "cilium.io", Version: "v2", Resource: "ciliumnetworkpolicies"}
	listKinds  = map[schema.GroupVersionResource]string{
		hrGVR: "HelmReleaseList", ociGVR: "OCIRepositoryList", agentGVR: "AgentList", harnessGVR: "HarnessList",
		mcGVR: "ModelConfigList", poolGVR: "WorkerPoolList", cnpGVR: "CiliumNetworkPolicyList",
	}
)

func obj(apiVersion, kind, ns, name string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion, "kind": kind,
		"metadata": map[string]any{"name": name, "namespace": ns, "labels": map[string]any{"from": "platform"}},
		"spec":     spec,
	}}
}

func namespace(name string, labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

// platform is what the platform namespace kagent holds.
func platform() []runtime.Object {
	return []runtime.Object{
		obj("api.kagent.dev/v1alpha3", "Harness", "kagent", "kagent", map[string]any{
			"kagent":    map[string]any{},
			"workload":  map[string]any{"image": "ghcr.io/kagent@sha256:abc"},
			"substrate": map[string]any{"workerPoolRef": map[string]any{"name": "kagent-default"}, "snapshotPolicy": map[string]any{"location": "s3://snapshots"}},
		}),
		obj("ate.dev/v1alpha1", "WorkerPool", "kagent", "kagent-default", map[string]any{
			"replicas": int64(3), "workerImage": "ateom@sha256:def", "template": map[string]any{"serviceAccountName": "kagent-default"},
		}),
		obj("api.kagent.dev/v1alpha3", "ModelConfig", "kagent", "default-model-config", map[string]any{
			"provider": "Anthropic", "model": "claude", "apiKeySecret": keySecret,
		}),
		obj("cilium.io/v2", "CiliumNetworkPolicy", "kagent", "substrate-workers", map[string]any{
			"endpointSelector": map[string]any{"matchExpressions": []any{map[string]any{"key": "ate.dev/worker-pool", "operator": "Exists"}}},
		}),
	}
}

type fixture struct {
	dyn   *dynamicfake.FakeDynamicClient
	typed *kubefake.Clientset
	r     *Reconciler
}

func newFixture(t *testing.T, dynObjs []runtime.Object, typedObjs ...runtime.Object) *fixture {
	t.Helper()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, append(platform(), dynObjs...)...)
	typed := kubefake.NewClientset(append([]runtime.Object{
		namespace("kagent", map[string]string{OrganizationLabel: "giantswarm"}),
		namespace("org-a", map[string]string{OrganizationLabel: "a"}),
		namespace("other", nil),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: keySecret, Namespace: "kagent"}, Data: map[string][]byte{"ANTHROPIC_API_KEY": []byte("sk")}},
	}, typedObjs...)...)
	r := New(kube.FromInterfaces(dyn, typed, typed.Discovery()), Config{
		PlatformNamespace:  "kagent",
		HarnessName:        "kagent",
		ModelConfigName:    "default-model-config",
		ModelSecretName:    keySecret,
		WorkerPoolReplicas: 1,
		FluxServiceAccount: "agents-flux",
		FluxClusterRole:    "agent-platform-tenant-flux",
		NetworkPolicies:    []string{"substrate-workers"},
		NetworkPolicyGVR:   cnpGVR,
		OCIRepository: func(ns string) *unstructured.Unstructured {
			return obj("source.toolkit.fluxcd.io/v1", "OCIRepository", ns, "agent", map[string]any{"url": "oci://example/agent", "ref": map[string]any{"semver": "2.x"}})
		},
		ChartName:        "agent",
		HelmReleaseGVR:   hrGVR,
		OCIRepositoryGVR: ociGVR,
		AgentGVR:         agentGVR,
		HarnessGVR:       harnessGVR,
		ModelConfigGVR:   mcGVR,
		WorkerPoolGVR:    poolGVR,
	}, nil)
	return &fixture{dyn: dyn, typed: typed, r: r}
}

func (f *fixture) get(t *testing.T, gvr schema.GroupVersionResource, ns, name string) *unstructured.Unstructured {
	t.Helper()
	o, err := f.dyn.Resource(gvr).Namespace(ns).Get(context.Background(), name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	require.NoError(t, err)
	return o
}

func agentRelease(ns string) *unstructured.Unstructured {
	return obj("helm.toolkit.fluxcd.io/v2", "HelmRelease", ns, "sre", map[string]any{
		"chartRef": map[string]any{"kind": "OCIRepository", "name": "agent", "namespace": ns},
	})
}

// A tenant without agents gets its Flux objects and network policies, and no
// workers, model or Harness.
func TestTenantWithoutAgents(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	f.r.ReconcileAll(ctx)

	assert.Equal(t, []string{"org-a"}, f.r.Namespaces(), "labelled namespaces only, never the platform namespace")

	oci := f.get(t, ociGVR, "org-a", "agent")
	require.NotNil(t, oci)
	assert.Equal(t, ScopeNamespace, oci.GetLabels()[ObjectLabel])
	cnp := f.get(t, cnpGVR, "org-a", "substrate-workers")
	require.NotNil(t, cnp)
	assert.Equal(t, "kagent/substrate-workers", cnp.GetAnnotations()[CopiedFromAnnotation])
	assert.Empty(t, cnp.GetLabels()["from"], "the source's labels are not copied")

	_, err := f.typed.CoreV1().ServiceAccounts("org-a").Get(ctx, "agents-flux", metav1.GetOptions{})
	require.NoError(t, err)
	rb, err := f.typed.RbacV1().RoleBindings("org-a").Get(ctx, "agents-flux", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "agent-platform-tenant-flux", rb.RoleRef.Name)
	assert.Equal(t, "ClusterRole", rb.RoleRef.Kind)
	require.Len(t, rb.Subjects, 1)
	assert.Equal(t, "org-a", rb.Subjects[0].Namespace)

	assert.Nil(t, f.get(t, harnessGVR, "org-a", "kagent"))
	assert.Nil(t, f.get(t, poolGVR, "org-a", "kagent-default"))
	assert.Nil(t, f.get(t, mcGVR, "org-a", "default-model-config"))
	_, err = f.typed.CoreV1().Secrets("org-a").Get(ctx, keySecret, metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))

	assert.Nil(t, f.get(t, ociGVR, "other", "agent"), "an unlabelled namespace is not a tenant")
}

// The first agent brings the Harness, a one-replica WorkerPool with its
// ServiceAccount and the default model; the last one takes them away, and a
// tenant's own ModelConfig stays.
func TestTenantAgentsComeAndGo(t *testing.T) {
	own := obj("api.kagent.dev/v1alpha3", "ModelConfig", "org-a", "own", map[string]any{"provider": "OpenAI"})
	f := newFixture(t, []runtime.Object{agentRelease("org-a"), own})
	ctx := context.Background()
	f.r.ReconcileAll(ctx)

	h := f.get(t, harnessGVR, "org-a", "kagent")
	require.NotNil(t, h)
	assert.Equal(t, ScopeAgents, h.GetLabels()[ObjectLabel])
	pool := f.get(t, poolGVR, "org-a", "kagent-default")
	require.NotNil(t, pool)
	replicas, _, _ := unstructured.NestedInt64(pool.Object, "spec", "replicas")
	assert.Equal(t, int64(1), replicas, "the tenant's replica count, not the platform pool's")
	assert.Equal(t, "org-a", pool.GetLabels()[workerPoolNamespaceLabel])
	assert.Equal(t, "kagent-default", pool.GetLabels()[workerPoolNameLabel])
	_, err := f.typed.CoreV1().ServiceAccounts("org-a").Get(ctx, "kagent-default", metav1.GetOptions{})
	require.NoError(t, err, "the workers' ServiceAccount")
	require.NotNil(t, f.get(t, mcGVR, "org-a", "default-model-config"))
	secret, err := f.typed.CoreV1().Secrets("org-a").Get(ctx, keySecret, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, []byte("sk"), secret.Data["ANTHROPIC_API_KEY"])

	// The last agent goes.
	require.NoError(t, f.dyn.Resource(hrGVR).Namespace("org-a").Delete(ctx, "sre", metav1.DeleteOptions{}))
	require.NoError(t, f.r.ReconcileNamespace(ctx, "org-a"))
	assert.Nil(t, f.get(t, harnessGVR, "org-a", "kagent"))
	assert.Nil(t, f.get(t, poolGVR, "org-a", "kagent-default"))
	assert.Nil(t, f.get(t, mcGVR, "org-a", "default-model-config"))
	_, err = f.typed.CoreV1().Secrets("org-a").Get(ctx, keySecret, metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
	_, err = f.typed.CoreV1().ServiceAccounts("org-a").Get(ctx, "kagent-default", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
	assert.NotNil(t, f.get(t, mcGVR, "org-a", "own"), "a tenant's own ModelConfig is never touched")
	assert.NotNil(t, f.get(t, ociGVR, "org-a", "agent"), "the namespace's objects stay")
	_, err = f.typed.CoreV1().ServiceAccounts("org-a").Get(ctx, "agents-flux", metav1.GetOptions{})
	assert.NoError(t, err)
}

// An Agent object counts as an agent; a release placed on a workload cluster
// does not (it runs on that cluster's Harness).
func TestTenantHoldsAgents(t *testing.T) {
	agent := obj("api.kagent.dev/v1alpha3", "Agent", "org-a", "bare", map[string]any{})
	f := newFixture(t, []runtime.Object{agent})
	f.r.ReconcileAll(context.Background())
	assert.NotNil(t, f.get(t, harnessGVR, "org-a", "kagent"))

	remote := agentRelease("org-a")
	remote.Object["spec"].(map[string]any)["kubeConfig"] = map[string]any{"secretRef": map[string]any{"name": "wc1-kubeconfig"}}
	f = newFixture(t, []runtime.Object{remote})
	f.r.ReconcileAll(context.Background())
	assert.Nil(t, f.get(t, harnessGVR, "org-a", "kagent"))
}

// An object of the same name the reconciler did not write is left as it is;
// its own copy is brought back when it drifts.
func TestTenantOwnership(t *testing.T) {
	theirs := obj("api.kagent.dev/v1alpha3", "Harness", "org-a", "kagent", map[string]any{"claude": map[string]any{}})
	f := newFixture(t, []runtime.Object{agentRelease("org-a"), theirs})
	ctx := context.Background()
	f.r.ReconcileAll(ctx)
	h := f.get(t, harnessGVR, "org-a", "kagent")
	assert.Empty(t, h.GetLabels()[ObjectLabel])
	_, isClaude, _ := unstructured.NestedMap(h.Object, "spec", "claude")
	assert.True(t, isClaude, "not overwritten")

	mc := f.get(t, mcGVR, "org-a", "default-model-config")
	require.NoError(t, unstructured.SetNestedField(mc.Object, "other", "spec", "model"))
	_, err := f.dyn.Resource(mcGVR).Namespace("org-a").Update(ctx, mc, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, f.r.ReconcileNamespace(ctx, "org-a"))
	model, _, _ := unstructured.NestedString(f.get(t, mcGVR, "org-a", "default-model-config").Object, "spec", "model")
	assert.Equal(t, "claude", model)
}

// The ModelConfig's key Secret is copied only under the configured name: the
// reconciler may read and write no other Secret.
func TestTenantOtherSecretRefused(t *testing.T) {
	f := newFixture(t, []runtime.Object{agentRelease("org-a")})
	f.r.cfg.ModelSecretName = "something-else"
	err := f.r.ReconcileNamespace(context.Background(), "org-a")
	require.Error(t, err)
	assert.Contains(t, err.Error(), keySecret)
	_, err = f.typed.CoreV1().Secrets("org-a").Get(context.Background(), keySecret, metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
}

// Kicking a namespace that is not a tenant does nothing.
func TestReconcileNamespaceIgnoresOthers(t *testing.T) {
	f := newFixture(t, nil)
	require.NoError(t, f.r.ReconcileNamespace(context.Background(), "other"))
	require.NoError(t, f.r.ReconcileNamespace(context.Background(), "kagent"))
	require.NoError(t, f.r.ReconcileNamespace(context.Background(), "missing"))
	assert.Empty(t, f.r.Namespaces())
	assert.Nil(t, f.get(t, ociGVR, "kagent", "agent"))
}

// The agent chart's source is created once and never moved: a range changes
// only through the migrate command.
func TestTenantOCIRepositoryIsNotMoved(t *testing.T) {
	oci := obj("source.toolkit.fluxcd.io/v1", "OCIRepository", "org-a", "agent", map[string]any{"url": "oci://example/agent", "ref": map[string]any{"semver": "1.x"}})
	oci.SetLabels(map[string]string{ObjectLabel: ScopeNamespace})
	f := newFixture(t, []runtime.Object{oci})
	f.r.ReconcileAll(context.Background())
	semver, _, _ := unstructured.NestedString(f.get(t, ociGVR, "org-a", "agent").Object, "spec", "ref", "semver")
	assert.Equal(t, "1.x", semver)
}
