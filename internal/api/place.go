package api

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/giantswarm/agent-manager/internal/agents"
)

// place reads where an agent tool acts from its organization, cluster and
// namespace arguments. No tool takes an organization's namespace:
//
//   - organization alone: the organization's Agent Platform namespace,
//     which its Organization reports; namespace is refused with it.
//   - organization with cluster: the agent runs on that workload cluster of
//     the organization, and namespace is the namespace on the cluster.
//   - no organization: namespace is a managed platform namespace of the
//     installation (default its kagent namespace); an organization's
//     namespace is refused with the organization's name.
//
// withCluster is false for a tool without the cluster argument. The answer
// names the organization, for the result to carry.
func (t *tools) place(ctx context.Context, req mcp.CallToolRequest, withCluster bool) (agents.Location, string, error) {
	ns := req.GetString(argNamespace, "")
	organization := req.GetString(argOrganization, "")
	cluster := ""
	if withCluster {
		cluster = req.GetString(argCluster, "")
	}
	switch {
	case cluster != "" && organization == "":
		return agents.Location{}, "", fmt.Errorf("%w: cluster needs organization, the name of the organization whose workload cluster it is", agents.ErrInvalid)
	case cluster != "":
		return agents.Location{Namespace: ns, Target: agents.Target{Organization: organization, Cluster: cluster}}, organization, nil
	case organization != "":
		if ns != "" {
			return agents.Location{}, "", fmt.Errorf("%w: namespace %s with organization %s: the organization decides the namespace; pass organization alone (namespace is only for the namespace on a workload cluster, with cluster)", agents.ErrInvalid, ns, organization)
		}
		if t.orgs == nil || !t.orgs.OrganizationToolsEnabled() {
			return agents.Location{}, "", fmt.Errorf("%w: organization %s: this installation serves no organizations (tenants.enabled)", agents.ErrUnsupported, organization)
		}
		found, err := t.orgs.Resolve(ctx, organization)
		if err != nil {
			return agents.Location{}, "", err
		}
		return agents.Location{Namespace: found.Name}, organization, nil
	}
	owner, ok, err := t.orgs.OrganizationOfNamespace(ctx, ns)
	if err != nil {
		return agents.Location{}, "", err
	}
	switch {
	case ok && owner != "":
		return agents.Location{}, "", fmt.Errorf("%w: namespace %s is the namespace of organization %s: pass organization %q instead of namespace", agents.ErrInvalid, ns, owner, owner)
	case ok:
		// An organization the caller has no right in is not named.
		return agents.Location{}, "", fmt.Errorf("%w: namespace %s is an organization's namespace: pass the organization's name as organization instead of namespace (list_organizations lists yours)", agents.ErrInvalid, ns)
	}
	return agents.Location{Namespace: ns}, "", nil
}

// jsonResultIn is jsonResult with the organization the call addressed named
// in the answer's organization field, so that a caller never maps a
// namespace back to its organization. An answer that names one already, or
// that is not a JSON object, is left as it is.
func jsonResultIn(v any, organization string) (*mcp.CallToolResult, error) {
	if organization == "" {
		return jsonResult(v)
	}
	data, err := json.Marshal(v)
	if err != nil {
		return jsonResult(v)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return jsonResult(v)
	}
	if _, ok := fields[argOrganization]; !ok {
		fields[argOrganization], _ = json.Marshal(organization)
	}
	return jsonResult(fields)
}
