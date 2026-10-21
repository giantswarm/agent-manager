package agents

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/giantswarm/agent-manager/internal/identity"
	"github.com/giantswarm/agent-manager/internal/kagent"
	apiv1alpha1 "github.com/giantswarm/agent-manager/internal/kagent/gen/kagent/api/v1alpha1"
	"github.com/giantswarm/agent-manager/internal/kagent/kagenttest"
	"github.com/giantswarm/agent-manager/internal/muster"
)

const callerToken = "caller-id-token"

func newSessionFixture(t *testing.T) (*Service, *kagenttest.Controller) {
	t.Helper()
	svc, ctrl, _ := newBoundSessionFixture(t)
	return svc, ctrl
}

// newBoundSessionFixture is newSessionFixture with muster's session
// bindings, which it answers too. Its agent sre declares the toolset
// preset:read-only.
func newBoundSessionFixture(t *testing.T) (*Service, *kagenttest.Controller, *fakeBindings) {
	t.Helper()
	return newBoundSessionFixtureWith(t, sessionAgent("sre", []any{"preset:read-only"})...)
}

// newBoundSessionFixtureWith is newBoundSessionFixture with the given objects
// in place of its agent.
func newBoundSessionFixtureWith(t *testing.T, objs ...runtime.Object) (*Service, *kagenttest.Controller, *fakeBindings) {
	t.Helper()
	f := newFixture(t, objs)
	ctrl, conn := kagenttest.New(t)
	f.svc.cfg.Sessions = kagent.NewClient(conn)
	bindings := &fakeBindings{}
	f.svc.cfg.Bindings = bindings
	return f.svc, ctrl, bindings
}

// sessionAgent is agent name in kagent as its release renders it: the
// declared toolset (nil: none, implicit full access).
func sessionAgent(name string, toolset []any) []runtime.Object {
	values := map[string]any{"agent": map[string]any{"name": name}}
	var header []string
	if toolset != nil {
		values[ToolsetValuesKey] = toolset
		for _, sel := range toolset {
			header = append(header, sel.(string))
		}
	}
	return []runtime.Object{
		helmRelease("kagent", name, values, true, nil),
		agentObject("kagent", name, name, "kagent", true, true),
		remoteMCPServer("kagent", name, header...),
	}
}

// fakeBindings records muster's session bindings as the caller made them.
type fakeBindings struct {
	mu        sync.Mutex
	BindErr   error
	UnbindErr error
	Bound     []fakeBind
	Unbound   []fakeBind
}

type fakeBind struct {
	Token   string
	Binding muster.Binding
}

func (f *fakeBindings) Bind(_ context.Context, token string, b muster.Binding) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.BindErr != nil {
		return f.BindErr
	}
	f.Bound = append(f.Bound, fakeBind{Token: token, Binding: b})
	return nil
}

func (f *fakeBindings) Unbind(_ context.Context, token, actor string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.UnbindErr != nil {
		return f.UnbindErr
	}
	f.Unbound = append(f.Unbound, fakeBind{Token: token, Binding: muster.Binding{Actor: actor}})
	return nil
}

func TestSessionsUnsupportedWithoutATarget(t *testing.T) {
	f := newFixture(t, []runtime.Object{})
	ctx := identity.ContextWithToken(t.Context(), callerToken)
	assert.False(t, f.svc.Info(ctx).Capabilities["sessions"])
	_, err := f.svc.ListSessions(ctx, "", "sre", false)
	require.ErrorIs(t, err, ErrUnsupported)
	_, err = f.svc.GetSession(ctx, SessionQuery{SessionID: uuid.NewString()})
	require.ErrorIs(t, err, ErrUnsupported)
	_, err = f.svc.StartSession(ctx, StartSession{Name: "sre", Message: "hi"})
	require.ErrorIs(t, err, ErrUnsupported)
	_, err = f.svc.DeleteSession(ctx, uuid.NewString())
	require.ErrorIs(t, err, ErrUnsupported)
}

func TestSessionsNeverRunWithoutTheCallerToken(t *testing.T) {
	svc, ctrl := newSessionFixture(t)
	assert.True(t, svc.Info(t.Context()).Capabilities["sessions"])
	_, err := svc.ListSessions(t.Context(), "", "sre", false)
	require.ErrorIs(t, err, ErrUnauthenticated)
	_, err = svc.GetSession(t.Context(), SessionQuery{SessionID: uuid.NewString()})
	require.ErrorIs(t, err, ErrUnauthenticated)
	_, err = svc.StartSession(t.Context(), StartSession{Name: "sre", Message: "hi"})
	require.ErrorIs(t, err, ErrUnauthenticated)
	_, err = svc.DeleteSession(t.Context(), uuid.NewString())
	require.ErrorIs(t, err, ErrUnauthenticated)
	assert.Empty(t, ctrl.Calls(), "nothing reaches kagent without a caller")
}

func TestSessionArgumentsAreChecked(t *testing.T) {
	svc, ctrl := newSessionFixture(t)
	ctx := identity.ContextWithToken(t.Context(), callerToken)
	for name, in := range map[string]StartSession{
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
	_, err = svc.GetSession(ctx, SessionQuery{})
	require.ErrorIs(t, err, ErrInvalid)
	_, err = svc.DeleteSession(ctx, " ")
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

	summary, err := svc.GetSession(ctx, SessionQuery{SessionID: session.ID})
	require.NoError(t, err)
	assert.Equal(t, DetailSummary, summary.Detail)
	assert.Equal(t, 1, summary.TotalTurns)
	assert.Empty(t, summary.Tasks)
	require.Len(t, summary.Turns, 1)
	assert.Equal(t, Turn{Index: 0, TaskID: turn.Task.ID, State: "TASK_STATE_COMPLETED", User: "hello", Reply: "echo: hello"}, summary.Turns[0])

	detail, err := svc.GetSession(ctx, SessionQuery{SessionID: session.ID, Detail: DetailFull})
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
	lists := methodCalls(ctrl, "ListTasks")
	last := lists[len(lists)-1].(*a2apb.ListTasksRequest)
	assert.Equal(t, "tenant/sre", last.GetTenant())
	assert.Equal(t, session.ContextID, last.GetContextId())
}

func TestStartSessionWithoutAMessageStartsNoTurn(t *testing.T) {
	svc, ctrl := newSessionFixture(t)
	ctx := identity.ContextWithToken(t.Context(), callerToken)

	started, err := svc.StartSession(ctx, StartSession{Namespace: "tenant", Name: "sre", SessionName: "triage", RequestID: "req-1"})
	require.NoError(t, err)
	assert.Nil(t, started.Turn)
	data, err := json.Marshal(started)
	require.NoError(t, err)
	assert.NotContains(t, string(data), `"turn"`, "the answer has no turn")
	var session struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		State string `json:"state"`
	}
	require.NoError(t, json.Unmarshal(started.Session, &session))
	assert.Equal(t, "RUNTIME_STATE_READY", session.State)
	assert.Equal(t, "triage", session.Name)

	var methods []string
	for _, call := range ctrl.Calls() {
		methods = append(methods, call.Method)
	}
	assert.Equal(t, []string{"CreateSession"}, methods, "the session is created and no message is sent")
	create := ctrl.Calls()[0].Request.(*apiv1alpha1.CreateSessionRequest)
	assert.Equal(t, "req-1", create.GetRequestId())

	detail, err := svc.GetSession(ctx, SessionQuery{SessionID: session.ID})
	require.NoError(t, err)
	assert.Zero(t, detail.TotalTurns)
	assert.Empty(t, detail.Turns, "no turn was started")

	// A retry with the same requestId answers the same session, still without a turn.
	again, err := svc.StartSession(ctx, StartSession{Namespace: "tenant", Name: "sre", SessionName: "triage", RequestID: "req-1"})
	require.NoError(t, err)
	assert.JSONEq(t, string(started.Session), string(again.Session))
	assert.Nil(t, again.Turn)
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
	var session struct {
		ContextID string `json:"contextId"`
	}
	require.NoError(t, json.Unmarshal(started.Session, &session))
	assert.Len(t, ctrl.Tasks(session.ContextID), 1, "the first message was accepted once")
}

func TestStartSessionTwiceSendsTheFirstMessageOnce(t *testing.T) {
	svc, ctrl := newSessionFixture(t)
	ctx := identity.ContextWithToken(t.Context(), callerToken)
	in := StartSession{Name: "sre", Message: "hello", RequestID: "req-1"}

	first, err := svc.StartSession(ctx, in)
	require.NoError(t, err)
	select {
	case <-ctrl.TurnEnded():
	case <-time.After(5 * time.Second):
		t.Fatal("the turn's stream stayed open")
	}
	again, err := svc.StartSession(ctx, in)
	require.NoError(t, err)

	assert.JSONEq(t, string(first.Session), string(again.Session), "the same request id answers the same session")
	sends := methodCalls(ctrl, "SendStreamingMessage")
	require.Len(t, sends, 1, "the retry sent no second first message")
	var turn, retried struct {
		Task struct {
			ID      string `json:"id"`
			History []any  `json:"history"`
			Status  struct {
				State string `json:"state"`
			} `json:"status"`
		} `json:"task"`
	}
	require.NoError(t, json.Unmarshal(first.Turn, &turn))
	require.NoError(t, json.Unmarshal(again.Turn, &retried))
	assert.Equal(t, turn.Task.ID, retried.Task.ID, "the retry answers the turn the first attempt started")
	assert.Equal(t, "TASK_STATE_COMPLETED", retried.Task.Status.State, "in its latest state")
	assert.Empty(t, retried.Task.History, "without the conversation")

	// A different request id is another start: another session and turn.
	other, err := svc.StartSession(ctx, StartSession{Name: "sre", Message: "hello", RequestID: "req-2"})
	require.NoError(t, err)
	assert.NotEqual(t, string(first.Session), string(other.Session))
	sends = methodCalls(ctrl, "SendStreamingMessage")
	require.Len(t, sends, 2)
	assert.NotEqual(t, sends[0].(*a2apb.SendMessageRequest).GetMessage().GetMessageId(),
		sends[1].(*a2apb.SendMessageRequest).GetMessage().GetMessageId(), "each request id names its own first message")
}

func TestStartSessionSendsItsMessageAfterTheCallersOwnFirstTurn(t *testing.T) {
	svc, ctrl := newSessionFixture(t)
	ctx := identity.ContextWithToken(t.Context(), callerToken)
	created, err := svc.StartSession(ctx, StartSession{Name: "sre", RequestID: "req-1"})
	require.NoError(t, err)
	var session struct {
		ContextID string `json:"contextId"`
	}
	require.NoError(t, json.Unmarshal(created.Session, &session))
	// The caller sent the first message itself; a start with a message under
	// the same request id finds another first turn and sends its message.
	_, err = svc.cfg.Sessions.SendMessage(ctx, callerToken, kagent.Ref{Namespace: "kagent", Name: "sre"}, session.ContextID, uuid.NewString(), "the caller's own")
	require.NoError(t, err)
	_, err = svc.StartSession(ctx, StartSession{Name: "sre", Message: "hello", RequestID: "req-1"})
	require.NoError(t, err)
	assert.Len(t, methodCalls(ctrl, "SendStreamingMessage"), 2)
	assert.Len(t, ctrl.Tasks(session.ContextID), 2)
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
	_, err := svc.GetSession(ctx, SessionQuery{SessionID: uuid.NewString()})
	require.ErrorIs(t, err, ErrNotFound)
	_, err = svc.GetSession(ctx, SessionQuery{SessionID: "not-a-uuid"})
	require.ErrorIs(t, err, ErrInvalid)
}

func TestDeleteSessionRemovesTheCallersOwn(t *testing.T) {
	svc, ctrl := newSessionFixture(t)
	ctx := identity.ContextWithToken(t.Context(), callerToken)
	session := ctrl.AddSession("kagent", "sre", &a2apb.Task{Id: uuid.NewString()})
	kept := ctrl.AddSession("kagent", "sre")

	deleted, err := svc.DeleteSession(ctx, session.GetId())
	require.NoError(t, err)
	var answered apiv1alpha1.Session
	require.NoError(t, protojson.Unmarshal(deleted.Session, &answered))
	assert.Equal(t, session.GetId(), answered.GetId())
	assert.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_DELETED, answered.GetState())
	assert.Empty(t, ctrl.Tasks(session.GetContextId()), "the runtime state goes with it")

	call := ctrl.Calls()[len(ctrl.Calls())-1]
	assert.Equal(t, "DeleteSession", call.Method)
	assert.Equal(t, []string{"Bearer " + callerToken}, call.Metadata.Get("authorization"), "deleted as the caller")

	list, err := svc.ListSessions(ctx, "", "sre", false)
	require.NoError(t, err)
	require.Len(t, list.Sessions, 1)
	assert.Contains(t, string(list.Sessions[0]), kept.GetId())
	_, err = svc.GetSession(ctx, SessionQuery{SessionID: session.GetId()})
	require.ErrorIs(t, err, ErrNotFound)
	_, err = svc.DeleteSession(ctx, session.GetId())
	require.ErrorIs(t, err, ErrNotFound, "a second delete finds nothing")
}

func TestDeleteSessionRefusesAnotherPersonsSession(t *testing.T) {
	svc, ctrl := newSessionFixture(t)
	ctx := identity.ContextWithToken(t.Context(), callerToken)
	theirs := ctrl.AddSessionOf("dev@lab.local", "kagent", "sre")

	_, err := svc.DeleteSession(ctx, theirs.GetId())
	require.ErrorIs(t, err, ErrNotFound)
	assert.Contains(t, err.Error(), theirs.GetId())
	list, err := svc.ListSessions(ctx, "", "sre", true)
	require.NoError(t, err)
	require.Len(t, list.Sessions, 1, "their session stays")
	assert.Contains(t, string(list.Sessions[0]), "RUNTIME_STATE_READY")
}

func TestDeleteSessionWhileAnotherOperationRunsAsksForARetry(t *testing.T) {
	svc, ctrl := newSessionFixture(t)
	session := ctrl.AddSession("kagent", "sre")
	ctrl.Fail["DeleteSession"] = status.Error(codes.Aborted, "session operation conflict")
	_, err := svc.DeleteSession(identity.ContextWithToken(t.Context(), callerToken), session.GetId())
	require.ErrorIs(t, err, ErrConflict)
	assert.Contains(t, err.Error(), "session operation conflict")
	assert.Contains(t, err.Error(), "retry the delete")
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
