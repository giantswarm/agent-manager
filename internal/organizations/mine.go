package organizations

import (
	"context"
	"fmt"

	"golang.org/x/sync/errgroup"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/identity"
)

// Mine is list_my_organizations's answer: the New agent picker's list.
type Mine struct {
	Organizations []Namespace `json:"organizations"`
	// None tells a person in no organization who can add them.
	None string `json:"none,omitempty"`
}

// ListMine lists the organization namespaces in which the caller may list
// Agents, which every organization role grants: agent-manager's
// ServiceAccount lists the namespaces and keeps the labelled ones, and a
// SelfSubjectAccessReview with the caller's own token asks for each, so the
// API server decides on the caller's identity. A namespace whose review fails
// is left out.
func (s *Service) ListMine(ctx context.Context) (*Mine, error) {
	candidates, err := s.cfg.Namespaces.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agents.ErrUnsupported, err)
	}
	c, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	agentsGVR := s.kagentGVR("agents")
	allowed := make([]bool, len(candidates))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(8)
	for i, ns := range candidates {
		g.Go(func() error {
			review := &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{
				ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: ns.Name, Verb: "list", Group: agentsGVR.Group, Resource: agentsGVR.Resource},
			}}
			res, err := c.Typed().AuthorizationV1().SelfSubjectAccessReviews().Create(gctx, review, metav1.CreateOptions{})
			if apierrors.IsUnauthorized(err) {
				// The caller's token is refused: no review answers, and
				// "no organization" would be the wrong answer. The answer
				// names no namespace: the ServiceAccount found them, and
				// the caller has shown access to none.
				return errTokenRefused
			}
			if err != nil {
				s.log.Debug("access review failed; leaving the organization out", identity.LogAttr(ctx), "namespace", ns.Name, "error", err)
				return nil
			}
			allowed[i] = res.Status.Allowed
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	out := &Mine{Organizations: []Namespace{}}
	for i, ns := range candidates {
		if allowed[i] {
			out.Organizations = append(out.Organizations, ns)
		}
	}
	if len(out.Organizations) == 0 {
		out.None = fmt.Sprintf("you belong to no organization: an admin of an organization adds you (add_organization_member), or someone with %s creates one with you as its first admin", RoleOrganizationsAdmin)
	}
	return out, nil
}
