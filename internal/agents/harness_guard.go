package agents

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agent-manager/internal/identity"
	"github.com/giantswarm/agent-manager/internal/muster"
)

// An agent on a Harness whose runtime is `claude` (spec.claude) runs Claude
// Code with Bash in a sandbox: its writes are git and the forge's CLI from its
// workspace, so whatever it reaches through muster has to be read-only, and
// its network is the platform's list for its toolchain. agent-manager checks
// both at write time, on create_agent, update_agent and validate_agent, on the
// values it writes: a HelmRelease applied directly bypasses the check. An
// agent on any other Harness is not touched.

// ClaudeRuntime is the Harness spec field that marks Claude Code.
const ClaudeRuntime = "claude"

// DefaultMusterURL is the muster URL the agent chart composes when
// ComposeConfig.MusterURL is empty: the muster the agent will reach, so the
// one its toolset is resolved with.
const DefaultMusterURL = "http://muster.agent-platform.svc.cluster.local:8090/mcp"

// AgentPlatformPreset is the preset of the agent-platform tools (agent
// lifecycle and invocation): a coding agent reaches only the read-only ones
// (listing and reading agents and sessions), so it cannot create, change or
// invoke agents.
const AgentPlatformPreset = "preset:agent-platform"

// deniedTools are tool names a claude Harness agent never reaches, under any
// server prefix muster gives them.
var deniedTools = []string{"invoke_agent_instance"}

// ToolsetResolver resolves toolsets as the caller (muster's filter_tools).
type ToolsetResolver interface {
	ResolveToolsets(ctx context.Context, url, token string, toolsets ...[]string) ([]muster.Resolution, error)
}

// egressNoteHeading opens the egress note agent-manager composes into a
// claude Harness agent's system prompt; everything from it on is the
// platform's and is rewritten on every write.
const egressNoteHeading = "## Network access (set by the platform)"

// isClaudeHarness reports whether a Harness object runs Claude Code.
func isClaudeHarness(h *unstructured.Unstructured) bool {
	if h == nil {
		return false
	}
	_, found, _ := unstructured.NestedMap(h.Object, "spec", ClaudeRuntime)
	return found
}

// harnessEgress is the platform's origin list for a Harness: what a coding
// agent on it may reach besides what its revision compiles.
func (s *Service) harnessEgress(harness string) []string {
	return slices.Clone(s.cfg.HarnessEgress[harness])
}

// guardClaude checks and completes the values of an agent on a claude
// Harness: the egress becomes the Harness's list (requested origins outside
// it are refused), the system prompt carries the egress note, and the
// toolset must resolve, through muster as the caller, to read-only tools
// outside the deny list. requested is the egress the caller asked for, nil
// when the write leaves it alone. Every refusal is returned; values are
// completed even when one is.
func (s *Service) guardClaude(ctx context.Context, st *site, harness string, values map[string]any, requested []string) []error {
	var errs []error
	agentBlock, _ := values["agent"].(map[string]any)
	if agentBlock == nil {
		agentBlock = map[string]any{}
		values["agent"] = agentBlock
	}

	allowed := s.harnessEgress(harness)
	for _, origin := range requested {
		if !slices.Contains(allowed, origin) {
			errs = append(errs, invalidf("egress origin %q is not one of Harness %s's origins (%s): a claude Harness agent reaches its toolchain's origins only, which the platform sets", origin, harness, listOrNone(allowed)))
		}
	}
	if len(allowed) > 0 {
		agentBlock["egress"] = toAnySlice(allowed)
	} else {
		delete(agentBlock, "egress")
	}

	prompt, _ := agentBlock["systemMessage"].(string)
	agentBlock["systemMessage"] = withEgressNote(prompt, allowed)
	if err := ValidateSystemMessage(agentBlock["systemMessage"].(string)); err != nil {
		errs = append(errs, invalidf("systemMessage with the platform's egress note: %v", err))
	}

	errs = append(errs, refuseApproval(values)...)
	return append(errs, s.checkClaudeToolset(ctx, st, harness, values)...)
}

// refuseApproval refuses a requireApproval binding: a coding agent's muster
// tools are read-only and approving a call is muster's, so a binding asking
// for one has nothing to guard. Tool bindings beside muster's are refused
// too: their tools are not resolved through muster, so nobody has checked
// them.
func refuseApproval(values map[string]any) []error {
	var errs []error
	if on, _, _ := unstructured.NestedBool(values, "muster", "requireApproval"); on {
		errs = append(errs, invalidf("muster.requireApproval: a claude Harness agent takes no requireApproval binding: its muster tools are read-only, and approving a call is muster's"))
	}
	if extra, _ := values["extraTools"].([]any); len(extra) > 0 {
		errs = append(errs, invalidf("extraTools: a claude Harness agent reaches tools through muster only, where its toolset is checked; drop the %d extra binding(s)", len(extra)))
	}
	if tools, found, _ := unstructured.NestedFieldNoCopy(values, "extraAgentSpec", "tools"); found && tools != nil {
		errs = append(errs, invalidf("extraAgentSpec.tools: a claude Harness agent reaches tools through muster only, where its toolset is checked"))
	}
	return errs
}

// checkClaudeToolset resolves the toolset with muster's filter_tools as the
// caller and refuses an empty toolset, a tool without readOnlyHint, a denied
// tool and a selector that selects nothing: muster resolves the toolset again
// on every request, so a name that selects nothing today would let a tool
// appearing under it later reach the agent unchecked. A preset is the
// platform's and may select nothing (preset:none); the servers awaiting the
// caller's sign-in that only a preset spans are not refused either, since
// muster resolves the preset per request and admits the preset's tools only.
// A server or tool selector naming such a server is refused: its tools are
// unknown until the sign-in. Every failure to resolve refuses the write.
func (s *Service) checkClaudeToolset(ctx context.Context, st *site, harness string, values map[string]any) []error {
	selectors := stringSlice(values[ToolsetValuesKey])
	if len(selectors) == 0 {
		return []error{invalidf("toolset is required on a claude Harness agent (Harness %s): name its read-only tools, e.g. toolset: [\"preset:read-only\"]", harness)}
	}
	if s.cfg.Toolsets == nil {
		return []error{invalidf("toolset could not be checked: this agent-manager has no muster client, and a claude Harness agent is written only once its toolset is known to be read-only")}
	}
	url := orDefault(st.compose.MusterURL, DefaultMusterURL)
	token, _ := identity.TokenFromContext(ctx)
	toolsets := [][]string{selectors, {AgentPlatformPreset}}
	// muster reports the servers awaiting sign-in per toolset, so the
	// explicit selectors are resolved on their own to learn the ones they name.
	explicit := slices.DeleteFunc(slices.Clone(selectors), isPreset)
	if len(explicit) > 0 {
		toolsets = append(toolsets, explicit)
	}
	res, err := s.cfg.Toolsets.ResolveToolsets(ctx, url, token, toolsets...)
	if err == nil && len(res) != len(toolsets) {
		err = fmt.Errorf("muster answered %d resolutions for %d toolsets", len(res), len(toolsets))
	}
	if err != nil {
		return []error{invalidf("toolset could not be checked with muster's filter_tools (%v); a claude Harness agent is written only once its toolset is known to be read-only", err)}
	}
	got, platform := res[0], res[1]
	var errs []error
	var signIn []string
	if len(explicit) > 0 {
		signIn = slices.DeleteFunc(slices.Clone(res[2].RequiringAuth), func(s string) bool { return s == "" })
	}
	if len(signIn) > 0 {
		errs = append(errs, invalidf("toolset names servers whose tools are unknown until you sign in to them (%s): sign in through muster, then write the agent again", strings.Join(signIn, ", ")))
	}
	if unknown := unknownSelectors(got.Unmatched, signIn); len(unknown) > 0 {
		errs = append(errs, invalidf("toolset selectors resolve to no tool for you: %s; name existing read-only tools, servers or workflows, or a preset such as preset:read-only", strings.Join(unknown, ", ")))
	}
	platformWrites := map[string]bool{}
	for _, t := range platform.Tools {
		if !t.ReadOnly() {
			platformWrites[t.Name] = true
		}
	}
	var deny, writes []string
	for _, t := range got.Tools {
		switch {
		case platformWrites[t.Name] || isDeniedName(t.Name):
			deny = append(deny, t.Name)
		case !t.ReadOnly():
			writes = append(writes, t.Name)
		}
	}
	if len(deny) > 0 {
		sort.Strings(deny)
		errs = append(errs, invalidf("toolset reaches tools a claude Harness agent may not use (invoke_agent_instance and the %s tools that are not read-only): %s", AgentPlatformPreset, strings.Join(deny, ", ")))
	}
	if len(writes) > 0 {
		sort.Strings(writes)
		errs = append(errs, invalidf("toolset reaches tools that are not marked read-only (no readOnlyHint): %s; a claude Harness agent's writes are git and the forge's CLI from its workspace, so its muster tools are read-only (preset:read-only)", strings.Join(writes, ", ")))
	}
	return errs
}

// unknownSelectors are the unmatched selectors a claude Harness agent may not
// keep, sorted: every one but a preset, and but a server awaiting the
// caller's sign-in, which is refused for that already.
func unknownSelectors(unmatched, signIn []string) []string {
	var out []string
	for _, sel := range unmatched {
		if isPreset(sel) {
			continue
		}
		if server, ok := strings.CutPrefix(sel, "server:"); ok && slices.Contains(signIn, server) {
			continue
		}
		out = append(out, sel)
	}
	sort.Strings(out)
	return out
}

// isPreset reports whether a selector names one of the platform's presets.
func isPreset(sel string) bool {
	return strings.HasPrefix(sel, "preset:")
}

// isDeniedName matches a denied tool under any server prefix.
func isDeniedName(name string) bool {
	for _, d := range deniedTools {
		if name == d || strings.HasSuffix(name, "_"+d) {
			return true
		}
	}
	return false
}

// withEgressNote is prompt with the platform's egress note in place of any
// earlier one.
func withEgressNote(prompt string, origins []string) string {
	if i := strings.Index(prompt, egressNoteHeading); i >= 0 {
		prompt = prompt[:i]
	}
	prompt = strings.TrimRight(prompt, " \t\n")
	note := egressNote(origins)
	if prompt == "" {
		return note
	}
	return prompt + "\n\n" + note
}

// egressNote tells the agent what its sandbox reaches, so a denied request
// reads as policy rather than as an outage.
func egressNote(origins []string) string {
	reach := "no other origin"
	if len(origins) > 0 {
		reach = "only these origins: " + strings.Join(origins, ", ")
	}
	return egressNoteHeading + "\n\nBesides your model and your tools, your sandbox reaches " + reach +
		". A request to any other host is refused by the egress policy and shows up as a failed command or tool call (connection refused, reset or 403). It is not transient: do not retry it or work around it; say which host the task needs."
}

func listOrNone(list []string) string {
	if len(list) == 0 {
		return "none"
	}
	return strings.Join(list, ", ")
}
