package agents

import (
	"encoding/json"
	"regexp"
	"strings"
)

// A toolset is the list of selectors an agent declares — the chart's top-level
// `toolset` value, rendered by the agent chart as the X-Muster-Toolset header
// on the agent's muster tool entry — that bounds which of the gateway's tools
// the agent's meta-tools can see and call. agent-manager validates the inline
// grammar and composes the list exactly as given; resolving a toolset to tools
// is muster's job, per caller. It is composition, not authorization: the
// invoking human's identity and the backends' own authorization stay the
// boundary.

const (
	// ToolsetValuesKey is the agent chart's top-level value carrying the
	// toolset (never `muster.toolNames`, which only filters muster's
	// meta-tools).
	ToolsetValuesKey = "toolset"
	// ToolsetHeader is the header the chart renders for the muster tool entry
	// (Agent.spec.declarative.tools[].headersFrom[]).
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

// toolNamesRemoved explains the removed argument to a caller still passing it.
const toolNamesRemoved = `toolNames never narrowed anything against muster (kagent filters muster's meta-tools only); declare a toolset instead, e.g. toolset: ["preset:read-only"]`

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
func presetSelectors() []string {
	out := make([]string, 0, len(ShippedPresets))
	for _, p := range ShippedPresets {
		out = append(out, "preset:"+p)
	}
	return out
}

// rejectToolNames refuses the removed `toolNames` argument with the reason.
// JSON null counts as absent.
func rejectToolNames(raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return invalidf("%s", toolNamesRemoved)
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
