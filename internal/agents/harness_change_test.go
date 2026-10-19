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

	dry, err := f.svc.ValidateUpdate(ctx, Update{Name: "verifier", Harness: str("claude")})
	require.NoError(t, err)
	assert.True(t, dry.Valid, dry.Errors)

	res, err := f.svc.Update(ctx, Update{Name: "verifier", Harness: str("claude")})
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
	dry, err := f.svc.ValidateUpdate(ctx, Update{Name: "verifier", Harness: str("codex")})
	require.NoError(t, err)
	assert.False(t, dry.Valid)
	assert.True(t, containsLine(dry.Errors, "modelConfig default-model-config (provider Anthropic, model claude-sonnet-4-6): Harness codex runs Codex, which speaks the OpenAI Responses API only"), dry.Errors)
	_, err = f.svc.Update(ctx, Update{Name: "verifier", Harness: str("codex")})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "speaks the OpenAI Responses API only")

	// The model changed with the Harness passes.
	_, err = f.svc.Update(ctx, Update{Name: "verifier", Harness: str("codex"), ModelConfig: str("gpt-responses")})
	require.NoError(t, err)
	assert.Equal(t, "codex", mustValues(mustGet(t, f, "verifier"))["agent"].(map[string]any)["harness"])

	// A Harness the namespace does not have is refused with the ones it has.
	dry, err = f.svc.ValidateUpdate(ctx, Update{Name: "verifier", Harness: str("opencode")})
	require.NoError(t, err)
	assert.True(t, containsLine(dry.Errors, `harness "opencode" does not exist in namespace kagent; valid: claude, codex, kagent, omp`), dry.Errors)
	_, err = f.svc.Update(ctx, Update{Name: "verifier", Harness: str("opencode")})
	require.ErrorIs(t, err, ErrInvalid)
}

// A Claude-format plugin goes with an agent to claude, never to codex.
func TestUpdateHarnessChecksThePlugins(t *testing.T) {
	f, ctx := codingFixture(t)
	spec := coderSpec("preset:full")
	spec.Plugins = Plugins{{Path: "claude-format", Git: &GitSkill{URL: skillsRepo, Commit: tagHead}}}
	_, err := f.svc.Create(ctx, spec)
	require.NoError(t, err)

	_, err = f.svc.Update(ctx, Update{Name: "coder", Harness: str("codex"), ModelConfig: str("gpt-responses")})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "is a Claude-format plugin")
	_, err = f.svc.Update(ctx, Update{Name: "coder", Harness: str("omp")})
	require.NoError(t, err)
}

// Leaving a coding Harness for a kagent one takes the platform's egress list
// and prompt note with it: they were the coding Harness's, not the agent's.
func TestUpdateFromACodingHarnessDropsThePlatformsEgress(t *testing.T) {
	f, ctx := codingFixture(t)
	_, err := f.svc.Create(ctx, coderSpec("preset:read-only"))
	require.NoError(t, err)

	res, err := f.svc.Update(ctx, Update{Name: "coder", Harness: str("kagent")})
	require.NoError(t, err)
	agentBlock := res.After["agent"].(map[string]any)
	assert.Equal(t, "kagent", agentBlock["harness"])
	assert.NotContains(t, agentBlock, "egress")
	assert.Equal(t, "You keep the GitOps repositories tidy.", agentBlock["systemMessage"])

	// Egress passed with the change is the agent's own and stays.
	_, err = f.svc.Update(ctx, Update{Name: "coder", Harness: str("claude")})
	require.NoError(t, err)
	res, err = f.svc.Update(ctx, Update{Name: "coder", Harness: str("kagent"), Egress: &[]string{"https://example.com"}})
	require.NoError(t, err)
	assert.Equal(t, []any{"https://example.com"}, res.After["agent"].(map[string]any)["egress"])
}

// "" selects the platform Harness, as an omitted harness does on create.
func TestUpdateEmptyHarnessIsThePlatforms(t *testing.T) {
	f, ctx := codingFixture(t)
	_, err := f.svc.Create(ctx, coderSpec("preset:read-only"))
	require.NoError(t, err)
	res, err := f.svc.Update(ctx, Update{Name: "coder", Harness: str("")})
	require.NoError(t, err)
	assert.Equal(t, DefaultHarnessName, res.After["agent"].(map[string]any)["harness"])
}
