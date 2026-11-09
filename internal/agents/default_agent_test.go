package agents

import (
	"encoding/json"
	"testing"
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

// platformRelease is the platform's default agent as the agent-platform
// chart ships it: a release in the platform's namespace that renders the
// Agent into kagent (targetNamespace, releaseName).
func platformRelease(ns string) *unstructured.Unstructured {
	hr := helmRelease(ns, "default-agent", map[string]any{"modelConfig": map[string]any{"name": "default-model-config"}}, true, map[string]any{
		PlatformManagedByLabel: PlatformManagedByValue, DefaultAgentLabel: "true"})
	spec := hr.Object["spec"].(map[string]any)
	spec["targetNamespace"], spec["releaseName"] = "kagent", "default-agent"
	return hr
}

// A labelled HelmRelease reports the Agent it renders, as a portal agent id
// names it, wherever the release lives; with its Agent labelled too, that
// is one agent, not two.
func TestTheDefaultIsTheAgentAReleaseRenders(t *testing.T) {
	for _, releaseNS := range []string{"agent-platform", "tenant"} {
		t.Run(releaseNS, func(t *testing.T) {
			f := seeded(t)
			mustCreate(t, f, hrGVR, platformRelease(releaseNS))
			assert.Equal(t, "kagent/default-agent", f.svc.Info(t.Context()).DefaultAgent, "the release alone")

			obj := agentObject("kagent", "default-agent", "default-agent", releaseNS, true, true)
			labels := obj.GetLabels()
			labels[PlatformManagedByLabel], labels[DefaultAgentLabel] = PlatformManagedByValue, "true"
			obj.SetLabels(labels)
			mustCreate(t, f, agentGVR, obj)
			assert.Equal(t, "kagent/default-agent", f.svc.Info(t.Context()).DefaultAgent)
			assert.True(t, defaultsOf(t, f)["default-agent"])
		})
	}
}

// renderedAgent follows the chart and Flux: agent.name, else the release
// name, else [<targetNamespace>-]<name>; a release onto another cluster
// renders nothing here.
func TestRenderedAgent(t *testing.T) {
	hr := helmRelease("flux", "x", map[string]any{}, true, nil)
	ns, name, local := renderedAgent(hr)
	assert.Equal(t, []any{"flux", "x", true}, []any{ns, name, local})
	spec := hr.Object["spec"].(map[string]any)
	spec["targetNamespace"] = "kagent"
	ns, name, _ = renderedAgent(hr)
	assert.Equal(t, "kagent/kagent-x", ns+"/"+name)
	spec["releaseName"] = "rel"
	_, name, _ = renderedAgent(hr)
	assert.Equal(t, "rel", name)
	spec["values"] = map[string]any{"agent": map[string]any{"name": "named"}}
	_, name, _ = renderedAgent(hr)
	assert.Equal(t, "named", name)
	spec["kubeConfig"] = map[string]any{"secretRef": map[string]any{"name": "wc1-kubeconfig"}}
	_, _, local = renderedAgent(hr)
	assert.False(t, local)
}

// A release that renders outside the managed namespaces is not the default.
func TestADefaultOutsideTheManagedNamespacesIsNone(t *testing.T) {
	f := seeded(t)
	hr := platformRelease("agent-platform")
	hr.Object["spec"].(map[string]any)["targetNamespace"] = "elsewhere"
	mustCreate(t, f, hrGVR, hr)
	assert.Empty(t, f.svc.Info(t.Context()).DefaultAgent)
}

// With DefaultAgentTTL a caller's default is reused until it expires.
func TestTheDefaultAgentIsCached(t *testing.T) {
	f := seeded(t)
	f.svc.cfg.DefaultAgentTTL = time.Hour
	assert.Empty(t, f.svc.Info(t.Context()).DefaultAgent)
	mustCreate(t, f, hrGVR, platformRelease("agent-platform"))
	assert.Empty(t, f.svc.Info(t.Context()).DefaultAgent, "reused")
	f.svc.defaults.put("", "", time.Now().Add(-time.Second))
	assert.Equal(t, "kagent/default-agent", f.svc.Info(t.Context()).DefaultAgent, "read again once expired")
}
