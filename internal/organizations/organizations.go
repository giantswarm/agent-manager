// Package organizations holds agent-manager's organization tools: the
// AgentPlatformOrganizations of agent-platform-operator (create, change,
// delete, read) for an organizations-admin, and inside an organization
// namespace the members, models, connectors and egress policies for the
// organization's admins, plus the New agent picker's read path (epic
// giantswarm/giantswarm#38891, slice giantswarm/agent-manager#291).
//
// Every call acts as the signed-in person: the caller's own client writes,
// after a server-side dry run as the caller, so the API server's RBAC,
// schemas, CEL rules and admission policies answer, never a copy of them
// here. The privileged work (namespaces, `bind` of the operator's roles,
// erasure) is agent-platform-operator's. Only the picker reads with
// agent-manager's ServiceAccount, and only the namespaces' labels; what a
// person may do in one is asked as the person.
//
// No tool takes or returns a secret value: models, connectors and egress
// policies name their credential Secret by reference, and a missing or
// replaced credential is answered with the credential UI link.
// The authenticated HTTPS save is the only credential-value input.
package organizations

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	apov1alpha1 "github.com/giantswarm/agent-platform-operator/api/v1alpha1"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/kagent"
	"github.com/giantswarm/agent-manager/internal/kube"
)

// The roles the tools name: the ones agent-platform's connectivity chart
// renders (giantswarm/agent-platform#1270).
const (
	fieldMetadata = "metadata"
	// RoleOrganizationsAdmin creates, changes and deletes
	// AgentPlatformOrganizations, and binds members in their namespaces.
	RoleOrganizationsAdmin = "agent-platform:organizations-admin"
	// RoleAdmin, RoleEditor and RoleUser are an organization's members, by
	// RoleBinding in its namespace.
	RoleAdmin  = "agent-platform:admin"
	RoleEditor = "agent-platform:editor"
	RoleUser   = "agent-platform:user"
	// FirstAdminsBinding is the RoleBinding to RoleAdmin a create writes with
	// the organization; an admission policy keeps it from being deleted or
	// emptied while the namespace lives.
	FirstAdminsBinding = "organization-admins"
	// PruneAnnotation is Flux's prune marker, the one delete intent of an
	// AgentPlatformOrganization: an admission policy refuses deleting one that
	// does not carry it as PruneEnabled.
	PruneAnnotation = "kustomize.toolkit.fluxcd.io/prune"
	PruneEnabled    = "enabled"
	// CredentialPrefix starts the name of every credential Secret of an
	// organization: what the credential UI writes.
	CredentialPrefix = "tenant-credential-" // #nosec G101 -- a name prefix, not a credential
)

// GVR is the AgentPlatformOrganization resource.
var GVR = apov1alpha1.GroupVersion.WithResource("agentplatformorganizations")

var rbacGVR = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}

// Config is what the tools need besides the caller's client.
type Config struct {
	CredentialUIURL      string
	CredentialOperations *CredentialOperations
	// Namespaces finds the organization namespaces with the ServiceAccount.
	Namespaces *Namespaces
	// KagentAPIVersion is the api.kagent.dev version of the ModelConfigs,
	// RemoteMCPServers, EgressPolicies and Agents.
	KagentAPIVersion string
	// HelmReleaseAPIVersion is the helm.toolkit.fluxcd.io version the delete
	// preview counts HelmReleases in.
	HelmReleaseAPIVersion string
	// ReadyTimeout is how long a create waits for the organization's Ready
	// before it leaves the first admins to a second call; ReadyPoll how often
	// it looks.
	ReadyTimeout time.Duration
	ReadyPoll    time.Duration
	// Commit is where mode commit writes; nil: only mode apply.
	Commit *CommitConfig
	// Sessions lists the sessions of the organization's Agents for the
	// erase preview, as the caller; nil: the preview says they could not
	// be listed.
	Sessions SessionLister
}

// SessionLister lists an Agent's sessions in the agent runtime as the
// caller (kagent.Client).
type SessionLister interface {
	ListSessions(ctx context.Context, token string, agent kagent.Ref, allCreators bool, pageToken string) (*kagent.SessionPage, error)
}

// Service is the organization tools.
type Service struct {
	kube kube.Provider
	cfg  Config
	log  *slog.Logger
}

// New builds the tools on the caller's client provider.
func New(k kube.Provider, cfg Config, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	if cfg.KagentAPIVersion == "" {
		cfg.KagentAPIVersion = agents.DefaultKagentAPIVersion
	}
	if cfg.HelmReleaseAPIVersion == "" {
		cfg.HelmReleaseAPIVersion = agents.DefaultHelmReleaseAPIVersion
	}
	if cfg.ReadyTimeout <= 0 {
		cfg.ReadyTimeout = 2 * time.Minute
	}
	if cfg.ReadyPoll <= 0 {
		cfg.ReadyPoll = 2 * time.Second
	}
	return &Service{kube: k, cfg: cfg, log: log}
}

func (s *Service) kagentGVR(resource string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: agents.KagentAPIGroup, Version: s.cfg.KagentAPIVersion, Resource: resource}
}

// caller is the signed-in person's client.
func (s *Service) caller(ctx context.Context) (kube.Client, error) {
	c, err := s.kube.Client(ctx)
	if err != nil {
		if errors.Is(err, kube.ErrNoCallerToken) {
			return nil, fmt.Errorf("%w: the request carries no identity token to act with: %v", agents.ErrUnauthenticated, err)
		}
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}
	return c, nil
}

// ---- refusals -----------------------------------------------------------------

// wayOrganizationsAdmin names who can give the caller an organizations-admin's
// rights in parent.
func wayOrganizationsAdmin(parent string) string {
	return fmt.Sprintf("this needs the ClusterRole %s, bound to you in namespace %s or cluster-wide; a cluster-admin, or an admin of the Giant Swarm Organization that owns %s, binds it", RoleOrganizationsAdmin, parent, parent)
}

// wayFirstAdmins names who can bind the first admins of org when the
// creator may not: create_organization refuses before the write a creator
// whose bindings in the parent the operator does not mirror, so what is left
// is a mirror the operator has not written yet, or a creator it could not
// check.
func wayFirstAdmins(org *apov1alpha1.AgentPlatformOrganization) string {
	return fmt.Sprintf("your right to create it in %s does not reach its namespace %s; if agent-platform-operator mirrors your binding there, call create_organization again with the same arguments once it has; else someone with %s bound in %s or cluster-wide, or a cluster-admin, calls it to bind them", org.Namespace, org.Status.Namespace, RoleOrganizationsAdmin, org.Namespace)
}

// wayOrganizationAdmin names who can make the caller an admin of the
// organization in ns.
func wayOrganizationAdmin(ns string) string {
	return fmt.Sprintf("this needs %s in organization namespace %s; an admin of the organization adds you (add_organization_member), or an organizations-admin (%s) does", RoleAdmin, ns, RoleOrganizationsAdmin)
}

// kubeErr maps an API server error onto the domain sentinels; a refusal by
// RBAC or admission names way, the way out.
func kubeErr(err error, what, way string) error {
	switch {
	case err == nil:
		return nil
	case apierrors.IsNotFound(err):
		return fmt.Errorf("%w: %s", agents.ErrNotFound, what)
	case apierrors.IsForbidden(err):
		if way == "" {
			return fmt.Errorf("%w: %s: %w", agents.ErrForbidden, what, err)
		}
		return fmt.Errorf("%w: %s: %w; %s", agents.ErrForbidden, what, err, way)
	case apierrors.IsUnauthorized(err):
		return fmt.Errorf("%w: %s: %w", agents.ErrUnauthenticated, what, err)
	case apierrors.IsAlreadyExists(err), apierrors.IsConflict(err):
		return fmt.Errorf("%w: %s: %w", agents.ErrConflict, what, err)
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return fmt.Errorf("%w: %s: %w", agents.ErrInvalid, what, err)
	}
	return fmt.Errorf("%s: %w", what, err)
}

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", agents.ErrInvalid, fmt.Sprintf(format, args...))
}

// access is one SelfSubjectAccessReview the caller must pass.
type access struct {
	namespace, verb, group, resource, name string
}

func (a access) String() string {
	r := a.resource
	if a.group != "" {
		r += "." + a.group
	}
	if a.name != "" {
		r += "/" + a.name
	}
	if a.namespace == "" {
		return fmt.Sprintf("%s %s cluster-wide", a.verb, r)
	}
	return fmt.Sprintf("%s %s in %s", a.verb, r, a.namespace)
}

// requireAccess asks a SelfSubjectAccessReview as the caller for each access and
// refuses, naming way, at the first one RBAC denies.
func requireAccess(ctx context.Context, c kube.Client, way string, checks ...access) error {
	for _, a := range checks {
		allowed, err := reviewAccess(ctx, c, a)
		if err != nil {
			return err
		}
		if !allowed {
			return fmt.Errorf("%w: you may not %s; %s", agents.ErrForbidden, a.String(), way)
		}
	}
	return nil
}

// reviewAccess asks one SelfSubjectAccessReview as the caller. An empty
// namespace asks for the right in every namespace, which only a
// ClusterRoleBinding gives.
func reviewAccess(ctx context.Context, c kube.Client, a access) (bool, error) {
	review := &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{
		ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: a.namespace, Verb: a.verb, Group: a.group, Resource: a.resource, Name: a.name},
	}}
	res, err := c.Typed().AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return false, kubeErr(err, "access review for "+a.String(), "")
	}
	return res.Status.Allowed, nil
}

var dryRunAll = []string{metav1.DryRunAll}

// errTokenRefused answers a caller whose token the API server refuses (401)
// before the caller has shown access to any organization namespace: it
// names none of them, nor carries the API server's message about one.
var errTokenRefused = fmt.Errorf("%w: the API server refused your token (401): sign in again", agents.ErrUnauthenticated)

// write is one object write a tool makes: dry-run first, all of them, then
// applied in order.
type write struct {
	gvr    schema.GroupVersionResource
	kind   string // the object's kind, which names its file in commit mode
	op     string // create, update or delete
	obj    map[string]any
	ns     string
	name   string
	what   string
	reason string // the way out of a refusal
}

const (
	opCreate = "create"
	opUpdate = "update"
	opDelete = "delete"
)

// apply dry-runs every write as the caller, then, unless dryRun, applies them
// in order: nothing is written when any dry run is refused.
func apply(ctx context.Context, dyn dynamic.Interface, writes []write, dryRun bool) error {
	for _, w := range writes {
		if _, err := do(ctx, dyn, w, dryRunAll); err != nil {
			return err
		}
	}
	if dryRun {
		return nil
	}
	for _, w := range writes {
		if _, err := do(ctx, dyn, w, nil); err != nil {
			return err
		}
	}
	return nil
}

func do(ctx context.Context, dyn dynamic.Interface, w write, dryRun []string) (*unstructured.Unstructured, error) {
	res := dyn.Resource(w.gvr).Namespace(w.ns)
	var err error
	var object *unstructured.Unstructured
	switch w.op {
	case opCreate:
		object, err = res.Create(ctx, unstructuredOf(w.obj), metav1.CreateOptions{FieldManager: agents.FieldManager, DryRun: dryRun})
	case opUpdate:
		object, err = res.Update(ctx, unstructuredOf(w.obj), metav1.UpdateOptions{FieldManager: agents.FieldManager, DryRun: dryRun})
	case opDelete:
		err = res.Delete(ctx, w.name, metav1.DeleteOptions{DryRun: dryRun})
	}
	what := w.op + " " + w.what
	if dryRun != nil {
		what = "dry run of " + what
	}
	return object, kubeErr(err, what, w.reason)
}
