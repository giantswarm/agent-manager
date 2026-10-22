package agents

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agent-manager/internal/identity"
)

// claudeFixture is the seeded namespace plus a claude Harness and a coding
// agent's release on it.
func claudeFixture(t *testing.T, values map[string]any) (*fixture, context.Context) {
	t.Helper()
	f := seeded(t)
	mustCreate(t, f, f.svc.harnessGVR(), harness("kagent", "claude", "claude"))
	if values != nil {
		mustCreate(t, f, hrGVR, helmRelease("kagent", "coder", values, true, nil))
	}
	return f, identity.ContextWithToken(t.Context(), "caller-token")
}

func coderValues(toolset ...string) map[string]any {
	v := map[string]any{
		"agent":       map[string]any{"name": "coder", "harness": "claude", "systemMessage": "You keep the GitOps repositories tidy."},
		"modelConfig": map[string]any{"name": "default-model-config"},
	}
	if len(toolset) > 0 {
		v[ToolsetValuesKey] = toAnySlice(toolset)
	}
	return v
}

func coderSpec(toolset ...string) Spec {
	return Spec{Name: "coder", ModelConfig: "default-model-config", Harness: "claude", SystemMessage: "You keep the GitOps repositories tidy.", Toolset: toolset}
}

func containsLine(lines []string, want string) bool {
	for _, l := range lines {
		if strings.Contains(l, want) {
			return true
		}
	}
	return false
}

func mustGet(t *testing.T, f *fixture, name string) *unstructured.Unstructured {
	t.Helper()
	hr, err := f.dyn.Resource(hrGVR).Namespace("kagent").Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	return hr
}

// Which muster tools a coding agent reaches is the person's choice, as on any
// other agent: write tools, the agent-platform tools and agent invocation are
// written as given, on create and on update.
func TestClaudeHarnessTakesAnyToolset(t *testing.T) {
	for _, toolset := range [][]string{
		{"preset:read-only"},
		{"preset:full"},
		{"preset:agent-platform"},
		{"preset:read-only", "tool:x_kubernetes_delete"},
		{"tool:x_agent-manager_create_agent", "tool:x_kagent_invoke_agent_instance"},
		// kagent's Sandbox tools, none of which carries readOnlyHint.
		{"tool:x_kagent_list_sandbox_templates", "tool:x_kagent_create_sandbox", "tool:x_kagent_write_sandbox_file", "tool:x_kagent_start_sandbox_process", "tool:x_kagent_get_sandbox_process", "tool:x_kagent_read_sandbox_outputs", "tool:x_kagent_read_sandbox_file", "tool:x_kagent_suspend_sandbox", "tool:x_kagent_resume_sandbox", "tool:x_kagent_delete_sandbox"},
		{"server:kagent"},
		{"server:github", "workflow:incident-triage"},
	} {
		t.Run(strings.Join(toolset, ","), func(t *testing.T) {
			f, ctx := claudeFixture(t, coderValues("preset:read-only"))
			spec := coderSpec(toolset...)
			spec.Name = "fresh"
			dry, err := f.svc.ValidateCreate(ctx, spec)
			require.NoError(t, err)
			assert.True(t, dry.Valid, "validate create: %v", dry.Errors)
			_, err = f.svc.Create(ctx, spec)
			require.NoError(t, err)
			values := mustValues(mustGet(t, f, "fresh"))
			assert.Equal(t, toAnySlice(toolset), values[ToolsetValuesKey])
			assert.Equal(t, []any{"https://github.com", "https://*.githubusercontent.com"}, values["agent"].(map[string]any)["egress"], "the Harness's list is written on create")

			dryUpd, err := f.svc.ValidateUpdate(ctx, Update{Name: "coder", Toolset: &toolset})
			require.NoError(t, err)
			assert.True(t, dryUpd.Valid, "validate update: %v", dryUpd.Errors)
			_, err = f.svc.Update(ctx, Update{Name: "coder", Toolset: &toolset})
			require.NoError(t, err)
			assert.Equal(t, toAnySlice(toolset), mustValues(mustGet(t, f, "coder"))[ToolsetValuesKey])
		})
	}
}

func TestClaudeHarnessRefusesRequireApproval(t *testing.T) {
	// create_agent writes no requireApproval, so on create the guard meets it
	// only in values it composes; update meets it on a release written
	// elsewhere.
	f, ctx := claudeFixture(t, nil)
	st, err := f.svc.site(ctx, Location{})
	require.NoError(t, err)
	values := BuildValues(coderSpec("preset:read-only"), st.compose)
	values["muster"].(map[string]any)["requireApproval"] = true
	errs, err := f.svc.guardCoding(ctx, st, "claude", ClaudeRuntime, values, nil)
	require.NoError(t, err)
	require.Len(t, errs, 1)
	assert.Contains(t, errs[0].Error(), "takes no requireApproval binding")
	assert.Contains(t, errs[0].Error(), "no boundary against Claude Code")

	withApproval := coderValues("preset:read-only")
	withApproval["muster"] = map[string]any{"requireApproval": true}
	mustCreate(t, f, hrGVR, helmRelease("kagent", "coder", withApproval, true, nil))
	dry, err := f.svc.ValidateUpdate(ctx, Update{Name: "coder", Description: new("tidy")})
	require.NoError(t, err)
	assert.True(t, containsLine(dry.Errors, "takes no requireApproval binding"), dry.Errors)
	_, err = f.svc.Update(ctx, Update{Name: "coder", Description: new("tidy")})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "takes no requireApproval binding")

	// A claude Harness agent reaches its tools through muster only.
	extra := coderValues("preset:read-only")
	extra["extraTools"] = []any{map[string]any{"mcp": map[string]any{"server": map[string]any{"kind": "RemoteMCPServer", "name": "github"}}}}
	assert.Len(t, refuseApproval(ClaudeRuntime, extra), 1)
}

func TestClaudeHarnessEgressIsTheHarnessList(t *testing.T) {
	f, ctx := claudeFixture(t, coderValues("preset:read-only"))
	spec := coderSpec("preset:read-only")
	spec.Name = "other"
	spec.Egress = []string{"https://github.com", "https://registry.npmjs.org"}
	dry, err := f.svc.ValidateCreate(ctx, spec)
	require.NoError(t, err)
	assert.False(t, dry.Valid)
	assert.True(t, containsLine(dry.Errors, `egress origin "https://registry.npmjs.org" is not one of Harness claude's origins (https://github.com, https://*.githubusercontent.com)`), dry.Errors)
	_, err = f.svc.Create(ctx, spec)
	require.ErrorIs(t, err, ErrInvalid)

	// An origin of the list is accepted, and the whole list is written.
	spec.Egress = []string{"https://github.com"}
	_, err = f.svc.Create(ctx, spec)
	require.NoError(t, err)
	assert.Equal(t, []any{"https://github.com", "https://*.githubusercontent.com"}, mustValues(mustGet(t, f, "other"))["agent"].(map[string]any)["egress"])

	_, err = f.svc.Update(ctx, Update{Name: "coder", Egress: &[]string{"https://example.com"}})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), `egress origin "https://example.com" is not one of Harness claude's origins`)

	// An update rewrites the list, also on a release written before the guard.
	res, err := f.svc.Update(ctx, Update{Name: "coder", Egress: &[]string{}})
	require.NoError(t, err)
	assert.Equal(t, []any{"https://github.com", "https://*.githubusercontent.com"}, res.After["agent"].(map[string]any)["egress"])

	// A claude Harness the platform lists an empty list for reaches none.
	f.svc.cfg.HarnessEgress["claude-offline"] = []string{}
	mustCreate(t, f, f.svc.harnessGVR(), harness("kagent", "claude-offline", "claude"))
	spec = coderSpec("preset:read-only")
	spec.Name, spec.Harness = "hermit", "claude-offline"
	_, err = f.svc.Create(ctx, spec)
	require.NoError(t, err)
	assert.NotContains(t, mustValues(mustGet(t, f, "hermit"))["agent"].(map[string]any), "egress")
	spec.Name, spec.Egress = "hermit-2", []string{"https://proxy.golang.org"}
	dry, err = f.svc.ValidateCreate(ctx, spec)
	require.NoError(t, err)
	assert.True(t, containsLine(dry.Errors, "is not one of Harness claude-offline's origins (none)"), dry.Errors)
}

// A claude Harness the platform has no egress entry for is refused with a
// message naming the missing entry, never silently given no origins.
func TestClaudeHarnessWithoutEgressEntryIsRefused(t *testing.T) {
	const missing = "Harness claude-go runs Claude Code but agent-manager has no egress entry for it (chart harness.egress.claude-go"
	f, ctx := claudeFixture(t, nil)
	mustCreate(t, f, f.svc.harnessGVR(), harness("kagent", "claude-go", "claude"))
	spec := coderSpec("preset:read-only")
	spec.Name, spec.Harness = "gopher", "claude-go"

	dry, err := f.svc.ValidateCreate(ctx, spec)
	require.NoError(t, err)
	assert.False(t, dry.Valid)
	assert.True(t, containsLine(dry.Errors, missing), dry.Errors)
	_, err = f.svc.Create(ctx, spec)
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), missing)

	// A release written elsewhere meets the refusal on update.
	onGo := coderValues("preset:read-only")
	onGo["agent"].(map[string]any)["harness"] = "claude-go"
	mustCreate(t, f, hrGVR, helmRelease("kagent", "coder", onGo, true, nil))
	_, err = f.svc.Update(ctx, Update{Name: "coder", Description: new("tidy")})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), missing)
}

func TestClaudeHarnessSystemPromptCarriesTheEgressNote(t *testing.T) {
	f, ctx := claudeFixture(t, nil)
	_, err := f.svc.Create(ctx, coderSpec("preset:read-only"))
	require.NoError(t, err)
	prompt := mustValues(mustGet(t, f, "coder"))["agent"].(map[string]any)["systemMessage"].(string)
	assert.True(t, strings.HasPrefix(prompt, "You keep the GitOps repositories tidy.\n\n"+egressNoteHeading), prompt)
	assert.Contains(t, prompt, "only these origins: https://github.com, https://*.githubusercontent.com, and the registries of the toolchains mounted under /opt/kagent/tools.")

	// A new prompt gets the note once, never a second copy.
	res, err := f.svc.Update(ctx, Update{Name: "coder", SystemMessage: new("You review pull requests.")})
	require.NoError(t, err)
	prompt = res.After["agent"].(map[string]any)["systemMessage"].(string)
	assert.True(t, strings.HasPrefix(prompt, "You review pull requests.\n\n"+egressNoteHeading), prompt)
	assert.Equal(t, 1, strings.Count(prompt, egressNoteHeading))
	res, err = f.svc.Update(ctx, Update{Name: "coder", Description: new("tidy")})
	require.NoError(t, err)
	assert.Equal(t, []string{"agent.description"}, res.Changed, "an unchanged list rewrites nothing")

	// Without a prompt of its own the note is the prompt.
	assert.Equal(t, egressNote(nil), withEgressNote("", nil))
}

func TestDeclarativeAgentsAreUnchanged(t *testing.T) {
	f, ctx := claudeFixture(t, nil)
	spec := Spec{Name: "helper", ModelConfig: "default-model-config", SystemMessage: "Help.", Toolset: []string{"preset:full"}, Egress: []string{"https://example.com"}}
	dry, err := f.svc.ValidateCreate(ctx, spec)
	require.NoError(t, err)
	assert.True(t, dry.Valid, dry.Errors)
	_, err = f.svc.Create(ctx, spec)
	require.NoError(t, err)
	agentBlock := mustValues(mustGet(t, f, "helper"))["agent"].(map[string]any)
	assert.Equal(t, "Help.", agentBlock["systemMessage"])
	assert.Equal(t, []any{"https://example.com"}, agentBlock["egress"])

	_, err = f.svc.Update(ctx, Update{Name: "verifier", Toolset: &[]string{"preset:full"}, Egress: &[]string{"https://example.com"}})
	require.NoError(t, err)
}

// codingFixture is the seeded namespace plus the three coding Harnesses,
// claude, codex and omp (a byo runtime), each with its egress entry, and the
// ModelConfigs each runtime takes.
func codingFixture(t *testing.T) (*fixture, context.Context) {
	t.Helper()
	f, ctx := claudeFixture(t, nil)
	mustCreate(t, f, f.svc.harnessGVR(), harness("kagent", "codex", CodexRuntime))
	mustCreate(t, f, f.svc.harnessGVR(), harness("kagent", "omp", BYORuntime))
	f.svc.cfg.HarnessEgress["codex"] = []string{"https://github.com"}
	f.svc.cfg.HarnessEgress["omp"] = []string{"https://github.com", "https://registry.npmjs.org"}
	responses := modelConfig("kagent", "gpt-responses", "OpenAI", "gpt-5.1-codex")
	responses.Object["spec"].(map[string]any)["openAI"] = map[string]any{"apiFormat": "responses"}
	mustCreate(t, f, f.svc.modelConfigGVR(), responses)
	mustCreate(t, f, f.svc.modelConfigGVR(), modelConfig("kagent", "bedrock-gpt", "Bedrock", "gpt-oss-120b"))
	mustCreate(t, f, f.svc.modelConfigGVR(), modelConfig("kagent", "bedrock-claude", "Bedrock", "anthropic.claude-sonnet-4-6"))
	return f, ctx
}

// codingModel is the ModelConfig a coding Harness's runtime reaches.
var codingModel = map[string]string{"claude": "default-model-config", "codex": "gpt-responses", "omp": "default-model-config"}

// The guard follows the runtime kind: every Harness that does not run the
// kagent runtime gets the claude Harness's refusals, takes muster write
// tools, and has its Harness's origins written.
func TestCodingGuardOnEveryNonKagentHarness(t *testing.T) {
	for _, h := range []string{"claude", "codex", "omp"} {
		t.Run(h, func(t *testing.T) {
			f, ctx := codingFixture(t)
			spec := coderSpec("preset:read-only", "tool:x_kubernetes_delete", "tool:x_github_create_pull_request")
			spec.Harness, spec.ModelConfig = h, codingModel[h]
			dry, err := f.svc.ValidateCreate(ctx, spec)
			require.NoError(t, err)
			assert.True(t, dry.Valid, "a muster write tool on %s: %v", h, dry.Errors)
			_, err = f.svc.Create(ctx, spec)
			require.NoError(t, err)
			agentBlock := mustValues(mustGet(t, f, "coder"))["agent"].(map[string]any)
			assert.Equal(t, toAnySlice(f.svc.cfg.HarnessEgress[h]), agentBlock["egress"], "the Harness's origins are written")
			assert.Contains(t, agentBlock["systemMessage"], egressNoteHeading)

			for field, value := range map[string]any{
				"muster":         map[string]any{"requireApproval": true},
				"extraTools":     []any{map[string]any{"mcp": map[string]any{"server": map[string]any{"kind": "RemoteMCPServer", "name": "github"}}}},
				"extraAgentSpec": map[string]any{"tools": []any{map[string]any{"type": "McpServer"}}},
			} {
				written := coderValues("preset:read-only")
				written["agent"].(map[string]any)["harness"] = h
				written["modelConfig"] = map[string]any{"name": codingModel[h]}
				written[field] = value
				require.NoError(t, f.dyn.Resource(hrGVR).Namespace("kagent").Delete(ctx, "coder", metav1.DeleteOptions{}))
				mustCreate(t, f, hrGVR, helmRelease("kagent", "coder", written, true, nil))
				dryUpd, err := f.svc.ValidateUpdate(ctx, Update{Name: "coder", Description: new("tidy")})
				require.NoError(t, err)
				assert.False(t, dryUpd.Valid, "%s on %s", field, h)
				assert.True(t, containsLine(dryUpd.Errors, field), "%s on %s: %v", field, h, dryUpd.Errors)
				_, err = f.svc.Update(ctx, Update{Name: "coder", Description: new("tidy")})
				require.ErrorIs(t, err, ErrInvalid, "%s on %s", field, h)
			}
		})
	}
}

// A coding Harness added later, a fourth runtime, inherits the guard; a
// Harness without an egress entry is refused naming what it runs.
func TestCodingGuardInheritedByANewRuntime(t *testing.T) {
	f, ctx := codingFixture(t)
	mustCreate(t, f, f.svc.harnessGVR(), harness("kagent", "opencode", "opencode"))
	spec := coderSpec("preset:read-only")
	spec.Harness = "opencode"
	dry, err := f.svc.ValidateCreate(ctx, spec)
	require.NoError(t, err)
	assert.True(t, containsLine(dry.Errors, "Harness opencode runs a coding agent (runtime opencode) but agent-manager has no egress entry for it"), dry.Errors)

	spec.Harness = "codex-2"
	mustCreate(t, f, f.svc.harnessGVR(), harness("kagent", "codex-2", CodexRuntime))
	spec.ModelConfig = "gpt-responses"
	dry, err = f.svc.ValidateCreate(ctx, spec)
	require.NoError(t, err)
	assert.True(t, containsLine(dry.Errors, "Harness codex-2 runs Codex but agent-manager has no egress entry for it"), dry.Errors)
}

// validate_agent refuses a model the runtime cannot reach, by the rules of
// its compiler in kagent.
func TestCodingGuardRefusesAModelTheRuntimeCannotReach(t *testing.T) {
	f, ctx := codingFixture(t)
	for _, c := range []struct {
		harness, model string
		refusal        string
	}{
		{"codex", "default-model-config", "speaks the OpenAI Responses API only: pick an OpenAI ModelConfig"},
		{"codex", "qwen3-8-27b", `openAI.apiFormat is "chatCompletions", not "responses"`},
		{"codex", "bedrock-claude", "reaches only OpenAI gpt-* models on Bedrock"},
		{"codex", "gpt-responses", ""},
		{"codex", "bedrock-gpt", ""},
		{"omp", "default-model-config", ""},
		{"claude", "default-model-config", ""},
		{"claude", "bedrock-claude", ""},
		{"claude", "qwen3-8-27b", "speaks the Anthropic protocol only"},
	} {
		spec := coderSpec("preset:read-only")
		spec.Harness, spec.ModelConfig = c.harness, c.model
		dry, err := f.svc.ValidateCreate(ctx, spec)
		require.NoError(t, err)
		if c.refusal == "" {
			assert.True(t, dry.Valid, "%s on %s: %v", c.model, c.harness, dry.Errors)
			continue
		}
		assert.True(t, containsLine(dry.Errors, "modelConfig "+c.model), "%s on %s: %v", c.model, c.harness, dry.Errors)
		assert.True(t, containsLine(dry.Errors, c.refusal), "%s on %s: %v", c.model, c.harness, dry.Errors)
		_, err = f.svc.Create(ctx, spec)
		require.ErrorIs(t, err, ErrInvalid)
	}

	// An update that moves a codex agent to a model it cannot reach is refused.
	spec := coderSpec("preset:read-only")
	spec.Harness, spec.ModelConfig = "codex", "gpt-responses"
	_, err := f.svc.Create(ctx, spec)
	require.NoError(t, err)
	_, err = f.svc.Update(ctx, Update{Name: "coder", ModelConfig: new("default-model-config")})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "speaks the OpenAI Responses API only")
}

// One agent definition (prompt, skills, the muster toolset) passes on every
// coding Harness; a Claude-format plugin on codex is refused with the
// reason, an Agent Plugins one taken.
func TestOneDefinitionOnEveryCodingHarness(t *testing.T) {
	f, ctx := codingFixture(t)
	for _, h := range []string{"claude", "codex", "omp"} {
		spec := coderSpec("preset:full")
		spec.Harness, spec.ModelConfig = h, codingModel[h]
		spec.Skills = Skills{{Name: "runbooks", Path: "nested/runbooks", Git: &GitSkill{URL: skillsRepo, Ref: "main"}}}
		dry, err := f.svc.ValidateCreate(ctx, spec)
		require.NoError(t, err)
		assert.True(t, dry.Valid, "on %s: %v", h, dry.Errors)
	}

	claudePlugin := Plugin{Path: "claude-format", Git: &GitSkill{URL: skillsRepo, Commit: tagHead}}
	agentPlugin := Plugin{Path: "bundles/sre", Git: &GitSkill{URL: skillsRepo, Commit: tagHead}, Skills: []string{"triage"}}
	spec := coderSpec("preset:full")
	spec.Harness, spec.ModelConfig = "codex", "gpt-responses"
	spec.Plugins = Plugins{agentPlugin, claudePlugin}
	dry, err := f.svc.ValidateCreate(ctx, spec)
	require.NoError(t, err)
	assert.False(t, dry.Valid)
	assert.True(t, containsLine(dry.Errors, "plugins[1] ("+skillsRepo+") is a Claude-format plugin (.claude-plugin/plugin.json): Harness codex runs Codex, which reads the Agent Plugins format only"), dry.Errors)
	assert.Len(t, dry.Errors, 1, "the Agent Plugins bundle is taken")
	_, err = f.svc.Create(ctx, spec)
	require.ErrorIs(t, err, ErrInvalid)

	spec.Harness, spec.ModelConfig = "claude", "default-model-config"
	dry, err = f.svc.ValidateCreate(ctx, spec)
	require.NoError(t, err)
	assert.True(t, dry.Valid, "Claude Code reads a Claude-format plugin: %v", dry.Errors)
}

// codex and omp are accepted where the namespace has them and refused, with
// the valid names, where it has not.
func TestCodingHarnessesByName(t *testing.T) {
	f, ctx := claudeFixture(t, nil)
	for _, h := range []string{"codex", "omp"} {
		spec := coderSpec("preset:read-only")
		spec.Harness = h
		dry, err := f.svc.ValidateCreate(ctx, spec)
		require.NoError(t, err)
		assert.True(t, containsLine(dry.Errors, `harness "`+h+`" does not exist in namespace kagent; valid: claude, kagent`), dry.Errors)
	}
}

// The kagent runtime reads the Agent Plugins format only: a Claude-format git
// plugin is refused on create, an Agent Plugins one taken, an OCI one left to
// the golden boot.
func TestKagentHarnessRefusesClaudeFormatPlugins(t *testing.T) {
	f, ctx := claudeFixture(t, nil)
	spec := Spec{Name: "helper", ModelConfig: "default-model-config", SystemMessage: "Help.", Toolset: []string{"preset:read-only"}}
	spec.Plugins = Plugins{{Path: "claude-format", Git: &GitSkill{URL: skillsRepo, Commit: tagHead}}}
	dry, err := f.svc.ValidateCreate(ctx, spec)
	require.NoError(t, err)
	assert.False(t, dry.Valid)
	assert.True(t, containsLine(dry.Errors, "plugins[0] ("+skillsRepo+") is a Claude-format plugin (.claude-plugin/plugin.json): Harness kagent runs the kagent runtime, which reads the Agent Plugins format only (plugin.json at the plugin root)"), dry.Errors)
	_, err = f.svc.Create(ctx, spec)
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "is a Claude-format plugin")

	spec.Plugins = Plugins{{Path: "bundles/sre", Git: &GitSkill{URL: skillsRepo, Commit: tagHead}, Skills: []string{"triage"}}}
	dry, err = f.svc.ValidateCreate(ctx, spec)
	require.NoError(t, err)
	assert.True(t, dry.Valid, "an Agent Plugins package is taken: %v", dry.Errors)

	f.pinner.formatErr = errors.New("GitHub answered 503")
	dry, err = f.svc.ValidateCreate(ctx, spec)
	require.NoError(t, err)
	assert.True(t, containsLine(dry.Errors, "plugins[0] ("+skillsRepo+"): its format could not be checked for Harness kagent"), dry.Errors)

	f.pinner.calls = nil
	spec.Plugins = Plugins{{OCI: kubectlRef + "@" + kubectlSum, Skills: []string{"k8s"}}}
	_, err = f.svc.Create(ctx, spec)
	require.NoError(t, err)
	assert.Empty(t, f.pinner.calls, "an OCI plugin's format is not looked up")
}

// A plugin accepted on a write is not looked up again by an update that
// keeps the Harness and the pins: a GitHub outage does not block a prompt or
// model edit. A changed pin or a Harness change is looked up, and a lookup
// that fails refuses.
func TestUnchangedPluginPinsAreNotLookedUp(t *testing.T) {
	for _, tc := range []struct {
		harness, model, otherHarness, otherModel string
	}{
		{"kagent", "default-model-config", "codex", "gpt-responses"},
		{"codex", "gpt-responses", "kagent", "default-model-config"},
	} {
		t.Run(tc.harness, func(t *testing.T) {
			f, ctx := codingFixture(t)
			spec := coderSpec("preset:read-only")
			spec.Harness, spec.ModelConfig = tc.harness, tc.model
			spec.Plugins = Plugins{{Path: "bundles/sre", Git: &GitSkill{URL: skillsRepo, Commit: tagHead}, Skills: []string{"triage"}}}
			_, err := f.svc.Create(ctx, spec)
			require.NoError(t, err)

			f.pinner.formatErr = errors.New("GitHub answered 403: rate limit exceeded")
			f.pinner.calls = nil
			_, err = f.svc.Update(ctx, Update{Name: "coder", SystemMessage: new("You review pull requests.")})
			require.NoError(t, err)
			_, err = f.svc.Update(ctx, Update{Name: "coder", Plugins: &spec.Plugins})
			require.NoError(t, err, "the same pins passed again are the stored ones")
			assert.Empty(t, f.pinner.calls, "no plugin format lookup")

			for name, upd := range map[string]Update{
				"changed pin":    {Name: "coder", Plugins: &Plugins{{Path: "bundles/sre", Git: &GitSkill{URL: skillsRepo, Commit: mainHead}, Skills: []string{"triage"}}}},
				"Harness change": {Name: "coder", Harness: new(tc.otherHarness), ModelConfig: new(tc.otherModel)},
			} {
				dry, err := f.svc.ValidateUpdate(ctx, upd)
				require.NoError(t, err)
				assert.True(t, containsLine(dry.Errors, "plugins[0] ("+skillsRepo+"): its format could not be checked"), "%s: %v", name, dry.Errors)
				_, err = f.svc.Update(ctx, upd)
				require.ErrorIs(t, err, ErrInvalid, name)
			}
		})
	}
}
