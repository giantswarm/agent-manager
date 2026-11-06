package agents

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func defaultValues(name string) map[string]any {
	return map[string]any{"agent": map[string]any{"name": name}, "modelConfig": map[string]any{"name": "default-model-config"}, ToolsetValuesKey: []any{"preset:full"}}
}

func defaultsOf(t *testing.T, f *fixture) map[string]bool {
	t.Helper()
	list, err := f.svc.List(t.Context(), In(""), nil)
	require.NoError(t, err)
	out := map[string]bool{}
	for _, a := range list {
		out[a.Name] = a.Default
		assert.Equal(t, a.Default, a.Summary().Default, "the summary carries it")
	}
	return out
}

// The agent labelled the default is reported so by list_agents, get_agent
// and get_info; with none, nothing is.
func TestTheLabelledAgentIsTheDefault(t *testing.T) {
	f := seeded(t)
	assert.Empty(t, f.svc.Info(t.Context()).DefaultAgent)
	assert.Equal(t, map[string]bool{"verifier": false}, defaultsOf(t, f))

	mustCreate(t, f, hrGVR, helmRelease("kagent", "default-agent", defaultValues("default-agent"), true, map[string]any{
		PlatformManagedByLabel: PlatformManagedByValue, DefaultAgentLabel: "true"}))

	assert.Equal(t, "kagent/default-agent", f.svc.Info(t.Context()).DefaultAgent)
	assert.Equal(t, map[string]bool{"verifier": false, "default-agent": true}, defaultsOf(t, f))
	got, err := f.svc.Get(t.Context(), In(""), "default-agent")
	require.NoError(t, err)
	assert.True(t, got.Default)
	got, err = f.svc.Get(t.Context(), In(""), "verifier")
	require.NoError(t, err)
	assert.False(t, got.Default)
}

// The label on the Agent alone suffices; a value other than "true" is no
// label.
func TestTheDefaultLabelOnTheAgentSuffices(t *testing.T) {
	f := seeded(t)
	obj := agentObject("kagent", "assistant", "", "", true, true)
	obj.SetLabels(map[string]string{DefaultAgentLabel: "true"})
	mustCreate(t, f, agentGVR, obj)
	off := agentObject("kagent", "other", "", "", true, true)
	off.SetLabels(map[string]string{DefaultAgentLabel: "false"})
	mustCreate(t, f, agentGVR, off)

	assert.Equal(t, "kagent/assistant", f.svc.Info(t.Context()).DefaultAgent)
	assert.Equal(t, map[string]bool{"verifier": false, "assistant": true, "other": false}, defaultsOf(t, f))
}

// Several labelled: the platform-owned one wins, then the first by
// namespace/name; only the winner is reported as the default.
func TestOneDefaultWhenSeveralAreLabelled(t *testing.T) {
	f := seeded(t)
	label := map[string]any{DefaultAgentLabel: "true"}
	mustCreate(t, f, hrGVR, helmRelease("kagent", "beta", defaultValues("beta"), true, label))
	mustCreate(t, f, hrGVR, helmRelease("kagent", "alpha", defaultValues("alpha"), true, label))
	assert.Equal(t, "kagent/alpha", f.svc.Info(t.Context()).DefaultAgent, "alphabetical")
	assert.Equal(t, map[string]bool{"verifier": false, "alpha": true, "beta": false}, defaultsOf(t, f))

	mustCreate(t, f, hrGVR, helmRelease("tenant", "zeta", defaultValues("zeta"), true, map[string]any{
		PlatformManagedByLabel: PlatformManagedByValue, DefaultAgentLabel: "true"}))
	assert.Equal(t, "tenant/zeta", f.svc.Info(t.Context()).DefaultAgent, "the platform's wins")
	assert.Equal(t, map[string]bool{"verifier": false, "alpha": false, "beta": false}, defaultsOf(t, f))
}

// get_info names the installation as the Dev Portal does, the first part of
// a portal agent id; empty when unset.
func TestInfoReportsTheInstallation(t *testing.T) {
	f := seeded(t)
	assert.Empty(t, f.svc.Info(t.Context()).Installation)

	f.svc.cfg.Installation = "golem"
	assert.Equal(t, "golem", f.svc.Info(t.Context()).Installation)
	raw, err := json.Marshal(f.svc.Info(t.Context()))
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"installation":"golem"`)
}
