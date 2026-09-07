package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/agent-manager/internal/chart"
)

func TestValidateToolsetGrammar(t *testing.T) {
	many := make([]string, MaxToolsetSelectors+1)
	for i := range many {
		many[i] = fmt.Sprintf("tool:x_a_%d", i)
	}

	cases := []struct {
		name     string
		toolset  []string
		required bool
		wantErr  []string // substrings; empty = valid
	}{
		{name: "absent on create is refused naming every shipped preset", toolset: nil, required: true,
			wantErr: []string{"toolset is required", "preset:read-only", "preset:none", "preset:infrastructure", "preset:agent-platform", "preset:full", "chat-only", "every tool the gateway exposes"}},
		{name: "absent on update is unchanged", toolset: nil, required: false},
		{name: "empty list points at preset:none", toolset: []string{}, required: true, wantErr: []string{"preset:none", "empty"}},
		{name: "empty list on update too", toolset: []string{}, required: false, wantErr: []string{"preset:none"}},
		{name: "one selector of each kind", toolset: []string{"preset:read-only", "server:github", "workflow:incident-triage", "tool:x_mcp-kubernetes_get_pods"}},
		{name: "exactly the cap", toolset: many[:MaxToolsetSelectors]},
		{name: "one over the cap says define a preset", toolset: many, wantErr: []string{"33 selectors", "define a preset"}},
		{name: "toolset: is reserved", toolset: []string{"preset:read-only", "toolset:shared"}, wantErr: []string{`"toolset:shared"`, "reserved"}},
		{name: "label: is preset-only", toolset: []string{"label:agent-platform.giantswarm.io/tool-group=infrastructure"}, wantErr: []string{"label:", "presets only", "preset:infrastructure"}},
		{name: "unknown kind", toolset: []string{"pattern:core_*"}, wantErr: []string{`"pattern:core_*"`, "preset:<name>, server:<name>, workflow:<name> or tool:<name>"}},
		{name: "missing name", toolset: []string{"server:"}, wantErr: []string{`"server:"`}},
		{name: "whitespace is not trimmed", toolset: []string{" preset:read-only"}, wantErr: []string{`" preset:read-only"`}},
		{name: "commas belong to the header, not a selector", toolset: []string{"preset:read-only,preset:none"}, wantErr: []string{"no whitespace or commas"}},
		{name: "kinds are case-sensitive", toolset: []string{"Preset:read-only"}, wantErr: []string{`"Preset:read-only"`}},
		{name: "empty selector", toolset: []string{""}, wantErr: []string{`""`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateToolset(tc.toolset, tc.required)
			if len(tc.wantErr) == 0 {
				assert.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, ErrInvalid)
			for _, want := range tc.wantErr {
				assert.Contains(t, err.Error(), want)
			}
		})
	}
}

func TestRejectToolNamesExplainsTheRemoval(t *testing.T) {
	assert.NoError(t, rejectToolNames(nil))
	assert.NoError(t, rejectToolNames(json.RawMessage("null")))
	for _, raw := range []string{`["x_a_b"]`, `[]`, `"x"`} {
		err := rejectToolNames(json.RawMessage(raw))
		require.ErrorIs(t, err, ErrInvalid, raw)
		assert.Contains(t, err.Error(), "toolNames never narrowed anything against muster")
		assert.Contains(t, err.Error(), "kagent filters muster's meta-tools only")
		assert.Contains(t, err.Error(), `toolset: ["preset:read-only"]`)
	}
}

func TestParseToolsetHeader(t *testing.T) {
	assert.Equal(t, []string{"preset:read-only", "workflow:incident-triage"}, ParseToolsetHeader("preset:read-only, workflow:incident-triage"))
	assert.Equal(t, []string{"preset:none"}, ParseToolsetHeader("preset:none"))
	assert.Nil(t, ParseToolsetHeader(" , "))
}

// declaringChart is a schema source whose schema declares `toolset` the way
// the agent chart does once it ships the value (top-level array, 1..32
// strings) — the registry schema after that chart release.
type declaringChart struct{}

func (declaringChart) Schema(context.Context) chart.Schema {
	s := chart.EmbeddedSchema()
	doc := s.Document.(map[string]any)
	props := doc["properties"].(map[string]any)
	props[ToolsetValuesKey] = map[string]any{
		"type": "array", "minItems": 1, "maxItems": MaxToolsetSelectors,
		"items": map[string]any{"type": "string", "pattern": `^(preset|server|workflow|tool):[^\s,]+$`},
	}
	s.Version, s.Source = "0.6.0", chart.SourceRegistry
	return s
}

func TestToolsetValidationDoesNotDependOnTheSchemaKnowingTheKey(t *testing.T) {
	ctx := context.Background()
	values := BuildValues(Spec{Name: "sre", ModelConfig: "mc", Toolset: []string{"preset:read-only", "workflow:incident-triage"}})
	assert.Equal(t, []any{"preset:read-only", "workflow:incident-triage"}, values[ToolsetValuesKey])
	_, hasMuster := values["muster"]
	assert.False(t, hasMuster, "never muster.toolNames")

	// The embedded schema (additionalProperties: false, no toolset) predates
	// the value: the key is left out of the schema check, the rest is judged.
	sch, violations := ValidateValues(ctx, embeddedChart{}, values)
	assert.Equal(t, chart.SourceEmbedded, sch.Source)
	assert.Empty(t, violations)
	bad := BuildValues(Spec{Name: "sre", ModelConfig: "mc", Runtime: "rust", Toolset: []string{"preset:read-only"}})
	_, violations = ValidateValues(ctx, embeddedChart{}, bad)
	require.Len(t, violations, 1)
	assert.Contains(t, violations[0], "/agent/runtime")

	// A schema that declares toolset validates it as well.
	_, violations = ValidateValues(ctx, declaringChart{}, values)
	assert.Empty(t, violations)
	_, violations = ValidateValues(ctx, declaringChart{}, map[string]any{"agent": map[string]any{"name": "sre"}, "modelConfig": map[string]any{"name": "mc"}, ToolsetValuesKey: []any{}})
	require.Len(t, violations, 1)
	assert.Contains(t, violations[0], "/toolset")
}
