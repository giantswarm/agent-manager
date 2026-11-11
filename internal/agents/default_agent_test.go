package agents

import (
	"encoding/json"
	"maps"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

// labelledAgent is an Agent with labels, rendered by the HelmRelease of
// the same name in ns.
func labelledAgent(ns, name string, labels map[string]string) *unstructured.Unstructured {
	obj := agentObject(ns, name, name, ns, true, true)
	all := obj.GetLabels()
	maps.Copy(all, labels)
	obj.SetLabels(all)
	return obj
}

// platformDefault is the platform's default agent as the agent-platform
// chart ships it: its HelmRelease and its Agent carry both marks.
var platformDefault = map[string]string{PlatformManagedByLabel: PlatformManagedByValue, DefaultAgentLabel: "true"}

// The agent labelled the default is reported so by list_agents, get_agent
// and get_info; with none, nothing is.
func TestTheLabelledAgentIsTheDefault(t *testing.T) {
	f := seeded(t)
	assert.Empty(t, f.svc.Info(t.Context()).DefaultAgent)
	assert.Equal(t, map[string]bool{"verifier": false}, defaultsOf(t, f))

	mustCreate(t, f, hrGVR, helmRelease("kagent", "default-agent", defaultValues("default-agent"), true, map[string]any{
		PlatformManagedByLabel: PlatformManagedByValue, DefaultAgentLabel: "true"}))
	mustCreate(t, f, agentGVR, labelledAgent("kagent", "default-agent", platformDefault))

	assert.Equal(t, "kagent/default-agent", f.svc.Info(t.Context()).DefaultAgent)
	assert.Equal(t, map[string]bool{"verifier": false, "default-agent": true}, defaultsOf(t, f))
	got, err := f.svc.Get(t.Context(), In(""), "default-agent")
	require.NoError(t, err)
	assert.True(t, got.Default)
	got, err = f.svc.Get(t.Context(), In(""), "verifier")
	require.NoError(t, err)
	assert.False(t, got.Default)
}

// The default is the labelled Agent: a labelled HelmRelease whose Agent is
// not rendered (yet) names nothing; a value other than "true" is no label.
func TestTheDefaultIsTheLabelledAgent(t *testing.T) {
	f := seeded(t)
	mustCreate(t, f, hrGVR, helmRelease("kagent", "pending", defaultValues("pending"), true, map[string]any{DefaultAgentLabel: "true"}))
	assert.Empty(t, f.svc.Info(t.Context()).DefaultAgent, "no Agent behind the release")

	obj := agentObject("kagent", "assistant", "", "", true, true)
	obj.SetLabels(map[string]string{DefaultAgentLabel: "true"})
	mustCreate(t, f, agentGVR, obj)
	off := agentObject("kagent", "other", "", "", true, true)
	off.SetLabels(map[string]string{DefaultAgentLabel: "false"})
	mustCreate(t, f, agentGVR, off)

	assert.Equal(t, "kagent/assistant", f.svc.Info(t.Context()).DefaultAgent)
	assert.Equal(t, map[string]bool{"verifier": false, "assistant": true, "other": false, "pending": false}, defaultsOf(t, f))
}

// Several labelled: the platform-owned one wins, then the first by
// namespace/name; only the winner is reported as the default.
func TestOneDefaultWhenSeveralAreLabelled(t *testing.T) {
	f := seeded(t)
	label := map[string]string{DefaultAgentLabel: "true"}
	mustCreate(t, f, agentGVR, labelledAgent("kagent", "beta", label))
	mustCreate(t, f, agentGVR, labelledAgent("kagent", "alpha", label))
	assert.Equal(t, "kagent/alpha", f.svc.Info(t.Context()).DefaultAgent, "alphabetical")
	assert.Equal(t, map[string]bool{"verifier": false, "alpha": true, "beta": false}, defaultsOf(t, f))

	mustCreate(t, f, agentGVR, labelledAgent("tenant", "zeta", platformDefault))
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

// The platform's marks are not labels a caller sets: create_agent and
// update_agent refuse them, so no agent of a caller becomes the default or
// platform-owned; an update that keeps a label as it is passes.
func TestCallersCannotSetThePlatformLabels(t *testing.T) {
	f := seeded(t)
	readOnly := []string{"preset:read-only"}
	for _, key := range []string{DefaultAgentLabel, PlatformManagedByLabel} {
		_, err := f.svc.Create(t.Context(), Spec{Name: "sre", ModelConfig: "default-model-config", Toolset: readOnly, Labels: map[string]string{key: "true"}})
		require.ErrorIs(t, err, ErrInvalid, key)
		assert.Contains(t, err.Error(), key)
		_, err = f.svc.Update(t.Context(), Update{Name: "verifier", Labels: &map[string]string{key: "true"}})
		require.ErrorIs(t, err, ErrInvalid, key)
	}
	assert.Empty(t, f.svc.Info(t.Context()).DefaultAgent)

	values := defaultValues("assistant")
	values["labels"] = map[string]any{DefaultAgentLabel: "true"}
	mustCreate(t, f, hrGVR, helmRelease("kagent", "assistant", values, true, nil))
	_, err := f.svc.Update(t.Context(), Update{Name: "assistant", Labels: &map[string]string{DefaultAgentLabel: "true", "team": "sre"}})
	require.NoError(t, err, "kept as it is")
	_, err = f.svc.Update(t.Context(), Update{Name: "assistant", Labels: &map[string]string{"team": "sre"}})
	require.NoError(t, err, "removed")
}

// An Agent outside the managed namespaces is not the default.
func TestADefaultOutsideTheManagedNamespacesIsNone(t *testing.T) {
	f := seeded(t)
	mustCreate(t, f, agentGVR, labelledAgent("elsewhere", "default-agent", platformDefault))
	assert.Empty(t, f.svc.Info(t.Context()).DefaultAgent)
}

// With DefaultAgentTTL a caller's default is reused until it expires.
func TestTheDefaultAgentIsCached(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := seeded(t)
		f.svc.cfg.DefaultAgentTTL = time.Minute
		assert.Empty(t, f.svc.Info(t.Context()).DefaultAgent)
		mustCreate(t, f, agentGVR, labelledAgent("kagent", "default-agent", platformDefault))
		assert.Empty(t, f.svc.Info(t.Context()).DefaultAgent, "reused")
		time.Sleep(time.Minute + time.Second)
		assert.Equal(t, "kagent/default-agent", f.svc.Info(t.Context()).DefaultAgent, "read again once expired")
	})
}
