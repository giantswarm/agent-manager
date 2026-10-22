package agents

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	apiv1alpha1 "github.com/giantswarm/agent-manager/internal/kagent/gen/kagent/api/v1alpha1"
	"github.com/giantswarm/agent-manager/internal/kagent/kagenttest"
)

// startBound starts a session of agent name where muster verifies the actor's
// own JWT and answers the binding it was bound with.
func startBound(t *testing.T, objs []runtime.Object, name string) ([]string, *SessionStart) {
	t.Helper()
	svc, ctrl, bindings := newBoundSessionFixtureWith(t, objs...)
	ctrl.ActorJWTProviders = []*apiv1alpha1.SessionProvider{musterProvider()}
	started, err := svc.StartSession(asPerson(t.Context(), "admin@lab.local"), StartSession{Name: name, Message: "hello"})
	require.NoError(t, err)
	require.Len(t, bindings.Bound, 1)
	return bindings.Bound[0].Binding.Toolset, started
}

func TestASessionIsBoundWithItsAgentsToolset(t *testing.T) {
	toolset, started := startBound(t, sessionAgent("sre", []any{"preset:read-only", "tool:x_gh_issues"}), "sre")
	assert.Equal(t, []string{"preset:read-only", "tool:x_gh_issues"}, toolset)
	assert.Equal(t, toolset, started.Toolset, "start_session says what the session is bound to")
}

func TestASessionOfAnAgentWithoutToolsIsBoundToNone(t *testing.T) {
	toolset, _ := startBound(t, sessionAgent("chat", []any{"preset:none"}), "chat")
	assert.Equal(t, []string{"preset:none"}, toolset)

	// A bare Agent without a muster server entry: no tools either.
	toolset, _ = startBound(t, []runtime.Object{agentObject("kagent", "bare", "", "", true, false)}, "bare")
	assert.Equal(t, []string{"preset:none"}, toolset)
}

func TestASessionOfAnImplicitFullAccessAgentIsBoundToNoToolset(t *testing.T) {
	toolset, started := startBound(t, sessionAgent("legacy", nil), "legacy")
	assert.Nil(t, toolset, "implicit full access binds no toolset and keeps today's behaviour")
	assert.Nil(t, started.Toolset)
}

// A workspace provider alone carries no toolset: the sandbox does not call
// muster with its own JWT, and the agent is not read.
func TestAWorkspaceOnlyBindingCarriesNoToolset(t *testing.T) {
	svc, ctrl, bindings := newBoundSessionFixtureWith(t)
	ctrl.Providers = githubProviders()
	ctx := asPerson(t.Context(), "admin@lab.local")
	grant := kagenttest.NewSigner(t).Sign(t, "admin@lab.local", 5*time.Minute, kagenttest.WorkspaceGrant())
	_, err := svc.StartSession(ctx, StartSession{Name: "absent", Message: "hello", WorkspaceGrant: grant})
	require.NoError(t, err)
	require.Len(t, bindings.Bound, 1)
	assert.Nil(t, bindings.Bound[0].Binding.Toolset)
}

// The toolset is the one the agent runs with at the session's start: a
// change applies to sessions started once its release is rendered, and a
// running session's binding is left alone.
func TestAToolsetChangeAppliesToNewSessions(t *testing.T) {
	svc, ctrl, bindings := newBoundSessionFixture(t)
	ctrl.ActorJWTProviders = []*apiv1alpha1.SessionProvider{musterProvider()}
	ctx := asPerson(t.Context(), "admin@lab.local")
	_, err := svc.StartSession(ctx, StartSession{Name: "sre", Message: "hello"})
	require.NoError(t, err)

	dyn, _, err := svc.dyn(ctx)
	require.NoError(t, err)
	hr, err := dyn.Resource(svc.helmReleaseGVR()).Namespace("kagent").Get(ctx, "sre", metav1.GetOptions{})
	require.NoError(t, err)
	require.NoError(t, unstructured.SetNestedStringSlice(hr.Object, []string{"preset:full"}, "spec", "values", ToolsetValuesKey))
	_, err = dyn.Resource(svc.helmReleaseGVR()).Namespace("kagent").Update(ctx, hr, metav1.UpdateOptions{})
	require.NoError(t, err)

	_, err = svc.StartSession(ctx, StartSession{Name: "sre", Message: "hello"})
	require.NoError(t, err)
	require.Len(t, bindings.Bound, 2, "the first session was not bound again")
	assert.Equal(t, []string{"preset:read-only"}, bindings.Bound[1].Binding.Toolset, "the release is not rendered yet: the agent still runs with its former toolset")

	// The release renders the new toolset on the agent's RemoteMCPServer.
	_, err = dyn.Resource(svc.mcpServerGVR()).Namespace("kagent").Update(ctx, remoteMCPServer("kagent", "sre", "preset:full"), metav1.UpdateOptions{})
	require.NoError(t, err)
	_, err = svc.StartSession(ctx, StartSession{Name: "sre", Message: "hello"})
	require.NoError(t, err)
	require.Len(t, bindings.Bound, 3)
	assert.Equal(t, []string{"preset:read-only"}, bindings.Bound[0].Binding.Toolset)
	assert.Equal(t, []string{"preset:full"}, bindings.Bound[2].Binding.Toolset)
	assert.NotEqual(t, bindings.Bound[0].Binding.Actor, bindings.Bound[2].Binding.Actor)
}

// An Agent whose tools bind its own RemoteMCPServer while that server is
// missing has an unknown toolset: the session is not started, never bound
// with implicit full access.
func TestASessionWhoseAgentsServerIsMissingIsNotStarted(t *testing.T) {
	objs := sessionAgent("sre", []any{"preset:read-only"})
	svc, ctrl, bindings := newBoundSessionFixtureWith(t, objs[:2]...)
	ctrl.ActorJWTProviders = []*apiv1alpha1.SessionProvider{musterProvider()}
	ctx := asPerson(t.Context(), "admin@lab.local")
	_, err := svc.StartSession(ctx, StartSession{Name: "sre", Message: "hello"})
	require.ErrorIs(t, err, ErrConflict)
	assert.Contains(t, err.Error(), "toolset is unknown")
	assert.Empty(t, bindings.Bound)
	assert.NotContains(t, calledMethods(ctrl), "SendStreamingMessage", "no turn ran unbound")
}

// A person who may start Sessions and read Agents, but not their
// HelmReleases (agent-platform:user, agent-platform:platform-user), gets a
// Session bound with the toolset get_agent reports to an editor.
func TestASessionIsBoundForACallerWhoMayNotReadHelmReleases(t *testing.T) {
	objs := append(sessionAgent("sre", []any{"preset:read-only", "tool:x_gh_issues"}), sessionAgent("legacy", nil)...)
	objs = append(objs, sessionAgent("chat", []any{"preset:none"})...)
	svc, ctrl, bindings := newBoundSessionFixtureWith(t, objs...)
	ctrl.ActorJWTProviders = []*apiv1alpha1.SessionProvider{musterProvider()}
	editor := asPerson(t.Context(), "admin@lab.local")
	reported := map[string][]string{}
	for _, name := range []string{"sre", "legacy", "chat"} {
		a, err := svc.Get(editor, In("kagent"), name)
		require.NoError(t, err)
		reported[name] = BoundToolset(a)
	}

	dyn, _, err := svc.dyn(editor)
	require.NoError(t, err)
	dyn.(*dynamicfake.FakeDynamicClient).PrependReactor("get", "helmreleases", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(svc.helmReleaseGVR().GroupResource(), "sre", errors.New("user may not get helmreleases"))
	})
	user := asPerson(t.Context(), "dev@lab.local")
	for i, name := range []string{"sre", "legacy", "chat"} {
		started, err := svc.StartSession(user, StartSession{Name: name, Message: "hello"})
		require.NoError(t, err, name)
		require.Len(t, bindings.Bound, i+1)
		assert.Equal(t, reported[name], bindings.Bound[i].Binding.Toolset, name)
		assert.Equal(t, reported[name], started.Toolset, name)
	}
}

// A toolset agent-manager cannot read is never bound as none: the session is
// deleted again, as after a refused bind.
func TestASessionWhoseAgentCannotBeReadIsNotStarted(t *testing.T) {
	svc, ctrl, bindings := newBoundSessionFixtureWith(t)
	ctrl.ActorJWTProviders = []*apiv1alpha1.SessionProvider{musterProvider()}
	ctx := asPerson(t.Context(), "admin@lab.local")
	_, err := svc.StartSession(ctx, StartSession{Name: "ghost", Message: "hello"})
	require.ErrorIs(t, err, ErrNotFound)
	assert.Contains(t, err.Error(), "toolset")
	assert.Empty(t, bindings.Bound)
	assert.NotContains(t, calledMethods(ctrl), "SendStreamingMessage", "no turn ran unbound")
	list, err := svc.ListSessions(ctx, "", "ghost", false)
	require.NoError(t, err)
	assert.Empty(t, list.Sessions, "the session was deleted again")
}

func TestTheToolsetIsReportedBoundWhereMusterTrustsTheActorJWT(t *testing.T) {
	objs := append(sessionAgent("sre", []any{"preset:read-only"}), sessionAgent("legacy", nil)...)
	objs = append(objs, sessionAgent("chat", []any{"preset:none"})...)
	svc, _, _ := newBoundSessionFixtureWith(t, objs...)
	ctx := asPerson(t.Context(), "admin@lab.local")

	bound := func() map[string]bool {
		list, err := svc.List(ctx, In("kagent"))
		require.NoError(t, err)
		out := map[string]bool{}
		for _, a := range list {
			out[a.Name] = a.ToolsetBound
			assert.Equal(t, a.ToolsetBound, a.Summary().ToolsetBound)
		}
		a, err := svc.Get(ctx, In("kagent"), "sre")
		require.NoError(t, err)
		assert.Equal(t, out["sre"], a.ToolsetBound, "get_agent and list_agents agree")
		return out
	}
	assert.Equal(t, map[string]bool{"sre": false, "legacy": false, "chat": false}, bound(), "declared, where muster does not trust the actor JWT")
	assert.False(t, svc.Info(ctx).Capabilities["toolsetBound"])

	svc.cfg.ToolsetBound = true
	assert.Equal(t, map[string]bool{"sre": true, "legacy": false, "chat": true}, bound(), "implicit full access stays labelled as such")
	assert.True(t, svc.Info(ctx).Capabilities["toolsetBound"])

	svc.cfg.Bindings = nil
	assert.False(t, svc.Info(ctx).Capabilities["toolsetBound"], "without bindings nothing is bound")
}
