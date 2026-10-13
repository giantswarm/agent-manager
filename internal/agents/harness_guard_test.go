package agents

import (
	"context"
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
	errs := f.svc.guardClaude("claude", values, nil)
	require.Len(t, errs, 1)
	assert.Contains(t, errs[0].Error(), "takes no requireApproval binding")

	withApproval := coderValues("preset:read-only")
	withApproval["muster"] = map[string]any{"requireApproval": true}
	mustCreate(t, f, hrGVR, helmRelease("kagent", "coder", withApproval, true, nil))
	dry, err := f.svc.ValidateUpdate(ctx, Update{Name: "coder", Description: str("tidy")})
	require.NoError(t, err)
	assert.True(t, containsLine(dry.Errors, "takes no requireApproval binding"), dry.Errors)
	_, err = f.svc.Update(ctx, Update{Name: "coder", Description: str("tidy")})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "takes no requireApproval binding")

	// A claude Harness agent reaches its tools through muster only.
	extra := coderValues("preset:read-only")
	extra["extraTools"] = []any{map[string]any{"mcp": map[string]any{"server": map[string]any{"kind": "RemoteMCPServer", "name": "github"}}}}
	assert.Len(t, refuseApproval(extra), 1)
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

	// A claude Harness the platform lists no origins for reaches none.
	mustCreate(t, f, f.svc.harnessGVR(), harness("kagent", "claude-go", "claude"))
	spec = coderSpec("preset:read-only")
	spec.Name, spec.Harness, spec.Egress = "gopher", "claude-go", []string{"https://proxy.golang.org"}
	dry, err = f.svc.ValidateCreate(ctx, spec)
	require.NoError(t, err)
	assert.True(t, containsLine(dry.Errors, "is not one of Harness claude-go's origins (none)"), dry.Errors)
}

func TestClaudeHarnessSystemPromptCarriesTheEgressNote(t *testing.T) {
	f, ctx := claudeFixture(t, nil)
	_, err := f.svc.Create(ctx, coderSpec("preset:read-only"))
	require.NoError(t, err)
	prompt := mustValues(mustGet(t, f, "coder"))["agent"].(map[string]any)["systemMessage"].(string)
	assert.True(t, strings.HasPrefix(prompt, "You keep the GitOps repositories tidy.\n\n"+egressNoteHeading), prompt)
	assert.Contains(t, prompt, "only these origins: https://github.com, https://*.githubusercontent.com")

	// A new prompt gets the note once, never a second copy.
	res, err := f.svc.Update(ctx, Update{Name: "coder", SystemMessage: str("You review pull requests.")})
	require.NoError(t, err)
	prompt = res.After["agent"].(map[string]any)["systemMessage"].(string)
	assert.True(t, strings.HasPrefix(prompt, "You review pull requests.\n\n"+egressNoteHeading), prompt)
	assert.Equal(t, 1, strings.Count(prompt, egressNoteHeading))
	res, err = f.svc.Update(ctx, Update{Name: "coder", Description: str("tidy")})
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
