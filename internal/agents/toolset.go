package agents

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
)

// A toolset is the list of selectors an agent declares — the chart's top-level
// `toolset` value, rendered by the agent chart as the X-Muster-Toolset header
// on the agent's own muster RemoteMCPServer — that bounds which of the
// gateway's tools the agent's meta-tools can see and call. agent-manager
// validates the inline grammar and composes the list exactly as given;
// resolving a toolset to tools is muster's job, per caller. It is composition,
// not authorization: the invoking human's identity and the backends' own
// authorization stay the boundary.

const (
	// ToolsetValuesKey is the agent chart's top-level value carrying the
	// toolset (never `muster.tools`, which narrows the MCP binding).
	ToolsetValuesKey = "toolset"
	// ToolsetHeader is the header the chart renders on the agent's
	// RemoteMCPServer (spec.headersFrom[]).
	ToolsetHeader = "X-Muster-Toolset"
	// MaxToolsetSelectors is the inline cap; a longer list belongs in a preset.
	MaxToolsetSelectors = 32
)

// ShippedPresets are the presets every installation has: muster's built-ins
// (read-only, none, full) and the two the platform charts ship
// (infrastructure, agent-platform).
var ShippedPresets = []string{"read-only", "none", "infrastructure", "agent-platform", "full"}

// selectorPattern is the inline selector grammar: exact, case-sensitive names,
// no whitespace or commas (the header joins selectors with commas).
var selectorPattern = regexp.MustCompile(`^(preset|server|workflow|tool):[^\s,]+$`)

const (
	selectorPrefixReserved   = "toolset:"
	selectorPrefixPresetOnly = "label:"
)

// presetNone is the toolset of an agent without tools.
const presetNone = "preset:none"

// BoundToolset is the toolset a Session binding carries for an agent as
// get_agent reports it: the declared selectors, preset:none for an agent
// without a muster server entry, nil for implicit full access (bound to no
// toolset, as before toolsets were bound).
func BoundToolset(a *Agent) []string {
	switch {
	case a.Toolset != nil:
		return a.Toolset
	case a.ImplicitFullAccess:
		return nil
	}
	return []string{presetNone}
}

// sessionToolset is the toolset a Session of agent namespace/name is bound
// with: the one its release rendered on the Agent and the agent's own
// RemoteMCPServer, read as the caller — what the agent's Sessions run with.
// Every role that may create Sessions may read both; the HelmRelease, with
// all of the agent's values, stays the editors' to read.
func (s *Service) sessionToolset(ctx context.Context, namespace, name string) ([]string, error) {
	st, err := s.site(ctx, In(namespace))
	if err != nil {
		return nil, err
	}
	obj, err := s.getAgentObject(ctx, st.agent, namespace, name)
	if err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, notFoundf("agent %s: no Agent of that name", st.describe(name))
	}
	server, err := s.getMCPServer(ctx, st.agent, namespace, name)
	if err != nil {
		return nil, err
	}
	a := Agent{}
	a.Toolset, a.ImplicitFullAccess = toolsetOf(obj, server)
	return BoundToolset(&a), nil
}

// ValidateToolset checks a declared toolset against the inline grammar:
// non-empty, at most MaxToolsetSelectors selectors, each
// preset:<name> | server:<name> | workflow:<name> | tool:<name>; `toolset:` is
// reserved and `label:` exists inside presets only. A nil list is "absent":
// refused when required (create), fine otherwise (update: unchanged).
func ValidateToolset(selectors []string, required bool) error {
	if selectors == nil {
		if required {
			return invalidf("toolset is required: declare which of the gateway's tools the agent can use, e.g. toolset: [\"preset:read-only\"]. Shipped presets: %s — preset:none for a chat-only agent without tools, preset:full for the deliberate choice of every tool the gateway exposes", strings.Join(presetSelectors(), ", "))
		}
		return nil
	}
	if len(selectors) == 0 {
		return invalidf("toolset is empty; use [\"preset:none\"] for an agent without tools (an empty list is refused so that \"no tools\" is never confused with implicit full access)")
	}
	if len(selectors) > MaxToolsetSelectors {
		return invalidf("toolset has %d selectors, more than the inline cap of %d: define a preset (muster toolsetPresets) and reference it as preset:<name>", len(selectors), MaxToolsetSelectors)
	}
	for _, sel := range selectors {
		if err := validateSelector(sel); err != nil {
			return err
		}
	}
	return nil
}

func validateSelector(sel string) error {
	switch {
	case strings.HasPrefix(sel, selectorPrefixReserved):
		return invalidf("toolset selector %q: the toolset: prefix is reserved for shared toolsets; reference a preset as preset:<name>", sel)
	case strings.HasPrefix(sel, selectorPrefixPresetOnly):
		return invalidf("toolset selector %q: label: selectors are allowed inside presets only; use a preset that selects by label (preset:infrastructure, preset:agent-platform)", sel)
	case !selectorPattern.MatchString(sel):
		return invalidf("toolset selector %q is not preset:<name>, server:<name>, workflow:<name> or tool:<name> (exact names, no whitespace or commas)", sel)
	}
	return nil
}

// presetSelectors renders the shipped presets as selectors, for messages.
// PresetSelectors are the shipped presets as selectors (preset:<name>).
func PresetSelectors() []string { return presetSelectors() }

func presetSelectors() []string {
	out := make([]string, 0, len(ShippedPresets))
	for _, p := range ShippedPresets {
		out = append(out, "preset:"+p)
	}
	return out
}

// removedArgument pairs a removed argument's raw value with its explanation.
type removedArgument struct {
	raw    json.RawMessage
	reason string
}

// rejectRemoved refuses the first removed argument a request still carries,
// with the reason it is gone. JSON null counts as absent.
func rejectRemoved(args ...removedArgument) error {
	for _, a := range args {
		if len(a.raw) == 0 || string(a.raw) == "null" {
			continue
		}
		return invalidf("%s", a.reason)
	}
	return nil
}

// removed lists the removed arguments of a create.
func (s Spec) removed() []removedArgument {
	return []removedArgument{{s.RemovedToolNames, toolNamesRemoved}, {s.RemovedRuntime, runtimeRemoved}}
}

// removed lists the removed arguments of an update.
func (u Update) removed() []removedArgument {
	return []removedArgument{{u.RemovedToolNames, toolNamesRemoved}, {u.RemovedRuntime, runtimeRemoved}}
}

// ParseToolsetHeader splits a rendered X-Muster-Toolset header value back into
// selectors (comma-separated, whitespace around selectors trimmed).
func ParseToolsetHeader(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		if sel := strings.TrimSpace(part); sel != "" {
			out = append(out, sel)
		}
	}
	return out
}

// stringSlice converts a values list to strings; nil when v is not a list.
func stringSlice(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
