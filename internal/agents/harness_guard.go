package agents

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// An agent on a Harness that does not run the `kagent` runtime (spec.claude,
// spec.codex, spec.byo, or any runtime added later) is a coding agent: it
// runs a coding CLI with a shell in a sandbox, and its network is the
// platform's list for its Harness. agent-manager writes that list at write
// time, on create_agent, update_agent and validate_agent, on the values it
// writes: a HelmRelease applied directly bypasses it. Which muster tools the
// agent reaches is its toolset, the person's choice as on any other agent. An
// agent on a kagent Harness is not touched, except that its plugins are held
// to the Agent Plugins format, as a codex or omp agent's are.

// The runtimes of a Harness: the one key of its spec besides the workload,
// the environment and the Substrate policy.
const (
	KagentRuntime = "kagent"
	ClaudeRuntime = "claude"
	CodexRuntime  = "codex"
	BYORuntime    = "byo"
)

// harnessSpecFields are the Harness spec fields that are no runtime.
var harnessSpecFields = map[string]bool{"workload": true, "env": true, "substrate": true}

// egressNoteHeading opens the egress note agent-manager composes into a
// coding agent's system prompt; everything from it on is the platform's and
// is rewritten on every write.
const egressNoteHeading = "## Network access (set by the platform)"

// toolchainOrigins is the egress note's name for the origins a Session's
// toolchains bring.
const toolchainOrigins = "the registries of the toolchains mounted under /opt/kagent/tools"

// harnessRuntime is the runtime a Harness object runs, empty for none (the
// platform Harness absent).
func harnessRuntime(h *unstructured.Unstructured) string {
	if h == nil {
		return ""
	}
	spec, _, _ := unstructured.NestedMap(h.Object, "spec")
	keys := make([]string, 0, 1)
	for key := range spec {
		if !harnessSpecFields[key] {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	if len(keys) == 0 {
		return ""
	}
	return keys[0]
}

// isCodingRuntime reports whether a Harness runtime runs a coding CLI: every
// runtime but kagent's.
func isCodingRuntime(runtime string) bool {
	return runtime != "" && runtime != KagentRuntime
}

// runtimeLabel names what a runtime runs, in messages.
func runtimeLabel(runtime string) string {
	switch runtime {
	case KagentRuntime:
		return "the kagent runtime"
	case ClaudeRuntime:
		return "Claude Code"
	case CodexRuntime:
		return "Codex"
	case BYORuntime:
		return "a coding agent of its own image (byo)"
	}
	return "a coding agent (runtime " + runtime + ")"
}

// harnessEgress is the platform's origin list for a Harness: what a coding
// agent on it may reach besides what its revision compiles. listed is false
// when the platform has no entry for the Harness at all, which an empty list
// (no origin on purpose) is not.
func (s *Service) harnessEgress(harness string) (origins []string, listed bool) {
	origins, listed = s.cfg.HarnessEgress[harness]
	return slices.Clone(origins), listed
}

// guardCoding checks and completes the values of an agent on a coding
// Harness: the egress becomes the Harness's list (requested origins outside
// it are refused), the system prompt carries the egress note, and the model
// and plugins are ones the runtime takes. requested is the egress the caller
// asked for, nil when the write leaves it alone. A Harness the platform has
// no origin entry for is refused: its agents would silently reach nothing.
// Every refusal is returned; values are completed even when one is.
func (s *Service) guardCoding(ctx context.Context, st *site, harness, runtime string, values map[string]any, requested []string) ([]error, error) {
	var errs []error
	agentBlock, _ := values["agent"].(map[string]any)
	if agentBlock == nil {
		agentBlock = map[string]any{}
		values["agent"] = agentBlock
	}

	allowed, listed := s.harnessEgress(harness)
	if !listed {
		errs = append(errs, invalidf("Harness %s runs %s but agent-manager has no egress entry for it (chart harness.egress.%s, --harness-egress): an agent on it would reach no origin; the platform adds the entry, an empty list when it should reach none", harness, runtimeLabel(runtime), harness))
	}
	for _, origin := range requested {
		if !slices.Contains(allowed, origin) {
			errs = append(errs, invalidf("egress origin %q is not one of Harness %s's origins (%s): a coding agent reaches its Harness's origins only, which the platform sets", origin, harness, listOrNone(allowed)))
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

	if err := refuseCodingEgressPolicies(harness, values); err != nil {
		errs = append(errs, err)
	}
	errs = append(errs, refuseApproval(runtime, values)...)
	if err := s.checkModelProtocol(ctx, st, harness, runtime, values); err != nil {
		if !isDomainError(err) {
			return nil, err
		}
		errs = append(errs, err)
	}
	return errs, nil
}

// refuseApproval refuses a requireApproval binding and tool bindings beside
// muster's: a coding agent reaches its tools through muster only, and a
// process its CLI starts can answer an approval itself.
func refuseApproval(runtime string, values map[string]any) []error {
	var errs []error
	if on, _, _ := unstructured.NestedBool(values, "muster", "requireApproval"); on {
		errs = append(errs, invalidf("muster.requireApproval: a coding agent takes no requireApproval binding: approval is no boundary against %s, which can answer it from its own sandbox; the agent reaches its tools through muster only, which authorizes every call as the person", runtimeLabel(runtime)))
	}
	if extra, _ := values["extraTools"].([]any); len(extra) > 0 {
		errs = append(errs, invalidf("extraTools: a coding agent reaches tools through muster only (its toolset); drop the %d extra binding(s)", len(extra)))
	}
	if tools, found, _ := unstructured.NestedFieldNoCopy(values, "extraAgentSpec", "tools"); found && tools != nil {
		errs = append(errs, invalidf("extraAgentSpec.tools: a coding agent reaches tools through muster only (its toolset)"))
	}
	return errs
}

// The ModelConfig providers and the OpenAI API format the runtimes' compilers
// accept.
const (
	providerAnthropic         = "Anthropic"
	providerAnthropicVertexAI = "AnthropicVertexAI"
	providerBedrock           = "Bedrock"
	providerOpenAI            = "OpenAI"
	openAIAPIFormatResponses  = "responses"
)

// checkModelProtocol refuses a ModelConfig the Harness's runtime cannot
// reach, by the rules its compiler in kagent enforces: claude speaks the
// Anthropic protocol (Anthropic, Bedrock, AnthropicVertexAI); codex the
// OpenAI Responses API (OpenAI with apiFormat responses, Bedrock gpt-*
// models). A byo runtime's protocol is its image's, unchecked here. A
// ModelConfig that is not there is requireModelConfig's to report.
func (s *Service) checkModelProtocol(ctx context.Context, st *site, harness, runtime string, values map[string]any) error {
	if runtime != ClaudeRuntime && runtime != CodexRuntime {
		return nil
	}
	name, _, _ := unstructured.NestedString(values, "modelConfig", "name")
	if name == "" {
		return nil
	}
	mc, err := s.getObject(ctx, st.agent, s.modelConfigGVR(), st.ns(), name, "modelconfig")
	if err != nil || mc == nil {
		return err
	}
	provider, _, _ := unstructured.NestedString(mc.Object, "spec", "provider")
	model, _, _ := unstructured.NestedString(mc.Object, "spec", "model")
	refuse := func(reason string) error {
		return invalidf("modelConfig %s (provider %s, model %s): Harness %s runs %s, which %s", name, provider, model, harness, runtimeLabel(runtime), reason)
	}
	switch runtime {
	case ClaudeRuntime:
		switch provider {
		case providerAnthropic, providerBedrock, providerAnthropicVertexAI:
			return nil
		}
		return refuse("speaks the Anthropic protocol only: pick an Anthropic, Bedrock or AnthropicVertexAI ModelConfig")
	default: // codex
		switch provider {
		case providerOpenAI:
			if format, _, _ := unstructured.NestedString(mc.Object, "spec", "openAI", "apiFormat"); format != openAIAPIFormatResponses {
				return refuse(fmt.Sprintf("speaks the OpenAI Responses API only: the ModelConfig's openAI.apiFormat is %q, not %q", orDefault(format, "chatCompletions"), openAIAPIFormatResponses))
			}
			return nil
		case providerBedrock:
			if !strings.HasPrefix(model, "gpt-") {
				return refuse("reaches only OpenAI gpt-* models on Bedrock")
			}
			return nil
		}
		return refuse("speaks the OpenAI Responses API only: pick an OpenAI ModelConfig with openAI.apiFormat responses, or a Bedrock gpt-* one")
	}
}

// PluginFormats tells a Claude-format plugin (a .claude-plugin/plugin.json
// manifest and no root plugin.json) from an Agent Plugins one. The skills
// resolver implements it for git sources.
type PluginFormats interface {
	// ClaudeFormatPlugin reports whether the plugin at path of repoURL at
	// commit is Claude-format.
	ClaudeFormatPlugin(ctx context.Context, repoURL, commit, path string) (bool, error)
}

// checkPlugins refuses a Claude-format git plugin on the Harnesses that read
// Agent Plugins only, kagent, codex and omp, which fail such an agent at
// boot; it is never converted. A plugin in accepted (the pins of a release
// that keeps its Harness) is not looked up again. An OCI plugin's format is
// known only once pulled: the runtime reports it. The rule is kagent's
// materialiser's, which ClaudeFormatPlugin copies.
func (s *Service) checkPlugins(ctx context.Context, harness string, h *unstructured.Unstructured, values map[string]any, accepted Plugins) []error {
	formats, ok := s.pinner.(PluginFormats)
	runs, only := agentPluginsReader(h)
	if !only || !ok {
		return nil
	}
	var errs []error
	for i, p := range pluginsFromValues(values) {
		if p.Git == nil || slices.ContainsFunc(accepted, p.samePin) {
			continue
		}
		claude, err := formats.ClaudeFormatPlugin(ctx, p.Git.URL, p.Git.Commit, p.Path)
		switch {
		case err != nil:
			errs = append(errs, invalidf("plugins[%d] (%s): its format could not be checked for Harness %s, which reads Agent Plugins only: %v", i, p.Git.URL, harness, err))
		case claude:
			errs = append(errs, invalidf("plugins[%d] (%s) is a Claude-format plugin (.claude-plugin/plugin.json): Harness %s runs %s, which reads the Agent Plugins format only (plugin.json at the plugin root); drop it or use its skills as skills", i, p.Git.URL, harness, runs))
		}
	}
	return errs
}

// ompHarnessImage is the repository name of the kagent line's omp-harness
// image, the one a BYO Harness runs oh-my-pi from.
const ompHarnessImage = "omp-harness"

// agentPluginsReader names what a Harness runs and reports whether it skips
// a Claude-format plugin's Claude part and so cannot load one: the kagent
// runtime, codex, and omp, whose adapter refuses a Claude-format plugin at
// boot. omp is a byo runtime, and spec.byo says nothing about what runs: a
// BYO Harness is omp by its workload image, the kagent line's omp-harness. Any
// other BYO image may read Claude-format plugins and is left alone.
func agentPluginsReader(h *unstructured.Unstructured) (string, bool) {
	switch runtime := harnessRuntime(h); runtime {
	case KagentRuntime, CodexRuntime:
		return runtimeLabel(runtime), true
	case BYORuntime:
		image, _, _ := unstructured.NestedString(h.Object, "spec", "workload", "image")
		if imageRepositoryName(image) == ompHarnessImage {
			return "oh-my-pi", true
		}
	}
	return "", false
}

// imageRepositoryName is the last path element of an image reference's
// repository, without its tag or digest.
func imageRepositoryName(image string) string {
	image, _, _ = strings.Cut(image, "@")
	name := image[strings.LastIndex(image, "/")+1:]
	name, _, _ = strings.Cut(name, ":")
	return name
}

// samePin reports whether o is p's git source at the same commit and path.
func (p Plugin) samePin(o Plugin) bool {
	return p.Git != nil && o.Git != nil && p.Git.URL == o.Git.URL && p.Git.Commit == o.Git.Commit && p.Path == o.Path
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
// reads as policy rather than as an outage. A Session's toolchains add their
// own origins, which differ per Session, so the note names them by place.
func egressNote(origins []string) string {
	reach := "only " + toolchainOrigins
	if len(origins) > 0 {
		reach = "only these origins: " + strings.Join(origins, ", ") + ", and " + toolchainOrigins
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
