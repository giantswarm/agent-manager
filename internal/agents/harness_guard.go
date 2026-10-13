package agents

import (
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// An agent on a Harness whose runtime is `claude` (spec.claude) runs Claude
// Code with Bash in a sandbox: its network is the platform's list for its
// toolchain. agent-manager writes that list at write time, on create_agent,
// update_agent and validate_agent, on the values it writes: a HelmRelease
// applied directly bypasses it. Which muster tools the agent reaches is its
// toolset, the person's choice as on any other agent. An agent on any other
// Harness is not touched.

// ClaudeRuntime is the Harness spec field that marks Claude Code.
const ClaudeRuntime = "claude"

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
// it are refused) and the system prompt carries the egress note. requested is
// the egress the caller asked for, nil when the write leaves it alone. Every
// refusal is returned; values are completed even when one is.
func (s *Service) guardClaude(harness string, values map[string]any, requested []string) []error {
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

	return append(errs, refuseApproval(values)...)
}

// refuseApproval refuses a requireApproval binding and tool bindings beside
// muster's: a claude Harness agent reaches its tools through muster only.
func refuseApproval(values map[string]any) []error {
	var errs []error
	if on, _, _ := unstructured.NestedBool(values, "muster", "requireApproval"); on {
		errs = append(errs, invalidf("muster.requireApproval: a claude Harness agent takes no requireApproval binding: it reaches its tools through muster only"))
	}
	if extra, _ := values["extraTools"].([]any); len(extra) > 0 {
		errs = append(errs, invalidf("extraTools: a claude Harness agent reaches tools through muster only (its toolset); drop the %d extra binding(s)", len(extra)))
	}
	if tools, found, _ := unstructured.NestedFieldNoCopy(values, "extraAgentSpec", "tools"); found && tools != nil {
		errs = append(errs, invalidf("extraAgentSpec.tools: a claude Harness agent reaches tools through muster only (its toolset)"))
	}
	return errs
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
