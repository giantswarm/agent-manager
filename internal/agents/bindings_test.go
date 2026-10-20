package agents

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	apiv1alpha1 "github.com/giantswarm/agent-manager/internal/kagent/gen/kagent/api/v1alpha1"
	"github.com/giantswarm/agent-manager/internal/kagent/kagenttest"
	"github.com/giantswarm/agent-manager/internal/muster"
)

// githubProviders are what kagent admits a GitHub workspace's session to:
// the provider instance github on its git and API hosts.
func githubProviders() []*apiv1alpha1.SessionProvider {
	return []*apiv1alpha1.SessionProvider{
		{Hostname: "github.com", Scheme: "Basic", Provider: "github"},
		{Hostname: "api.github.com", Scheme: "Bearer", Provider: "github"},
	}
}

func sessionID(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var s struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(raw, &s))
	return s.ID
}

func calledMethods(ctrl *kagenttest.Controller) []string {
	var out []string
	for _, call := range ctrl.Calls() {
		out = append(out, call.Method)
	}
	return out
}

func TestAWorkspaceSessionIsBoundToItsStarterBeforeItsFirstTurn(t *testing.T) {
	svc, ctrl, bindings := newBoundSessionFixture(t)
	ctrl.Providers = githubProviders()
	ctx := asPerson(t.Context(), "admin@lab.local")
	grant := kagenttest.NewSigner(t).Sign(t, "admin@lab.local", 5*time.Minute, kagenttest.WorkspaceGrant())
	// The first turn reaches the runtime only once the sandbox is bound.
	sends := 0
	started, err := svc.StartSession(ctx, StartSession{Name: "sre", Message: "hello", RequestID: "req-1", WorkspaceGrant: grant})
	require.NoError(t, err)
	id := sessionID(t, started.Session)

	require.Len(t, bindings.Bound, 1)
	assert.Equal(t, callerToken, bindings.Bound[0].Token, "muster is called as the person")
	assert.Equal(t, muster.Binding{
		Actor:     "spiffe://substrate-actor.local/actor/" + kagenttest.Atespace + "/session-" + strings.ToLower(id),
		Session:   "kagent/" + id,
		Providers: []string{"github"},
	}, bindings.Bound[0].Binding)
	for _, m := range calledMethods(ctrl) {
		if m == "SendStreamingMessage" || m == "SendMessage" {
			sends++
		}
	}
	assert.Equal(t, 1, sends, "the first turn was sent")

	// A retried start binds the same sandbox again, which muster accepts.
	_, err = svc.StartSession(ctx, StartSession{Name: "sre", Message: "hello", RequestID: "req-1", WorkspaceGrant: grant})
	require.NoError(t, err)
	require.Len(t, bindings.Bound, 2)
	assert.Equal(t, bindings.Bound[0], bindings.Bound[1])
}

// musterProvider is the entry kagent adds to a grant-admitted Session where
// muster verifies the actor's own JWT: the reserved provider muster on
// muster's host.
func musterProvider() *apiv1alpha1.SessionProvider {
	return &apiv1alpha1.SessionProvider{Hostname: "muster.example.com", Scheme: "Bearer", Provider: "muster"}
}

func TestAWorkspaceSessionIsBoundWithTheMusterProviderKagentAdmitted(t *testing.T) {
	svc, ctrl, bindings := newBoundSessionFixture(t)
	ctrl.Providers = append(githubProviders(), musterProvider())
	ctx := asPerson(t.Context(), "admin@lab.local")
	grant := kagenttest.NewSigner(t).Sign(t, "admin@lab.local", 5*time.Minute, kagenttest.WorkspaceGrant())

	started, err := svc.StartSession(ctx, StartSession{Name: "sre", Message: "hello", WorkspaceGrant: grant})
	require.NoError(t, err)
	require.Len(t, bindings.Bound, 1)
	assert.Equal(t, []string{"github", "muster"}, bindings.Bound[0].Binding.Providers, "muster once, as kagent listed it")

	_, err = svc.DeleteSession(ctx, sessionID(t, started.Session))
	require.NoError(t, err)
	require.Len(t, bindings.Unbound, 1)
	assert.Equal(t, bindings.Bound[0].Binding.Actor, bindings.Unbound[0].Binding.Actor)
}

func TestAWorkspaceSessionWithOnlyTheMusterProviderIsBound(t *testing.T) {
	svc, ctrl, bindings := newBoundSessionFixture(t)
	ctrl.Providers = []*apiv1alpha1.SessionProvider{musterProvider()}
	ctx := asPerson(t.Context(), "admin@lab.local")
	grant := kagenttest.NewSigner(t).Sign(t, "admin@lab.local", 5*time.Minute, kagenttest.WorkspaceGrant())

	_, err := svc.StartSession(ctx, StartSession{Name: "sre", Message: "hello", WorkspaceGrant: grant})
	require.NoError(t, err)
	require.Len(t, bindings.Bound, 1)
	assert.Equal(t, []string{"muster"}, bindings.Bound[0].Binding.Providers, "muster serves the sandbox as its person with no workspace provider")
}

// A Session started outside a workspace (a Slack thread's) carries muster
// where muster verifies the actor's own JWT, and is bound to its starter like a
// workspace Session, so its sandbox reaches muster as the person.
func TestASessionWithoutAWorkspaceIsBoundWithTheMusterProvider(t *testing.T) {
	svc, ctrl, bindings := newBoundSessionFixture(t)
	ctrl.Providers = githubProviders()
	ctrl.ActorJWTProviders = []*apiv1alpha1.SessionProvider{musterProvider()}
	ctx := asPerson(t.Context(), "admin@lab.local")

	started, err := svc.StartSession(ctx, StartSession{Name: "sre", Message: "hello"})
	require.NoError(t, err)
	id := sessionID(t, started.Session)
	requests := createRequests(ctrl)
	require.Len(t, requests, 1)
	assert.True(t, requests[0].GetBindActor(), "the session is created for its actor to be bound")
	require.Len(t, bindings.Bound, 1)
	assert.Equal(t, callerToken, bindings.Bound[0].Token, "muster is called as the person")
	assert.Equal(t, muster.Binding{
		Actor:     "spiffe://substrate-actor.local/actor/" + kagenttest.Atespace + "/session-" + strings.ToLower(id),
		Session:   "kagent/" + id,
		Providers: []string{"muster"},
	}, bindings.Bound[0].Binding, "muster alone: no workspace provider is admitted without a grant")

	_, err = svc.DeleteSession(ctx, id)
	require.NoError(t, err)
	require.Len(t, bindings.Unbound, 1)
	assert.Equal(t, bindings.Bound[0].Binding.Actor, bindings.Unbound[0].Binding.Actor)
}

func TestAFailedBindOfASessionWithoutAWorkspaceLeavesNoSession(t *testing.T) {
	svc, ctrl, bindings := newBoundSessionFixture(t)
	ctrl.ActorJWTProviders = []*apiv1alpha1.SessionProvider{musterProvider()}
	bindings.BindErr = fmt.Errorf("%w (403 Forbidden)", muster.ErrForbidden)
	ctx := asPerson(t.Context(), "admin@lab.local")

	_, err := svc.StartSession(ctx, StartSession{Name: "sre", Message: "hello"})
	require.ErrorIs(t, err, ErrForbidden)
	assert.Contains(t, err.Error(), "was not started")
	assert.NotContains(t, calledMethods(ctrl), "SendStreamingMessage", "no turn ran unbound")
	list, err := svc.ListSessions(ctx, "", "sre", false)
	require.NoError(t, err)
	assert.Empty(t, list.Sessions, "the session was deleted again")
}

// Without muster's bindings configured, a Session without a workspace starts
// as before: there is no bind to make.
func TestASessionWithoutAWorkspaceStartsWithoutMustersBindings(t *testing.T) {
	svc, ctrl, _ := newBoundSessionFixture(t)
	svc.cfg.Bindings = nil
	ctrl.ActorJWTProviders = []*apiv1alpha1.SessionProvider{musterProvider()}
	ctx := asPerson(t.Context(), "admin@lab.local")

	started, err := svc.StartSession(ctx, StartSession{Name: "sre", Message: "hello"})
	require.NoError(t, err)
	for _, request := range createRequests(ctrl) {
		assert.False(t, request.GetBindActor(), "nothing binds the actor, so the session asks for no actor JWT provider")
	}
	assert.NotContains(t, string(started.Session), "muster", "the session keeps the caller's credential towards muster")
}

func TestASessionWithoutProvidersIsNeverBound(t *testing.T) {
	svc, ctrl, bindings := newBoundSessionFixture(t)
	ctrl.Providers = githubProviders()
	ctx := asPerson(t.Context(), "admin@lab.local")

	started, err := svc.StartSession(ctx, StartSession{Name: "sre", Message: "hello"})
	require.NoError(t, err)
	assert.Empty(t, bindings.Bound)
	_, err = svc.DeleteSession(ctx, sessionID(t, started.Session))
	require.NoError(t, err)
	assert.Empty(t, bindings.Unbound, "nothing to unbind")
}

func TestAWorkspaceSessionWithoutProvidersIsNotBound(t *testing.T) {
	svc, _, bindings := newBoundSessionFixture(t)
	ctx := asPerson(t.Context(), "admin@lab.local")
	grant := kagenttest.NewSigner(t).Sign(t, "admin@lab.local", 5*time.Minute, kagenttest.WorkspaceGrant())

	_, err := svc.StartSession(ctx, StartSession{Name: "sre", Message: "hello", WorkspaceGrant: grant})
	require.NoError(t, err)
	assert.Empty(t, bindings.Bound, "muster has nothing to release to its sandbox")
}

func TestAFailedBindFailsTheStartNamingMusterAndLeavesNoSession(t *testing.T) {
	for name, tc := range map[string]struct {
		err      error
		sentinel error
	}{
		"another person's actor": {fmt.Errorf("%w (403 Forbidden)", muster.ErrForbidden), ErrForbidden},
		"unknown provider":       {fmt.Errorf("%w (400 Bad Request): unknown provider github", muster.ErrInvalid), ErrInvalid},
		"unreachable":            {errors.New("muster session bindings (muster:8090): connection refused"), nil},
	} {
		t.Run(name, func(t *testing.T) {
			svc, ctrl, bindings := newBoundSessionFixture(t)
			ctrl.Providers = githubProviders()
			bindings.BindErr = tc.err
			ctx := asPerson(t.Context(), "admin@lab.local")
			grant := kagenttest.NewSigner(t).Sign(t, "admin@lab.local", 5*time.Minute, kagenttest.WorkspaceGrant())

			_, err := svc.StartSession(ctx, StartSession{Name: "sre", Message: "hello", WorkspaceGrant: grant})
			require.Error(t, err)
			if tc.sentinel != nil {
				require.ErrorIs(t, err, tc.sentinel)
			}
			assert.Contains(t, err.Error(), "muster")
			assert.Contains(t, err.Error(), "was not started")
			assert.NotContains(t, calledMethods(ctrl), "SendStreamingMessage", "no turn ran unbound")
			assert.Contains(t, calledMethods(ctrl), "DeleteSession")
			list, err := svc.ListSessions(ctx, "", "sre", false)
			require.NoError(t, err)
			assert.Empty(t, list.Sessions, "the session was deleted again")
		})
	}
}

func TestAFailedBindWhoseDeleteFailsNamesTheSession(t *testing.T) {
	svc, ctrl, bindings := newBoundSessionFixture(t)
	ctrl.Providers = githubProviders()
	bindings.BindErr = errors.New("connection refused")
	ctrl.Fail["DeleteSession"] = status.Error(codes.Unavailable, "controller unavailable")
	ctx := asPerson(t.Context(), "admin@lab.local")
	grant := kagenttest.NewSigner(t).Sign(t, "admin@lab.local", 5*time.Minute, kagenttest.WorkspaceGrant())

	_, err := svc.StartSession(ctx, StartSession{Name: "sre", Message: "hello", WorkspaceGrant: grant})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "muster")
	assert.Contains(t, err.Error(), "delete it with delete_session")
}

func TestAWorkspaceGrantNeedsMustersBindings(t *testing.T) {
	svc, ctrl, _ := newBoundSessionFixture(t)
	svc.cfg.Bindings = nil
	ctx := asPerson(t.Context(), "admin@lab.local")
	grant := kagenttest.NewSigner(t).Sign(t, "admin@lab.local", 5*time.Minute, kagenttest.WorkspaceGrant())

	_, err := svc.StartSession(ctx, StartSession{Name: "sre", Message: "hello", WorkspaceGrant: grant})
	require.ErrorIs(t, err, ErrUnsupported)
	assert.Contains(t, err.Error(), "muster")
	assert.Empty(t, createRequests(ctrl), "no session was created")
}

func TestDeletingAWorkspaceSessionUnbindsIt(t *testing.T) {
	svc, ctrl, bindings := newBoundSessionFixture(t)
	ctrl.Providers = githubProviders()
	ctx := asPerson(t.Context(), "admin@lab.local")
	grant := kagenttest.NewSigner(t).Sign(t, "admin@lab.local", 5*time.Minute, kagenttest.WorkspaceGrant())
	started, err := svc.StartSession(ctx, StartSession{Name: "sre", WorkspaceGrant: grant})
	require.NoError(t, err)

	deleted, err := svc.DeleteSession(ctx, sessionID(t, started.Session))
	require.NoError(t, err)
	assert.Empty(t, deleted.Warning)
	require.Len(t, bindings.Unbound, 1)
	assert.Equal(t, callerToken, bindings.Unbound[0].Token)
	assert.Equal(t, bindings.Bound[0].Binding.Actor, bindings.Unbound[0].Binding.Actor)
	methods := calledMethods(ctrl)
	assert.Equal(t, []string{"GetSession", "DeleteSession"}, methods[len(methods)-2:], "the actor is read before kagent forgets it")
}

func TestAMissedUnbindIsAWarningOnTheDelete(t *testing.T) {
	svc, ctrl, bindings := newBoundSessionFixture(t)
	ctrl.Providers = githubProviders()
	ctx := asPerson(t.Context(), "admin@lab.local")
	grant := kagenttest.NewSigner(t).Sign(t, "admin@lab.local", 5*time.Minute, kagenttest.WorkspaceGrant())
	started, err := svc.StartSession(ctx, StartSession{Name: "sre", WorkspaceGrant: grant})
	require.NoError(t, err)
	bindings.UnbindErr = errors.New("connection refused")

	deleted, err := svc.DeleteSession(ctx, sessionID(t, started.Session))
	require.NoError(t, err, "the session is deleted")
	assert.Contains(t, deleted.Warning, "muster")
	assert.Contains(t, deleted.Warning, "idle TTL")
}

func TestASessionWithoutAnActorAddressIsNotBound(t *testing.T) {
	_, err := sessionBinding(&apiv1alpha1.Session{Id: "s", Providers: githubProviders(), A2AAuthority: "elsewhere.example"})
	require.ErrorIs(t, err, ErrConflict)
	assert.Contains(t, err.Error(), "muster")
	binding, err := sessionBinding(&apiv1alpha1.Session{Id: "s"})
	require.NoError(t, err)
	assert.Nil(t, binding)
}

// A Session kagent stored before SessionProvider.provider existed names its
// provider instances in audience; the binding reads them from there.
func TestASessionStoredBeforeProviderIsBoundByItsAudience(t *testing.T) {
	session := &apiv1alpha1.Session{
		Id:           "3ca7d52f-4b84-4586-9db3-413f6bbaa56d",
		Agent:        &apiv1alpha1.ResourceReference{Namespace: "kagent", Name: "sre"},
		A2AAuthority: "session-3ca7d52f-4b84-4586-9db3-413f6bbaa56d.kagent" + actorAuthoritySuffix,
		//nolint:staticcheck // audience is what a Session stored before provider carries.
		Providers: []*apiv1alpha1.SessionProvider{{Hostname: "muster.example.com", Audience: "muster", Scheme: "Bearer"}},
	}
	binding, err := sessionBinding(session)
	require.NoError(t, err)
	require.NotNil(t, binding)
	assert.Equal(t, []string{"muster"}, binding.Providers)
}
