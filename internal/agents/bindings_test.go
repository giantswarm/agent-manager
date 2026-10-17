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
		{Hostname: "github.com", Audience: "github", Scheme: "Basic", Username: "x-access-token"},
		{Hostname: "api.github.com", Audience: "github", Scheme: "Bearer"},
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

func TestASessionWithoutAWorkspaceIsNeverBound(t *testing.T) {
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
