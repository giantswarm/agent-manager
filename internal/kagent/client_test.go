package kagent_test

import (
	"fmt"
	"testing"
	"time"

	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/giantswarm/agent-manager/internal/kagent"
	apiv1alpha1 "github.com/giantswarm/agent-manager/internal/kagent/gen/kagent/api/v1alpha1"
	"github.com/giantswarm/agent-manager/internal/kagent/kagenttest"
)

const token = "caller-id-token"

var sre = kagent.Ref{Namespace: "kagent", Name: "sre"}

func TestParseTarget(t *testing.T) {
	for _, tc := range []struct {
		target   string
		hostPort string
		tls      bool
		err      string
	}{
		{target: "grpc://agentgateway.agent-platform.svc.cluster.local:8080", hostPort: "agentgateway.agent-platform.svc.cluster.local:8080"},
		{target: "grpcs://agentgateway.example.io", hostPort: "agentgateway.example.io:443", tls: true},
		{target: "grpcs://agentgateway.example.io:8443", hostPort: "agentgateway.example.io:8443", tls: true},
		{target: "grpc://agentgateway:8080/", hostPort: "agentgateway:8080"},
		{target: "grpc://agentgateway", err: "must name a port"},
		{target: "https://agentgateway.example.io", err: "scheme"},
		{target: "grpc://agentgateway:8080/kagent", err: "no path"},
		{target: "agentgateway:8080", err: "scheme"},
	} {
		t.Run(tc.target, func(t *testing.T) {
			hostPort, useTLS, err := kagent.ParseTarget(tc.target)
			if tc.err != "" {
				require.ErrorContains(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.hostPort, hostPort)
			assert.Equal(t, tc.tls, useTLS)
		})
	}
}

// assertCallerOnly: the bearer is the call's one identity; no identity or
// routing header the gateway owns is ever sent.
func assertCallerOnly(t *testing.T, calls []kagenttest.Call) {
	t.Helper()
	require.NotEmpty(t, calls)
	for _, call := range calls {
		assert.Equal(t, []string{"Bearer " + token}, call.Metadata.Get("authorization"), call.Method)
		for _, header := range []string{"x-user-id", "x-kagent-agent-instance-id", "x-share-token"} {
			assert.Empty(t, call.Metadata.Get(header), "%s sent %s", call.Method, header)
		}
	}
}

func TestListSessionsWalksPagesForOneAgent(t *testing.T) {
	ctrl, conn := kagenttest.New(t)
	for range 150 {
		ctrl.AddSession("kagent", "sre")
	}
	ctrl.AddSession("kagent", "other")
	c := kagent.NewClient(conn)

	sessions, err := c.ListSessions(t.Context(), token, sre, true)
	require.NoError(t, err)
	assert.Len(t, sessions, 150)
	for _, s := range sessions {
		assert.Equal(t, "sre", s.GetAgent().GetName())
	}

	calls := ctrl.Calls()
	require.Len(t, calls, 2, "two pages of 100")
	assertCallerOnly(t, calls)
	first := calls[0].Request.(*apiv1alpha1.ListSessionsRequest)
	assert.Equal(t, "kagent", first.GetAgent().GetNamespace())
	assert.Equal(t, "sre", first.GetAgent().GetName())
	assert.True(t, first.GetAllCreators())
	assert.EqualValues(t, 100, first.GetPage().GetLimit())
	assert.Equal(t, "100", calls[1].Request.(*apiv1alpha1.ListSessionsRequest).GetPage().GetPageToken())
}

func TestListSessionTasksAddressesTheAgentAndContext(t *testing.T) {
	ctrl, conn := kagenttest.New(t)
	var tasks []*a2apb.Task
	for i := range 120 {
		tasks = append(tasks, &a2apb.Task{Id: fmt.Sprintf("task-%03d", i)})
	}
	s := ctrl.AddSession("kagent", "sre", tasks...)
	c := kagent.NewClient(conn)

	got, err := c.ListSessionTasks(t.Context(), token, sre, s.GetContextId())
	require.NoError(t, err)
	require.Len(t, got, 120)
	assert.Equal(t, "task-000", got[0].GetId(), "oldest first")

	calls := ctrl.Calls()
	require.Len(t, calls, 2)
	assertCallerOnly(t, calls)
	req := calls[0].Request.(*a2apb.ListTasksRequest)
	assert.Equal(t, "kagent/sre", req.GetTenant())
	assert.Equal(t, s.GetContextId(), req.GetContextId())
	assert.True(t, req.GetIncludeArtifacts())

	// The wrong Agent cannot read the session.
	_, err = c.ListSessionTasks(t.Context(), token, kagent.Ref{Namespace: "kagent", Name: "other"}, s.GetContextId())
	assert.Equal(t, codes.NotFound, status.Code(err))
}

func TestCreateSessionAndSendMessage(t *testing.T) {
	ctrl, conn := kagenttest.New(t)
	c := kagent.NewClient(conn)

	s, err := c.CreateSession(t.Context(), token, sre, "req-1", "triage")
	require.NoError(t, err)
	again, err := c.CreateSession(t.Context(), token, sre, "req-1", "triage")
	require.NoError(t, err)
	assert.Equal(t, s.GetId(), again.GetId(), "a retry with the same request id is the same session")

	first, err := c.SendMessage(t.Context(), token, sre, s.GetContextId(), "msg-1", "hello")
	require.NoError(t, err)
	task := first.GetTask()
	require.NotNil(t, task, "the first event is the accepted task")
	assert.Equal(t, a2apb.TaskState_TASK_STATE_SUBMITTED, task.GetStatus().GetState())

	// The stream is closed once the turn is accepted; the turn finishes anyway.
	select {
	case <-ctrl.TurnEnded():
	case <-time.After(5 * time.Second):
		t.Fatal("the client kept the stream open")
	}
	tasks := ctrl.Tasks(s.GetContextId())
	require.Len(t, tasks, 1)
	assert.Equal(t, a2apb.TaskState_TASK_STATE_COMPLETED, tasks[0].GetStatus().GetState())

	calls := ctrl.Calls()
	assertCallerOnly(t, calls)
	send := calls[len(calls)-1].Request.(*a2apb.SendMessageRequest)
	assert.Equal(t, "kagent/sre", send.GetTenant())
	assert.Equal(t, s.GetId(), send.GetMessage().GetContextId(), "the session is the message's context")
	assert.Empty(t, send.GetMessage().GetTaskId(), "a first message starts a task")
	assert.Equal(t, a2apb.Role_ROLE_USER, send.GetMessage().GetRole())
	assert.Equal(t, "msg-1", send.GetMessage().GetMessageId())
	assert.Equal(t, "hello", send.GetMessage().GetParts()[0].GetText())
}

func TestCreateSessionRefusedWithoutAReadyRevision(t *testing.T) {
	ctrl, conn := kagenttest.New(t)
	ctrl.NotReady["kagent/sre"] = true
	_, err := kagent.NewClient(conn).CreateSession(t.Context(), token, sre, "req-1", "")
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
}

func TestNoTokenNoCall(t *testing.T) {
	ctrl, conn := kagenttest.New(t)
	c := kagent.NewClient(conn)
	_, err := c.ListSessions(t.Context(), "", sre, false)
	require.ErrorIs(t, err, kagent.ErrNoToken)
	_, err = c.GetSession(t.Context(), "", "id")
	require.ErrorIs(t, err, kagent.ErrNoToken)
	_, err = c.ListSessionTasks(t.Context(), "", sre, "id")
	require.ErrorIs(t, err, kagent.ErrNoToken)
	_, err = c.CreateSession(t.Context(), "", sre, "r", "")
	require.ErrorIs(t, err, kagent.ErrNoToken)
	_, err = c.SendMessage(t.Context(), "", sre, "id", "m", "hi")
	require.ErrorIs(t, err, kagent.ErrNoToken)
	assert.Empty(t, ctrl.Calls())
}
