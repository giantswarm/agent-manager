package agents

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestRejectRemovedExplainsEveryRemovedArgument(t *testing.T) {
	assert.NoError(t, rejectRemoved(Spec{}.removed()...))
	assert.NoError(t, rejectRemoved(Spec{RemovedToolNames: json.RawMessage("null"), RemovedRuntime: json.RawMessage("null")}.removed()...))
	for _, raw := range []string{`["x_a_b"]`, `[]`, `"x"`} {
		err := rejectRemoved(Spec{RemovedToolNames: json.RawMessage(raw)}.removed()...)
		require.ErrorIs(t, err, ErrInvalid, raw)
		assert.Contains(t, err.Error(), "toolNames never narrowed anything against muster")
		assert.Contains(t, err.Error(), `toolset: ["preset:read-only"]`)
	}
	// runtime: gone with the per-agent Deployment (D5: one platform Harness on
	// the Go ADK), refused with the reason on create and update alike.
	for _, err := range []error{
		rejectRemoved(Spec{RemovedRuntime: json.RawMessage(`"python"`)}.removed()...),
		rejectRemoved(Update{RemovedRuntime: json.RawMessage(`"go"`)}.removed()...),
	} {
		require.ErrorIs(t, err, ErrInvalid)
		assert.Contains(t, err.Error(), "runtime is gone")
		assert.Contains(t, err.Error(), "Harness")
		assert.Contains(t, err.Error(), "no Python runtime")
	}
}

func TestParseToolsetHeader(t *testing.T) {
	assert.Equal(t, []string{"preset:read-only", "workflow:incident-triage"}, ParseToolsetHeader("preset:read-only, workflow:incident-triage"))
	assert.Equal(t, []string{"preset:none"}, ParseToolsetHeader("preset:none"))
	assert.Nil(t, ParseToolsetHeader(" , "))
}
