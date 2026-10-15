package tenants

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var (
	hrGVR      = schema.GroupVersionResource{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"}
	ociGVR     = schema.GroupVersionResource{Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "ocirepositories"}
	agentGVR   = schema.GroupVersionResource{Group: "api.kagent.dev", Version: "v1alpha3", Resource: "agents"}
	harnessGVR = schema.GroupVersionResource{Group: "api.kagent.dev", Version: "v1alpha3", Resource: "harnesses"}
	mcGVR      = schema.GroupVersionResource{Group: "api.kagent.dev", Version: "v1alpha3", Resource: "modelconfigs"}
	poolGVR    = schema.GroupVersionResource{Group: "ate.dev", Version: "v1alpha1", Resource: "workerpools"}
)

func obj(apiVersion, kind, ns, name string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion, "kind": kind,
		"metadata": map[string]any{"name": name, "namespace": ns, "labels": map[string]any{"from": "platform"}},
		"spec":     spec,
	}}
}

// field is a string field of o; empty when o is nil.
func field(o *unstructured.Unstructured, path ...string) string {
	if o == nil {
		return ""
	}
	v, _, _ := unstructured.NestedString(o.Object, path...)
	return v
}

func agentRelease(ns string) *unstructured.Unstructured {
	return obj("helm.toolkit.fluxcd.io/v2", "HelmRelease", ns, "sre", map[string]any{
		"chartRef": map[string]any{"kind": "OCIRepository", "name": "agent", "namespace": ns},
	})
}

// cluster is the envtest API server with the reconciler running as
// agent-manager runs it, as the chart's ServiceAccount, under the chart's
// RBAC and admission policy.
type cluster struct {
	*testEnv
	set *Set
	// config is the reconciler's Config.
	config Config
	// poolUpdates counts the reconciler's update requests of WorkerPools,
	// no-op updates included (the API server stores those without a new
	// resourceVersion).
	poolUpdates atomic.Int64
}

func (c *cluster) create(t *testing.T, gvr schema.GroupVersionResource, o *unstructured.Unstructured) {
	t.Helper()
	_, err := c.dyn.Resource(gvr).Namespace(o.GetNamespace()).Create(t.Context(), o, metav1.CreateOptions{})
	require.NoError(t, err)
}

// get reads an object as the admin; nil when absent.
func (c *cluster) get(t *testing.T, gvr schema.GroupVersionResource, ns, name string) *unstructured.Unstructured {
	t.Helper()
	o, err := c.dyn.Resource(gvr).Namespace(ns).Get(t.Context(), name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	require.NoError(t, err)
	return o
}

func (c *cluster) namespace(t *testing.T, name string, labels map[string]string) {
	t.Helper()
	_, err := c.admin.CoreV1().Namespaces().Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}, metav1.CreateOptions{})
	require.NoError(t, err)
}

func (c *cluster) secret(t *testing.T, ns, name string) *corev1.Secret {
	t.Helper()
	s, err := c.admin.CoreV1().Secrets(ns).Get(t.Context(), name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	require.NoError(t, err)
	return s
}

func (c *cluster) serviceAccount(t *testing.T, ns, name string) bool {
	t.Helper()
	_, err := c.admin.CoreV1().ServiceAccounts(ns).Get(t.Context(), name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false
	}
	require.NoError(t, err)
	return true
}

// eventually waits for cond: the watches, not a resync, must bring it about
// (the sync period is an hour).
func eventually(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	require.Eventually(t, cond, 20*time.Second, 100*time.Millisecond, msg)
}

// startCluster seeds the platform namespace kagent (itself labelled, never a
// tenant), the tenant org-a, the unlabelled namespace plain, and org-c, a
// tenant that already holds its own Harness and an OCIRepository on another
// range; then runs the reconciler.
func startCluster(t *testing.T) *cluster {
	env := startEnvtest(t, "tenants.networkPolicy.flavor=kubernetes", "tenants.networkPolicy.names={substrate-workers}")
	c := &cluster{testEnv: env}
	ctx := t.Context()

	c.namespace(t, "kagent", map[string]string{OrganizationLabel: "giantswarm"})
	c.namespace(t, "org-a", map[string]string{OrganizationLabel: "a"})
	c.namespace(t, "plain", nil)
	c.namespace(t, "org-c", nil)
	c.create(t, harnessGVR, obj("api.kagent.dev/v1alpha3", "Harness", "kagent", "kagent", map[string]any{
		"workload":  map[string]any{"image": "ghcr.io/kagent@sha256:abc"},
		"substrate": map[string]any{"workerPoolRef": map[string]any{"name": "kagent-default"}},
	}))
	c.create(t, poolGVR, obj("ate.dev/v1alpha1", "WorkerPool", "kagent", "kagent-default", map[string]any{
		"replicas": int64(3), "workerImage": "ateom@sha256:def", "template": map[string]any{"serviceAccountName": "kagent-default"},
	}))
	c.create(t, mcGVR, obj("api.kagent.dev/v1alpha3", "ModelConfig", "kagent", "default-model-config", map[string]any{
		"provider": "Anthropic", "model": "claude", "apiKeySecret": keySecret,
	}))
	_, err := c.admin.NetworkingV1().NetworkPolicies("kagent").Create(ctx, &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "substrate-workers", Labels: map[string]string{"from": "platform"}},
		Spec:       networkingv1.NetworkPolicySpec{PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	for _, name := range []string{keySecret, "other"} {
		_, err := c.admin.CoreV1().Secrets("kagent").Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name}, Data: map[string][]byte{"ANTHROPIC_API_KEY": []byte("sk")},
		}, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	c.create(t, harnessGVR, obj("api.kagent.dev/v1alpha3", "Harness", "org-c", "kagent", map[string]any{"claude": map[string]any{}}))
	oci := obj("source.toolkit.fluxcd.io/v1", "OCIRepository", "org-c", "agent", map[string]any{"url": "oci://example/agent", "ref": map[string]any{"semver": "1.x"}})
	oci.SetLabels(map[string]string{ObjectLabel: ScopeNamespace})
	c.create(t, ociGVR, oci)

	c.config = Config{
		PlatformNamespace:  "kagent",
		HarnessName:        "kagent",
		ModelConfigName:    "default-model-config",
		ModelSecretName:    keySecret,
		WorkerPoolReplicas: 1,
		FluxServiceAccount: "agents-flux",
		FluxClusterRole:    "agent-platform-tenant-flux",
		NetworkPolicies:    []string{"substrate-workers"},
		NetworkPolicyGVK:   networkingv1.SchemeGroupVersion.WithKind("NetworkPolicy"),
		OCIRepository: func(ns string) *unstructured.Unstructured {
			return obj("source.toolkit.fluxcd.io/v1", "OCIRepository", ns, "agent", map[string]any{"url": "oci://example/agent", "ref": map[string]any{"semver": "2.x"}})
		},
		ChartName:               "agent",
		HelmReleaseGVK:          schema.GroupVersionKind{Group: "helm.toolkit.fluxcd.io", Version: "v2", Kind: "HelmRelease"},
		OCIRepositoryGVK:        schema.GroupVersionKind{Group: "source.toolkit.fluxcd.io", Version: "v1", Kind: "OCIRepository"},
		AgentGVK:                schema.GroupVersionKind{Group: "api.kagent.dev", Version: "v1alpha3", Kind: "Agent"},
		HarnessGVK:              schema.GroupVersionKind{Group: "api.kagent.dev", Version: "v1alpha3", Kind: "Harness"},
		ModelConfigGVK:          schema.GroupVersionKind{Group: "api.kagent.dev", Version: "v1alpha3", Kind: "ModelConfig"},
		WorkerPoolGVK:           schema.GroupVersionKind{Group: "ate.dev", Version: "v1alpha1", Kind: "WorkerPool"},
		SyncPeriod:              time.Hour,
		LeaderElectionNamespace: releaseNamespace,
	}
	restCfg := env.as(env.reconciler)
	restCfg.WrapTransport = func(rt http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method == http.MethodPut && strings.Contains(req.URL.Path, "/workerpools/") {
				c.poolUpdates.Add(1)
			}
			return rt.RoundTrip(req)
		})
	}
	c.set, err = NewSet(restCfg, c.config, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	require.NoError(t, err)
	mgrCtx, cancel := context.WithCancel(context.Background())
	done, err := c.set.Run(mgrCtx)
	require.NoError(t, err, "the manager's cache syncs as the ServiceAccount")
	require.True(t, c.set.Status().Running)
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return c
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// TestTenantReconcilerEnvtest runs the reconciler on a real kube-apiserver
// as the chart's ServiceAccount, so every read, watch and write also passes
// the chart's RBAC and admission policy: a cache that lists more Secrets
// than the model key, or a write the policy refuses, fails it.
func TestTenantReconcilerEnvtest(t *testing.T) {
	c := startCluster(t)
	ctx := t.Context()

	t.Run("a tenant without agents gets its Flux objects and network policies only", func(t *testing.T) {
		eventually(t, func() bool { return c.get(t, ociGVR, "org-a", "agent") != nil }, "the OCIRepository")
		assert.Equal(t, ScopeNamespace, c.get(t, ociGVR, "org-a", "agent").GetLabels()[ObjectLabel])
		eventually(t, func() bool {
			_, err := c.admin.NetworkingV1().NetworkPolicies("org-a").Get(ctx, "substrate-workers", metav1.GetOptions{})
			return err == nil
		}, "the network policy")
		np, err := c.admin.NetworkingV1().NetworkPolicies("org-a").Get(ctx, "substrate-workers", metav1.GetOptions{})
		require.NoError(t, err)
		assert.Equal(t, "kagent/substrate-workers", np.Annotations[CopiedFromAnnotation])
		assert.Empty(t, np.Labels["from"], "the source's labels are not copied")
		eventually(t, func() bool { return c.serviceAccount(t, "org-a", "agents-flux") }, "the Flux ServiceAccount")
		eventually(t, func() bool {
			_, err := c.admin.RbacV1().RoleBindings("org-a").Get(ctx, "agents-flux", metav1.GetOptions{})
			return err == nil
		}, "the Flux RoleBinding")
		binding, err := c.admin.RbacV1().RoleBindings("org-a").Get(ctx, "agents-flux", metav1.GetOptions{})
		require.NoError(t, err)
		assert.Equal(t, "agent-platform-tenant-flux", binding.RoleRef.Name)
		assert.Equal(t, "ClusterRole", binding.RoleRef.Kind)
		require.Len(t, binding.Subjects, 1)
		assert.Equal(t, "org-a", binding.Subjects[0].Namespace)

		assert.Nil(t, c.get(t, harnessGVR, "org-a", "kagent"))
		assert.Nil(t, c.get(t, poolGVR, "org-a", "kagent-default"))
		assert.Nil(t, c.get(t, mcGVR, "org-a", "default-model-config"))
		assert.Nil(t, c.secret(t, "org-a", keySecret))
		assert.Nil(t, c.get(t, ociGVR, "plain", "agent"), "an unlabelled namespace is not a tenant")
		assert.Nil(t, c.get(t, ociGVR, "kagent", "agent"), "the platform namespace is never a tenant")
		assert.Equal(t, []string{"org-a"}, c.set.Namespaces())
	})

	t.Run("an object the reconciler did not write is left as it is, and the OCIRepository is not moved", func(t *testing.T) {
		ns, err := c.admin.CoreV1().Namespaces().Get(ctx, "org-c", metav1.GetOptions{})
		require.NoError(t, err)
		ns.Labels = map[string]string{OrganizationLabel: "c"}
		_, err = c.admin.CoreV1().Namespaces().Update(ctx, ns, metav1.UpdateOptions{})
		require.NoError(t, err)
		c.create(t, hrGVR, agentRelease("org-c"))
		// The tenant's copies show the namespace was reconciled with an agent.
		eventually(t, func() bool { return c.get(t, mcGVR, "org-c", "default-model-config") != nil }, "org-c's ModelConfig")
		h := c.get(t, harnessGVR, "org-c", "kagent")
		assert.Empty(t, h.GetLabels()[ObjectLabel])
		_, isClaude, _ := unstructured.NestedMap(h.Object, "spec", "claude")
		assert.True(t, isClaude, "not overwritten")
		semver, _, _ := unstructured.NestedString(c.get(t, ociGVR, "org-c", "agent").Object, "spec", "ref", "semver")
		assert.Equal(t, "1.x", semver)
		assert.Equal(t, []string{"org-a", "org-c"}, c.set.Namespaces(), "a namespace labelled at run time is a tenant at once")
	})

	t.Run("the first agent brings the Harness, a WorkerPool and the default model", func(t *testing.T) {
		c.create(t, mcGVR, obj("api.kagent.dev/v1alpha3", "ModelConfig", "org-a", "own", map[string]any{"provider": "OpenAI"}))
		// A HelmRelease without a spec (the CRD does not require one) is no
		// agent, and the cache that trims it does not stop the reconciler.
		noSpec := agentRelease("org-a")
		noSpec.SetName("no-spec")
		delete(noSpec.Object, "spec")
		c.create(t, hrGVR, noSpec)
		c.create(t, hrGVR, agentRelease("org-a"))
		eventually(t, func() bool { return c.secret(t, "org-a", keySecret) != nil }, "the key Secret")
		eventually(t, func() bool { return c.get(t, harnessGVR, "org-a", "kagent") != nil }, "the Harness")
		assert.Equal(t, ScopeAgents, c.get(t, harnessGVR, "org-a", "kagent").GetLabels()[ObjectLabel])
		eventually(t, func() bool { return c.get(t, poolGVR, "org-a", "kagent-default") != nil }, "the WorkerPool")
		pool := c.get(t, poolGVR, "org-a", "kagent-default")
		replicas, _, _ := unstructured.NestedInt64(pool.Object, "spec", "replicas")
		assert.Equal(t, int64(1), replicas, "the tenant's replica count, not the platform pool's")
		assert.Equal(t, "org-a", pool.GetLabels()[workerPoolNamespaceLabel])
		assert.Equal(t, "kagent-default", pool.GetLabels()[workerPoolNameLabel])
		eventually(t, func() bool { return c.serviceAccount(t, "org-a", "kagent-default") }, "the workers' ServiceAccount")
		eventually(t, func() bool { return c.get(t, mcGVR, "org-a", "default-model-config") != nil }, "the ModelConfig")
		assert.Equal(t, []byte("sk"), c.secret(t, "org-a", keySecret).Data["ANTHROPIC_API_KEY"])
	})

	t.Run("a WorkerPool copy is the platform spec but replicas: a field removed there leaves every copy", func(t *testing.T) {
		pools := c.dyn.Resource(poolGVR).Namespace("kagent")
		setPlatformPool := func(edit func(spec map[string]any)) {
			t.Helper()
			require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
				pool, err := pools.Get(ctx, "kagent-default", metav1.GetOptions{})
				if err != nil {
					return err
				}
				edit(pool.Object["spec"].(map[string]any))
				_, err = pools.Update(ctx, pool, metav1.UpdateOptions{})
				return err
			}))
		}
		setPlatformPool(func(spec map[string]any) { spec["nodeSelector"] = map[string]any{"pool": "old"} })
		for _, ns := range []string{"org-a", "org-c"} {
			eventually(t, func() bool {
				return field(c.get(t, poolGVR, ns, "kagent-default"), "spec", "nodeSelector", "pool") == "old"
			}, ns+"'s copy gets the new field")
		}
		setPlatformPool(func(spec map[string]any) { delete(spec, "nodeSelector") })
		for _, ns := range []string{"org-a", "org-c"} {
			eventually(t, func() bool {
				_, found, _ := unstructured.NestedFieldNoCopy(c.get(t, poolGVR, ns, "kagent-default").Object, "spec", "nodeSelector")
				return !found
			}, ns+"'s copy loses the removed field")
			// What agent-platform-tenant-workerpool-copy admits: the spec of
			// the platform pool, the server's defaults included, but replicas.
			want := c.get(t, poolGVR, "kagent", "kagent-default").Object["spec"].(map[string]any)
			want["replicas"] = c.config.WorkerPoolReplicas
			assert.Equal(t, want, c.get(t, poolGVR, ns, "kagent-default").Object["spec"])
		}
	})

	t.Run("an unchanged platform WorkerPool writes no copy again", func(t *testing.T) {
		before := c.poolUpdates.Load()
		for range 3 {
			_, err := c.set.reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "org-a"}})
			require.NoError(t, err)
		}
		assert.Equal(t, before, c.poolUpdates.Load(), "the copy and the platform pool both carry the server's defaults")
	})

	t.Run("a copy that drifts is brought back at once", func(t *testing.T) {
		mc := c.get(t, mcGVR, "org-a", "default-model-config")
		require.NotNil(t, mc)
		require.NoError(t, unstructured.SetNestedField(mc.Object, "other", "spec", "model"))
		_, err := c.dyn.Resource(mcGVR).Namespace("org-a").Update(ctx, mc, metav1.UpdateOptions{})
		require.NoError(t, err)
		eventually(t, func() bool {
			return field(c.get(t, mcGVR, "org-a", "default-model-config"), "spec", "model") == "claude"
		}, "the ModelConfig's model restored")

		require.NoError(t, c.dyn.Resource(harnessGVR).Namespace("org-a").Delete(ctx, "kagent", metav1.DeleteOptions{}))
		eventually(t, func() bool { return c.get(t, harnessGVR, "org-a", "kagent") != nil }, "a deleted copy is made again")
	})

	t.Run("a change to a platform source reaches every tenant with agents", func(t *testing.T) {
		src := c.secret(t, "kagent", keySecret)
		src.Data["ANTHROPIC_API_KEY"] = []byte("rotated")
		_, err := c.admin.CoreV1().Secrets("kagent").Update(ctx, src, metav1.UpdateOptions{})
		require.NoError(t, err)
		for _, ns := range []string{"org-a", "org-c"} {
			eventually(t, func() bool {
				s := c.secret(t, ns, keySecret)
				return s != nil && string(s.Data["ANTHROPIC_API_KEY"]) == "rotated"
			}, ns+"'s key rotated")
		}
	})

	t.Run("the cache holds the labelled namespaces, the copies, the platform sources and the model key only", func(t *testing.T) {
		cache := c.set.reconciler().mgr.GetCache()
		err := cache.Get(ctx, client.ObjectKey{Name: "plain"}, &corev1.Namespace{})
		assert.True(t, apierrors.IsNotFound(err), "an unlabelled namespace: %v", err)

		mcs := &unstructured.UnstructuredList{}
		mcs.SetGroupVersionKind(schema.GroupVersionKind{Group: "api.kagent.dev", Version: "v1alpha3", Kind: "ModelConfigList"})
		require.NoError(t, cache.List(ctx, mcs))
		var cached []string
		for _, mc := range mcs.Items {
			cached = append(cached, mc.GetNamespace()+"/"+mc.GetName())
		}
		assert.ElementsMatch(t, []string{"kagent/default-model-config", "org-a/default-model-config", "org-c/default-model-config"}, cached,
			"the tenant's own ModelConfig org-a/own is not cached")

		secrets := &corev1.SecretList{}
		require.NoError(t, cache.List(ctx, secrets))
		cached = nil
		for _, s := range secrets.Items {
			cached = append(cached, s.Namespace+"/"+s.Name)
		}
		assert.ElementsMatch(t, []string{"kagent/" + keySecret, "org-a/" + keySecret, "org-c/" + keySecret}, cached)
	})

	t.Run("the key Secret is copied only under the configured name", func(t *testing.T) {
		mc := c.get(t, mcGVR, "kagent", "default-model-config")
		require.NoError(t, unstructured.SetNestedField(mc.Object, "other", "spec", "apiKeySecret"))
		_, err := c.dyn.Resource(mcGVR).Namespace("kagent").Update(ctx, mc, metav1.UpdateOptions{})
		require.NoError(t, err)
		eventually(t, func() bool {
			return field(c.get(t, mcGVR, "org-a", "default-model-config"), "spec", "apiKeySecret") == "other"
		}, "the copy follows the source")
		_, err = c.set.reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "org-a"}})
		require.ErrorContains(t, err, `the default ModelConfig's key Secret "other" is not the tenant model Secret "`+keySecret+`"`)
		assert.Nil(t, c.secret(t, "org-a", "other"))

		// Back to the configured name for the subtests after.
		mc, err = c.dyn.Resource(mcGVR).Namespace("kagent").Get(ctx, "default-model-config", metav1.GetOptions{})
		require.NoError(t, err)
		require.NoError(t, unstructured.SetNestedField(mc.Object, keySecret, "spec", "apiKeySecret"))
		_, err = c.dyn.Resource(mcGVR).Namespace("kagent").Update(ctx, mc, metav1.UpdateOptions{})
		require.NoError(t, err)
	})

	t.Run("the last agent takes them away; the namespace's objects and the tenant's own stay", func(t *testing.T) {
		require.NoError(t, c.dyn.Resource(hrGVR).Namespace("org-a").Delete(ctx, "sre", metav1.DeleteOptions{}))
		eventually(t, func() bool { return c.secret(t, "org-a", keySecret) == nil }, "the key Secret removed")
		eventually(t, func() bool { return c.get(t, harnessGVR, "org-a", "kagent") == nil }, "the Harness removed")
		eventually(t, func() bool { return c.get(t, poolGVR, "org-a", "kagent-default") == nil }, "the WorkerPool removed")
		eventually(t, func() bool { return c.get(t, mcGVR, "org-a", "default-model-config") == nil }, "the ModelConfig removed")
		eventually(t, func() bool { return !c.serviceAccount(t, "org-a", "kagent-default") }, "the workers' ServiceAccount removed")
		assert.NotNil(t, c.get(t, mcGVR, "org-a", "own"), "a tenant's own ModelConfig is never touched")
		assert.NotNil(t, c.get(t, ociGVR, "org-a", "agent"), "the namespace's objects stay")
		assert.True(t, c.serviceAccount(t, "org-a", "agents-flux"))
	})

	t.Run("an Agent object counts as an agent; a release on a workload cluster does not", func(t *testing.T) {
		c.namespace(t, "org-b", map[string]string{OrganizationLabel: "b"})
		remote := agentRelease("org-b")
		remote.Object["spec"].(map[string]any)["kubeConfig"] = map[string]any{"secretRef": map[string]any{"name": "wc1-kubeconfig"}}
		c.create(t, hrGVR, remote)
		c.create(t, agentGVR, obj("api.kagent.dev/v1alpha3", "Agent", "org-b", "bare", map[string]any{}))
		eventually(t, func() bool { return c.get(t, harnessGVR, "org-b", "kagent") != nil }, "the Agent brings the Harness")
		require.NoError(t, c.dyn.Resource(agentGVR).Namespace("org-b").Delete(ctx, "bare", metav1.DeleteOptions{}))
		eventually(t, func() bool { return c.get(t, harnessGVR, "org-b", "kagent") == nil }, "with only the remote release left, the Harness goes")
	})

	t.Run("a namespace that loses the label is no tenant any more, and keeps what it holds", func(t *testing.T) {
		ns, err := c.admin.CoreV1().Namespaces().Get(ctx, "org-b", metav1.GetOptions{})
		require.NoError(t, err)
		delete(ns.Labels, OrganizationLabel)
		_, err = c.admin.CoreV1().Namespaces().Update(ctx, ns, metav1.UpdateOptions{})
		require.NoError(t, err)
		eventually(t, func() bool { return !slices.Contains(c.set.Namespaces(), "org-b") }, "org-b no longer a tenant")
		assert.NotNil(t, c.get(t, ociGVR, "org-b", "agent"), "nothing is deleted from a namespace that leaves")
	})

	t.Run("the replicas elect the reconciler's leader with the Lease", func(t *testing.T) {
		lease, err := c.admin.CoordinationV1().Leases(releaseNamespace).Get(ctx, LeaderElectionID, metav1.GetOptions{})
		require.NoError(t, err)
		require.NotNil(t, lease.Spec.HolderIdentity)
		assert.NotEmpty(t, *lease.Spec.HolderIdentity)
	})

	t.Run("a kind the API server does not serve is refused before the manager starts, by name", func(t *testing.T) {
		cfg := c.config
		cfg.WorkerPoolGVK.Version = "v1beta9"
		cfg.NetworkPolicyGVK = schema.GroupVersionKind{Group: "cilium.io", Version: "v2", Kind: "CiliumNetworkPolicy"}
		_, err := NewSet(c.as(c.reconciler), cfg, nil)
		require.EqualError(t, err, "the API server does not serve ate.dev/v1beta9 WorkerPool, cilium.io/v2 CiliumNetworkPolicy, which the tenant reconciler watches")
	})

	// Last: it replaces the reconciler.
	t.Run("a lost Lease stops the reconciler but not the tenants; a new one reconciles once it holds the Lease", func(t *testing.T) {
		leases := c.admin.CoordinationV1().Leases(releaseNamespace)
		require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
			lease, err := leases.Get(ctx, LeaderElectionID, metav1.GetOptions{})
			if err != nil {
				return err
			}
			other, hour, now := "another-replica", int32(3600), metav1.NewMicroTime(time.Now())
			lease.Spec.HolderIdentity, lease.Spec.LeaseDurationSeconds = &other, &hour
			lease.Spec.AcquireTime, lease.Spec.RenewTime = &now, &now
			_, err = leases.Update(ctx, lease, metav1.UpdateOptions{})
			return err
		}))
		require.Eventually(t, func() bool { return c.set.Status().Error != "" }, time.Minute, 200*time.Millisecond, "the reconciler stops")
		assert.Equal(t, "leader election lost", c.set.Status().Error)
		assert.NotNil(t, c.set.Status().FailedAt)
		assert.False(t, c.set.Status().Running)
		assert.Contains(t, c.set.Namespaces(), "org-a", "the tenants are still known")

		c.namespace(t, "org-d", map[string]string{OrganizationLabel: "d"})
		require.Eventually(t, func() bool { return slices.Contains(c.set.Namespaces(), "org-d") }, time.Minute, 200*time.Millisecond,
			"the new reconciler's cache knows a tenant labelled after the failure")
		assert.True(t, c.set.Status().Running, "a new reconciler runs")
		assert.Equal(t, "leader election lost", c.set.Status().Error, "the last failure stays")
		assert.Never(t, func() bool { return c.get(t, ociGVR, "org-d", "agent") != nil }, 3*time.Second, 200*time.Millisecond,
			"nothing reconciles here while another replica holds the Lease")

		require.NoError(t, leases.Delete(ctx, LeaderElectionID, metav1.DeleteOptions{}))
		require.Eventually(t, func() bool { return c.get(t, ociGVR, "org-d", "agent") != nil }, time.Minute, 200*time.Millisecond,
			"the new reconciler takes the free Lease and reconciles")
	})
}
