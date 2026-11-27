package organizations

import (
	"context"
	"fmt"

	orgv1alpha1 "github.com/giantswarm/organization-operator/api/v1alpha1"
	authorizationv1 "k8s.io/api/authorization/v1"

	"github.com/giantswarm/agent-manager/internal/agents"
)

// Every tool takes an organization by its name, the name of its
// Organization, and never its namespace: Resolve finds the namespace in the
// agentPlatform entry of the Organization's status.namespaces.

// errOrganizationNotFound answers an organization that does not exist and
// one the caller has no right in alike, as get_organization does: the
// answer never tells the caller that an organization it may not see exists.
func errOrganizationNotFound(name string) error {
	return fmt.Errorf("%w: organization %s not found; list_organizations lists yours", agents.ErrNotFound, name)
}

// errAgentPlatformNotEnabled answers an Organization whose Agent Platform
// is not enabled: one that never had the switch on, or one whose switch is
// off although the agent-platform chart's admission policy refuses a
// switch-off. No tool writes there.
func errAgentPlatformNotEnabled(name string) error {
	return fmt.Errorf("%w: the Agent Platform is not enabled for organization %s (its Organization's spec.capabilities.agentPlatform.enabled is not true); nothing was written", agents.ErrConflict, name)
}

// errOrganizationNotReady answers an organization whose namespace
// agent-platform-operator has not reported yet.
func errOrganizationNotReady(name string) error {
	return fmt.Errorf("%w: organization %s is not ready yet: agent-platform-operator has not reported its namespace (the agentPlatform entry of status.namespaces); nothing was written, try again shortly", agents.ErrConflict, name)
}

// Resolve finds the Agent Platform namespace of the organization name: the
// namespace its Organization reports in the agentPlatform entry of
// status.namespaces, which agent-platform-operator keeps and labels with the
// organization. It
// answers not found for an Organization that does not exist and for one the
// caller has no right in (Visible), a conflict for one with the Agent
// Platform switched off, and a conflict for one whose namespace is not
// reported yet, or is not the one the operator labelled for it.
func (s *Service) Resolve(ctx context.Context, name string) (Namespace, error) {
	if s.cfg.Namespaces == nil {
		return Namespace{}, fmt.Errorf("%w: organization management is disabled", agents.ErrUnsupported)
	}
	if name == "" {
		return Namespace{}, invalidf("organization (the name of its Organization) is required; list_organizations lists yours")
	}
	org, ok, err := s.cfg.Namespaces.Organization(ctx, name)
	if err != nil {
		return Namespace{}, fmt.Errorf("%w: %v", agents.ErrUnsupported, err)
	}
	if !ok {
		return Namespace{}, errOrganizationNotFound(name)
	}
	visible, err := s.visible(ctx, org)
	if err != nil {
		return Namespace{}, err
	}
	if !visible {
		return Namespace{}, errOrganizationNotFound(name)
	}
	if !agentPlatformEnabled(org) {
		return Namespace{}, errAgentPlatformNotEnabled(name)
	}
	reported := namespaceOf(org)
	if reported == "" {
		return Namespace{}, errOrganizationNotReady(name)
	}
	ns, ok, err := s.cfg.Namespaces.Get(ctx, reported)
	if err != nil {
		return Namespace{}, fmt.Errorf("%w: %v", agents.ErrUnsupported, err)
	}
	// The namespace must be the one the operator keeps for this very
	// Organization: labelled with its name and, where the label is set, its
	// UID. A namespace of an earlier organization of the same name, or of
	// another one, is not written into.
	if !ok || ns.Organization != name || (ns.OrganizationUID != "" && org.UID != "" && ns.OrganizationUID != string(org.UID)) {
		return Namespace{}, errOrganizationNotReady(name)
	}
	return ns, nil
}

// visible reports whether the caller has a right in org, as
// list_organizations decides whether it lists it: it manages the
// Organization (update), or it is a member of its namespace (list Agents
// there). Both are asked as the caller.
func (s *Service) visible(ctx context.Context, org *orgv1alpha1.Organization) (bool, error) {
	c, err := s.caller(ctx)
	if err != nil {
		return false, err
	}
	if ns := namespaceOf(org); ns != "" {
		agentsGVR := s.kagentGVR("agents")
		member, _, err := s.allowed(ctx, c, authorizationv1.ResourceAttributes{Namespace: ns, Verb: "list", Group: agentsGVR.Group, Resource: agentsGVR.Resource})
		if err != nil || member {
			return member, err
		}
	}
	manage, _, err := s.allowed(ctx, c, authorizationv1.ResourceAttributes{Verb: "update", Group: GVR.Group, Resource: GVR.Resource, Name: org.Name})
	return manage, err
}

// OrganizationOfNamespace reports whether ns is an organization's Agent
// Platform namespace, by the label agent-platform-operator puts on it, and
// names that organization when the caller has a right in it (Resolve's
// rule): for any other caller the name is empty, so the answer never names
// an organization the caller may not see. ok is false for any other
// namespace, and when organizations are not served.
func (s *Service) OrganizationOfNamespace(ctx context.Context, ns string) (name string, ok bool, err error) {
	if s == nil || s.cfg.Namespaces == nil || ns == "" {
		return "", false, nil
	}
	found, ok, err := s.cfg.Namespaces.Get(ctx, ns)
	if err != nil {
		return "", false, fmt.Errorf("%w: %v", agents.ErrUnsupported, err)
	}
	if !ok {
		return "", false, nil
	}
	org, exists, err := s.cfg.Namespaces.Organization(ctx, found.Organization)
	if err != nil {
		// The caller's right in the organization is not known yet: the
		// answer does not name it, the log does.
		s.log.Warn("the Organization of an organization namespace could not be read", agents.UserAttr(ctx), fieldNamespace, ns, "error", err)
		return "", false, fmt.Errorf("%w: the organization of namespace %s could not be read; try again shortly", agents.ErrUnsupported, ns)
	}
	if !exists {
		return "", true, nil
	}
	visible, err := s.visible(ctx, org)
	if err != nil || !visible {
		return "", true, err
	}
	return found.Organization, true, nil
}

// organization resolves an organization argument: Resolve.
func (s *Service) organization(ctx context.Context, name string) (Namespace, error) {
	return s.Resolve(ctx, name)
}
