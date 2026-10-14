package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/agent-manager/internal/identity"
	"github.com/giantswarm/agent-manager/internal/kagent"
	"github.com/giantswarm/agent-manager/internal/kagent/kagenttest"
)

// envelope is an MCP CallToolResult carrying text.
func envelope(text string, isError bool) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "isError": isError}
}

func encoded(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func agentSays(parts ...*a2apb.Part) *a2apb.Message {
	return kagenttest.Message(a2apb.Role_ROLE_AGENT, parts...)
}

func userSays(text string) *a2apb.Message {
	return kagenttest.Message(a2apb.Role_ROLE_USER, kagenttest.Text(text))
}

func taskOf(id string, state a2apb.TaskState, reply string, history ...*a2apb.Message) *a2apb.Task {
	status := &a2apb.TaskStatus{State: state}
	if reply != "" {
		status.Message = agentSays(kagenttest.Text(reply))
	}
	return &a2apb.Task{Id: id, Status: status, History: history}
}

// bigLogs is a tool response far larger than a summary.
var bigLogs = strings.Repeat("log line ", 20_000)

// conversation seeds a session of three turns: tool calls that succeed
// through muster's nested envelopes, one that fails, one with a large
// response, and a turn still working on a call.
func conversation(t *testing.T) (*Service, *kagenttest.Controller, context.Context, string) {
	t.Helper()
	svc, ctrl := newSessionFixture(t)
	nested := envelope(encoded(envelope(`{"pods":3}`, false)), false)
	s := ctrl.AddSession("kagent", "sre",
		taskOf("task-0", a2apb.TaskState_TASK_STATE_COMPLETED, "all good",
			userSays("check pods"),
			agentSays(kagenttest.ToolCall("adk_", "c1", "call_tool", map[string]any{"name": "x_kubernetes_list", "args": map[string]any{"kind": "pods"}})),
			agentSays(kagenttest.ToolResult("adk_", "c1", "call_tool", nested)),
		),
		taskOf("task-1", a2apb.TaskState_TASK_STATE_COMPLETED, "found it",
			userSays("why is it failing"),
			agentSays(kagenttest.ToolCall("kagent_", "c2", "get_logs", map[string]any{"pod": "web-0"})),
			agentSays(kagenttest.ToolResult("kagent_", "c2", "get_logs", map[string]any{"result": "forbidden", "isError": true})),
			agentSays(kagenttest.ToolCall("kagent_", "c3", "get_logs", map[string]any{"pod": "web-1"})),
			agentSays(kagenttest.ToolResult("kagent_", "c3", "get_logs", map[string]any{"result": bigLogs})),
		),
		taskOf("task-2", a2apb.TaskState_TASK_STATE_WORKING, "",
			userSays("and now?"),
			agentSays(kagenttest.ToolCall("adk_", "c4", "get_events", nil)),
		),
	)
	return svc, ctrl, identity.ContextWithToken(t.Context(), callerToken), s.GetId()
}

func TestGetSessionSummaryLeavesToolPayloadsOut(t *testing.T) {
	svc, ctrl, ctx, id := conversation(t)

	got, err := svc.GetSession(ctx, SessionQuery{SessionID: id})
	require.NoError(t, err)
	assert.Equal(t, 3, got.TotalTurns)
	assert.False(t, got.Truncated)
	require.Len(t, got.Turns, 3)
	assert.Equal(t, Turn{Index: 0, TaskID: "task-0", State: "TASK_STATE_COMPLETED", User: "check pods", Reply: "all good",
		ToolCalls: []ToolCallStub{{Index: 0, ID: "c1", Name: "call_tool"}}}, got.Turns[0])
	assert.Equal(t, []ToolCallStub{{Index: 0, ID: "c2", Name: "get_logs", Error: true}, {Index: 1, ID: "c3", Name: "get_logs"}}, got.Turns[1].ToolCalls)
	assert.Equal(t, []ToolCallStub{{Index: 0, ID: "c4", Name: "get_events", Pending: true}}, got.Turns[2].ToolCalls)
	assert.Empty(t, got.Turns[2].Reply)

	answer := encoded(got)
	assert.NotContains(t, answer, "log line", "no tool response")
	assert.NotContains(t, answer, "web-0", "no tool arguments")
	assert.Less(t, len(answer), 4096)

	list := methodCalls(ctrl, "ListTasks")
	require.Len(t, list, 1)
	req := list[0].(*a2apb.ListTasksRequest)
	require.NotNil(t, req.HistoryLength)
	assert.Zero(t, req.GetHistoryLength(), "the turns are listed without their history")
	assert.False(t, req.GetIncludeArtifacts())
	assert.Len(t, methodCalls(ctrl, "GetTask"), 3, "each turn answered is read whole")
}

// methodCalls are the requests of the calls of method the fake served.
func methodCalls(ctrl *kagenttest.Controller, method string) []any {
	var out []any
	for _, call := range ctrl.Calls() {
		if call.Method == method {
			out = append(out, call.Request)
		}
	}
	return out
}

func TestPollingTheLastTurnReadsOnlyIt(t *testing.T) {
	svc, ctrl, ctx, id := conversation(t)

	got, err := svc.GetSession(ctx, SessionQuery{SessionID: id, LastTurns: 1})
	require.NoError(t, err)
	require.Len(t, got.Turns, 1)
	assert.Equal(t, 2, got.Turns[0].Index)
	assert.Equal(t, 3, got.TotalTurns)
	gets := methodCalls(ctrl, "GetTask")
	require.Len(t, gets, 1)
	assert.Equal(t, "task-2", gets[0].(*a2apb.GetTaskRequest).GetId())
}

func TestGetSessionReadsAtMostMaxFetchedTurns(t *testing.T) {
	svc, ctrl := newSessionFixture(t)
	ctx := identity.ContextWithToken(t.Context(), callerToken)
	tasks := make([]*a2apb.Task, maxFetchedTurns+50)
	for i := range tasks {
		tasks[i] = taskOf(fmt.Sprintf("task-%d", i), a2apb.TaskState_TASK_STATE_COMPLETED, "ok")
	}
	s := ctrl.AddSession("kagent", "sre", tasks...)

	got, err := svc.GetSession(ctx, SessionQuery{SessionID: s.GetId()})
	require.NoError(t, err)
	assert.Len(t, got.Turns, maxFetchedTurns)
	assert.Equal(t, 50, got.OmittedTurns)
	assert.Equal(t, len(tasks)-1, got.Turns[len(got.Turns)-1].Index)
	assert.Contains(t, got.Hint, "lastTurns")
	assert.Len(t, methodCalls(ctrl, "GetTask"), maxFetchedTurns)
}

func TestGetSessionRefusesAConversationLongerThanItLists(t *testing.T) {
	svc, ctrl := newSessionFixture(t)
	ctx := identity.ContextWithToken(t.Context(), callerToken)
	tasks := make([]*a2apb.Task, kagent.MaxListedTasks+1)
	for i := range tasks {
		tasks[i] = &a2apb.Task{Id: fmt.Sprintf("task-%d", i)}
	}
	s := ctrl.AddSession("kagent", "sre", tasks...)

	_, err := svc.GetSession(ctx, SessionQuery{SessionID: s.GetId(), LastTurns: 1})
	require.ErrorIs(t, err, ErrConflict)
	assert.Contains(t, err.Error(), "more than 10000 turns")
	_, err = svc.GetSessionTurn(ctx, TurnQuery{SessionID: s.GetId(), Turn: "last"})
	require.ErrorIs(t, err, ErrConflict)
	assert.Empty(t, methodCalls(ctrl, "GetTask"), "no turn is answered as the newest")
}

func TestASessionWithoutTurnsAnswersAnEmptyList(t *testing.T) {
	svc, ctrl := newSessionFixture(t)
	ctx := identity.ContextWithToken(t.Context(), callerToken)
	s := ctrl.AddSession("kagent", "sre")
	for detail, key := range map[string]string{DetailSummary: `"turns":[]`, DetailFull: `"tasks":[]`} {
		got, err := svc.GetSession(ctx, SessionQuery{SessionID: s.GetId(), Detail: detail})
		require.NoError(t, err)
		assert.Contains(t, encoded(got), key, detail)
	}
}

func TestAnArtifactReplyIsAnswered(t *testing.T) {
	svc, ctrl := newSessionFixture(t)
	ctx := identity.ContextWithToken(t.Context(), callerToken)
	task := taskOf("task-0", a2apb.TaskState_TASK_STATE_COMPLETED, "", userSays("report please"))
	task.Artifacts = []*a2apb.Artifact{
		{ArtifactId: "a1", Name: "report", Parts: []*a2apb.Part{kagenttest.Text("the report")}},
		{ArtifactId: "a2", Name: "big", Parts: []*a2apb.Part{kagenttest.Text(strings.Repeat("x", 20_000))}},
	}
	s := ctrl.AddSession("kagent", "sre", task)

	got, err := svc.GetSession(ctx, SessionQuery{SessionID: s.GetId()})
	require.NoError(t, err)
	require.Len(t, got.Turns, 1)
	assert.Equal(t, 2, got.Turns[0].Artifacts)
	assert.True(t, strings.HasPrefix(got.Turns[0].Reply, "the report\n"), "the artifacts answer for an empty status message")

	// Artifacts follow the messages, paged as they are; one too big alone is
	// clipped in the transcript and named in the full view.
	turn, err := svc.GetSessionTurn(ctx, TurnQuery{SessionID: s.GetId(), Turn: "0", MaxBytes: MinSessionMaxBytes})
	require.NoError(t, err)
	assert.Equal(t, 3, turn.TotalMessages)
	require.Len(t, turn.Messages, 2)
	assert.Equal(t, TurnMessage{Index: 1, Role: "artifact", Name: "report", Parts: []PartView{{Text: "the report"}}}, turn.Messages[1])
	assert.Equal(t, 2, turn.NextMessage)
	big, err := svc.GetSessionTurn(ctx, TurnQuery{SessionID: s.GetId(), Turn: "0", FromMessage: 2, MaxBytes: MinSessionMaxBytes})
	require.NoError(t, err)
	require.Len(t, big.Messages, 1)
	assert.True(t, big.Messages[0].Clipped)
	assert.Contains(t, big.Messages[0].Parts[0].Text, "bytes clipped]")
	assert.LessOrEqual(t, len(encoded(big)), MinSessionMaxBytes)
	full, err := svc.GetSessionTurn(ctx, TurnQuery{SessionID: s.GetId(), Turn: "0", Detail: DetailFull})
	require.NoError(t, err)
	require.Len(t, full.History, 3)
	assert.Contains(t, string(full.History[1]), `"artifact":{"artifactId":"a1"`)
}

func TestGetSessionTranscriptClipsArgumentsAndStubsResults(t *testing.T) {
	svc, _, ctx, id := conversation(t)

	got, err := svc.GetSession(ctx, SessionQuery{SessionID: id, Detail: DetailTranscript, LastTurns: 2})
	require.NoError(t, err)
	require.Len(t, got.Turns, 2, "the last two turns")
	assert.Equal(t, 1, got.Turns[0].Index)
	assert.Zero(t, got.OmittedTurns, "turns left out by lastTurns are not omitted by the budget")
	assert.Empty(t, got.Turns[0].User, "the messages hold the user's message")
	assert.Empty(t, got.Turns[0].Reply, "the messages hold the reply")
	assert.Empty(t, got.Turns[0].ToolCalls, "the messages hold the tool calls")
	msgs := got.Turns[0].Messages
	require.Len(t, msgs, 6, "four tool messages, the user's and the status message")
	assert.Equal(t, TurnMessage{Index: 0, Role: "user", Parts: []PartView{{Text: "why is it failing"}}}, msgs[0])
	assert.Equal(t, &ToolCallView{Index: 0, ID: "c2", Name: "get_logs", Args: `{"pod":"web-0"}`}, msgs[1].Parts[0].ToolCall)
	assert.Equal(t, "agent", msgs[1].Role)
	big := msgs[4].Parts[0].ToolResult
	require.NotNil(t, big)
	assert.Equal(t, 1, big.Index)
	assert.Greater(t, big.Bytes, len(bigLogs))
	assert.Equal(t, PartView{Text: "found it"}, msgs[5].Parts[0])
	assert.NotContains(t, encoded(got), "log line")
}

func TestGetSessionKeepsTheNewestTurnsWithinMaxBytes(t *testing.T) {
	svc, ctrl := newSessionFixture(t)
	ctx := identity.ContextWithToken(t.Context(), callerToken)
	var tasks []*a2apb.Task
	for i := range 40 {
		tasks = append(tasks, taskOf("task-"+string(rune('a'+i%26))+strings.Repeat("x", i), a2apb.TaskState_TASK_STATE_COMPLETED, strings.Repeat("reply ", 100), userSays("question")))
	}
	s := ctrl.AddSession("kagent", "sre", tasks...)

	for _, detail := range []string{DetailSummary, DetailTranscript, DetailFull} {
		got, err := svc.GetSession(ctx, SessionQuery{SessionID: s.GetId(), Detail: detail, MaxBytes: MinSessionMaxBytes})
		require.NoError(t, err, detail)
		assert.LessOrEqual(t, len(encoded(got)), MinSessionMaxBytes, detail)
		assert.True(t, got.Truncated, detail)
		assert.NotEmpty(t, got.Hint, detail)
		kept := len(got.Turns) + len(got.Tasks)
		require.Positive(t, kept, detail)
		assert.Equal(t, 40, got.OmittedTurns+kept, detail)
		if detail != DetailFull {
			assert.Equal(t, 39, got.Turns[len(got.Turns)-1].Index, "the newest turn is kept")
		}
	}
}

func TestGetSessionClipsANewestTurnThatAloneIsTooBig(t *testing.T) {
	svc, ctrl := newSessionFixture(t)
	ctx := identity.ContextWithToken(t.Context(), callerToken)
	s := ctrl.AddSession("kagent", "sre", taskOf("task-0", a2apb.TaskState_TASK_STATE_COMPLETED, strings.Repeat("é", 10_000), userSays("question")))

	got, err := svc.GetSession(ctx, SessionQuery{SessionID: s.GetId(), MaxBytes: MinSessionMaxBytes})
	require.NoError(t, err)
	require.Len(t, got.Turns, 1)
	assert.True(t, got.Turns[0].Clipped)
	assert.True(t, got.Truncated)
	assert.Contains(t, got.Turns[0].Reply, "bytes clipped]")
	assert.LessOrEqual(t, len(encoded(got)), MinSessionMaxBytes)
	assert.Zero(t, got.OmittedTurns)
	assert.Equal(t, clippedTurnHint, got.Hint, "no older turn was left out")

	full, err := svc.GetSession(ctx, SessionQuery{SessionID: s.GetId(), Detail: DetailFull, MaxBytes: MinSessionMaxBytes})
	require.NoError(t, err)
	assert.Empty(t, full.Tasks, "a raw task is never cut")
	assert.Equal(t, 1, full.OmittedTurns)
	assert.Contains(t, full.Hint, "get_session_turn")
}

func TestGetSessionChecksItsArguments(t *testing.T) {
	svc, ctrl, ctx, id := conversation(t)
	calls := len(ctrl.Calls())
	for name, q := range map[string]SessionQuery{
		"unknown detail":    {SessionID: id, Detail: "everything"},
		"negative turns":    {SessionID: id, LastTurns: -1},
		"budget too small":  {SessionID: id, MaxBytes: MinSessionMaxBytes - 1},
		"budget too large":  {SessionID: id, MaxBytes: MaxSessionMaxBytes + 1},
		"session id absent": {},
	} {
		_, err := svc.GetSession(ctx, q)
		require.ErrorIs(t, err, ErrInvalid, name)
	}
	assert.Len(t, ctrl.Calls(), calls, "nothing reaches kagent")
}

func TestGetSessionTurnAddressesATurn(t *testing.T) {
	svc, ctrl, ctx, id := conversation(t)

	for _, tc := range []struct {
		ref  IndexOrID
		want string
	}{{"last", "task-2"}, {"1", "task-1"}, {"task-0", "task-0"}} {
		got, err := svc.GetSessionTurn(ctx, TurnQuery{SessionID: id, Turn: tc.ref})
		require.NoError(t, err, tc.ref)
		assert.Equal(t, tc.want, got.TaskID, tc.ref)
		assert.Equal(t, 3, got.TotalTurns)
		assert.Equal(t, DetailTranscript, got.Detail)
	}
	calls := ctrl.Calls()
	list := calls[len(calls)-2].Request.(*a2apb.ListTasksRequest)
	require.NotNil(t, list.HistoryLength)
	assert.Zero(t, list.GetHistoryLength(), "the turn is found without reading any history")
	get := calls[len(calls)-1].Request.(*a2apb.GetTaskRequest)
	assert.Equal(t, "kagent/sre", get.GetTenant())
	assert.Equal(t, "task-0", get.GetId())

	got, err := svc.GetSessionTurn(ctx, TurnQuery{SessionID: id, Turn: "1"})
	require.NoError(t, err)
	assert.Equal(t, 6, got.TotalMessages)
	assert.Len(t, got.Messages, 6)
	assert.Zero(t, got.NextMessage)
	assert.False(t, got.Truncated)

	// Another session's task is not a turn of this one.
	other := ctrl.AddSession("kagent", "sre", taskOf("task-elsewhere", a2apb.TaskState_TASK_STATE_COMPLETED, "secret"))
	_, err = svc.GetSessionTurn(ctx, TurnQuery{SessionID: id, Turn: "task-elsewhere"})
	require.ErrorIs(t, err, ErrNotFound)
	_, err = svc.GetSessionTurn(ctx, TurnQuery{SessionID: other.GetId(), Turn: "0"})
	require.NoError(t, err)
	for _, ref := range []IndexOrID{"3", "-1", "nope"} {
		_, err = svc.GetSessionTurn(ctx, TurnQuery{SessionID: id, Turn: ref})
		require.ErrorIs(t, err, ErrNotFound, ref)
	}
}

func TestGetSessionTurnAnswersAToolCallWhole(t *testing.T) {
	svc, _, ctx, id := conversation(t)

	got, err := svc.GetSessionTurn(ctx, TurnQuery{SessionID: id, Turn: "0", ToolCall: "0"})
	require.NoError(t, err)
	require.NotNil(t, got.ToolCall)
	assert.Empty(t, got.Messages)
	assert.JSONEq(t, `{"name":"x_kubernetes_list","args":{"kind":"pods"}}`, string(got.ToolCall.Args))
	assert.JSONEq(t, `{"pods":3}`, string(got.ToolCall.Response), "unwrapped through both MCP envelopes")
	assert.False(t, got.ToolCall.Error)

	failed, err := svc.GetSessionTurn(ctx, TurnQuery{SessionID: id, Turn: "1", ToolCall: "c2"})
	require.NoError(t, err)
	assert.True(t, failed.ToolCall.Error)
	assert.JSONEq(t, `"forbidden"`, string(failed.ToolCall.Response))

	big, err := svc.GetSessionTurn(ctx, TurnQuery{SessionID: id, Turn: "1", ToolCall: "1", MaxBytes: 10_000})
	require.NoError(t, err)
	assert.True(t, big.Truncated)
	assert.Nil(t, big.ToolCall.Response)
	assert.True(t, strings.HasPrefix(big.ToolCall.ResponseText, "log line log line"))
	assert.Contains(t, big.ToolCall.ResponseText, "bytes clipped]")
	assert.LessOrEqual(t, len(encoded(big)), 10_000)

	pending, err := svc.GetSessionTurn(ctx, TurnQuery{SessionID: id, Turn: "last", ToolCall: "0"})
	require.NoError(t, err)
	assert.True(t, pending.ToolCall.Pending)

	_, err = svc.GetSessionTurn(ctx, TurnQuery{SessionID: id, Turn: "0", ToolCall: "7"})
	require.ErrorIs(t, err, ErrNotFound)
}

func TestAToolCallsSmallResponseStaysWholeBesideLargeArguments(t *testing.T) {
	svc, ctrl := newSessionFixture(t)
	ctx := identity.ContextWithToken(t.Context(), callerToken)
	s := ctrl.AddSession("kagent", "sre", taskOf("task-0", a2apb.TaskState_TASK_STATE_COMPLETED, "done",
		agentSays(kagenttest.ToolCall("adk_", "c1", "write_file", map[string]any{"content": strings.Repeat("y", 50_000)})),
		agentSays(kagenttest.ToolResult("adk_", "c1", "write_file", map[string]any{"ok": true})),
	))

	got, err := svc.GetSessionTurn(ctx, TurnQuery{SessionID: s.GetId(), Turn: "0", ToolCall: "0", MaxBytes: 10_000})
	require.NoError(t, err)
	assert.True(t, got.Truncated)
	assert.JSONEq(t, `{"ok":true}`, string(got.ToolCall.Response), "the response was not clipped")
	assert.Empty(t, got.ToolCall.ResponseText)
	assert.Nil(t, got.ToolCall.Args)
	assert.Greater(t, len(got.ToolCall.ArgsText), 8_000, "the arguments get the room the response leaves")
	assert.LessOrEqual(t, len(encoded(got)), 10_000)
}

func TestGetSessionTurnPagesItsMessages(t *testing.T) {
	svc, _, ctx, id := conversation(t)
	q := TurnQuery{SessionID: id, Turn: "1", Detail: DetailFull, MaxBytes: MinSessionMaxBytes}

	var seen int
	for range 10 {
		got, err := svc.GetSessionTurn(ctx, q)
		require.NoError(t, err)
		assert.LessOrEqual(t, len(encoded(got)), MinSessionMaxBytes)
		require.NotEmpty(t, got.History, "every page makes progress")
		seen += len(got.History)
		if got.NextMessage == 0 {
			break
		}
		assert.True(t, got.Truncated)
		assert.Equal(t, q.FromMessage+len(got.History), got.NextMessage)
		q.FromMessage = got.NextMessage
	}
	assert.Equal(t, 6, seen)
}

func TestGetSessionTurnChecksItsArguments(t *testing.T) {
	svc, ctrl, ctx, id := conversation(t)
	calls := len(ctrl.Calls())
	for name, q := range map[string]TurnQuery{
		"no turn":             {SessionID: id},
		"summary detail":      {SessionID: id, Turn: "0", Detail: DetailSummary},
		"negative message":    {SessionID: id, Turn: "0", FromMessage: -1},
		"budget out of range": {SessionID: id, Turn: "0", MaxBytes: 10},
	} {
		_, err := svc.GetSessionTurn(ctx, q)
		require.ErrorIs(t, err, ErrInvalid, name)
	}
	assert.Len(t, ctrl.Calls(), calls, "nothing reaches kagent")
	_, err := svc.GetSessionTurn(ctx, TurnQuery{SessionID: id, Turn: "0", FromMessage: 9})
	require.ErrorIs(t, err, ErrInvalid)
}

func TestIndexOrIDReadsNumbersAndStrings(t *testing.T) {
	var q TurnQuery
	require.NoError(t, json.Unmarshal([]byte(`{"turn":3,"toolCall":"c1"}`), &q))
	assert.Equal(t, IndexOrID("3"), q.Turn)
	assert.Equal(t, IndexOrID("c1"), q.ToolCall)
	require.Error(t, json.Unmarshal([]byte(`{"turn":{}}`), &q))
}
