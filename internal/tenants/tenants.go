// Package tenants keeps the namespaces of the installation's tenants ready to
// hold agents. A tenant is a namespace labelled giantswarm.io/organization
// (organization-operator creates org-<name> with it for every Organization;
// on an installation without Organizations the platform team labels the
// namespace). Tenants come and go at runtime, so nothing about them can be
// rendered by a chart: the reconciler discovers them and keeps, in each,
//
//   - always: the agent chart's OCIRepository, the ServiceAccount the agents'
//     HelmReleases run as with its RoleBinding to the tenant Flux ClusterRole,
//     and the network policies copied from the platform namespace;
//   - only while the namespace holds an agent: copies of the platform
//     Harness, of the WorkerPool it runs on (with the tenant's replica count)
//     and the pool's ServiceAccount, and of the default ModelConfig with its
//     API key Secret. They go again with the last agent, so an organization
//     without agents runs no workers.
//
// The reconciler is background work and runs as agent-manager's own
// ServiceAccount. It writes only objects it labels as its own and never
// changes or deletes one it did not create (a tenant's own ModelConfig, say).
// It never writes memberships: who may use a tenant is declared.
package tenants

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/giantswarm/agent-manager/internal/kube"
)

const (
	// OrganizationLabel marks a namespace as a tenant.
	OrganizationLabel = "giantswarm.io/organization"
	// ObjectLabel marks what the reconciler writes, with when it is kept:
	// ScopeNamespace objects always, ScopeAgents objects while the namespace
	// holds an agent. An object without it is never changed or deleted.
	ObjectLabel    = "agent-manager.giantswarm.io/tenant-object"
	ScopeNamespace = "namespace"
	ScopeAgents    = "agents"
	// CopiedFromAnnotation names the platform object a copy was made from.
	CopiedFromAnnotation = "agent-manager.giantswarm.io/copied-from"
	// managedByLabel / managedByValue: the platform's managed-by marker,
	// the value agent-manager stamps on everything it writes.
	managedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "agent-manager"
	// The placement labels kagent reads from a WorkerPool: the actors of a
	// namespace select the workers of their own namespace's pool by them.
	workerPoolNameLabel      = "kagent.dev/worker-pool-name"
	workerPoolNamespaceLabel = "kagent.dev/worker-pool-namespace"
	// FieldManager names agent-manager as the writer of the fields.
	fieldManager = "agent-manager"
)

// Config is what the reconciler keeps and where it copies it from.
type Config struct {
	// PlatformNamespace is the platform tenant (kagent): the source of every
	// copy, never a tenant itself even when labelled.
	PlatformNamespace string
	// HarnessName is the platform Harness copied into a tenant with agents.
	HarnessName string
	// ModelConfigName is the default ModelConfig copied into a tenant with
	// agents; ModelSecretName the one Secret name its API key may live in
	// (the reconciler's RBAC and admission policy allow no other).
	ModelConfigName string
	ModelSecretName string
	// WorkerPoolReplicas is the replica count of a tenant's WorkerPool.
	WorkerPoolReplicas int64
	// FluxServiceAccount is the ServiceAccount the agents' HelmReleases run
	// as (spec.serviceAccountName); FluxClusterRole the ClusterRole it is
	// bound to in the tenant. Empty FluxServiceAccount keeps neither.
	FluxServiceAccount string
	FluxClusterRole    string
	// NetworkPolicies are the names of the network policies copied from the
	// platform namespace, of the kind NetworkPolicyGVR names (empty: none).
	NetworkPolicies  []string
	NetworkPolicyGVR schema.GroupVersionResource
	// OCIRepository composes the agent chart's source for a namespace;
	// ChartName is its name, which an agent's HelmRelease references.
	OCIRepository func(namespace string) *unstructured.Unstructured
	ChartName     string
	// The served versions of what the reconciler reads and writes.
	HelmReleaseGVR   schema.GroupVersionResource
	OCIRepositoryGVR schema.GroupVersionResource
	AgentGVR         schema.GroupVersionResource
	HarnessGVR       schema.GroupVersionResource
	ModelConfigGVR   schema.GroupVersionResource
	WorkerPoolGVR    schema.GroupVersionResource
	// Interval is how often every tenant is reconciled.
	Interval time.Duration
}

// Reconciler keeps the tenant namespaces.
type Reconciler struct {
	dyn   dynamic.Interface
	typed kubernetes.Interface
	cfg   Config
	log   *slog.Logger
	kick  chan string

	mu         sync.RWMutex
	namespaces []string
}

// New builds the reconciler on the ServiceAccount's client.
func New(c kube.Client, cfg Config, log *slog.Logger) *Reconciler {
	if log == nil {
		log = slog.Default()
	}
	if cfg.Interval <= 0 {
		cfg.Interval = time.Minute
	}
	if cfg.WorkerPoolReplicas < 0 {
		cfg.WorkerPoolReplicas = 0
	}
	return &Reconciler{dyn: c.Dynamic(), typed: c.Typed(), cfg: cfg, log: log, kick: make(chan string, 16)}
}

// Namespaces are the tenant namespaces found by the last pass, sorted.
func (r *Reconciler) Namespaces() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Clone(r.namespaces)
}

// ModelConfig is the ModelConfig a tenant receives with its first agent.
func (r *Reconciler) ModelConfig() string { return r.cfg.ModelConfigName }

// Kick asks for namespace to be reconciled now, without waiting for the
// next pass: a create there gets its Harness and ModelConfig at once. It
// never blocks; a kick that finds the queue full waits for the next pass.
func (r *Reconciler) Kick(namespace string) {
	select {
	case r.kick <- namespace:
	default:
	}
}

// Run reconciles every tenant on each interval, and a kicked namespace at
// once, until ctx ends.
func (r *Reconciler) Run(ctx context.Context) {
	t := time.NewTicker(r.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.ReconcileAll(ctx)
		case ns := <-r.kick:
			if err := r.ReconcileNamespace(ctx, ns); err != nil {
				r.log.Warn("tenant reconcile failed", "namespace", ns, "error", err)
			}
		}
	}
}

// ReconcileAll discovers the tenants and reconciles each. A namespace that
// fails is logged and retried on the next pass; the others go on.
func (r *Reconciler) ReconcileAll(ctx context.Context) {
	list, err := r.typed.CoreV1().Namespaces().List(ctx, metav1.ListOptions{LabelSelector: OrganizationLabel})
	if err != nil {
		r.log.Warn("listing the tenant namespaces failed; keeping the last set", "error", err)
		return
	}
	var found []string
	for i := range list.Items {
		if r.isTenant(&list.Items[i]) {
			found = append(found, list.Items[i].Name)
		}
	}
	slices.Sort(found)
	r.mu.Lock()
	r.namespaces = found
	r.mu.Unlock()
	for _, ns := range found {
		if err := r.reconcile(ctx, ns); err != nil {
			r.log.Warn("tenant reconcile failed", "namespace", ns, "error", err)
		}
	}
}

// ReconcileNamespace reconciles one namespace if it is a tenant, and adds it
// to the known set.
func (r *Reconciler) ReconcileNamespace(ctx context.Context, name string) error {
	ns, err := r.typed.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get namespace %s: %w", name, err)
	}
	if !r.isTenant(ns) {
		return nil
	}
	r.mu.Lock()
	if !slices.Contains(r.namespaces, name) {
		r.namespaces = append(r.namespaces, name)
		slices.Sort(r.namespaces)
	}
	r.mu.Unlock()
	return r.reconcile(ctx, name)
}

// isTenant: labelled, not the platform namespace, not going away.
func (r *Reconciler) isTenant(ns *corev1.Namespace) bool {
	if _, ok := ns.Labels[OrganizationLabel]; !ok {
		return false
	}
	return ns.Name != r.cfg.PlatformNamespace && ns.DeletionTimestamp == nil && ns.Status.Phase != corev1.NamespaceTerminating
}

// reconcile keeps one tenant.
func (r *Reconciler) reconcile(ctx context.Context, ns string) error {
	var errs []error
	if r.cfg.OCIRepository != nil {
		errs = append(errs, r.ensure(ctx, r.cfg.OCIRepositoryGVR, ns, ScopeNamespace, r.cfg.OCIRepository(ns), ""))
	}
	if sa := r.cfg.FluxServiceAccount; sa != "" {
		errs = append(errs, r.ensureServiceAccount(ctx, ns, sa, ScopeNamespace), r.ensureFluxRoleBinding(ctx, ns))
	}
	if !r.cfg.NetworkPolicyGVR.Empty() {
		for _, name := range r.cfg.NetworkPolicies {
			errs = append(errs, r.copyObject(ctx, r.cfg.NetworkPolicyGVR, name, ns, ScopeNamespace))
		}
	}
	holds, err := r.holdsAgents(ctx, ns)
	if err != nil {
		// Unknown: nothing is removed on a failed read.
		return errors.Join(append(errs, err)...)
	}
	if holds {
		errs = append(errs, r.provision(ctx, ns))
	} else {
		errs = append(errs, r.release(ctx, ns))
	}
	return errors.Join(errs...)
}

// holdsAgents: an Agent object, or a HelmRelease of the agent chart that runs
// on this cluster (one placed on a workload cluster runs there, with that
// cluster's Harness).
func (r *Reconciler) holdsAgents(ctx context.Context, ns string) (bool, error) {
	agents, err := r.dyn.Resource(r.cfg.AgentGVR).Namespace(ns).List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil {
		return false, fmt.Errorf("list agents in %s: %w", ns, err)
	}
	if len(agents.Items) > 0 {
		return true, nil
	}
	hrs, err := r.dyn.Resource(r.cfg.HelmReleaseGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, fmt.Errorf("list helmreleases in %s: %w", ns, err)
	}
	for i := range hrs.Items {
		hr := &hrs.Items[i]
		kind, _, _ := unstructured.NestedString(hr.Object, "spec", "chartRef", "kind")
		name, _, _ := unstructured.NestedString(hr.Object, "spec", "chartRef", "name")
		_, remote, _ := unstructured.NestedMap(hr.Object, "spec", "kubeConfig")
		if kind == "OCIRepository" && name == r.cfg.ChartName && !remote {
			return true, nil
		}
	}
	return false, nil
}

// provision copies the platform Harness, its WorkerPool and the pool's
// ServiceAccount, and the default ModelConfig with its key Secret.
func (r *Reconciler) provision(ctx context.Context, ns string) error {
	var errs []error
	harness, err := r.platformObject(ctx, r.cfg.HarnessGVR, r.cfg.HarnessName)
	switch {
	case err != nil:
		errs = append(errs, err)
	case harness == nil:
		r.log.Warn("the platform Harness is missing; the tenant gets none", "namespace", ns, "harness", r.cfg.HarnessName, "platformNamespace", r.cfg.PlatformNamespace)
	default:
		if pool, _, _ := unstructured.NestedString(harness.Object, "spec", "substrate", "workerPoolRef", "name"); pool != "" {
			errs = append(errs, r.copyWorkerPool(ctx, ns, pool))
		}
		errs = append(errs, r.ensure(ctx, r.cfg.HarnessGVR, ns, ScopeAgents, copyOf(harness, ns), objectRef(harness)))
	}
	mc, err := r.platformObject(ctx, r.cfg.ModelConfigGVR, r.cfg.ModelConfigName)
	switch {
	case err != nil:
		errs = append(errs, err)
	case mc == nil:
		r.log.Warn("the platform's default ModelConfig is missing; the tenant gets none", "namespace", ns, "modelConfig", r.cfg.ModelConfigName, "platformNamespace", r.cfg.PlatformNamespace)
	default:
		if secret, _, _ := unstructured.NestedString(mc.Object, "spec", "apiKeySecret"); secret != "" {
			errs = append(errs, r.copySecret(ctx, ns, secret))
		}
		errs = append(errs, r.ensure(ctx, r.cfg.ModelConfigGVR, ns, ScopeAgents, copyOf(mc, ns), objectRef(mc)))
	}
	return errors.Join(errs...)
}

// copyWorkerPool copies the platform WorkerPool with the tenant's replica
// count and its own placement labels, and the ServiceAccount its workers run
// as.
func (r *Reconciler) copyWorkerPool(ctx context.Context, ns, name string) error {
	src, err := r.platformObject(ctx, r.cfg.WorkerPoolGVR, name)
	if err != nil {
		return err
	}
	if src == nil {
		r.log.Warn("the platform WorkerPool is missing; the tenant gets none", "namespace", ns, "workerPool", name)
		return nil
	}
	pool := copyOf(src, ns)
	if err := unstructured.SetNestedField(pool.Object, r.cfg.WorkerPoolReplicas, "spec", "replicas"); err != nil {
		return err
	}
	labels := pool.GetLabels()
	labels[workerPoolNameLabel] = name
	labels[workerPoolNamespaceLabel] = ns
	pool.SetLabels(labels)
	var errs []error
	if sa, _, _ := unstructured.NestedString(pool.Object, "spec", "template", "serviceAccountName"); sa != "" {
		errs = append(errs, r.ensureServiceAccount(ctx, ns, sa, ScopeAgents))
	}
	return errors.Join(append(errs, r.ensure(ctx, r.cfg.WorkerPoolGVR, ns, ScopeAgents, pool, objectRef(src)))...)
}

// release removes what provision wrote, once the last agent is gone.
func (r *Reconciler) release(ctx context.Context, ns string) error {
	var errs []error
	selector := metav1.ListOptions{LabelSelector: ObjectLabel + "=" + ScopeAgents}
	pools, err := r.dyn.Resource(r.cfg.WorkerPoolGVR).Namespace(ns).List(ctx, selector)
	if err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("list workerpools in %s: %w", ns, err))
	} else if pools != nil {
		for i := range pools.Items {
			pool := &pools.Items[i]
			errs = append(errs, r.delete(ctx, r.cfg.WorkerPoolGVR, ns, pool.GetName()))
			if sa, _, _ := unstructured.NestedString(pool.Object, "spec", "template", "serviceAccountName"); sa != "" {
				errs = append(errs, r.deleteServiceAccount(ctx, ns, sa))
			}
		}
	}
	errs = append(errs, r.deleteAll(ctx, r.cfg.HarnessGVR, ns, selector))
	mcs, err := r.dyn.Resource(r.cfg.ModelConfigGVR).Namespace(ns).List(ctx, selector)
	if err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("list modelconfigs in %s: %w", ns, err))
	} else if mcs != nil {
		for i := range mcs.Items {
			mc := &mcs.Items[i]
			errs = append(errs, r.delete(ctx, r.cfg.ModelConfigGVR, ns, mc.GetName()))
			if secret, _, _ := unstructured.NestedString(mc.Object, "spec", "apiKeySecret"); secret == r.cfg.ModelSecretName && secret != "" {
				errs = append(errs, r.deleteSecret(ctx, ns, secret))
			}
		}
	}
	return errors.Join(errs...)
}

// ---- unstructured objects -------------------------------------------------

// platformObject reads an object of the platform namespace; nil when absent.
func (r *Reconciler) platformObject(ctx context.Context, gvr schema.GroupVersionResource, name string) (*unstructured.Unstructured, error) {
	obj, err := r.dyn.Resource(gvr).Namespace(r.cfg.PlatformNamespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get %s %s/%s: %w", gvr.Resource, r.cfg.PlatformNamespace, name, err)
	}
	return obj, nil
}

// copyObject copies a platform object of gvr into ns; a missing source is
// logged and skipped.
func (r *Reconciler) copyObject(ctx context.Context, gvr schema.GroupVersionResource, name, ns, scope string) error {
	src, err := r.platformObject(ctx, gvr, name)
	if err != nil {
		return err
	}
	if src == nil {
		r.log.Warn("a platform object to copy is missing", "resource", gvr.Resource, "name", name, "platformNamespace", r.cfg.PlatformNamespace, "namespace", ns)
		return nil
	}
	return r.ensure(ctx, gvr, ns, scope, copyOf(src, ns), objectRef(src))
}

// copyOf is src's spec under the same name in ns, with no metadata of src's.
func copyOf(src *unstructured.Unstructured, ns string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": src.GetAPIVersion(),
		"kind":       src.GetKind(),
		"metadata":   map[string]any{"name": src.GetName(), "namespace": ns},
	}}
	if spec, ok := src.Object["spec"]; ok {
		obj.Object["spec"] = runtime.DeepCopyJSONValue(spec)
	}
	obj.SetLabels(map[string]string{})
	return obj
}

func objectRef(src *unstructured.Unstructured) string {
	return src.GetNamespace() + "/" + src.GetName()
}

// ensure creates obj, or brings the reconciler's own copy back to it. An
// object of that name the reconciler did not write is left alone.
func (r *Reconciler) ensure(ctx context.Context, gvr schema.GroupVersionResource, ns, scope string, obj *unstructured.Unstructured, from string) error {
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[ObjectLabel] = scope
	labels[managedByLabel] = managedByValue
	obj.SetLabels(labels)
	if from != "" {
		obj.SetAnnotations(map[string]string{CopiedFromAnnotation: from})
	}
	obj.SetNamespace(ns)
	client := r.dyn.Resource(gvr).Namespace(ns)
	existing, err := client.Get(ctx, obj.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if _, err := client.Create(ctx, obj, metav1.CreateOptions{FieldManager: fieldManager}); err != nil {
			return fmt.Errorf("create %s %s/%s: %w", gvr.Resource, ns, obj.GetName(), err)
		}
		r.log.Info("tenant object created", "namespace", ns, "resource", gvr.Resource, "name", obj.GetName())
		return nil
	}
	if err != nil {
		return fmt.Errorf("get %s %s/%s: %w", gvr.Resource, ns, obj.GetName(), err)
	}
	if existing.GetLabels()[ObjectLabel] == "" {
		r.log.Debug("leaving an object the reconciler did not write", "namespace", ns, "resource", gvr.Resource, "name", obj.GetName())
		return nil
	}
	if contains(existing.Object["spec"], obj.Object["spec"]) && existing.GetLabels()[ObjectLabel] == scope && subset(existing.GetLabels(), labels) {
		return nil
	}
	existing.Object["spec"] = obj.Object["spec"]
	merged := existing.GetLabels()
	for k, v := range labels {
		merged[k] = v
	}
	existing.SetLabels(merged)
	if _, err := client.Update(ctx, existing, metav1.UpdateOptions{FieldManager: fieldManager}); err != nil {
		return fmt.Errorf("update %s %s/%s: %w", gvr.Resource, ns, obj.GetName(), err)
	}
	r.log.Info("tenant object updated", "namespace", ns, "resource", gvr.Resource, "name", obj.GetName())
	return nil
}

// delete removes the reconciler's own object of that name.
func (r *Reconciler) delete(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) error {
	client := r.dyn.Resource(gvr).Namespace(ns)
	existing, err := client.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get %s %s/%s: %w", gvr.Resource, ns, name, err)
	}
	if existing.GetLabels()[ObjectLabel] == "" {
		return nil
	}
	if err := client.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete %s %s/%s: %w", gvr.Resource, ns, name, err)
	}
	r.log.Info("tenant object deleted", "namespace", ns, "resource", gvr.Resource, "name", name)
	return nil
}

func (r *Reconciler) deleteAll(ctx context.Context, gvr schema.GroupVersionResource, ns string, selector metav1.ListOptions) error {
	list, err := r.dyn.Resource(gvr).Namespace(ns).List(ctx, selector)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("list %s in %s: %w", gvr.Resource, ns, err)
	}
	var errs []error
	for i := range list.Items {
		errs = append(errs, r.delete(ctx, gvr, ns, list.Items[i].GetName()))
	}
	return errors.Join(errs...)
}

// contains reports whether every field of want is in have with the same
// value: the API server adds defaults to what was written, which is no
// drift.
func contains(have, want any) bool {
	switch w := want.(type) {
	case map[string]any:
		h, ok := have.(map[string]any)
		if !ok {
			return false
		}
		for k, v := range w {
			if !contains(h[k], v) {
				return false
			}
		}
		return true
	case []any:
		h, ok := have.([]any)
		if !ok || len(h) != len(w) {
			return false
		}
		for i := range w {
			if !contains(h[i], w[i]) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(have, want)
	}
}

func subset(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// ---- typed objects ----------------------------------------------------------

func (r *Reconciler) labels(scope string) map[string]string {
	return map[string]string{ObjectLabel: scope, managedByLabel: managedByValue}
}

// ensureServiceAccount creates the ServiceAccount; one that exists is kept
// as it is (a ServiceAccount has nothing to drift).
func (r *Reconciler) ensureServiceAccount(ctx context.Context, ns, name, scope string) error {
	client := r.typed.CoreV1().ServiceAccounts(ns)
	_, err := client.Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get serviceaccount %s/%s: %w", ns, name, err)
	}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: r.labels(scope)}}
	if _, err := client.Create(ctx, sa, metav1.CreateOptions{FieldManager: fieldManager}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create serviceaccount %s/%s: %w", ns, name, err)
	}
	r.log.Info("tenant object created", "namespace", ns, "resource", "serviceaccounts", "name", name)
	return nil
}

func (r *Reconciler) deleteServiceAccount(ctx context.Context, ns, name string) error {
	client := r.typed.CoreV1().ServiceAccounts(ns)
	sa, err := client.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get serviceaccount %s/%s: %w", ns, name, err)
	}
	if sa.Labels[ObjectLabel] != ScopeAgents {
		return nil
	}
	if err := client.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete serviceaccount %s/%s: %w", ns, name, err)
	}
	r.log.Info("tenant object deleted", "namespace", ns, "resource", "serviceaccounts", "name", name)
	return nil
}

// ensureFluxRoleBinding binds the tenant Flux ClusterRole to the agents'
// ServiceAccount in ns. A RoleBinding's roleRef cannot change, so the
// reconciler's own binding to another role is replaced.
func (r *Reconciler) ensureFluxRoleBinding(ctx context.Context, ns string) error {
	name := r.cfg.FluxServiceAccount
	want := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: r.labels(ScopeNamespace)},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: r.cfg.FluxClusterRole},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: name, Namespace: ns}},
	}
	client := r.typed.RbacV1().RoleBindings(ns)
	existing, err := client.Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return fmt.Errorf("get rolebinding %s/%s: %w", ns, name, err)
	case existing.Labels[ObjectLabel] == "":
		return nil
	case existing.RoleRef == want.RoleRef && reflect.DeepEqual(existing.Subjects, want.Subjects):
		return nil
	case existing.RoleRef == want.RoleRef:
		existing.Subjects = want.Subjects
		if _, err := client.Update(ctx, existing, metav1.UpdateOptions{FieldManager: fieldManager}); err != nil {
			return fmt.Errorf("update rolebinding %s/%s: %w", ns, name, err)
		}
		return nil
	default:
		if err := client.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("replace rolebinding %s/%s: %w", ns, name, err)
		}
	}
	if _, err := client.Create(ctx, want, metav1.CreateOptions{FieldManager: fieldManager}); err != nil {
		return fmt.Errorf("create rolebinding %s/%s: %w", ns, name, err)
	}
	r.log.Info("tenant object created", "namespace", ns, "resource", "rolebindings", "name", name)
	return nil
}

// copySecret copies the default ModelConfig's API key Secret. Only the
// configured name is copied: the reconciler may read and write no other
// Secret.
func (r *Reconciler) copySecret(ctx context.Context, ns, name string) error {
	if name != r.cfg.ModelSecretName {
		return fmt.Errorf("the default ModelConfig's key Secret %q is not the tenant model Secret %q the reconciler may copy; set it to %q", name, r.cfg.ModelSecretName, name)
	}
	src, err := r.typed.CoreV1().Secrets(r.cfg.PlatformNamespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		r.log.Warn("the default ModelConfig's key Secret is missing", "secret", name, "platformNamespace", r.cfg.PlatformNamespace, "namespace", ns)
		return nil
	}
	if err != nil {
		return fmt.Errorf("get secret %s/%s: %w", r.cfg.PlatformNamespace, name, err)
	}
	client := r.typed.CoreV1().Secrets(ns)
	existing, err := client.Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: r.labels(ScopeAgents),
				Annotations: map[string]string{CopiedFromAnnotation: r.cfg.PlatformNamespace + "/" + name}},
			Type: src.Type,
			Data: src.Data,
		}
		if _, err := client.Create(ctx, secret, metav1.CreateOptions{FieldManager: fieldManager}); err != nil {
			return fmt.Errorf("create secret %s/%s: %w", ns, name, err)
		}
		r.log.Info("tenant object created", "namespace", ns, "resource", "secrets", "name", name)
		return nil
	case err != nil:
		return fmt.Errorf("get secret %s/%s: %w", ns, name, err)
	case existing.Labels[ObjectLabel] == "":
		return nil
	case reflect.DeepEqual(existing.Data, src.Data):
		return nil
	}
	existing.Data = src.Data
	if _, err := client.Update(ctx, existing, metav1.UpdateOptions{FieldManager: fieldManager}); err != nil {
		return fmt.Errorf("update secret %s/%s: %w", ns, name, err)
	}
	r.log.Info("tenant object updated", "namespace", ns, "resource", "secrets", "name", name)
	return nil
}

func (r *Reconciler) deleteSecret(ctx context.Context, ns, name string) error {
	client := r.typed.CoreV1().Secrets(ns)
	secret, err := client.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get secret %s/%s: %w", ns, name, err)
	}
	if secret.Labels[ObjectLabel] != ScopeAgents {
		return nil
	}
	if err := client.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete secret %s/%s: %w", ns, name, err)
	}
	r.log.Info("tenant object deleted", "namespace", ns, "resource", "secrets", "name", name)
	return nil
}
