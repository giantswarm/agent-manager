package organizations

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	apov1alpha1 "github.com/giantswarm/agent-platform-operator/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/identity"
	"github.com/giantswarm/agent-manager/internal/kube"
)

// Condition is one status condition of an organization.
type Condition struct {
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// Organization is an AgentPlatformOrganization as the tools report it.
type Organization struct {
	Name string `json:"name"`
	// Parent is the namespace the object lives in.
	Parent      string `json:"parent"`
	DisplayName string `json:"displayName,omitempty"`
	// Namespace is the organization namespace, once the operator created it.
	Namespace      string   `json:"namespace,omitempty"`
	Harnesses      []string `json:"harnesses"`
	WorkerReplicas int32    `json:"workerReplicas"`
	SnapshotPrefix string   `json:"snapshotPrefix,omitempty"`
	// Ready, DefaultModelConfigured and Orphaned are the operator's
	// conditions, absent until it set them.
	Ready                  *Condition `json:"ready,omitempty"`
	DefaultModelConfigured *Condition `json:"defaultModelConfigured,omitempty"`
	Orphaned               *Condition `json:"orphaned,omitempty"`
	// Deleting: the object is deleted and the operator erases it;
	// ErasureStep names the step it is on.
	Deleting    bool   `json:"deleting,omitempty"`
	ErasureStep string `json:"erasureStep,omitempty"`
	// GitOps names the Flux Kustomization that applies the object: it is
	// changed in git, not live.
	GitOps string `json:"gitops,omitempty"`
}

func organizationOf(u *unstructured.Unstructured) (*apov1alpha1.AgentPlatformOrganization, error) {
	org := &apov1alpha1.AgentPlatformOrganization{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, org); err != nil {
		return nil, fmt.Errorf("read AgentPlatformOrganization %s/%s: %w", u.GetNamespace(), u.GetName(), err)
	}
	return org, nil
}

func summary(org *apov1alpha1.AgentPlatformOrganization) Organization {
	out := Organization{
		Name: org.Name, Parent: org.Namespace, DisplayName: org.Annotations[apov1alpha1.DisplayNameAnnotation],
		Namespace: org.Status.Namespace, Harnesses: slices.Clone(org.Spec.Harnesses), WorkerReplicas: org.Spec.Workers.Replicas,
		SnapshotPrefix: org.Status.SnapshotPrefix, Deleting: org.DeletionTimestamp != nil, ErasureStep: org.Status.ErasureStep,
		GitOps: org.Labels[agents.KustomizationNameLabel],
	}
	if out.Harnesses == nil {
		out.Harnesses = []string{}
	}
	cond := func(t string) *Condition {
		c := meta.FindStatusCondition(org.Status.Conditions, t)
		if c == nil {
			return nil
		}
		return &Condition{Status: string(c.Status), Reason: c.Reason, Message: c.Message}
	}
	out.Ready = cond(apov1alpha1.ConditionReady)
	out.DefaultModelConfigured = cond(apov1alpha1.ConditionDefaultModelConfigured)
	out.Orphaned = cond(apov1alpha1.ConditionOrphaned)
	return out
}

func ready(org *apov1alpha1.AgentPlatformOrganization) bool {
	return org.Status.Namespace != "" && meta.IsStatusConditionTrue(org.Status.Conditions, apov1alpha1.ConditionReady)
}

// ---- reads ------------------------------------------------------------------------

// List lists the AgentPlatformOrganizations of parent, or of every namespace
// when parent is empty, as the caller.
func (s *Service) List(ctx context.Context, parent string) ([]Organization, error) {
	c, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	list, err := c.Dynamic().Resource(GVR).Namespace(parent).List(ctx, metav1.ListOptions{})
	if err != nil {
		where := "every namespace"
		way := fmt.Sprintf("listing in every namespace needs %s bound cluster-wide; name the parent namespace (namespace) where yours is bound", RoleOrganizationsAdmin)
		if parent != "" {
			where, way = parent, wayOrganizationsAdmin(parent)
		}
		return nil, kubeErr(err, "list AgentPlatformOrganizations in "+where, way)
	}
	out := make([]Organization, 0, len(list.Items))
	for i := range list.Items {
		org, err := organizationOf(&list.Items[i])
		if err != nil {
			return nil, err
		}
		out = append(out, summary(org))
	}
	slices.SortFunc(out, func(a, b Organization) int {
		return strings.Compare(a.Parent+"/"+a.Name, b.Parent+"/"+b.Name)
	})
	return out, nil
}

// Details is get_organization's answer: the organization and what a delete
// takes along.
type Details struct {
	Organization
	Erases *Erasure `json:"erases,omitempty"`
}

// Erasure is what deleting an organization erases, as the caller can read
// it in the organization namespace.
type Erasure struct {
	Namespace      string   `json:"namespace"`
	Agents         []string `json:"agents"`
	HelmReleases   []string `json:"helmReleases"`
	Models         []string `json:"models"`
	Connectors     []string `json:"connectors"`
	EgressPolicies []string `json:"egressPolicies"`
	// Credentials are the credential Secrets the models, connectors and
	// egress policies name, by name only.
	Credentials []string `json:"credentials"`
	// Members are the RoleBindings to the organization's roles
	// (agent-platform:admin, editor and user).
	Members        []string `json:"members"`
	SnapshotPrefix string   `json:"snapshotPrefix,omitempty"`
	// Kept is what a delete does not erase.
	Kept string `json:"kept"`
	// Unreadable names what the caller may not read there, with why.
	Unreadable []string `json:"unreadable,omitempty"`
}

// Get reads one organization and what deleting it erases.
func (s *Service) Get(ctx context.Context, parent, name string) (*Details, error) {
	c, _, org, err := s.get(ctx, parent, name)
	if err != nil {
		return nil, err
	}
	out := &Details{Organization: summary(org)}
	if org.Status.Namespace != "" {
		out.Erases = s.erasure(ctx, c.Dynamic(), org)
	}
	return out, nil
}

// get reads an organization as the caller: the object as the API server
// sent it, which a write changes in place so that fields this version's
// types do not know survive, and its typed view.
func (s *Service) get(ctx context.Context, parent, name string) (kube.Client, *unstructured.Unstructured, *apov1alpha1.AgentPlatformOrganization, error) {
	if parent == "" || name == "" {
		return nil, nil, nil, invalidf("namespace (the parent namespace) and name are required")
	}
	c, err := s.caller(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	u, err := c.Dynamic().Resource(GVR).Namespace(parent).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, nil, nil, kubeErr(err, fmt.Sprintf("get AgentPlatformOrganization %s/%s", parent, name), wayOrganizationsAdmin(parent))
	}
	org, err := organizationOf(u)
	return c, u, org, err
}

// erasure reads, as the caller, what a delete of org erases. An
// organizations-admin reads the organization's contents only once bound in
// it, so what it may not read is named, not failed.
func (s *Service) erasure(ctx context.Context, dyn dynamic.Interface, org *apov1alpha1.AgentPlatformOrganization) *Erasure {
	ns := org.Status.Namespace
	out := &Erasure{Namespace: ns, SnapshotPrefix: org.Status.SnapshotPrefix,
		Kept: "session tombstones stay in the agent runtime; everything else of the organization namespace is erased, worker snapshots and connector sign-ins included"}
	credentials := map[string]bool{}
	read := func(what string, gvr func() ([]unstructured.Unstructured, error), into *[]string, each func(*unstructured.Unstructured)) {
		items, err := gvr()
		*into = []string{}
		if err != nil {
			out.Unreadable = append(out.Unreadable, fmt.Sprintf("%s: %v (an organizations-admin reads them once it binds itself in the organization, which its members see)", what, err))
			return
		}
		for i := range items {
			*into = append(*into, items[i].GetName())
			if each != nil {
				each(&items[i])
			}
		}
	}
	lister := func(gvr schema.GroupVersionResource) func() ([]unstructured.Unstructured, error) {
		return func() ([]unstructured.Unstructured, error) {
			list, err := dyn.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{})
			if err != nil {
				return nil, err
			}
			return list.Items, nil
		}
	}
	hrGV, err := schema.ParseGroupVersion(s.cfg.HelmReleaseAPIVersion)
	if err != nil {
		hrGV = schema.GroupVersion{Group: "helm.toolkit.fluxcd.io", Version: "v2"}
	}
	read("agents", lister(s.kagentGVR("agents")), &out.Agents, nil)
	read("HelmReleases", lister(hrGV.WithResource("helmreleases")), &out.HelmReleases, nil)
	read("models", lister(s.kagentGVR("modelconfigs")), &out.Models, func(u *unstructured.Unstructured) {
		if secret, _, _ := unstructured.NestedString(u.Object, "spec", "apiKeySecret"); secret != "" {
			credentials[secret] = true
		}
	})
	read("connectors", lister(s.kagentGVR("remotemcpservers")), &out.Connectors, func(u *unstructured.Unstructured) {
		for _, h := range connectorCredentials(u) {
			credentials[h.Secret] = true
		}
	})
	read("egress policies", lister(s.kagentGVR("egresspolicies")), &out.EgressPolicies, func(u *unstructured.Unstructured) {
		for _, d := range egressDestinations(u) {
			if d.CredentialSecret != "" {
				credentials[d.CredentialSecret] = true
			}
		}
	})
	var bindings []string
	out.Members = []string{}
	read("members", lister(rbacGVR), &bindings, func(u *unstructured.Unstructured) {
		kind, _, _ := unstructured.NestedString(u.Object, "roleRef", "kind")
		role, _, _ := unstructured.NestedString(u.Object, "roleRef", "name")
		if kind == "ClusterRole" && (role == RoleAdmin || role == RoleEditor || role == RoleUser) {
			out.Members = append(out.Members, u.GetName())
		}
	})
	out.Credentials = make([]string, 0, len(credentials))
	for name := range credentials {
		out.Credentials = append(out.Credentials, name)
	}
	slices.Sort(out.Credentials)
	return out
}

// ---- create -----------------------------------------------------------------------

// CreateRequest is create_organization's arguments.
type CreateRequest struct {
	// Parent is the namespace the object lives in.
	Parent      string
	Name        string
	DisplayName string
	Harnesses   []string
	// WorkerReplicas is spec.workers.replicas; nil leaves the CRD's default.
	WorkerReplicas *int32
	// AdminGroups and AdminUsers are the first admins, bound by the
	// RoleBinding organization-admins to agent-platform:admin. A group
	// first: membership then changes in the identity provider.
	AdminGroups []string
	AdminUsers  []string
	DryRun      bool
	// Mode is apply (default) or commit.
	Mode string
}

// CreateResult is create_organization's answer.
type CreateResult struct {
	Organization Organization `json:"organization"`
	DryRun       bool         `json:"dryRun,omitempty"`
	// Created: the object was written by this call; false when an earlier
	// call wrote it and this one finishes its first admins.
	Created bool `json:"created"`
	// FirstAdmins is the RoleBinding organization-admins, written once the
	// organization is Ready.
	FirstAdmins *Binding `json:"firstAdmins,omitempty"`
	// Pending says what is left and how to finish it.
	Pending string `json:"pending,omitempty"`
	// Note says what this call did not do.
	Note        string `json:"note,omitempty"`
	RequestedBy string `json:"requestedBy,omitempty"`
	// Commit is the pull request of a create in mode commit.
	Commit *CommitResult `json:"commit,omitempty"`
}

// Binding is a RoleBinding as the tools report it.
type Binding struct {
	Name     string    `json:"name"`
	Role     string    `json:"role"`
	Subjects []Subject `json:"subjects"`
	Written  bool      `json:"written"`
}

// Subject is one RoleBinding subject.
type Subject struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

// Create writes an AgentPlatformOrganization as the caller, waits for the
// operator to make it Ready, then writes its first admins as the caller.
// Before any write it asks, as the caller, whether the first admins can be
// bound (create RoleBindings and bind agent-platform:admin in the parent,
// which the operator mirrors into the organization namespace) and dry-runs
// the object, so a refusal comes before the object exists. A create of an
// organization that exists without its first admins finishes them.
func (s *Service) Create(ctx context.Context, req CreateRequest) (*CreateResult, error) {
	if req.Parent == "" || req.Name == "" {
		return nil, invalidf("namespace (the parent namespace) and name are required")
	}
	subjects, err := subjectsOf(req.AdminGroups, req.AdminUsers)
	if err != nil {
		return nil, err
	}
	if len(subjects) == 0 {
		return nil, invalidf("the first admins are required: adminGroups (an identity provider group, preferred) or adminUsers; they are bound to %s in the organization namespace as %s", RoleAdmin, FirstAdminsBinding)
	}
	mode, err := s.mode(req.Mode)
	if err != nil {
		return nil, err
	}
	c, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	way := wayOrganizationsAdmin(req.Parent)
	if err := requireAccess(ctx, c, way,
		access{namespace: req.Parent, verb: "create", group: GVR.Group, resource: GVR.Resource},
		access{namespace: req.Parent, verb: "create", group: rbacGVR.Group, resource: rbacGVR.Resource},
		access{namespace: req.Parent, verb: "bind", group: rbacGVR.Group, resource: "clusterroles", name: RoleAdmin},
	); err != nil {
		return nil, err
	}
	res := &CreateResult{DryRun: req.DryRun, RequestedBy: identity.Caller(ctx)}
	if mode == agents.ModeCommit {
		return s.createCommit(ctx, c, req, subjects, res)
	}
	orgs := c.Dynamic().Resource(GVR).Namespace(req.Parent)
	what := fmt.Sprintf("AgentPlatformOrganization %s/%s", req.Parent, req.Name)

	var org *apov1alpha1.AgentPlatformOrganization
	existing, err := orgs.Get(ctx, req.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		obj := s.organizationObject(req)
		w := write{gvr: GVR, kind: orgKind, op: opCreate, obj: obj, ns: req.Parent, name: req.Name, what: what, reason: way}
		if err := apply(ctx, c.Dynamic(), []write{w}, req.DryRun); err != nil {
			return nil, err
		}
		if req.DryRun {
			planned, err := organizationOf(unstructuredOf(obj))
			if err != nil {
				return nil, err
			}
			res.Organization = summary(planned)
			res.Created = true
			res.FirstAdmins = &Binding{Name: FirstAdminsBinding, Role: RoleAdmin, Subjects: subjects}
			res.Pending = "dry run: nothing was written; the first admins are bound once the organization is Ready"
			return res, nil
		}
		res.Created = true
		s.log.Info("organization created", identity.LogAttr(ctx), "parent", req.Parent, "name", req.Name)
	case err != nil:
		return nil, kubeErr(err, "get "+what, way)
	default:
		org, err = organizationOf(existing)
		if err != nil {
			return nil, err
		}
		if err := refuseLive(org); err != nil {
			return nil, err
		}
		if org.DeletionTimestamp != nil {
			return nil, fmt.Errorf("%w: %s is being erased (%s); create it again once get_organization answers not_found", agents.ErrConflict, what, orDefault(org.Status.ErasureStep, "starting"))
		}
		if done, err := s.hasFirstAdmins(ctx, c, org); err != nil || done {
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("%w: %s exists with its first admins; update_organization changes it, add_organization_member adds admins", agents.ErrConflict, what)
		}
		if req.DryRun {
			res.Organization = summary(org)
			res.FirstAdmins = &Binding{Name: FirstAdminsBinding, Role: RoleAdmin, Subjects: subjects}
			res.Pending = "dry run: the organization exists without its first admins; a call without dryRun binds them once it is Ready"
			return res, nil
		}
	}

	org, err = s.waitReady(ctx, c, req.Parent, req.Name)
	if err != nil {
		return nil, err
	}
	res.Organization = summary(org)
	if !ready(org) {
		res.Pending = fmt.Sprintf("the organization is not Ready after %s (%s), so its first admins are not bound yet; call create_organization again with the same arguments to bind them once it is", s.cfg.ReadyTimeout, describeReady(res.Organization.Ready))
		return res, nil
	}
	// The parent's check holds for an organizations-admin, whose bindings the
	// operator mirrors into the new namespace. Rights there from another
	// source (a Giant Swarm Organization admin's cluster-admin in its
	// org-<name>) are not mirrored: ask again where the binding goes, so the
	// answer names who can finish it instead of a Forbidden write.
	if err := requireAccess(ctx, c, wayFirstAdmins(org),
		access{namespace: org.Status.Namespace, verb: "create", group: rbacGVR.Group, resource: rbacGVR.Resource},
		access{namespace: org.Status.Namespace, verb: "bind", group: rbacGVR.Group, resource: "clusterroles", name: RoleAdmin},
	); err != nil {
		res.Pending = fmt.Sprintf("the organization %s exists and is Ready, but its first admins are not bound: %v", what, err)
		return res, nil
	}
	binding, err := s.bindFirstAdmins(ctx, c, org, subjects)
	if err != nil {
		return nil, fmt.Errorf("%w (the organization %s exists; call create_organization again with the same arguments to bind its first admins)", err, what)
	}
	res.FirstAdmins = binding
	if !res.Created {
		res.Note = "the organization existed, so only its first admins were bound; its display name, harnesses and worker replicas are as they were (update_organization changes them)"
	}
	return res, nil
}

func describeReady(c *Condition) string {
	if c == nil {
		return "the operator has not reported Ready yet"
	}
	out := "Ready=" + c.Status
	if c.Reason != "" {
		out += " " + c.Reason
	}
	if c.Message != "" {
		out += ": " + c.Message
	}
	return out
}

func (s *Service) organizationObject(req CreateRequest) map[string]any {
	meta := map[string]any{"name": req.Name, "namespace": req.Parent}
	if req.DisplayName != "" {
		meta["annotations"] = map[string]any{apov1alpha1.DisplayNameAnnotation: req.DisplayName}
	}
	spec := map[string]any{}
	if len(req.Harnesses) > 0 {
		h := make([]any, len(req.Harnesses))
		for i, name := range req.Harnesses {
			h[i] = name
		}
		spec["harnesses"] = h
	}
	if req.WorkerReplicas != nil {
		spec["workers"] = map[string]any{"replicas": int64(*req.WorkerReplicas)}
	}
	return map[string]any{
		"apiVersion": apov1alpha1.GroupVersion.String(), "kind": "AgentPlatformOrganization",
		fieldMetadata: meta, "spec": spec,
	}
}

// waitReady reads the organization until it is Ready or the wait ends,
// telling the caller it is still working.
func (s *Service) waitReady(ctx context.Context, c kube.Client, parent, name string) (*apov1alpha1.AgentPlatformOrganization, error) {
	progress := agents.ProgressFrom(ctx)
	deadline := time.Now().Add(s.cfg.ReadyTimeout)
	for {
		u, err := c.Dynamic().Resource(GVR).Namespace(parent).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, kubeErr(err, fmt.Sprintf("get AgentPlatformOrganization %s/%s", parent, name), wayOrganizationsAdmin(parent))
		}
		org, err := organizationOf(u)
		if err != nil {
			return nil, err
		}
		if ready(org) || !time.Now().Before(deadline) {
			return org, nil
		}
		progress(fmt.Sprintf("waiting for organization %s/%s to be Ready (%s)", parent, name, describeReady(summary(org).Ready)))
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(s.cfg.ReadyPoll):
		}
	}
}

func (s *Service) hasFirstAdmins(ctx context.Context, c kube.Client, org *apov1alpha1.AgentPlatformOrganization) (bool, error) {
	if org.Status.Namespace == "" {
		return false, nil
	}
	_, err := c.Dynamic().Resource(rbacGVR).Namespace(org.Status.Namespace).Get(ctx, FirstAdminsBinding, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, kubeErr(err, fmt.Sprintf("get RoleBinding %s/%s", org.Status.Namespace, FirstAdminsBinding), wayOrganizationsAdmin(org.Namespace))
	}
	return true, nil
}

// bindFirstAdmins writes organization-admins as the caller, dry run first.
func (s *Service) bindFirstAdmins(ctx context.Context, c kube.Client, org *apov1alpha1.AgentPlatformOrganization, subjects []Subject) (*Binding, error) {
	ns := org.Status.Namespace
	obj := roleBinding(ns, FirstAdminsBinding, RoleAdmin, subjects)
	w := write{gvr: rbacGVR, kind: kindRoleBinding, op: opCreate, obj: obj, ns: ns, name: FirstAdminsBinding,
		what: fmt.Sprintf("RoleBinding %s/%s", ns, FirstAdminsBinding), reason: wayOrganizationsAdmin(org.Namespace)}
	if err := apply(ctx, c.Dynamic(), []write{w}, false); err != nil {
		return nil, err
	}
	s.log.Info("organization first admins bound", identity.LogAttr(ctx), "namespace", ns, "subjects", len(subjects))
	return &Binding{Name: FirstAdminsBinding, Role: RoleAdmin, Subjects: subjects, Written: true}, nil
}

// ---- update -----------------------------------------------------------------------

// UpdateRequest is update_organization's arguments; nil leaves a field.
type UpdateRequest struct {
	Parent         string
	Name           string
	DisplayName    *string
	Harnesses      *[]string
	WorkerReplicas *int32
	DryRun         bool
	// Mode is apply (default) or commit.
	Mode string
}

// UpdateResult is update_organization's answer.
type UpdateResult struct {
	Before      Organization `json:"before"`
	After       Organization `json:"after"`
	Changed     []string     `json:"changed"`
	DryRun      bool         `json:"dryRun,omitempty"`
	RequestedBy string       `json:"requestedBy,omitempty"`
	// Commit is the pull request of a change in mode commit.
	Commit *CommitResult `json:"commit,omitempty"`
}

// Update changes an organization as the caller, dry run first: live in
// apply mode, in its file in commit mode.
func (s *Service) Update(ctx context.Context, req UpdateRequest) (*UpdateResult, error) {
	mode, err := s.mode(req.Mode)
	if err != nil {
		return nil, err
	}
	c, u, org, err := s.get(ctx, req.Parent, req.Name)
	if err != nil {
		return nil, err
	}
	if mode == agents.ModeCommit {
		return s.updateCommit(ctx, c, u, org, req)
	}
	if err := refuseLive(org); err != nil {
		return nil, err
	}
	res := &UpdateResult{Before: summary(org), DryRun: req.DryRun, RequestedBy: identity.Caller(ctx)}
	next := u.DeepCopy()
	if res.Changed, err = changeOrganization(next, req); err != nil {
		return nil, err
	}
	after, err := organizationOf(next)
	if err != nil {
		return nil, err
	}
	res.After = summary(after)
	if len(res.Changed) == 0 {
		return res, nil
	}
	w := write{gvr: GVR, kind: orgKind, op: opUpdate, obj: next.Object, ns: req.Parent, name: req.Name,
		what: fmt.Sprintf("AgentPlatformOrganization %s/%s", req.Parent, req.Name), reason: wayOrganizationsAdmin(req.Parent)}
	if err := apply(ctx, c.Dynamic(), []write{w}, req.DryRun); err != nil {
		return nil, err
	}
	if !req.DryRun {
		s.log.Info("organization updated", identity.LogAttr(ctx), "parent", req.Parent, "name", req.Name, "changed", res.Changed)
	}
	return res, nil
}

// changeOrganization applies an update's fields to next, which holds an
// organization (the live object, or its file in commit mode), and names the
// fields that changed against what next held.
func changeOrganization(next *unstructured.Unstructured, req UpdateRequest) ([]string, error) {
	cur, err := organizationOf(next)
	if err != nil {
		return nil, err
	}
	changed := []string{}
	if req.DisplayName != nil && *req.DisplayName != cur.Annotations[apov1alpha1.DisplayNameAnnotation] {
		annotations := next.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		if *req.DisplayName == "" {
			delete(annotations, apov1alpha1.DisplayNameAnnotation)
		} else {
			annotations[apov1alpha1.DisplayNameAnnotation] = *req.DisplayName
		}
		next.SetAnnotations(annotations)
		changed = append(changed, "displayName")
	}
	if req.Harnesses != nil && !slices.Equal(*req.Harnesses, cur.Spec.Harnesses) {
		if err := unstructured.SetNestedStringSlice(next.Object, *req.Harnesses, "spec", "harnesses"); err != nil {
			return nil, err
		}
		changed = append(changed, "harnesses")
	}
	if req.WorkerReplicas != nil && *req.WorkerReplicas != cur.Spec.Workers.Replicas {
		if err := unstructured.SetNestedField(next.Object, int64(*req.WorkerReplicas), "spec", "workers", "replicas"); err != nil {
			return nil, err
		}
		changed = append(changed, "workerReplicas")
	}
	return changed, nil
}

// refuseLive refuses a live write to an organization Flux applies from git:
// Flux would undo it.
func refuseLive(org *apov1alpha1.AgentPlatformOrganization) error {
	if ks := org.Labels[agents.KustomizationNameLabel]; ks != "" {
		return fmt.Errorf("%w: AgentPlatformOrganization %s/%s is applied from git by the Flux Kustomization %s, which would revert a live change: use mode commit to change it with a pull request", agents.ErrGitOpsOwned, org.Namespace, org.Name, ks)
	}
	return nil
}

// ---- delete -----------------------------------------------------------------------

// DeleteRequest is delete_organization's arguments.
type DeleteRequest struct {
	Parent string
	Name   string
	// Confirm must be "<parent>/<name>" for the delete to happen; without it
	// the answer is what the delete would erase.
	Confirm string
	DryRun  bool
	// Mode is apply (default) or commit.
	Mode string
}

// DeleteResult is delete_organization's answer.
type DeleteResult struct {
	Organization Organization `json:"organization"`
	Erases       *Erasure     `json:"erases,omitempty"`
	Deleted      bool         `json:"deleted"`
	DryRun       bool         `json:"dryRun,omitempty"`
	Next         string       `json:"next,omitempty"`
	RequestedBy  string       `json:"requestedBy,omitempty"`
	// Commit is the pull request that removes the organization from git,
	// the first step of a delete in mode commit.
	Commit *CommitResult `json:"commit,omitempty"`
}

// Delete deletes an organization as the caller, which erases it: the
// operator's finalizer removes its namespace, sessions and snapshots. Without
// the typed confirmation it only answers what would be erased. It marks the
// object with Flux's prune: enabled first, the one delete intent the
// admission policy accepts, after an access review for the delete and a dry
// run of the mark.
func (s *Service) Delete(ctx context.Context, req DeleteRequest) (*DeleteResult, error) {
	mode, err := s.mode(req.Mode)
	if err != nil {
		return nil, err
	}
	c, u, org, err := s.get(ctx, req.Parent, req.Name)
	if err != nil {
		return nil, err
	}
	if mode == agents.ModeApply {
		if err := refuseLive(org); err != nil {
			return nil, err
		}
	}
	confirm := req.Parent + "/" + req.Name
	res := &DeleteResult{Organization: summary(org), DryRun: req.DryRun, RequestedBy: identity.Caller(ctx)}
	if org.Status.Namespace != "" {
		res.Erases = s.erasure(ctx, c.Dynamic(), org)
	}
	if org.DeletionTimestamp != nil {
		res.Next = "the organization is being erased already (" + orDefault(org.Status.ErasureStep, "starting") + ")"
		return res, nil
	}
	way := wayOrganizationsAdmin(req.Parent)
	if err := requireAccess(ctx, c, way, access{namespace: req.Parent, verb: "delete", group: GVR.Group, resource: GVR.Resource, name: req.Name}); err != nil {
		return nil, err
	}
	if mode == agents.ModeCommit {
		// The first step opens the pull request; the erase follows only
		// once agent-manager has seen it merged and the file gone.
		if answered, err := s.deleteCommit(ctx, c, u, org, req, res); err != nil || answered {
			if err != nil {
				return nil, err
			}
			return res, nil
		}
	}
	if req.Confirm != confirm {
		res.Next = fmt.Sprintf("nothing was deleted: show the person what is erased and, once they typed it, call delete_organization again with confirm %q", confirm)
		return res, nil
	}
	what := "AgentPlatformOrganization " + confirm
	orgs := c.Dynamic().Resource(GVR).Namespace(req.Parent)
	// The delete intent comes first: the admission policy refuses a delete,
	// a dry run of it too, of an object without it. So the delete is
	// dry-run once the mark is written, and the mark goes again when that
	// dry run is refused.
	marked := false
	if org.Annotations[PruneAnnotation] != PruneEnabled {
		next := u.DeepCopy()
		annotations := next.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[PruneAnnotation] = PruneEnabled
		next.SetAnnotations(annotations)
		w := write{gvr: GVR, kind: orgKind, op: opUpdate, obj: next.Object, ns: req.Parent, name: req.Name, what: what + " (" + PruneAnnotation + ": " + PruneEnabled + ")", reason: way}
		if err := apply(ctx, c.Dynamic(), []write{w}, req.DryRun); err != nil {
			return nil, err
		}
		marked = !req.DryRun
	}
	if req.DryRun {
		res.Next = "dry run: nothing was written; the delete itself is dry-run once the delete intent is on the object, which a dry run does not write"
		return res, nil
	}
	if err := orgs.Delete(ctx, req.Name, metav1.DeleteOptions{DryRun: dryRunAll}); err != nil {
		err = kubeErr(err, "dry run of delete "+what, way)
		if marked {
			if unmarkErr := s.unmarkPrune(ctx, c, req.Parent, req.Name); unmarkErr != nil {
				return nil, fmt.Errorf("%w; the object keeps %s: %s, remove it: %v", err, PruneAnnotation, PruneEnabled, unmarkErr)
			}
		}
		return nil, err
	}
	if err := orgs.Delete(ctx, req.Name, metav1.DeleteOptions{}); err != nil {
		err = kubeErr(err, "delete "+what, way)
		if marked {
			if unmarkErr := s.unmarkPrune(ctx, c, req.Parent, req.Name); unmarkErr != nil {
				return nil, fmt.Errorf("%w; the object keeps %s: %s, remove it: %v", err, PruneAnnotation, PruneEnabled, unmarkErr)
			}
		}
		return nil, err
	}
	res.Deleted = true
	res.Next = "the operator erases the organization now; get_organization reports its erasure step until it is gone"
	s.log.Info("organization deleted", identity.LogAttr(ctx), "parent", req.Parent, "name", req.Name)
	return res, nil
}

// unmarkPrune takes the delete intent off an organization a delete did not
// delete.
func (s *Service) unmarkPrune(ctx context.Context, c kube.Client, parent, name string) error {
	orgs := c.Dynamic().Resource(GVR).Namespace(parent)
	u, err := orgs.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	annotations := u.GetAnnotations()
	delete(annotations, PruneAnnotation)
	u.SetAnnotations(annotations)
	_, err = orgs.Update(ctx, u, metav1.UpdateOptions{FieldManager: agents.FieldManager})
	return err
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// ---- helpers ----------------------------------------------------------------------

func unstructuredOf(obj map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: obj}
}

// subjectsOf makes RoleBinding subjects, the groups first.
func subjectsOf(groups, users []string) ([]Subject, error) {
	out := make([]Subject, 0, len(groups)+len(users))
	for _, g := range groups {
		if strings.TrimSpace(g) == "" {
			return nil, invalidf("an empty group name")
		}
		out = append(out, Subject{Kind: "Group", Name: g})
	}
	for _, u := range users {
		if strings.TrimSpace(u) == "" {
			return nil, invalidf("an empty user name")
		}
		out = append(out, Subject{Kind: "User", Name: u})
	}
	return out, nil
}

func roleBinding(ns, name, role string, subjects []Subject) map[string]any {
	subs := make([]any, 0, len(subjects))
	for _, s := range subjects {
		subs = append(subs, subjectObject(s))
	}
	return map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
		fieldMetadata: map[string]any{"name": name, "namespace": ns, "labels": map[string]any{agents.ManagedByLabel: agents.ManagedByValue}},
		"roleRef":     map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": role},
		"subjects":    subs,
	}
}

func subjectObject(s Subject) map[string]any {
	out := map[string]any{"kind": s.Kind, "name": s.Name}
	if s.Kind == "ServiceAccount" {
		out["namespace"] = s.Namespace
	} else {
		out["apiGroup"] = "rbac.authorization.k8s.io"
	}
	return out
}
