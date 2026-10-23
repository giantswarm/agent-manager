package agents

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A Harness change runs the checks a create on the new Harness runs, and
// writes agent.harness only when they all pass.
func TestUpdateMovesAnAgentToAnotherHarness(t *testing.T) {
	f, ctx := codingFixture(t)

	dry, err := f.svc.ValidateUpdate(ctx, Update{Name: "verifier", Harness: new("claude")})
	require.NoError(t, err)
	assert.True(t, dry.Valid, dry.Errors)

	res, err := f.svc.Update(ctx, Update{Name: "verifier", Harness: new("claude")})
	require.NoError(t, err)
	assert.Contains(t, res.Changed, "agent.harness")
	agentBlock := mustValues(mustGet(t, f, "verifier"))["agent"].(map[string]any)
	assert.Equal(t, "claude", agentBlock["harness"])
	assert.Equal(t, "verifier", agentBlock["name"], "the name stays")
	assert.Equal(t, toAnySlice(f.svc.cfg.HarnessEgress["claude"]), agentBlock["egress"], "the new Harness's origins are written")
	assert.Contains(t, agentBlock["systemMessage"], egressNoteHeading)
}

func TestUpdateRefusesAHarnessItsChecksFail(t *testing.T) {
	f, ctx := codingFixture(t)

	// The verifier's Anthropic model is no model codex reaches.
	before := mustValues(mustGet(t, f, "verifier"))
	dry, err := f.svc.ValidateUpdate(ctx, Update{Name: "verifier", Harness: new("codex")})
	require.NoError(t, err)
	assert.False(t, dry.Valid)
	assert.True(t, containsLine(dry.Errors, "modelConfig default-model-config (provider Anthropic, model claude-sonnet-4-6): Harness codex runs Codex, which speaks the OpenAI Responses API only"), dry.Errors)
	_, err = f.svc.Update(ctx, Update{Name: "verifier", Harness: new("codex")})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "speaks the OpenAI Responses API only")
	assert.Equal(t, before, mustValues(mustGet(t, f, "verifier")), "a refused change writes nothing")

	// The model changed with the Harness passes.
	_, err = f.svc.Update(ctx, Update{Name: "verifier", Harness: new("codex"), ModelConfig: new("gpt-responses")})
	require.NoError(t, err)
	assert.Equal(t, "codex", mustValues(mustGet(t, f, "verifier"))["agent"].(map[string]any)["harness"])

	// A Harness the namespace does not have is refused with the ones it has.
	dry, err = f.svc.ValidateUpdate(ctx, Update{Name: "verifier", Harness: new("opencode")})
	require.NoError(t, err)
	assert.True(t, containsLine(dry.Errors, `harness "opencode" does not exist in namespace kagent; valid: claude, codex, kagent, omp`), dry.Errors)
	_, err = f.svc.Update(ctx, Update{Name: "verifier", Harness: new("opencode")})
	require.ErrorIs(t, err, ErrInvalid)
}

// A Claude-format plugin goes with an agent to claude, never to codex or omp.
func TestUpdateHarnessChecksThePlugins(t *testing.T) {
	f, ctx := codingFixture(t)
	spec := coderSpec("preset:full")
	spec.Plugins = Plugins{{Path: "claude-format", Git: &GitSkill{URL: skillsRepo, Commit: tagHead}}}
	_, err := f.svc.Create(ctx, spec)
	require.NoError(t, err)
	before := mustValues(mustGet(t, f, "coder"))

	for _, upd := range []Update{
		{Name: "coder", Harness: new("codex"), ModelConfig: new("gpt-responses")},
		{Name: "coder", Harness: new("omp")},
	} {
		dry, err := f.svc.ValidateUpdate(ctx, upd)
		require.NoError(t, err)
		assert.False(t, dry.Valid)
		assert.True(t, containsLine(dry.Errors, "plugins[0] ("+skillsRepo+") is a Claude-format plugin (.claude-plugin/plugin.json): Harness "+*upd.Harness+" runs"), dry.Errors)
		_, err = f.svc.Update(ctx, upd)
		require.ErrorIs(t, err, ErrInvalid, *upd.Harness)
		assert.Contains(t, err.Error(), "is a Claude-format plugin")
		assert.Equal(t, before, mustValues(mustGet(t, f, "coder")), "a refused change to %s writes nothing", *upd.Harness)
	}
}

// A claude agent binding a Claude-format plugin does not move to kagent,
// whose revision would never become Ready.
func TestUpdateHarnessToKagentChecksThePlugins(t *testing.T) {
	f, ctx := codingFixture(t)
	spec := coderSpec("preset:read-only")
	spec.Plugins = Plugins{{Path: "claude-format", Git: &GitSkill{URL: skillsRepo, Commit: tagHead}}}
	_, err := f.svc.Create(ctx, spec)
	require.NoError(t, err)
	before := mustValues(mustGet(t, f, "coder"))

	toKagent := Update{Name: "coder", Harness: new("kagent")}
	dry, err := f.svc.ValidateUpdate(ctx, toKagent)
	require.NoError(t, err)
	assert.False(t, dry.Valid)
	assert.True(t, containsLine(dry.Errors, "plugins[0] ("+skillsRepo+") is a Claude-format plugin (.claude-plugin/plugin.json): Harness kagent runs the kagent runtime"), dry.Errors)
	_, err = f.svc.Update(ctx, toKagent)
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "is a Claude-format plugin")
	assert.Equal(t, before, mustValues(mustGet(t, f, "coder")), "a refused change writes nothing")
}

// A coding Harness agent-manager has no origin list for is refused on a move
// as on a create, and so is an origin outside the new Harness's list.
func TestUpdateHarnessChecksTheEgress(t *testing.T) {
	f, ctx := codingFixture(t)
	mustCreate(t, f, f.svc.harnessGVR(), harness("kagent", "claude-go", ClaudeRuntime))
	before := mustValues(mustGet(t, f, "verifier"))

	for _, tc := range []struct {
		name   string
		update Update
		want   string
	}{
		{"no egress entry", Update{Name: "verifier", Harness: new("claude-go")},
			"Harness claude-go runs Claude Code but agent-manager has no egress entry for it"},
		{"origin outside the list", Update{Name: "verifier", Harness: new("claude"), Egress: &[]string{"https://example.com"}},
			`egress origin "https://example.com" is not one of Harness claude's origins`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dry, err := f.svc.ValidateUpdate(ctx, tc.update)
			require.NoError(t, err)
			assert.False(t, dry.Valid)
			assert.True(t, containsLine(dry.Errors, tc.want), dry.Errors)
			_, err = f.svc.Update(ctx, tc.update)
			require.ErrorIs(t, err, ErrInvalid)
			assert.Contains(t, err.Error(), tc.want)
			assert.Equal(t, before, mustValues(mustGet(t, f, "verifier")), "a refused change writes nothing")
		})
	}
}

// A binding a kagent agent may carry but a coding agent takes none of
// refuses the move; create_agent writes none of them, so the release is one
// written elsewhere.
func TestUpdateHarnessRefusesTheBindingsACodingAgentTakesNone(t *testing.T) {
	for _, tc := range []struct {
		name  string
		key   string
		value any
		want  string
	}{
		{"requireApproval", "muster", map[string]any{"requireApproval": true}, "takes no requireApproval binding"},
		{"extraTools", "extraTools", []any{map[string]any{"mcp": map[string]any{"server": map[string]any{"kind": "RemoteMCPServer", "name": "github"}}}}, "extraTools: a coding agent reaches tools through muster only"},
		{"extraAgentSpec.tools", "extraAgentSpec", map[string]any{"tools": []any{map[string]any{"type": "McpServer"}}}, "extraAgentSpec.tools: a coding agent reaches tools through muster only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, ctx := codingFixture(t)
			values := map[string]any{
				"agent":       map[string]any{"name": "binder", "harness": DefaultHarnessName},
				"modelConfig": map[string]any{"name": "default-model-config"},
				tc.key:        tc.value,
			}
			mustCreate(t, f, hrGVR, helmRelease("kagent", "binder", values, true, nil))
			before := mustValues(mustGet(t, f, "binder"))

			dry, err := f.svc.ValidateUpdate(ctx, Update{Name: "binder", Harness: new("claude")})
			require.NoError(t, err)
			assert.False(t, dry.Valid)
			assert.True(t, containsLine(dry.Errors, tc.want), dry.Errors)
			_, err = f.svc.Update(ctx, Update{Name: "binder", Harness: new("claude")})
			require.ErrorIs(t, err, ErrInvalid)
			assert.Contains(t, err.Error(), tc.want)
			assert.Equal(t, before, mustValues(mustGet(t, f, "binder")), "a refused change writes nothing")
		})
	}
}

// Every failing check is listed by the dry run, not the first alone.
func TestValidateUpdateListsEveryHarnessRefusal(t *testing.T) {
	f, ctx := codingFixture(t)
	mustCreate(t, f, f.svc.harnessGVR(), harness("kagent", "claude-go", ClaudeRuntime))
	values := map[string]any{
		"agent":       map[string]any{"name": "binder", "harness": DefaultHarnessName},
		"modelConfig": map[string]any{"name": "default-model-config"},
		"muster":      map[string]any{"requireApproval": true},
	}
	mustCreate(t, f, hrGVR, helmRelease("kagent", "binder", values, true, nil))

	dry, err := f.svc.ValidateUpdate(ctx, Update{Name: "binder", Harness: new("claude-go")})
	require.NoError(t, err)
	assert.False(t, dry.Valid)
	assert.True(t, containsLine(dry.Errors, "has no egress entry for it"), dry.Errors)
	assert.True(t, containsLine(dry.Errors, "takes no requireApproval binding"), dry.Errors)
}

// Leaving a coding Harness for a kagent one takes the platform's egress list
// and prompt note with it: they were the coding Harness's, not the agent's.
func TestUpdateFromACodingHarnessDropsThePlatformsEgress(t *testing.T) {
	f, ctx := codingFixture(t)
	_, err := f.svc.Create(ctx, coderSpec("preset:read-only"))
	require.NoError(t, err)

	res, err := f.svc.Update(ctx, Update{Name: "coder", Harness: new("kagent")})
	require.NoError(t, err)
	agentBlock := res.After["agent"].(map[string]any)
	assert.Equal(t, "kagent", agentBlock["harness"])
	assert.NotContains(t, agentBlock, "egress")
	assert.Equal(t, "You keep the GitOps repositories tidy.", agentBlock["systemMessage"])

	// Egress passed with the change is the agent's own and stays.
	_, err = f.svc.Update(ctx, Update{Name: "coder", Harness: new("claude")})
	require.NoError(t, err)
	res, err = f.svc.Update(ctx, Update{Name: "coder", Harness: new("kagent"), Egress: &[]string{"https://example.com"}})
	require.NoError(t, err)
	assert.Equal(t, []any{"https://example.com"}, res.After["agent"].(map[string]any)["egress"])
}

// "" selects the platform Harness, as an omitted harness does on create.
func TestUpdateEmptyHarnessIsThePlatforms(t *testing.T) {
	f, ctx := codingFixture(t)
	_, err := f.svc.Create(ctx, coderSpec("preset:read-only"))
	require.NoError(t, err)
	res, err := f.svc.Update(ctx, Update{Name: "coder", Harness: new("")})
	require.NoError(t, err)
	assert.Equal(t, DefaultHarnessName, res.After["agent"].(map[string]any)["harness"])
}
