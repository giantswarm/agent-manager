package organizations

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"golang.org/x/sync/errgroup"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/identity"
	"github.com/giantswarm/agent-manager/internal/kube"
)

// Permissions is what the caller may do with an organization, each the
// answer of a SelfSubjectAccessReview asked with the caller's own token.
type Permissions struct {
	// Manage: update the AgentPlatformOrganization in its parent namespace,
	// the right of agent-platform:organizations-admin. A failed review
	// answers false: Manage and Member decide whether the organization is
	// listed at all.
	Manage bool `json:"manage"`
	// Member: list the Agents of its organization namespace, which every
	// organization role grants. A failed review answers false, as for
	// Manage.
	Member bool `json:"member"`
	// CreateAgents: create Agents in its organization namespace, which
	// agent-platform:editor and agent-platform:admin grant and
	// agent-platform:user does not. False while it has no namespace; nil,
	// and left out of the answer, when its review failed: not known. It asks
	// for the Agent, the resource a New agent makes; create_agent writes its
	// HelmRelease, which the same roles grant.
	CreateAgents *bool `json:"createAgents,omitempty"`
}

// Listed is one organization of list_organizations: its status, what the
// caller may do with it, and the rest of its summary only for a caller who
// manages it.
type Listed struct {
	Name        string `json:"name"`
	Parent      string `json:"parent"`
	DisplayName string `json:"displayName,omitempty"`
	// Namespace is the organization namespace, once the operator created it.
	Namespace string     `json:"namespace,omitempty"`
	Orphaned  *Condition `json:"orphaned,omitempty"`
	Deleting  bool       `json:"deleting,omitempty"`
	// Permissions is what the caller may do with it.
	Permissions Permissions `json:"permissions"`
	// Organization is the summary get_organization answers, for a caller
	// who manages it.
	Organization *Organization `json:"organization,omitempty"`
}

// Listing is list_organizations's answer.
type Listing struct {
	Organizations []Listed `json:"organizations"`
	// None tells a caller who manages and belongs to no organization who can
	// add them.
	None string `json:"none,omitempty"`
}

// List lists the organizations of parent, or of every parent namespace when
// parent is empty, that the caller manages or belongs to. agent-manager's
// ServiceAccount reads the AgentPlatformOrganizations; for each, up to three
// SelfSubjectAccessReviews with the caller's own token, run concurrently,
// decide its Permissions, whether it is listed (manage or member) and what of
// it: the API server decides on the caller's identity. A review that fails
// answers no for manage and member, and leaves createAgents out (not known);
// a refused token fails the call.
func (s *Service) List(ctx context.Context, parent string) (*Listing, error) {
	if s.cfg.Namespaces == nil {
		return nil, fmt.Errorf("%w: agent-manager serves the organization tools without a ServiceAccount to list them", agents.ErrUnsupported)
	}
	objects, err := s.cfg.Namespaces.Organizations(ctx, parent)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agents.ErrUnsupported, err)
	}
	c, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	permissions := make([]Permissions, len(objects))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(8)
	// ask answers one review into one flag; each flag is written by one
	// review only. A failed review answers no.
	ask := func(flag *bool, attributes authorizationv1.ResourceAttributes) {
		g.Go(func() error {
			allowed, _, err := s.allowed(gctx, c, attributes)
			*flag = allowed
			return err
		})
	}
	// askKnown answers one review into a flag that a failed review leaves
	// nil: not known.
	askKnown := func(flag **bool, attributes authorizationv1.ResourceAttributes) {
		g.Go(func() error {
			allowed, known, err := s.allowed(gctx, c, attributes)
			if known {
				*flag = &allowed
			}
			return err
		})
	}
	agentsGVR := s.kagentGVR("agents")
	for i := range objects {
		org, p := &objects[i], &permissions[i]
		ask(&p.Manage, authorizationv1.ResourceAttributes{
			Namespace: org.Namespace, Verb: "update", Group: GVR.Group, Resource: GVR.Resource, Name: org.Name,
		})
		// No namespace yet: nothing to belong to or create agents in.
		if ns := org.Status.Namespace; ns != "" {
			ask(&p.Member, authorizationv1.ResourceAttributes{
				Namespace: ns, Verb: "list", Group: agentsGVR.Group, Resource: agentsGVR.Resource,
			})
			askKnown(&p.CreateAgents, authorizationv1.ResourceAttributes{
				Namespace: ns, Verb: "create", Group: agentsGVR.Group, Resource: agentsGVR.Resource,
			})
		} else {
			p.CreateAgents = new(bool)
		}
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	out := &Listing{Organizations: []Listed{}}
	for i := range objects {
		p := permissions[i]
		if !p.Manage && !p.Member {
			continue
		}
		summary := s.summary(&objects[i])
		listed := Listed{
			Name: summary.Name, Parent: summary.Parent, DisplayName: summary.DisplayName, Namespace: summary.Namespace,
			Orphaned: summary.Orphaned, Deleting: summary.Deleting, Permissions: p,
		}
		if p.Manage {
			listed.Organization = &summary
		}
		out.Organizations = append(out.Organizations, listed)
	}
	slices.SortFunc(out.Organizations, func(a, b Listed) int {
		return strings.Compare(a.Parent+"/"+a.Name, b.Parent+"/"+b.Name)
	})
	if len(out.Organizations) == 0 {
		out.None = fmt.Sprintf("you manage and belong to no organization: an admin of an organization adds you (add_organization_member), or someone with %s creates one with you as its first admin", RoleOrganizationsAdmin)
	}
	return out, nil
}

// allowed asks the API server whether the caller may do what attributes
// name; known is false when the review failed. A refused token is an error;
// any other failure is logged and answers allowed and known false. A review
// that neither allows nor denies and carries an evaluation error (a failed
// authorization webhook, for one) failed too.
func (s *Service) allowed(ctx context.Context, c kube.Client, attributes authorizationv1.ResourceAttributes) (allowed, known bool, err error) {
	review := &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &attributes}}
	res, err := c.Typed().AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
	if apierrors.IsUnauthorized(err) {
		// No review answers, and "no organization" would be the wrong
		// answer. The error names nothing the ServiceAccount found.
		return false, false, errTokenRefused
	}
	if err == nil && !res.Status.Allowed && !res.Status.Denied && res.Status.EvaluationError != "" {
		err = errors.New(res.Status.EvaluationError)
	}
	if err != nil {
		s.log.Debug("access review failed", identity.LogAttr(ctx), "namespace", attributes.Namespace, "verb", attributes.Verb, "resource", attributes.Resource, "error", err)
		return false, false, nil
	}
	return res.Status.Allowed, true, nil
}
