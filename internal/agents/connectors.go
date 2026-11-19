package agents

import (
	"context"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
)

// An agent's connectors are RemoteMCPServers of its organization namespace
// (set_organization_connector writes them) that its template binds beside the
// muster binding: one `extraTools` entry of the agent chart each,
// `{mcp: {server: {kind: RemoteMCPServer, name}}}`. kagent's tool binding
// references a server of the agent's own namespace only (it has no namespace
// field), and kagent adds the server's origin to the agent's egress itself.

// extraToolsValuesKey is the agent chart's list of tool bindings appended
// after the muster binding.
const extraToolsValuesKey = "extraTools"

// connectorOf is the name of the connector an extraTools entry binds: an
// `mcp` entry whose server is a kagent RemoteMCPServer. Any other entry
// (a subAgent, a server of another kind) is no connector.
func connectorOf(entry any) (string, bool) {
	m, _ := entry.(map[string]any)
	binding, _ := m["mcp"].(map[string]any)
	server, _ := binding["server"].(map[string]any)
	kind, _ := server["kind"].(string)
	name, _ := server["name"].(string)
	if kind != kindRemoteMCPServer || name == "" {
		return "", false
	}
	if group, _ := server["apiGroup"].(string); group != "" && group != KagentAPIGroup {
		return "", false
	}
	return name, true
}

// connectorEntry is the extraTools entry that binds connector name.
func connectorEntry(name string) map[string]any {
	return map[string]any{"mcp": map[string]any{"server": map[string]any{"kind": kindRemoteMCPServer, "name": name}}}
}

// connectorsFromValues reads the connectors of a release's values, in the
// order of its extraTools.
func connectorsFromValues(values map[string]any) []string {
	raw, _ := values[extraToolsValuesKey].([]any)
	var out []string
	for _, entry := range raw {
		if name, ok := connectorOf(entry); ok {
			out = append(out, name)
		}
	}
	return out
}

// setConnectors replaces the connector entries of values' extraTools with one
// entry for each name. Every other entry stays, in its order, before them; the
// entry of a connector that stays keeps its other fields (a tools list,
// requireApproval). With no entry left, a key that was there becomes an empty
// list; one that was not there stays out. A release with valuesFrom takes no
// connectors (refuseConnectorsUnderValuesFrom).
func setConnectors(values map[string]any, names []string) {
	raw, had := values[extraToolsValuesKey].([]any)
	out := make([]any, 0, len(raw)+len(names))
	current := map[string]any{}
	for _, entry := range raw {
		name, ok := connectorOf(entry)
		if !ok {
			out = append(out, entry)
			continue
		}
		if _, seen := current[name]; !seen {
			current[name] = entry
		}
	}
	for _, name := range names {
		if entry, ok := current[name]; ok {
			out = append(out, entry)
		} else {
			out = append(out, connectorEntry(name))
		}
	}
	if len(out) == 0 && !had {
		delete(values, extraToolsValuesKey)
		return
	}
	values[extraToolsValuesKey] = out
}

// refuseConnectorsUnderToolsOverride refuses connectors on values whose
// extraAgentSpec.tools the chart merges over the rendered tool list: it
// replaces that list whole, the connector bindings with it.
func refuseConnectorsUnderToolsOverride(values map[string]any) error {
	if len(connectorsFromValues(values)) == 0 {
		return nil
	}
	if tools, found, _ := unstructured.NestedFieldNoCopy(values, "extraAgentSpec", "tools"); found && tools != nil {
		return invalidf("connectors: the agent's values set extraAgentSpec.tools, which replaces its whole tool list, so the chart would bind no connector; take extraAgentSpec.tools out where the values are managed first")
	}
	return nil
}

// ValidateConnectors checks the form of a connectors list: each a
// RemoteMCPServer name of its own, named once. A name with a namespace or a
// kind is refused: a connector is always one of the agent's own organization.
func ValidateConnectors(names []string) error {
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if strings.ContainsAny(name, "/:") {
			return invalidf("connectors: %q names a namespace or a kind: a connector is always a RemoteMCPServer of the agent's own organization namespace; give its name alone (list_organization_connectors)", name)
		}
		if msgs := validation.IsDNS1123Subdomain(name); len(msgs) > 0 {
			return invalidf("connectors: %q is no RemoteMCPServer name: %s", name, strings.Join(msgs, "; "))
		}
		if seen[name] {
			return invalidf("connectors: %q is named twice", name)
		}
		seen[name] = true
	}
	return nil
}

// MusterBindings maps each RemoteMCPServer of servers that is an agent's
// muster binding to that agent's name. The agent chart renders an agent's
// Agent and its muster RemoteMCPServer under one name (agent.name) from one
// HelmRelease, and helm-controller labels every object it applies with that
// release (HelmReleaseNameLabel, HelmReleaseNamespaceLabel) after the render,
// over any label the values set. So a server is a binding when an Agent of
// the same name carries the same two labels: whatever the agent's values
// set (labels, muster.url), wherever its release lives. servers and agents
// are of one namespace. A RemoteMCPServer that a release renders without an
// Agent of its name (a connector of another chart) is none.
func MusterBindings(servers, agents []unstructured.Unstructured) map[string]string {
	releaseOf := make(map[string][2]string, len(agents))
	for i := range agents {
		if name, ns := ownerOf(&agents[i]); name != "" {
			releaseOf[agents[i].GetName()] = [2]string{name, ns}
		}
	}
	out := map[string]string{}
	for i := range servers {
		name, ns := ownerOf(&servers[i])
		if name == "" {
			continue
		}
		if release, ok := releaseOf[servers[i].GetName()]; ok && release == [2]string{name, ns} {
			out[servers[i].GetName()] = servers[i].GetName()
		}
	}
	return out
}

// refuseConnectorsUnderValuesFrom refuses connectors on a release with
// spec.valuesFrom: Flux merges those values beneath spec.values, so an
// extraTools list there binds connectors that get_agent (which reads
// spec.values) does not report, and an inline extraTools list replaces it
// whole, with its other entries.
func refuseConnectorsUnderValuesFrom(hr *unstructured.Unstructured) error {
	if from, _, _ := unstructured.NestedSlice(hr.Object, "spec", "valuesFrom"); len(from) > 0 {
		return invalidf("connectors: HelmRelease %s/%s takes values from spec.valuesFrom, which can bind tools (extraTools) that agent-manager neither reads nor keeps; change its connectors where the valuesFrom values are managed", hr.GetNamespace(), hr.GetName())
	}
	return nil
}

// requireConnectors checks, as the caller and before any write, that each
// name is a RemoteMCPServer of the agent's namespace, and that the namespace
// is an organization namespace on the installation: connectors exist there
// only (set_organization_connector, list_organization_connectors). The
// muster RemoteMCPServer of an agent (the agent's own, or another agent's,
// whose toolset it carries; MusterBindings) is no connector.
func (s *Service) requireConnectors(ctx context.Context, st *site, agent string, names []string) error {
	if len(names) == 0 {
		return nil
	}
	if err := ValidateConnectors(names); err != nil {
		return err
	}
	ns := st.ns()
	if st.loc.IsSet() {
		return invalidf("connectors: an agent on a workload cluster takes no connector: connectors are RemoteMCPServers of an organization namespace on the installation")
	}
	org, err := s.isTenant(ctx, ns)
	if err != nil {
		return err
	}
	if !org {
		return invalidf("connectors: namespace %s is no organization namespace: a connector is a RemoteMCPServer of an organization's own namespace (set_organization_connector), and only an agent of that namespace takes it", ns)
	}
	list, err := st.agent.Resource(s.mcpServerGVR()).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return wrapKube(err, "list remotemcpservers in "+ns)
	}
	agentList, err := st.agent.Resource(s.agentGVR()).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return wrapKube(err, "list agents in "+ns)
	}
	known := make(map[string]bool, len(list.Items))
	for i := range list.Items {
		known[list.Items[i].GetName()] = true
	}
	bindings := MusterBindings(list.Items, agentList.Items)
	var missing []string
	for _, name := range names {
		if name == agent {
			return invalidf("connectors: %q is the agent's own muster binding, no connector (its toolset selects muster's tools)", name)
		}
		if of, ok := bindings[name]; ok {
			return invalidf("connectors: RemoteMCPServer %q is the muster binding of agent %q, rendered with it by one HelmRelease: no connector (list_organization_connectors marks it musterBindingOf)", name, of)
		}
		if !known[name] {
			missing = append(missing, `"`+name+`"`)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return invalidf("connectors: RemoteMCPServer %s does not exist in namespace %s (list_organization_connectors lists the organization's connectors)", strings.Join(missing, ", "), ns)
}
