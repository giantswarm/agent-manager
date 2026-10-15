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
// The reconciler is a controller-runtime controller whose work-queue key is a
// tenant namespace: a change to the namespace, to an agent in it, to one of
// its copies or to a platform object they are copied from reconciles the
// namespaces it concerns. It is background work and runs as agent-manager's
// own ServiceAccount, on the replica that holds the leader Lease. It writes
// only objects it labels as its own and never changes or deletes one it did
// not create (a tenant's own ModelConfig, say). It never writes memberships:
// who may use a tenant is declared.
package tenants

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
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
	// LeaderElectionID names the Lease the replicas elect the reconciler's
	// leader with.
	LeaderElectionID = "agent-manager-tenants"
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
	// platform namespace, of the kind NetworkPolicyGVK names (empty: none).
	NetworkPolicies  []string
	NetworkPolicyGVK schema.GroupVersionKind
	// OCIRepository composes the agent chart's source for a namespace;
	// ChartName is its name, which an agent's HelmRelease references.
	OCIRepository func(namespace string) *unstructured.Unstructured
	ChartName     string
	// The served versions of what the reconciler reads and writes.
	HelmReleaseGVK   schema.GroupVersionKind
	OCIRepositoryGVK schema.GroupVersionKind
	AgentGVK         schema.GroupVersionKind
	HarnessGVK       schema.GroupVersionKind
	ModelConfigGVK   schema.GroupVersionKind
	WorkerPoolGVK    schema.GroupVersionKind
	// SyncPeriod is how often every tenant is reconciled without a change:
	// the safety net under the watches.
	SyncPeriod time.Duration
	// LeaderElectionNamespace holds the leader Lease; empty is the
	// namespace of the pod's ServiceAccount.
	LeaderElectionNamespace string
}

// Reconciler keeps the tenant namespaces.
type Reconciler struct {
	mgr    manager.Manager
	client client.Client
	cfg    Config
	log    *slog.Logger
}

// New builds the reconciler and its manager on the ServiceAccount's
// rest.Config. Start runs it.
func New(restCfg *rest.Config, cfg Config, log *slog.Logger) (*Reconciler, error) {
	if log == nil {
		log = slog.Default()
	}
	if cfg.SyncPeriod <= 0 {
		cfg.SyncPeriod = 10 * time.Minute
	}
	if cfg.WorkerPoolReplicas < 0 {
		cfg.WorkerPoolReplicas = 0
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := rbacv1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	r := &Reconciler{cfg: cfg, log: log}
	// One RESTMapper, for the check and the manager.
	httpClient, err := rest.HTTPClientFor(restCfg)
	if err != nil {
		return nil, fmt.Errorf("tenant manager: %w", err)
	}
	mapper, err := apiutil.NewDynamicRESTMapper(restCfg, httpClient)
	if err != nil {
		return nil, fmt.Errorf("tenant manager: %w", err)
	}
	if err := r.checkServed(mapper); err != nil {
		return nil, err
	}
	mgr, err := ctrl.NewManager(restCfg, manager.Options{
		Scheme:         scheme,
		MapperProvider: func(*rest.Config, *http.Client) (meta.RESTMapper, error) { return mapper, nil },
		Logger:         logr.FromSlogHandler(log.Handler()),
		Cache:          r.cacheOptions(),
		// Unstructured reads come from the cache too, under its limits.
		Client: client.Options{Cache: &client.CacheOptions{Unstructured: true}},
		// The HTTP server owns the pod's ports.
		Metrics:                       metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress:        "0",
		LeaderElection:                true,
		LeaderElectionID:              LeaderElectionID,
		LeaderElectionNamespace:       cfg.LeaderElectionNamespace,
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		return nil, fmt.Errorf("tenant manager: %w", err)
	}
	r.mgr, r.client = mgr, mgr.GetClient()
	if err := r.setupWithManager(mgr); err != nil {
		return nil, fmt.Errorf("tenant controller: %w", err)
	}
	return r, nil
}

// checkServed asks API discovery for every kind the controller watches
// beyond the core ones, and names every one that is not served: the manager
// would fail on the first, or minutes later on a watch that never syncs.
func (r *Reconciler) checkServed(mapper meta.RESTMapper) error {
	kinds := []schema.GroupVersionKind{r.cfg.AgentGVK, r.cfg.HarnessGVK, r.cfg.ModelConfigGVK, r.cfg.WorkerPoolGVK, r.cfg.HelmReleaseGVK, r.cfg.OCIRepositoryGVK}
	if !r.cfg.NetworkPolicyGVK.Empty() {
		kinds = append(kinds, r.cfg.NetworkPolicyGVK)
	}
	var missing []string
	for _, gvk := range kinds {
		_, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		switch {
		case meta.IsNoMatchError(err):
			missing = append(missing, gvk.GroupVersion().String()+" "+gvk.Kind)
		case err != nil:
			return fmt.Errorf("discovering %s: %w", gvk, err)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the API server does not serve %s, which the tenant reconciler watches", strings.Join(missing, ", "))
	}
	return nil
}

// cacheOptions limit what the manager caches: the labelled namespaces, the
// reconciler's own copies, the platform namespace's sources, and of the
// Secrets the model key alone.
func (r *Reconciler) cacheOptions() cache.Options {
	copies := hasLabel(ObjectLabel)
	// Sources live in the platform namespace; copies everywhere else.
	sourcesAndCopies := cache.ByObject{Namespaces: map[string]cache.Config{
		r.cfg.PlatformNamespace: {},
		cache.AllNamespaces:     {LabelSelector: copies},
	}}
	keyName := fields.OneTermEqualSelector("metadata.name", r.cfg.ModelSecretName)
	byObject := map[client.Object]cache.ByObject{
		&corev1.Namespace{}:      {Label: hasLabel(OrganizationLabel)},
		&corev1.ServiceAccount{}: {Label: copies},
		&rbacv1.RoleBinding{}:    {Label: copies},
		&corev1.Secret{}: {Namespaces: map[string]cache.Config{
			r.cfg.PlatformNamespace: {FieldSelector: keyName},
			cache.AllNamespaces:     {FieldSelector: keyName, LabelSelector: copies},
		}},
		object(r.cfg.OCIRepositoryGVK): {Label: copies},
		object(r.cfg.HarnessGVK):       sourcesAndCopies,
		object(r.cfg.ModelConfigGVK):   sourcesAndCopies,
		object(r.cfg.WorkerPoolGVK):    sourcesAndCopies,
		// Every HelmRelease is watched; only what tells an agent's apart is
		// kept.
		object(r.cfg.HelmReleaseGVK): {Transform: trimHelmRelease},
	}
	if !r.cfg.NetworkPolicyGVK.Empty() {
		byObject[object(r.cfg.NetworkPolicyGVK)] = sourcesAndCopies
	}
	return cache.Options{SyncPeriod: &r.cfg.SyncPeriod, ByObject: byObject}
}

// hasLabel selects the objects that carry key.
func hasLabel(key string) labels.Selector {
	req, err := labels.NewRequirement(key, selection.Exists, nil)
	if err != nil {
		panic(err) // the keys are constants
	}
	return labels.NewSelector().Add(*req)
}

// trimHelmRelease keeps of a HelmRelease its identity and the fields
// holdsAgents reads.
func trimHelmRelease(obj any) (any, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return obj, nil
	}
	trimmed := &unstructured.Unstructured{Object: map[string]any{"apiVersion": u.GetAPIVersion(), "kind": u.GetKind()}}
	trimmed.SetName(u.GetName())
	trimmed.SetNamespace(u.GetNamespace())
	trimmed.SetUID(u.GetUID())
	trimmed.SetResourceVersion(u.GetResourceVersion())
	trimmed.SetGeneration(u.GetGeneration())
	trimmed.SetDeletionTimestamp(u.GetDeletionTimestamp())
	src, _ := u.Object["spec"].(map[string]any)
	spec := map[string]any{}
	for _, field := range []string{"chartRef", "kubeConfig"} {
		if v, ok := src[field]; ok {
			spec[field] = runtime.DeepCopyJSONValue(v)
		}
	}
	trimmed.Object["spec"] = spec
	return trimmed, nil
}

// setupWithManager registers the controller: one work item per tenant
// namespace.
func (r *Reconciler) setupWithManager(mgr manager.Manager) error {
	ownNamespace := handler.EnqueueRequestsFromMapFunc(r.namespaceOf)
	isCopy := predicate.NewPredicateFuncs(func(o client.Object) bool {
		_, ok := o.GetLabels()[ObjectLabel]
		return ok
	})
	isTenant := predicate.NewPredicateFuncs(func(o client.Object) bool {
		_, ok := o.GetLabels()[OrganizationLabel]
		return ok
	})
	// An agent-chart HelmRelease, before or after a change of its spec.
	agentRelease := predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return r.isAgentChart(e.Object) },
		DeleteFunc:  func(e event.DeleteEvent) bool { return r.isAgentChart(e.Object) },
		GenericFunc: func(e event.GenericEvent) bool { return r.isAgentChart(e.Object) },
		UpdateFunc: func(e event.UpdateEvent) bool {
			return e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() && (r.isAgentChart(e.ObjectOld) || r.isAgentChart(e.ObjectNew))
		},
	}
	// A status write changes neither the spec (the generation) nor the
	// labels: nothing the reconciler copies or decides on.
	specOrLabels := predicate.Or(predicate.GenerationChangedPredicate{}, predicate.LabelChangedPredicate{})
	agent := &metav1.PartialObjectMetadata{}
	agent.SetGroupVersionKind(r.cfg.AgentGVK)

	// A replaced reconciler's controller has the same name in the process.
	skipNameValidation := true
	b := ctrl.NewControllerManagedBy(mgr).
		Named("tenants").
		WithOptions(controller.Options{SkipNameValidation: &skipNameValidation}).
		For(&corev1.Namespace{}, builder.WithPredicates(isTenant)).
		Watches(agent, ownNamespace, builder.OnlyMetadata).
		Watches(object(r.cfg.HelmReleaseGVK), ownNamespace, builder.WithPredicates(agentRelease)).
		Watches(&corev1.ServiceAccount{}, ownNamespace, builder.WithPredicates(isCopy)).
		Watches(&rbacv1.RoleBinding{}, ownNamespace, builder.WithPredicates(isCopy)).
		Watches(object(r.cfg.OCIRepositoryGVK), ownNamespace, builder.WithPredicates(isCopy, specOrLabels)).
		// A copy reconciles its namespace; a source every tenant.
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.copyOrSource))
	kinds := []schema.GroupVersionKind{r.cfg.HarnessGVK, r.cfg.ModelConfigGVK, r.cfg.WorkerPoolGVK}
	if !r.cfg.NetworkPolicyGVK.Empty() {
		kinds = append(kinds, r.cfg.NetworkPolicyGVK)
	}
	for _, gvk := range kinds {
		b = b.Watches(object(gvk), handler.EnqueueRequestsFromMapFunc(r.copyOrSource), builder.WithPredicates(specOrLabels))
	}
	return b.Complete(r)
}

func (r *Reconciler) namespaceOf(_ context.Context, o client.Object) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: o.GetNamespace()}}}
}

// copyOrSource maps a platform-namespace object to every tenant, a labelled
// copy to its namespace, and anything else to nothing.
func (r *Reconciler) copyOrSource(ctx context.Context, o client.Object) []reconcile.Request {
	if o.GetNamespace() == r.cfg.PlatformNamespace {
		var reqs []reconcile.Request
		for _, ns := range r.Namespaces() {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKey{Name: ns}})
		}
		return reqs
	}
	if _, ok := o.GetLabels()[ObjectLabel]; ok {
		return r.namespaceOf(ctx, o)
	}
	return nil
}

func (r *Reconciler) isAgentChart(o client.Object) bool {
	u, ok := o.(*unstructured.Unstructured)
	if !ok {
		return false
	}
	kind, _, _ := unstructured.NestedString(u.Object, "spec", "chartRef", "kind")
	name, _, _ := unstructured.NestedString(u.Object, "spec", "chartRef", "name")
	return kind == "OCIRepository" && name == r.cfg.ChartName
}

// Start runs the manager until ctx ends: the cache on every replica, the
// controller on the leader.
func (r *Reconciler) Start(ctx context.Context) error { return r.mgr.Start(ctx) }

// WaitForCacheSync waits until the tenant namespaces are known, so the first
// request sees them; false when ctx ends first.
func (r *Reconciler) WaitForCacheSync(ctx context.Context) bool {
	if _, err := r.mgr.GetCache().GetInformer(ctx, &corev1.Namespace{}); err != nil {
		return false
	}
	return r.mgr.GetCache().WaitForCacheSync(ctx)
}

// Namespaces are the tenant namespaces, sorted.
func (r *Reconciler) Namespaces() []string {
	list := &corev1.NamespaceList{}
	if err := r.client.List(context.Background(), list, client.HasLabels{OrganizationLabel}); err != nil {
		r.log.Warn("listing the tenant namespaces failed", "error", err)
		return nil
	}
	var found []string
	for i := range list.Items {
		if r.isTenant(&list.Items[i]) {
			found = append(found, list.Items[i].Name)
		}
	}
	slices.Sort(found)
	return found
}

// ModelConfig is the ModelConfig a tenant receives with its first agent.
func (r *Reconciler) ModelConfig() string { return r.cfg.ModelConfigName }

// Reconcile keeps the namespace req names, if it is a tenant.
func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	ns := &corev1.Namespace{}
	if err := r.client.Get(ctx, client.ObjectKey{Name: req.Name}, ns); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if !r.isTenant(ns) {
		return reconcile.Result{}, nil
	}
	return reconcile.Result{}, r.reconcile(ctx, ns.Name)
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
		errs = append(errs, r.ensureExists(ctx, ns, ScopeNamespace, r.cfg.OCIRepository(ns)))
	}
	if sa := r.cfg.FluxServiceAccount; sa != "" {
		errs = append(errs, r.ensureServiceAccount(ctx, ns, sa, ScopeNamespace), r.ensureFluxRoleBinding(ctx, ns))
	}
	if !r.cfg.NetworkPolicyGVK.Empty() {
		for _, name := range r.cfg.NetworkPolicies {
			errs = append(errs, r.copyObject(ctx, r.cfg.NetworkPolicyGVK, name, ns, ScopeNamespace))
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
	agents := &metav1.PartialObjectMetadataList{}
	agents.SetGroupVersionKind(r.cfg.AgentGVK.GroupVersion().WithKind(r.cfg.AgentGVK.Kind + "List"))
	if err := r.client.List(ctx, agents, client.InNamespace(ns)); err != nil {
		return false, fmt.Errorf("list agents in %s: %w", ns, err)
	}
	if len(agents.Items) > 0 {
		return true, nil
	}
	hrs, err := r.list(ctx, r.cfg.HelmReleaseGVK, client.InNamespace(ns))
	if err != nil {
		return false, fmt.Errorf("list helmreleases in %s: %w", ns, err)
	}
	for i := range hrs.Items {
		hr := &hrs.Items[i]
		_, remote, _ := unstructured.NestedMap(hr.Object, "spec", "kubeConfig")
		if r.isAgentChart(hr) && !remote {
			return true, nil
		}
	}
	return false, nil
}

// provision copies the platform Harness, its WorkerPool and the pool's
// ServiceAccount, and the default ModelConfig with its key Secret.
func (r *Reconciler) provision(ctx context.Context, ns string) error {
	var errs []error
	harness, err := r.platformObject(ctx, r.cfg.HarnessGVK, r.cfg.HarnessName)
	switch {
	case err != nil:
		errs = append(errs, err)
	case harness == nil:
		r.log.Warn("the platform Harness is missing; the tenant gets none", "namespace", ns, "harness", r.cfg.HarnessName, "platformNamespace", r.cfg.PlatformNamespace)
	default:
		if pool, _, _ := unstructured.NestedString(harness.Object, "spec", "substrate", "workerPoolRef", "name"); pool != "" {
			errs = append(errs, r.copyWorkerPool(ctx, ns, pool))
		}
		errs = append(errs, r.ensure(ctx, ns, ScopeAgents, copyOf(harness, ns), objectRef(harness)))
	}
	mc, err := r.platformObject(ctx, r.cfg.ModelConfigGVK, r.cfg.ModelConfigName)
	switch {
	case err != nil:
		errs = append(errs, err)
	case mc == nil:
		r.log.Warn("the platform's default ModelConfig is missing; the tenant gets none", "namespace", ns, "modelConfig", r.cfg.ModelConfigName, "platformNamespace", r.cfg.PlatformNamespace)
	default:
		if secret, _, _ := unstructured.NestedString(mc.Object, "spec", "apiKeySecret"); secret != "" {
			errs = append(errs, r.copySecret(ctx, ns, secret))
		}
		errs = append(errs, r.ensure(ctx, ns, ScopeAgents, copyOf(mc, ns), objectRef(mc)))
	}
	return errors.Join(errs...)
}

// copyWorkerPool copies the platform WorkerPool with the tenant's replica
// count and its own placement labels, and the ServiceAccount its workers run
// as.
func (r *Reconciler) copyWorkerPool(ctx context.Context, ns, name string) error {
	src, err := r.platformObject(ctx, r.cfg.WorkerPoolGVK, name)
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
	return errors.Join(append(errs, r.ensure(ctx, ns, ScopeAgents, pool, objectRef(src)))...)
}

// release removes what provision wrote, once the last agent is gone.
func (r *Reconciler) release(ctx context.Context, ns string) error {
	var errs []error
	selector := []client.ListOption{client.InNamespace(ns), client.MatchingLabels{ObjectLabel: ScopeAgents}}
	pools, err := r.list(ctx, r.cfg.WorkerPoolGVK, selector...)
	if err != nil {
		errs = append(errs, fmt.Errorf("list workerpools in %s: %w", ns, err))
	} else {
		for i := range pools.Items {
			pool := &pools.Items[i]
			errs = append(errs, r.delete(ctx, pool))
			if sa, _, _ := unstructured.NestedString(pool.Object, "spec", "template", "serviceAccountName"); sa != "" {
				errs = append(errs, r.deleteServiceAccount(ctx, ns, sa))
			}
		}
	}
	harnesses, err := r.list(ctx, r.cfg.HarnessGVK, selector...)
	if err != nil {
		errs = append(errs, fmt.Errorf("list harnesses in %s: %w", ns, err))
	} else {
		for i := range harnesses.Items {
			errs = append(errs, r.delete(ctx, &harnesses.Items[i]))
		}
	}
	mcs, err := r.list(ctx, r.cfg.ModelConfigGVK, selector...)
	if err != nil {
		errs = append(errs, fmt.Errorf("list modelconfigs in %s: %w", ns, err))
	} else {
		for i := range mcs.Items {
			mc := &mcs.Items[i]
			errs = append(errs, r.delete(ctx, mc))
			if secret, _, _ := unstructured.NestedString(mc.Object, "spec", "apiKeySecret"); secret == r.cfg.ModelSecretName && secret != "" {
				errs = append(errs, r.deleteSecret(ctx, ns, secret))
			}
		}
	}
	return errors.Join(errs...)
}

// ---- unstructured objects -------------------------------------------------

// object is an empty object of gvk.
func object(gvk schema.GroupVersionKind) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	return u
}

func (r *Reconciler) list(ctx context.Context, gvk schema.GroupVersionKind, opts ...client.ListOption) (*unstructured.UnstructuredList, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
	if err := r.client.List(ctx, list, opts...); err != nil {
		return nil, err
	}
	return list, nil
}

// platformObject reads an object of the platform namespace; nil when absent.
func (r *Reconciler) platformObject(ctx context.Context, gvk schema.GroupVersionKind, name string) (*unstructured.Unstructured, error) {
	obj := object(gvk)
	err := r.client.Get(ctx, client.ObjectKey{Namespace: r.cfg.PlatformNamespace, Name: name}, obj)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get %s %s/%s: %w", gvk.Kind, r.cfg.PlatformNamespace, name, err)
	}
	return obj, nil
}

// copyObject copies a platform object of gvk into ns; a missing source is
// logged and skipped.
func (r *Reconciler) copyObject(ctx context.Context, gvk schema.GroupVersionKind, name, ns, scope string) error {
	src, err := r.platformObject(ctx, gvk, name)
	if err != nil {
		return err
	}
	if src == nil {
		r.log.Warn("a platform object to copy is missing", "kind", gvk.Kind, "name", name, "platformNamespace", r.cfg.PlatformNamespace, "namespace", ns)
		return nil
	}
	return r.ensure(ctx, ns, scope, copyOf(src, ns), objectRef(src))
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

// stamp marks obj as the reconciler's own, in ns.
func stamp(obj *unstructured.Unstructured, ns, scope, from string) map[string]string {
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
	return labels
}

// create writes obj. An object of that name that exists already is left as
// it is: the cache holds only the reconciler's own objects, so one it does
// not show is another writer's (or the reconciler's own the cache has not
// seen yet, whose event reconciles the namespace again).
func (r *Reconciler) create(ctx context.Context, obj client.Object, kind string) error {
	err := r.client.Create(ctx, obj, client.FieldOwner(fieldManager))
	switch {
	case apierrors.IsAlreadyExists(err):
		r.log.Debug("leaving an object the reconciler did not write", "namespace", obj.GetNamespace(), "kind", kind, "name", obj.GetName())
		return nil
	case err != nil:
		return fmt.Errorf("create %s %s/%s: %w", kind, obj.GetNamespace(), obj.GetName(), err)
	}
	r.log.Info("tenant object created", "namespace", obj.GetNamespace(), "kind", kind, "name", obj.GetName())
	return nil
}

// ensure creates obj, or brings the reconciler's own copy back to it. An
// object of that name the reconciler did not write is left alone.
func (r *Reconciler) ensure(ctx context.Context, ns, scope string, obj *unstructured.Unstructured, from string) error {
	labels := stamp(obj, ns, scope, from)
	kind := obj.GetKind()
	existing := object(obj.GroupVersionKind())
	err := r.client.Get(ctx, client.ObjectKeyFromObject(obj), existing)
	if apierrors.IsNotFound(err) {
		return r.create(ctx, obj, kind)
	}
	if err != nil {
		return fmt.Errorf("get %s %s/%s: %w", kind, ns, obj.GetName(), err)
	}
	if existing.GetLabels()[ObjectLabel] == "" {
		r.log.Debug("leaving an object the reconciler did not write", "namespace", ns, "kind", kind, "name", obj.GetName())
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
	if err := r.client.Update(ctx, existing, client.FieldOwner(fieldManager)); err != nil {
		return fmt.Errorf("update %s %s/%s: %w", kind, ns, obj.GetName(), err)
	}
	r.log.Info("tenant object updated", "namespace", ns, "kind", kind, "name", obj.GetName())
	return nil
}

// ensureExists creates obj when nothing of its name exists and never
// changes it after: the agent chart's source moves to another range only
// through the migrate command, as in the configured namespaces.
func (r *Reconciler) ensureExists(ctx context.Context, ns, scope string, obj *unstructured.Unstructured) error {
	stamp(obj, ns, scope, "")
	err := r.client.Get(ctx, client.ObjectKeyFromObject(obj), object(obj.GroupVersionKind()))
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get %s %s/%s: %w", obj.GetKind(), ns, obj.GetName(), err)
	}
	return r.create(ctx, obj, obj.GetKind())
}

// delete removes one of the reconciler's own objects.
func (r *Reconciler) delete(ctx context.Context, obj client.Object) error {
	if obj.GetLabels()[ObjectLabel] == "" {
		return nil
	}
	kind := obj.GetObjectKind().GroupVersionKind().Kind
	if err := r.client.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("delete %s %s/%s: %w", kind, obj.GetNamespace(), obj.GetName(), err)
	}
	r.log.Info("tenant object deleted", "namespace", obj.GetNamespace(), "kind", kind, "name", obj.GetName())
	return nil
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
	err := r.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &corev1.ServiceAccount{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get ServiceAccount %s/%s: %w", ns, name, err)
	}
	return r.create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: r.labels(scope)}}, "ServiceAccount")
}

func (r *Reconciler) deleteServiceAccount(ctx context.Context, ns, name string) error {
	sa := &corev1.ServiceAccount{}
	if err := r.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, sa); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get ServiceAccount %s/%s: %w", ns, name, err)
	}
	if sa.Labels[ObjectLabel] != ScopeAgents {
		return nil
	}
	sa.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("ServiceAccount"))
	return r.delete(ctx, sa)
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
	existing := &rbacv1.RoleBinding{}
	err := r.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, existing)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return fmt.Errorf("get RoleBinding %s/%s: %w", ns, name, err)
	case existing.Labels[ObjectLabel] == "":
		return nil
	case existing.RoleRef == want.RoleRef && reflect.DeepEqual(existing.Subjects, want.Subjects):
		return nil
	case existing.RoleRef == want.RoleRef:
		existing.Subjects = want.Subjects
		if err := r.client.Update(ctx, existing, client.FieldOwner(fieldManager)); err != nil {
			return fmt.Errorf("update RoleBinding %s/%s: %w", ns, name, err)
		}
		return nil
	default:
		if err := r.client.Delete(ctx, existing); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("replace RoleBinding %s/%s: %w", ns, name, err)
		}
	}
	return r.create(ctx, want, "RoleBinding")
}

// copySecret copies the default ModelConfig's API key Secret. Only the
// configured name is copied: the reconciler may read and write no other
// Secret.
func (r *Reconciler) copySecret(ctx context.Context, ns, name string) error {
	if name != r.cfg.ModelSecretName {
		return fmt.Errorf("the default ModelConfig's key Secret %q is not the tenant model Secret %q the reconciler may copy; set it to %q", name, r.cfg.ModelSecretName, name)
	}
	src := &corev1.Secret{}
	if err := r.client.Get(ctx, client.ObjectKey{Namespace: r.cfg.PlatformNamespace, Name: name}, src); err != nil {
		if apierrors.IsNotFound(err) {
			r.log.Warn("the default ModelConfig's key Secret is missing", "secret", name, "platformNamespace", r.cfg.PlatformNamespace, "namespace", ns)
			return nil
		}
		return fmt.Errorf("get Secret %s/%s: %w", r.cfg.PlatformNamespace, name, err)
	}
	existing := &corev1.Secret{}
	err := r.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, existing)
	switch {
	case apierrors.IsNotFound(err):
		return r.create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: r.labels(ScopeAgents),
				Annotations: map[string]string{CopiedFromAnnotation: r.cfg.PlatformNamespace + "/" + name}},
			Type: src.Type,
			Data: src.Data,
		}, "Secret")
	case err != nil:
		return fmt.Errorf("get Secret %s/%s: %w", ns, name, err)
	case existing.Labels[ObjectLabel] == "":
		return nil
	case reflect.DeepEqual(existing.Data, src.Data):
		return nil
	}
	existing.Data = src.Data
	if err := r.client.Update(ctx, existing, client.FieldOwner(fieldManager)); err != nil {
		return fmt.Errorf("update Secret %s/%s: %w", ns, name, err)
	}
	r.log.Info("tenant object updated", "namespace", ns, "kind", "Secret", "name", name)
	return nil
}

func (r *Reconciler) deleteSecret(ctx context.Context, ns, name string) error {
	secret := &corev1.Secret{}
	if err := r.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get Secret %s/%s: %w", ns, name, err)
	}
	if secret.Labels[ObjectLabel] != ScopeAgents {
		return nil
	}
	secret.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
	return r.delete(ctx, secret)
}
