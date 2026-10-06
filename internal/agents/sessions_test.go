package agents

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/giantswarm/agent-manager/internal/identity"
	"github.com/giantswarm/agent-manager/internal/kagent"
	"github.com/giantswarm/agent-manager/internal/kagent/kagenttest"
)

const callerToken = "caller-id-token"

func newSessionFixture(t *testing.T) (*Service, *kagenttest.Controller) {
	t.Helper()
	f := newFixture(t, []runtime.Object{})
	ctrl, conn := kagenttest.New(t)
	f.svc.cfg.Sessions = kagent.NewClient(conn)
	return f.svc, ctrl
}

func TestSessionsUnsupportedWithoutATarget(t *testing.T) {
	f := newFixture(t, []runtime.Object{})
	ctx := identity.ContextWithToken(t.Context(), callerToken)
	assert.False(t, f.svc.Info(ctx).Capabilities["sessions"])
	_, err := f.svc.ListSessions(ctx, "", "sre", false)
	require.ErrorIs(t, err, ErrUnsupported)
	_, err = f.svc.GetSession(ctx, uuid.NewString())
	require.ErrorIs(t, err, ErrUnsupported)
	_, err = f.svc.StartSession(ctx, StartSession{Name: "sre", Message: "hi"})
	require.ErrorIs(t, err, ErrUnsupported)
}

func TestSessionsNeverRunWithoutTheCallerToken(t *testing.T) {
	svc, ctrl := newSessionFixture(t)
	assert.True(t, svc.Info(t.Context()).Capabilities["sessions"])
	_, err := svc.ListSessions(t.Context(), "", "sre", false)
	require.ErrorIs(t, err, ErrUnauthenticated)
	_, err = svc.GetSession(t.Context(), uuid.NewString())
	require.ErrorIs(t, err, ErrUnauthenticated)
	_, err = svc.StartSession(t.Context(), StartSession{Name: "sre", Message: "hi"})
	require.ErrorIs(t, err, ErrUnauthenticated)
	assert.Empty(t, ctrl.Calls(), "nothing reaches kagent without a caller")
}

func TestSessionArgumentsAreChecked(t *testing.T) {
	svc, ctrl := newSessionFixture(t)
	ctx := identity.ContextWithToken(t.Context(), callerToken)
	for name, in := range map[string]StartSession{
		"no message":          {Name: "sre"},
		"blank message":       {Name: "sre", Message: "  "},
		"long message":        {Name: "sre", Message: strings.Repeat("x", MaxSessionMessageLength+1)},
		"long request id":     {Name: "sre", Message: "hi", RequestID: strings.Repeat("r", 129)},
		"long session name":   {Name: "sre", Message: "hi", SessionName: strings.Repeat("n", 201)},
		"bad agent name":      {Name: "SRE", Message: "hi"},
		"unmanaged namespace": {Namespace: "elsewhere", Name: "sre", Message: "hi"},
	} {
		_, err := svc.StartSession(ctx, in)
		require.ErrorIs(t, err, ErrInvalid, name)
	}
	_, err := svc.ListSessions(ctx, "elsewhere", "sre", false)
	require.ErrorIs(t, err, ErrInvalid)
	_, err = svc.GetSession(ctx, "")
	require.ErrorIs(t, err, ErrInvalid)
	assert.Empty(t, ctrl.Calls())
}

func TestStartSessionThenReadTheAnswer(t *testing.T) {
	svc, ctrl := newSessionFixture(t)
	ctx := identity.ContextWithToken(t.Context(), callerToken)

	started, err := svc.StartSession(ctx, StartSession{Namespace: "tenant", Name: "sre", Message: "hello", SessionName: "triage"})
	require.NoError(t, err)
	var session struct {
		ID        string            `json:"id"`
		ContextID string            `json:"contextId"`
		Creator   string            `json:"creator"`
		Name      string            `json:"name"`
		State     string            `json:"state"`
		Agent     map[string]string `json:"agent"`
	}
	require.NoError(t, json.Unmarshal(started.Session, &session))
	assert.Equal(t, "RUNTIME_STATE_READY", session.State)
	assert.Equal(t, map[string]string{"namespace": "tenant", "name": "sre"}, session.Agent)
	assert.Equal(t, "triage", session.Name)
	assert.Equal(t, kagenttest.Creator, session.Creator)
	var turn struct {
		Task struct {
			ID     string `json:"id"`
			Status struct {
				State string `json:"state"`
			} `json:"status"`
		} `json:"task"`
	}
	require.NoError(t, json.Unmarshal(started.Turn, &turn))
	assert.Equal(t, "TASK_STATE_SUBMITTED", turn.Task.Status.State)

	select {
	case <-ctrl.TurnEnded():
	case <-time.After(5 * time.Second):
		t.Fatal("the turn's stream stayed open")
	}

	detail, err := svc.GetSession(ctx, session.ID)
	require.NoError(t, err)
	require.Len(t, detail.Tasks, 1)
	var task struct {
		ID     string `json:"id"`
		Status struct {
			State   string `json:"state"`
			Message struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"message"`
		} `json:"status"`
	}
	require.NoError(t, json.Unmarshal(detail.Tasks[0], &task))
	assert.Equal(t, turn.Task.ID, task.ID)
	assert.Equal(t, "TASK_STATE_COMPLETED", task.Status.State)
	assert.Equal(t, "echo: hello", task.Status.Message.Parts[0].Text)

	list, err := svc.ListSessions(ctx, "tenant", "sre", false)
	require.NoError(t, err)
	assert.Equal(t, AgentRef{Namespace: "tenant", Name: "sre"}, list.Agent)
	require.Len(t, list.Sessions, 1)
	assert.Contains(t, string(list.Sessions[0]), session.ID)

	for _, call := range ctrl.Calls() {
		assert.Equal(t, []string{"Bearer " + callerToken}, call.Metadata.Get("authorization"), call.Method)
	}
	// The conversation was read from the session's own Agent and context.
	last := ctrl.Calls()[len(ctrl.Calls())-2].Request.(*a2apb.ListTasksRequest)
	assert.Equal(t, "tenant/sre", last.GetTenant())
	assert.Equal(t, session.ContextID, last.GetContextId())
}

func TestStartSessionRetryReusesTheSession(t *testing.T) {
	svc, ctrl := newSessionFixture(t)
	ctx := identity.ContextWithToken(t.Context(), callerToken)
	ctrl.Fail["SendStreamingMessage"] = status.Error(codes.Unavailable, "runtime unavailable")

	_, err := svc.StartSession(ctx, StartSession{Name: "sre", Message: "hello", RequestID: "req-1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `start again with requestId "req-1"`)

	delete(ctrl.Fail, "SendStreamingMessage")
	started, err := svc.StartSession(ctx, StartSession{Name: "sre", Message: "hello", RequestID: "req-1"})
	require.NoError(t, err)
	list, err := svc.ListSessions(ctx, "", "sre", false)
	require.NoError(t, err)
	require.Len(t, list.Sessions, 1, "the retry answered the session the first attempt created")
	assert.JSONEq(t, string(list.Sessions[0]), string(started.Session))
}

func TestStartSessionRefusedWhileTheAgentIsNotReady(t *testing.T) {
	svc, ctrl := newSessionFixture(t)
	ctrl.NotReady["kagent/sre"] = true
	_, err := svc.StartSession(identity.ContextWithToken(t.Context(), callerToken), StartSession{Name: "sre", Message: "hi"})
	require.ErrorIs(t, err, ErrConflict)
	assert.Contains(t, err.Error(), "no ready revision")
}

func TestGetSessionErrors(t *testing.T) {
	svc, _ := newSessionFixture(t)
	ctx := identity.ContextWithToken(t.Context(), callerToken)
	_, err := svc.GetSession(ctx, uuid.NewString())
	require.ErrorIs(t, err, ErrNotFound)
	_, err = svc.GetSession(ctx, "not-a-uuid")
	require.ErrorIs(t, err, ErrInvalid)
}

func TestKagentErrorMapping(t *testing.T) {
	for code, want := range map[codes.Code]error{
		codes.NotFound:           ErrNotFound,
		codes.InvalidArgument:    ErrInvalid,
		codes.FailedPrecondition: ErrConflict,
		codes.AlreadyExists:      ErrConflict,
		codes.PermissionDenied:   ErrForbidden,
		codes.Unauthenticated:    ErrUnauthenticated,
		codes.Unimplemented:      ErrUnsupported,
	} {
		err := kagentError(status.Error(code, "because"), "x")
		require.ErrorIs(t, err, want, code.String())
		assert.Contains(t, err.Error(), "because")
	}
	err := kagentError(status.Error(codes.Unavailable, "no route"), "x")
	for _, sentinel := range []error{ErrNotFound, ErrInvalid, ErrConflict, ErrForbidden, ErrUnauthenticated, ErrUnsupported} {
		assert.False(t, errors.Is(err, sentinel))
	}
	assert.Contains(t, err.Error(), "Unavailable")
	require.ErrorIs(t, kagentError(kagent.ErrNoToken, "x"), ErrUnauthenticated)
}
