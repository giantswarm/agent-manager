package organizations

import (
	"context"
	"fmt"
	"slices"
	"strings"

	apov1alpha1 "github.com/giantswarm/agent-platform-operator/api/v1alpha1"
	orgv1alpha1 "github.com/giantswarm/organization-operator/api/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/identity"
	"github.com/giantswarm/agent-manager/internal/kagent"
	apiv1alpha1 "github.com/giantswarm/agent-manager/internal/kagent/gen/kagent/api/v1alpha1"
	"github.com/giantswarm/agent-manager/internal/kube"
)

// Condition is one status condition of an organization.
type Condition struct {
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// Organization is an Organization with the Agent Platform enabled, as the
// tools report it.
type Organization struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName,omitempty"`
	// Namespace is the organization namespace, once the operator reported
	// it (status.agentPlatform.namespace).
	Namespace string `json:"namespace,omitempty"`
	// Admins are spec.admins: the Users and Groups the organization is
	// handed over to, which agent-platform-operator binds to
	// agent-platform:admin in its namespace.
	Admins         []Subject `json:"admins"`
	Harnesses      []string  `json:"harnesses"`
	WorkerReplicas int32     `json:"workerReplicas"`
	SnapshotPrefix string    `json:"snapshotPrefix,omitempty"`
	// Ready, DefaultModelConfigured and Orphaned are the operator's
	// conditions, absent until it set them.
	Ready                  *Condition `json:"ready,omitempty"`
	DefaultModelConfigured *Condition `json:"defaultModelConfigured,omitempty"`
	Orphaned               *Condition `json:"orphaned,omitempty"`
	// LastAppliedRevision is the operator's status.lastAppliedRevision: the
	// last source revision Flux applied the organization from, kept while it
	// is Orphaned.
	LastAppliedRevision string `json:"lastAppliedRevision,omitempty"`
	// LastAppliedRevisionURL is that revision's commit on GitHub, when mode
	// commit names the installation repository and the revision is a git
	// commit.
	LastAppliedRevisionURL string `json:"lastAppliedRevisionUrl,omitempty"`
	// LastAppliedFile is the repository path mode commit keeps the
	// organization's file at, and LastAppliedFileURL that file's GitHub page
	// at LastAppliedRevision: where a removed file is restored from. Both are
	// set with LastAppliedRevisionURL.
	LastAppliedFile    string `json:"lastAppliedFile,omitempty"`
	LastAppliedFileURL string `json:"lastAppliedFileUrl,omitempty"`
	// Deleting: the object is deleted and the operator erases it;
	// ErasureStep names the step it is on.
	Deleting    bool   `json:"deleting,omitempty"`
	ErasureStep string `json:"erasureStep,omitempty"`
	// GitOps names the Flux Kustomization that applies the object: it is
	// changed in git, not live.
	GitOps string `json:"gitops,omitempty"`
}

func organizationOf(u *unstructured.Unstructured) (*orgv1alpha1.Organization, error) {
	org := &orgv1alpha1.Organization{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, org); err != nil {
		return nil, fmt.Errorf("read Organization %s: %w", u.GetName(), err)
	}
	return org, nil
}

// agentPlatformEnabled reports whether org has the Agent Platform switched
// on (spec.agentPlatform.enabled): the Organizations the tools work on.
func agentPlatformEnabled(org *orgv1alpha1.Organization) bool {
	return org.Spec.AgentPlatform != nil && org.Spec.AgentPlatform.Enabled
}

// agentPlatformOf is org's spec.agentPlatform and status.agentPlatform,
// empty where they are not set.
func agentPlatformOf(org *orgv1alpha1.Organization) (orgv1alpha1.AgentPlatformSpec, orgv1alpha1.AgentPlatformStatus) {
	var spec orgv1alpha1.AgentPlatformSpec
	var status orgv1alpha1.AgentPlatformStatus
	if org.Spec.AgentPlatform != nil {
		spec = *org.Spec.AgentPlatform
	}
	if org.Status.AgentPlatform != nil {
		status = *org.Status.AgentPlatform
	}
	return spec, status
}

// namespaceOf is the organization namespace the operator reported, empty
// until it did.
func namespaceOf(org *orgv1alpha1.Organization) string {
	_, status := agentPlatformOf(org)
	return status.Namespace
}

func (s *Service) summary(org *orgv1alpha1.Organization) Organization {
	spec, status := agentPlatformOf(org)
	out := Organization{
		Name: org.Name, DisplayName: org.Annotations[apov1alpha1.DisplayNameAnnotation],
		Namespace: status.Namespace, Admins: []Subject{}, Harnesses: slices.Clone(spec.Harnesses), WorkerReplicas: spec.Workers.ReplicasOrDefault(),
		SnapshotPrefix: status.SnapshotPrefix, Deleting: org.DeletionTimestamp != nil, ErasureStep: status.ErasureStep,
		GitOps: org.Labels[agents.KustomizationNameLabel], LastAppliedRevision: status.LastAppliedRevision,
	}
	for _, a := range org.Spec.Admins {
		out.Admins = append(out.Admins, Subject{Kind: a.Kind, Name: a.Name})
	}
	if s.cfg.Commit != nil {
		repository := s.cfg.Commit.Objects.Repository
		out.LastAppliedRevisionURL = commitURL(repository, status.LastAppliedRevision)
		if out.LastAppliedRevisionURL != "" {
			out.LastAppliedFile = s.cfg.Commit.objectFile(org.Name)
			out.LastAppliedFileURL = fileURL(repository, status.LastAppliedRevision, out.LastAppliedFile)
		}
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

// ---- reads ------------------------------------------------------------------------

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
	// OrganizationNamespace is organization-operator's namespace of the
	// Organization (status.namespace, org-<name>), on an installation where
	// it runs: it deletes that namespace, and everything in it, with the
	// Organization.
	OrganizationNamespace string `json:"organizationNamespace,omitempty"`
	// Sessions counts, per Agent, the sessions of the organization the erase
	// deletes, as the caller sees them across every creator: running ones
	// stop, stopped ones lose their worker snapshots and cannot be resumed.
	// Only Agents with a session are listed. Null when they could not be
	// listed, and then SessionsUnavailable says why.
	Sessions            []AgentSessions `json:"sessions"`
	SessionsUnavailable string          `json:"sessionsUnavailable,omitempty"`
	// Kept is what a delete does not erase.
	Kept string `json:"kept"`
	// Unreadable names what the caller may not read there, with why.
	Unreadable []string `json:"unreadable,omitempty"`
}

// AgentSessions counts one Agent's sessions the erase deletes.
type AgentSessions struct {
	Agent string `json:"agent"`
	// Running are in RuntimeState CREATING or READY.
	Running int `json:"running"`
	// Stopped are in RuntimeState SUSPENDED or FAILED.
	Stopped int `json:"stopped"`
}

// Get reads one organization and what deleting it erases.
func (s *Service) Get(ctx context.Context, name string) (*Details, error) {
	c, _, org, err := s.get(ctx, name)
	if err != nil {
		return nil, err
	}
	return &Details{Organization: s.summary(org), Erases: s.erases(ctx, c.Dynamic(), org)}, nil
}

// erases is what deleting org erases: its Agent Platform namespace as the
// caller reads it, once the operator reported one, and organization-operator's
// namespace, which goes with the Organization. Nil when it has neither.
func (s *Service) erases(ctx context.Context, dyn dynamic.Interface, org *orgv1alpha1.Organization) *Erasure {
	var out *Erasure
	if namespaceOf(org) != "" {
		out = s.erasure(ctx, dyn, org)
	}
	if ns := org.Status.Namespace; ns != "" {
		if out == nil {
			out = &Erasure{Kept: "the organization has no Agent Platform namespace yet"}
		}
		out.OrganizationNamespace = ns
		out.Kept += fmt.Sprintf("; organization-operator deletes the namespace %s, and everything in it, with the Organization", ns)
	}
	return out
}

// get reads an organization as the caller: the object as the API server
// sent it, which a write changes in place so that fields this version's
// types do not know survive, and its typed view. An Organization without the
// Agent Platform enabled is not one of the tools' organizations.
func (s *Service) get(ctx context.Context, name string) (kube.Client, *unstructured.Unstructured, *orgv1alpha1.Organization, error) {
	if name == "" {
		return nil, nil, nil, invalidf("name is required")
	}
	c, err := s.caller(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	u, err := c.Dynamic().Resource(GVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, nil, nil, kubeErr(err, "get Organization "+name, wayOrganizationsAdmin())
	}
	org, err := organizationOf(u)
	if err != nil {
		return nil, nil, nil, err
	}
	if !agentPlatformEnabled(org) {
		return nil, nil, nil, fmt.Errorf("%w: Organization %s does not have the Agent Platform enabled (spec.agentPlatform.enabled)", agents.ErrNotFound, name)
	}
	return c, u, org, nil
}

// erasure reads, as the caller, what a delete of org erases. An
// organizations-admin reads the organization's contents only once bound in
// it, so what it may not read is named, not failed.
func (s *Service) erasure(ctx context.Context, dyn dynamic.Interface, org *orgv1alpha1.Organization) *Erasure {
	_, status := agentPlatformOf(org)
	ns := status.Namespace
	out := &Erasure{Namespace: ns, SnapshotPrefix: status.SnapshotPrefix,
		Kept: "session tombstones stay in the agent runtime; everything else of the organization namespace is erased, worker snapshots and connector sign-ins included"}
	credentials := map[string]bool{}
	read := func(what string, gvr func() ([]unstructured.Unstructured, error), into *[]string, each func(*unstructured.Unstructured)) bool {
		items, err := gvr()
		*into = []string{}
		if err != nil {
			out.Unreadable = append(out.Unreadable, fmt.Sprintf("%s: %v (an organizations-admin reads them once it binds itself in the organization, which its members see)", what, err))
			return false
		}
		for i := range items {
			*into = append(*into, items[i].GetName())
			if each != nil {
				each(&items[i])
			}
		}
		return true
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
	agentsRead := read("agents", lister(s.kagentGVR("agents")), &out.Agents, nil)
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
	out.Sessions, out.SessionsUnavailable = s.erasedSessions(ctx, ns, out.Agents, agentsRead)
	return out
}

// erasedSessions counts, as the caller and across every creator, the
// sessions of the organization's Agents the erase deletes. It answers why
// when it cannot list them.
func (s *Service) erasedSessions(ctx context.Context, ns string, agentNames []string, agentsRead bool) ([]AgentSessions, string) {
	if s.cfg.Sessions == nil {
		return nil, "agent-manager has no agent runtime target (--kagent-target), so it lists no sessions"
	}
	if !agentsRead {
		return nil, "the organization's agents could not be read (see unreadable)"
	}
	token, ok := identity.TokenFromContext(ctx)
	if !ok {
		return nil, "the request carries no identity token, and sessions are listed as the caller"
	}
	out := []AgentSessions{}
	for _, name := range agentNames {
		page, err := s.cfg.Sessions.ListSessions(ctx, token, kagent.Ref{Namespace: ns, Name: name}, true, "")
		if err != nil {
			return nil, sessionsRefusal(name, err)
		}
		if page.NextPageToken != "" {
			return nil, fmt.Sprintf("the sessions of agent %s number more than %d, the most read for one agent, so they are not counted", name, kagent.MaxListedSessions)
		}
		count := AgentSessions{Agent: name}
		for _, session := range page.Sessions {
			switch session.GetState() {
			case apiv1alpha1.RuntimeState_RUNTIME_STATE_CREATING, apiv1alpha1.RuntimeState_RUNTIME_STATE_READY:
				count.Running++
			case apiv1alpha1.RuntimeState_RUNTIME_STATE_SUSPENDED, apiv1alpha1.RuntimeState_RUNTIME_STATE_FAILED:
				count.Stopped++
			}
		}
		if count.Running+count.Stopped > 0 {
			out = append(out, count)
		}
	}
	return out, ""
}

// sessionsRefusal says why an Agent's sessions could not be listed: the
// controller's own words for a refusal of the caller, the status code alone
// for anything else, whose message may name the runtime's address.
func sessionsRefusal(agent string, err error) string {
	st, ok := status.FromError(err)
	if !ok {
		return fmt.Sprintf("the sessions of agent %s could not be listed", agent)
	}
	switch st.Code() {
	case codes.PermissionDenied, codes.Unauthenticated, codes.NotFound:
		return fmt.Sprintf("the sessions of agent %s: %s: %s", agent, st.Code(), st.Message())
	default:
		return fmt.Sprintf("the sessions of agent %s: the agent runtime answered %s", agent, st.Code())
	}
}

// ---- create -----------------------------------------------------------------------

// CreateRequest is create_organization's arguments.
type CreateRequest struct {
	Name        string
	DisplayName string
	Harnesses   []string
	// WorkerReplicas is spec.agentPlatform.workers.replicas; nil leaves the
	// CRD's default.
	WorkerReplicas *int32
	// AdminGroups and AdminUsers are spec.admins, which agent-platform-operator
	// binds to agent-platform:admin in the organization namespace. A group
	// first: membership then changes in the identity provider. None: the
	// platform team manages the organization.
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
	// Created: the object was written by this call.
	Created bool `json:"created"`
	// Next says what happens after the write, and how to see it.
	Next        string `json:"next,omitempty"`
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

// operatorNext is what agent-platform-operator does once an organization is
// written.
const operatorNext = "agent-platform-operator creates the organization namespace and binds spec.admins to " + RoleAdmin + " there; list_organizations and get_organization show the namespace once it reports it"

// Create writes an Organization with the Agent Platform enabled and its
// admins in spec.admins, as the caller. It writes no RoleBinding:
// agent-platform-operator derives the admins' binding from spec.admins.
// Before the write it asks, as the caller, for create on Organizations, and
// dry-runs the object, so a refusal comes before the object exists. An
// Organization of that name that exists is refused: the tools do not turn
// the Agent Platform on for an existing Organization.
func (s *Service) Create(ctx context.Context, req CreateRequest) (*CreateResult, error) {
	if req.Name == "" {
		return nil, invalidf("name is required")
	}
	admins, err := subjectsOf(req.AdminGroups, req.AdminUsers)
	if err != nil {
		return nil, err
	}
	mode, err := s.mode(req.Mode)
	if err != nil {
		return nil, err
	}
	c, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	way := wayOrganizationsAdmin()
	if err := requireAccess(ctx, c, way, access{verb: "create", group: GVR.Group, resource: GVR.Resource}); err != nil {
		return nil, err
	}
	what := "Organization " + req.Name
	existing, err := c.Dynamic().Resource(GVR).Get(ctx, req.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return nil, kubeErr(err, "get "+what, way)
	default:
		return nil, refuseExisting(existing, what)
	}
	res := &CreateResult{DryRun: req.DryRun, RequestedBy: identity.Caller(ctx)}
	if mode == agents.ModeCommit {
		return s.createCommit(ctx, c, req, admins, res)
	}
	obj := s.organizationObject(req, admins)
	if err := apply(ctx, c.Dynamic(), []write{{gvr: GVR, kind: orgKind, op: opCreate, obj: obj, name: req.Name, what: what, reason: way}}, req.DryRun); err != nil {
		return nil, err
	}
	planned, err := organizationOf(unstructuredOf(obj))
	if err != nil {
		return nil, err
	}
	res.Organization = s.summary(planned)
	if req.DryRun {
		res.Next = "dry run: nothing was written"
		return res, nil
	}
	res.Created = true
	res.Next = operatorNext
	s.log.Info("organization created", identity.LogAttr(ctx), "name", req.Name, "admins", len(admins))
	return res, nil
}

// refuseExisting refuses a create of an Organization that exists, live.
func refuseExisting(existing *unstructured.Unstructured, what string) error {
	org, err := organizationOf(existing)
	if err != nil {
		return err
	}
	switch {
	case org.DeletionTimestamp != nil:
		return fmt.Errorf("%w: %s is being erased; create it again once get_organization answers not_found", agents.ErrConflict, what)
	case agentPlatformEnabled(org):
		return fmt.Errorf("%w: %s exists with the Agent Platform; update_organization changes it", agents.ErrConflict, what)
	}
	return fmt.Errorf("%w: %s exists without the Agent Platform; the organization tools do not turn it on for an existing Organization", agents.ErrConflict, what)
}

func (s *Service) organizationObject(req CreateRequest, admins []Subject) map[string]any {
	meta := map[string]any{"name": req.Name}
	if req.DisplayName != "" {
		meta["annotations"] = map[string]any{apov1alpha1.DisplayNameAnnotation: req.DisplayName}
	}
	agentPlatform := map[string]any{"enabled": true}
	if len(req.Harnesses) > 0 {
		h := make([]any, len(req.Harnesses))
		for i, name := range req.Harnesses {
			h[i] = name
		}
		agentPlatform["harnesses"] = h
	}
	if req.WorkerReplicas != nil {
		agentPlatform["workers"] = map[string]any{"replicas": int64(*req.WorkerReplicas)}
	}
	spec := map[string]any{"agentPlatform": agentPlatform}
	if len(admins) > 0 {
		spec["admins"] = adminsObject(admins)
	}
	return map[string]any{
		"apiVersion": orgv1alpha1.GroupVersion.String(), "kind": orgKind,
		fieldMetadata: meta, "spec": spec,
	}
}

// adminsObject is spec.admins of admins.
func adminsObject(admins []Subject) []any {
	out := make([]any, 0, len(admins))
	for _, a := range admins {
		out = append(out, map[string]any{"kind": a.Kind, "name": a.Name})
	}
	return out
}

// ---- update -----------------------------------------------------------------------

// UpdateRequest is update_organization's arguments; nil leaves a field.
type UpdateRequest struct {
	Name           string
	DisplayName    *string
	Harnesses      *[]string
	WorkerReplicas *int32
	// AdminGroups and AdminUsers replace the Groups, or the Users, of
	// spec.admins; nil leaves them.
	AdminGroups *[]string
	AdminUsers  *[]string
	DryRun      bool
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
	c, u, org, err := s.get(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	// After the caller's read: a caller who may not read the organization
	// gets forbidden from get, never its state.
	if err := s.erasing(org, orDefault(namespaceOf(org), "(none yet)")); err != nil {
		return nil, err
	}
	if mode == agents.ModeCommit {
		return s.updateCommit(ctx, c, u, org, req)
	}
	if err := refuseLive(org); err != nil {
		return nil, err
	}
	res := &UpdateResult{Before: s.summary(org), DryRun: req.DryRun, RequestedBy: identity.Caller(ctx)}
	next := u.DeepCopy()
	if res.Changed, err = changeOrganization(next, req); err != nil {
		return nil, err
	}
	after, err := organizationOf(next)
	if err != nil {
		return nil, err
	}
	res.After = s.summary(after)
	if len(res.Changed) == 0 {
		return res, nil
	}
	w := write{gvr: GVR, kind: orgKind, op: opUpdate, obj: next.Object, name: req.Name,
		what: "Organization " + req.Name, reason: wayOrganizationsAdmin()}
	if err := apply(ctx, c.Dynamic(), []write{w}, req.DryRun); err != nil {
		return nil, err
	}
	if !req.DryRun {
		s.log.Info("organization updated", identity.LogAttr(ctx), "name", req.Name, "changed", res.Changed)
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
	spec, _ := agentPlatformOf(cur)
	if req.Harnesses != nil && !slices.Equal(*req.Harnesses, spec.Harnesses) {
		if err := unstructured.SetNestedStringSlice(next.Object, *req.Harnesses, "spec", "agentPlatform", "harnesses"); err != nil {
			return nil, err
		}
		changed = append(changed, "harnesses")
	}
	// An unset field is the CRD's default, not the requested value: write
	// the request then too, so an explicit 0 is never taken for "no change".
	// A request equal to the default is written as well: the value then stays
	// when the operator's default changes.
	if req.WorkerReplicas != nil && (spec.Workers.Replicas == nil || *req.WorkerReplicas != *spec.Workers.Replicas) {
		if err := unstructured.SetNestedField(next.Object, int64(*req.WorkerReplicas), "spec", "agentPlatform", "workers", "replicas"); err != nil {
			return nil, err
		}
		changed = append(changed, "workerReplicas")
	}
	if req.AdminGroups != nil || req.AdminUsers != nil {
		admins, err := changedAdmins(cur.Spec.Admins, req.AdminGroups, req.AdminUsers)
		if err != nil {
			return nil, err
		}
		if !slices.EqualFunc(admins, cur.Spec.Admins, func(a Subject, b orgv1alpha1.OrganizationAdmin) bool { return a.Kind == b.Kind && a.Name == b.Name }) {
			if len(admins) == 0 {
				unstructured.RemoveNestedField(next.Object, "spec", "admins")
			} else if err := unstructured.SetNestedSlice(next.Object, adminsObject(admins), "spec", "admins"); err != nil {
				return nil, err
			}
			changed = append(changed, "admins")
		}
	}
	return changed, nil
}

// changedAdmins is spec.admins with its Groups replaced by groups and its
// Users by users, where each is set; the Groups first.
func changedAdmins(cur []orgv1alpha1.OrganizationAdmin, groups, users *[]string) ([]Subject, error) {
	kept := func(kind string) []string {
		var out []string
		for _, a := range cur {
			if a.Kind == kind {
				out = append(out, a.Name)
			}
		}
		return out
	}
	g, u := kept(orgv1alpha1.OrganizationAdminGroup), kept(orgv1alpha1.OrganizationAdminUser)
	if groups != nil {
		g = *groups
	}
	if users != nil {
		u = *users
	}
	return subjectsOf(g, u)
}

// refuseLive refuses a live write to an organization Flux applies from git:
// Flux would undo it.
func refuseLive(org *orgv1alpha1.Organization) error {
	if ks := org.Labels[agents.KustomizationNameLabel]; ks != "" {
		return fmt.Errorf("%w: Organization %s is applied from git by the Flux Kustomization %s, which would revert a live change: use mode commit to change it with a pull request", agents.ErrGitOpsOwned, org.Name, ks)
	}
	return nil
}

// ---- delete -----------------------------------------------------------------------

// DeleteRequest is delete_organization's arguments.
type DeleteRequest struct {
	Name string
	// Confirm must be the organization's name for the delete to happen; without it
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
	c, u, org, err := s.get(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	if mode == agents.ModeApply {
		if err := refuseLive(org); err != nil {
			return nil, err
		}
	}
	confirm := req.Name
	res := &DeleteResult{Organization: s.summary(org), Erases: s.erases(ctx, c.Dynamic(), org), DryRun: req.DryRun, RequestedBy: identity.Caller(ctx)}
	if org.DeletionTimestamp != nil {
		res.Next = "the organization is being erased already (" + orDefault(res.Organization.ErasureStep, "starting") + ")"
		return res, nil
	}
	way := wayOrganizationsAdmin()
	if err := requireAccess(ctx, c, way, access{verb: "delete", group: GVR.Group, resource: GVR.Resource, name: req.Name}); err != nil {
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
	what := "Organization " + req.Name
	orgs := c.Dynamic().Resource(GVR)
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
		w := write{gvr: GVR, kind: orgKind, op: opUpdate, obj: next.Object, name: req.Name, what: what + " (" + PruneAnnotation + ": " + PruneEnabled + ")", reason: way}
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
			if unmarkErr := s.unmarkPrune(ctx, c, req.Name); unmarkErr != nil {
				return nil, fmt.Errorf("%w; the object keeps %s: %s, remove it: %v", err, PruneAnnotation, PruneEnabled, unmarkErr)
			}
		}
		return nil, err
	}
	if err := orgs.Delete(ctx, req.Name, metav1.DeleteOptions{}); err != nil {
		err = kubeErr(err, "delete "+what, way)
		if marked {
			if unmarkErr := s.unmarkPrune(ctx, c, req.Name); unmarkErr != nil {
				return nil, fmt.Errorf("%w; the object keeps %s: %s, remove it: %v", err, PruneAnnotation, PruneEnabled, unmarkErr)
			}
		}
		return nil, err
	}
	res.Deleted = true
	res.Next = "the operator erases the organization now; get_organization reports its erasure step until it is gone"
	s.log.Info("organization deleted", identity.LogAttr(ctx), "name", req.Name)
	return res, nil
}

// unmarkPrune takes the delete intent off an organization a delete did not
// delete.
func (s *Service) unmarkPrune(ctx context.Context, c kube.Client, name string) error {
	orgs := c.Dynamic().Resource(GVR)
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
		fieldMetadata: map[string]any{"name": name, fieldNamespace: ns, "labels": map[string]any{agents.ManagedByLabel: agents.ManagedByValue}},
		"roleRef":     map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": role},
		"subjects":    subs,
	}
}

func subjectObject(s Subject) map[string]any {
	out := map[string]any{"kind": s.Kind, "name": s.Name}
	if s.Kind == "ServiceAccount" {
		out[fieldNamespace] = s.Namespace
	} else {
		out["apiGroup"] = "rbac.authorization.k8s.io"
	}
	return out
}
