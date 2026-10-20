package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/giantswarm/agent-manager/internal/identity"
	"github.com/giantswarm/agent-manager/internal/kagent"
	apiv1alpha1 "github.com/giantswarm/agent-manager/internal/kagent/gen/kagent/api/v1alpha1"
	"github.com/giantswarm/agent-manager/internal/muster"
)

const (
	// MaxSessionMessageLength bounds the first message of start_session, as
	// the portal bounds a message.
	MaxSessionMessageLength = 32000
	// maxRequestIDLength and maxSessionNameLength are the controller's bounds
	// on CreateSessionRequest, checked here so a bad value reads in our words.
	maxRequestIDLength   = 128
	maxSessionNameLength = 200
)

// SessionClient is what the session operations need from the kagent
// controller: SessionService and the A2A service, called through agentgateway
// with the caller's token (kagent.Client).
type SessionClient interface {
	ListSessions(ctx context.Context, token string, agent kagent.Ref, allCreators bool) ([]*apiv1alpha1.Session, error)
	GetSession(ctx context.Context, token, sessionID string) (*apiv1alpha1.Session, error)
	ListSessionTasks(ctx context.Context, token string, agent kagent.Ref, contextID string, opts kagent.TaskListOptions) (*kagent.TaskList, error)
	GetSessionTask(ctx context.Context, token string, agent kagent.Ref, taskID string) (*a2apb.Task, error)
	CreateSession(ctx context.Context, token string, in kagent.SessionCreate) (*apiv1alpha1.Session, error)
	SendMessage(ctx context.Context, token string, agent kagent.Ref, contextID, messageID, text string) (*a2apb.StreamResponse, error)
	DeleteSession(ctx context.Context, token, sessionID string) (*apiv1alpha1.Session, error)
}

// SessionBinder is muster's session bindings, called as the caller: a
// workspace Session's sandbox actor bound to the person who started it is
// what muster releases that person's provider tokens to (muster.Client).
type SessionBinder interface {
	Bind(ctx context.Context, token string, b muster.Binding) error
	Unbind(ctx context.Context, token, actor string) error
}

// SessionList is the answer of list_sessions: the Agent's sessions as kagent
// reports them (proto3 JSON of kagent.api.v1alpha1.Session, state as
// RUNTIME_STATE_*).
type SessionList struct {
	Agent    AgentRef          `json:"agent"`
	Sessions []json.RawMessage `json:"sessions"`
}

// SessionQuery is the input of get_session.
type SessionQuery struct {
	SessionID string `json:"sessionId"`
	// Detail is DetailSummary (default), DetailTranscript or DetailFull.
	Detail string `json:"detail,omitempty"`
	// LastTurns reads only the last n turns; 0 reads all.
	LastTurns int `json:"lastTurns,omitempty"`
	// MaxBytes bounds the answer; 0 is DefaultSessionMaxBytes.
	MaxBytes int `json:"maxBytes,omitempty"`
}

// SessionDetail is the answer of get_session: the session and its
// conversation, one turn per A2A task of the session's context, oldest first.
// Turns holds the summary and transcript views, Tasks the full view (proto3
// JSON of lf.a2a.v1.Task: status, history, artifacts); the view's list is
// always present. The newest turns that fit the budget are kept;
// OmittedTurns counts the older ones left out.
type SessionDetail struct {
	Session      json.RawMessage   `json:"session"`
	Detail       string            `json:"detail"`
	TotalTurns   int               `json:"totalTurns"`
	Truncated    bool              `json:"truncated,omitempty"`
	OmittedTurns int               `json:"omittedTurns,omitempty"`
	Hint         string            `json:"hint,omitempty"`
	Turns        []Turn            `json:"turns,omitzero"`
	Tasks        []json.RawMessage `json:"tasks,omitzero"`
}

// IndexOrID names a turn or a tool call by its index or its id. It reads a
// JSON number or string.
type IndexOrID string

// UnmarshalJSON implements json.Unmarshaler.
func (v *IndexOrID) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*v = IndexOrID(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(data, &n); err != nil {
		return fmt.Errorf("want an index or an id: %w", err)
	}
	*v = IndexOrID(n.String())
	return nil
}

// TurnQuery is the input of get_session_turn.
type TurnQuery struct {
	SessionID string `json:"sessionId"`
	// Turn is the turn's task id, its index or "last".
	Turn IndexOrID `json:"turn"`
	// Detail is DetailTranscript (default) or DetailFull.
	Detail string `json:"detail,omitempty"`
	// ToolCall answers one tool call of the turn whole, by its index or id.
	ToolCall IndexOrID `json:"toolCall,omitempty"`
	// FromMessage is the first message answered.
	FromMessage int `json:"fromMessage,omitempty"`
	// MaxBytes bounds the answer; 0 is DefaultSessionMaxBytes.
	MaxBytes int `json:"maxBytes,omitempty"`
}

// SessionTurn is the answer of get_session_turn: one turn, either its
// messages, then its artifacts, from FromMessage on that fit the budget
// (Messages in the transcript view; History in the full view, an
// lf.a2a.v1.Message or {artifact: lf.a2a.v1.Artifact} each, proto3 JSON;
// NextMessage continues them) or, asked for one, a tool call whole.
type SessionTurn struct {
	SessionID     string            `json:"sessionId"`
	Detail        string            `json:"detail"`
	Index         int               `json:"index"`
	TotalTurns    int               `json:"totalTurns"`
	TaskID        string            `json:"taskId"`
	State         string            `json:"state"`
	Timestamp     string            `json:"timestamp,omitempty"`
	TotalMessages int               `json:"totalMessages"`
	FromMessage   int               `json:"fromMessage"`
	NextMessage   int               `json:"nextMessage,omitempty"`
	Truncated     bool              `json:"truncated,omitempty"`
	Hint          string            `json:"hint,omitempty"`
	Messages      []TurnMessage     `json:"messages,omitzero"`
	History       []json.RawMessage `json:"history,omitzero"`
	ToolCall      *ToolCallDetail   `json:"toolCall,omitempty"`
}

// SessionStart is the answer of start_session: the new session and, when it
// was started with a first message, the first event of its first turn (proto3
// JSON of lf.a2a.v1.StreamResponse, usually {task} in TASK_STATE_SUBMITTED or
// WORKING). The turn keeps running; its answer is read with get_session.
// Workspace names the workspace the session works in, when it was started with
// a grant.
type SessionStart struct {
	Session   json.RawMessage `json:"session"`
	Turn      json.RawMessage `json:"turn,omitempty"`
	Workspace string          `json:"workspace,omitempty"`
}

// SessionDelete is the answer of delete_session: the session as the
// controller left it (RUNTIME_STATE_DELETED). Warning says what was left
// undone after the delete: a binding muster did not remove.
type SessionDelete struct {
	Session json.RawMessage `json:"session"`
	Warning string          `json:"warning,omitempty"`
}

// AgentRef names an Agent.
type AgentRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// StartSession is the input of start_session.
type StartSession struct {
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	// Message is the first user message of the conversation. Empty creates
	// the session without a turn: the caller sends the first message itself.
	Message string `json:"message,omitempty"`
	// RequestID makes a retried start idempotent: the same id answers the
	// session the first attempt created and sends its first message once,
	// answering the turn it started. Empty: a new id per call.
	RequestID string `json:"requestId,omitempty"`
	// SessionName is the conversation's display name; empty leaves it unnamed.
	SessionName string `json:"sessionName,omitempty"`
	// WorkspaceGrant starts the session in a workspace: the grant a workspace
	// manager's authorize_workspace returned to the caller. Empty starts it
	// without one, as the surfaces do today.
	WorkspaceGrant string `json:"workspaceGrant,omitempty"`
}

var protoJSON = protojson.MarshalOptions{}

// SessionsAvailable says whether the session operations are offered: a
// kagent controller target is configured.
func (s *Service) SessionsAvailable() bool { return s.cfg.Sessions != nil }

// ListSessions is the caller's sessions of one Agent (every person's with
// allCreators, when the controller grants the caller that).
func (s *Service) ListSessions(ctx context.Context, namespace, name string, allCreators bool) (*SessionList, error) {
	ref, token, err := s.sessionCall(ctx, namespace, name)
	if err != nil {
		return nil, err
	}
	sessions, err := s.cfg.Sessions.ListSessions(ctx, token, ref, allCreators)
	if err != nil {
		return nil, kagentError(err, fmt.Sprintf("sessions of agent %s", ref.Tenant()))
	}
	out := &SessionList{Agent: AgentRef{Namespace: ref.Namespace, Name: ref.Name}, Sessions: []json.RawMessage{}}
	for _, session := range sessions {
		raw, err := marshalProto(session)
		if err != nil {
			return nil, err
		}
		out.Sessions = append(out.Sessions, raw)
	}
	return out, nil
}

var (
	omittedTurnsHint = fmt.Sprintf("older turns were left out to fit maxBytes: raise maxBytes (at most %d), pick turns with lastTurns, or read one turn with get_session_turn", MaxSessionMaxBytes)
	clippedTurnHint  = "the newest turn alone exceeds maxBytes and was clipped: read it with get_session_turn"
)

const (
	// envelopeSlack is what an answer's fields set after its budget was
	// taken (truncated, omitted counts, a short hint) may add.
	envelopeSlack = 128
	// fetchBatch turns are read whole at a time, newest first, until the
	// budget is spent; maxFetchedTurns bounds how many one answer reads.
	fetchBatch      = 8
	maxFetchedTurns = 200
)

// GetSession is one session and its conversation, the A2A tasks of the
// session's context read from the session's Agent, in the view q asks for
// and within its budget. The tasks are listed without their history; only
// the newest that fit are read whole.
func (s *Service) GetSession(ctx context.Context, q SessionQuery) (*SessionDetail, error) {
	detail, err := sessionDetailLevel(q.Detail, DetailSummary, DetailSummary, DetailTranscript, DetailFull)
	if err != nil {
		return nil, err
	}
	maxBytes, err := sessionMaxBytes(q.MaxBytes)
	if err != nil {
		return nil, err
	}
	if q.LastTurns < 0 {
		return nil, invalidf("lastTurns must not be negative")
	}
	sc, err := s.readSession(ctx, q.SessionID)
	if err != nil {
		return nil, err
	}
	tasks, err := s.listTurns(ctx, sc, q.SessionID)
	if err != nil {
		return nil, err
	}
	out := &SessionDetail{Session: sc.raw, Detail: detail, TotalTurns: len(tasks)}
	budget := maxBytes - jsonSize(out) - len(omittedTurnsHint) - envelopeSlack
	first := 0
	if q.LastTurns > 0 {
		first = max(len(tasks)-q.LastTurns, 0)
	}
	fetch := func(batch []*a2apb.Task) ([]*a2apb.Task, error) { return s.fetchTasks(ctx, sc, batch) }
	clipped := false
	if detail == DetailFull {
		out.Tasks, out.OmittedTurns, _, err = packNewestTurns(tasks, first, budget, fetch, func(t *turn) (json.RawMessage, error) {
			return marshalProto(t.task)
		}, nil)
		if out.Tasks == nil {
			out.Tasks = []json.RawMessage{}
		}
	} else {
		render := func(t *turn) (Turn, error) { return t.summary(), nil }
		if detail == DetailTranscript {
			render = func(t *turn) (Turn, error) { return t.transcript(), nil }
		}
		out.Turns, out.OmittedTurns, clipped, err = packNewestTurns(tasks, first, budget, fetch, render, func(t *turn, budget int) (Turn, bool) {
			return fitTurn(t.summary(), budget)
		})
		if out.Turns == nil {
			out.Turns = []Turn{}
		}
	}
	if err != nil {
		return nil, err
	}
	switch {
	case out.OmittedTurns > 0:
		out.Truncated, out.Hint = true, omittedTurnsHint
	case clipped:
		out.Truncated, out.Hint = true, clippedTurnHint
	}
	return out, nil
}

// packNewestTurns reads the turns tasks[first:] whole, newest first and a
// batch at a time, renders them and keeps those that fit budget bytes. It
// answers them oldest first and how many older turns it left out. When even
// the newest does not fit, fit may cut it down (clipped); without fit it is
// left out too.
func packNewestTurns[T any](tasks []*a2apb.Task, first, budget int, fetch func([]*a2apb.Task) ([]*a2apb.Task, error), render func(*turn) (T, error), fit func(*turn, int) (T, bool)) (kept []T, omitted int, clipped bool, err error) {
	used := 0
	next := len(tasks) - 1
	fetched := 0
pack:
	for next >= first && fetched < maxFetchedTurns {
		n := min(fetchBatch, next-first+1, maxFetchedTurns-fetched)
		batch, err := fetch(tasks[next-n+1 : next+1])
		if err != nil {
			return nil, 0, false, err
		}
		fetched += n
		for j := n - 1; j >= 0; j-- {
			t := readTurn(next, batch[j])
			item, err := render(t)
			if err != nil {
				return nil, 0, false, err
			}
			if size := jsonSize(item) + 1; used+size <= budget {
				kept, used, next = append(kept, item), used+size, next-1
				continue
			}
			if len(kept) == 0 && fit != nil {
				if cut, ok := fit(t, budget-1); ok {
					kept, clipped, next = append(kept, cut), true, next-1
				}
			}
			break pack
		}
	}
	slices.Reverse(kept)
	return kept, next - first + 1, clipped, nil
}

// GetSessionTurn is one turn of a session's conversation: its messages and
// artifacts from q.FromMessage on in the view q asks for, or one of its tool
// calls whole, within q's budget.
func (s *Service) GetSessionTurn(ctx context.Context, q TurnQuery) (*SessionTurn, error) {
	detail, err := sessionDetailLevel(q.Detail, DetailTranscript, DetailTranscript, DetailFull)
	if err != nil {
		return nil, err
	}
	maxBytes, err := sessionMaxBytes(q.MaxBytes)
	if err != nil {
		return nil, err
	}
	if q.FromMessage < 0 {
		return nil, invalidf("fromMessage must not be negative")
	}
	ref := strings.TrimSpace(string(q.Turn))
	if ref == "" {
		return nil, invalidf("turn is required: a task id, an index or last")
	}
	sc, err := s.readSession(ctx, q.SessionID)
	if err != nil {
		return nil, err
	}
	tasks, err := s.listTurns(ctx, sc, q.SessionID)
	if err != nil {
		return nil, err
	}
	index := turnIndex(tasks, ref)
	if index < 0 {
		return nil, notFoundf("session %s has no turn %s (it has %d, indexed from 0)", q.SessionID, ref, len(tasks))
	}
	whole, err := s.fetchTasks(ctx, sc, tasks[index:index+1])
	if err != nil {
		return nil, err
	}
	task := whole[0]
	if task.GetContextId() != "" && task.GetContextId() != sc.contextID {
		return nil, notFoundf("session %s has no turn %s", q.SessionID, ref)
	}
	t := readTurn(index, task)
	out := &SessionTurn{
		SessionID: q.SessionID, Detail: detail, Index: index, TotalTurns: len(tasks), TaskID: task.GetId(),
		State: t.state(), Timestamp: t.timestamp(), TotalMessages: t.items(), FromMessage: q.FromMessage,
	}
	if q.ToolCall != "" {
		call := t.call(strings.TrimSpace(string(q.ToolCall)))
		if call == nil {
			return nil, notFoundf("turn %d of session %s has no tool call %s (it has %d, indexed from 0)", index, q.SessionID, q.ToolCall, len(t.calls))
		}
		answer, clipped := t.toolCallDetail(call, maxBytes-jsonSize(out)-envelopeSlack)
		out.ToolCall, out.Truncated = &answer, clipped
		return out, nil
	}
	if q.FromMessage > t.items() {
		return nil, invalidf("fromMessage %d is past the turn's %d messages", q.FromMessage, t.items())
	}
	budget := maxBytes - jsonSize(out) - envelopeSlack
	next := 0
	if detail == DetailFull {
		out.History, next, err = packForward(t.items(), q.FromMessage, budget, t.rawItem, func(i int, raw json.RawMessage, _ int) json.RawMessage {
			placeholder, _ := json.Marshal(map[string]any{
				"index":   i,
				"omitted": fmt.Sprintf("%d bytes exceed maxBytes: read it clipped with detail transcript, or its tool calls with toolCall", len(raw)),
			})
			return placeholder
		})
		if err != nil {
			return nil, err
		}
		if out.History == nil {
			out.History = []json.RawMessage{}
		}
	} else {
		out.Messages, next, _ = packForward(t.items(), q.FromMessage, budget, func(i int) (TurnMessage, error) {
			return t.messageView(i), nil
		}, func(_ int, m TurnMessage, budget int) TurnMessage { return fitMessage(m, budget) })
		if out.Messages == nil {
			out.Messages = []TurnMessage{}
		}
	}
	if next < t.items() {
		out.NextMessage, out.Truncated, out.Hint = next, true, "continue with fromMessage: "+fmt.Sprint(next)
	}
	return out, nil
}

// listTurns is the session's tasks without their history, oldest first:
// what its turns are addressed by. A conversation longer than agent-manager
// lists is refused, as its newest turns could not be named.
func (s *Service) listTurns(ctx context.Context, sc *sessionRef, sessionID string) ([]*a2apb.Task, error) {
	none := int32(0)
	list, err := s.cfg.Sessions.ListSessionTasks(ctx, sc.token, sc.agent, sc.contextID, kagent.TaskListOptions{HistoryLength: &none})
	if err != nil {
		return nil, kagentError(err, fmt.Sprintf("conversation of session %s", sessionID))
	}
	if !list.Complete {
		return nil, conflictf("session %s has more than %d turns, more than agent-manager reads", sessionID, kagent.MaxListedTasks)
	}
	return list.Tasks, nil
}

// fetchTasks reads tasks whole (A2AService/GetTask), fetchBatch at a time,
// in their order.
func (s *Service) fetchTasks(ctx context.Context, sc *sessionRef, tasks []*a2apb.Task) ([]*a2apb.Task, error) {
	out := make([]*a2apb.Task, len(tasks))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(fetchBatch)
	for i, task := range tasks {
		g.Go(func() error {
			whole, err := s.cfg.Sessions.GetSessionTask(gctx, sc.token, sc.agent, task.GetId())
			if err != nil {
				return kagentError(err, fmt.Sprintf("turn %s", task.GetId()))
			}
			out[i] = whole
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return out, nil
}

// turnIndex is the index of the turn ref names in tasks: "last", an index
// or a task id; -1 for none.
func turnIndex(tasks []*a2apb.Task, ref string) int {
	if ref == "last" {
		return len(tasks) - 1
	}
	if n, err := strconv.Atoi(ref); err == nil {
		if n < 0 || n >= len(tasks) {
			return -1
		}
		return n
	}
	for i, task := range tasks {
		if task.GetId() == ref {
			return i
		}
	}
	return -1
}

// packForward renders items from on while they fit budget bytes and answers
// them and the index of the first left out. The first item is always kept:
// fit cuts it down when it alone exceeds the budget.
func packForward[T any](n, from, budget int, render func(int) (T, error), fit func(int, T, int) T) ([]T, int, error) {
	var out []T
	used := 0
	i := from
	for ; i < n; i++ {
		item, err := render(i)
		if err != nil {
			return nil, 0, err
		}
		size := jsonSize(item) + 1
		if used+size > budget {
			if len(out) > 0 {
				break
			}
			item = fit(i, item, budget-1)
			size = jsonSize(item) + 1
		}
		out, used = append(out, item), used+size
	}
	return out, i, nil
}

// fitMessage cuts a message that alone exceeds budget: its texts are clipped
// to a share each, and its parts dropped when that is not enough.
func fitMessage(in TurnMessage, budget int) TurnMessage {
	out := in
	out.Clipped = true
	out.Parts = slices.Clone(in.Parts)
	share := budget / (len(out.Parts) + 1)
	for i, part := range out.Parts {
		if part.Text != "" {
			out.Parts[i].Text = clipText(part.Text, share)
		}
	}
	if jsonSize(out) > budget {
		out.Parts = nil
	}
	return out
}

// sessionRef is a session read for its conversation: the caller's token,
// the session's Agent and its A2A context.
type sessionRef struct {
	token     string
	raw       json.RawMessage
	agent     kagent.Ref
	contextID string
}

// readSession reads a session as the caller and resolves where its
// conversation is addressed.
func (s *Service) readSession(ctx context.Context, sessionID string) (*sessionRef, error) {
	if !s.SessionsAvailable() {
		return nil, errSessionsUnsupported
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil, invalidf("session id is required")
	}
	token, err := sessionToken(ctx)
	if err != nil {
		return nil, err
	}
	session, err := s.cfg.Sessions.GetSession(ctx, token, sessionID)
	if err != nil {
		return nil, kagentError(err, fmt.Sprintf("session %s", sessionID))
	}
	agent := session.GetAgent()
	if agent.GetNamespace() == "" || agent.GetName() == "" {
		return nil, conflictf("session %s names no Agent, so its conversation cannot be addressed", sessionID)
	}
	out := &sessionRef{token: token, agent: kagent.Ref{Namespace: agent.GetNamespace(), Name: agent.GetName()}, contextID: session.GetContextId()}
	if out.contextID == "" {
		out.contextID = session.GetId()
	}
	if out.raw, err = marshalProto(session); err != nil {
		return nil, err
	}
	return out, nil
}

// sessionDetailLevel is level, or def when it is empty, if allowed lists it.
func sessionDetailLevel(level, def string, allowed ...string) (string, error) {
	if level == "" {
		return def, nil
	}
	if !slices.Contains(allowed, level) {
		return "", invalidf("detail must be one of %s", strings.Join(allowed, ", "))
	}
	return level, nil
}

// sessionMaxBytes is the answer budget n asks for, DefaultSessionMaxBytes
// for 0.
func sessionMaxBytes(n int) (int, error) {
	if n == 0 {
		return DefaultSessionMaxBytes, nil
	}
	if n < MinSessionMaxBytes || n > MaxSessionMaxBytes {
		return 0, invalidf("maxBytes must be between %d and %d", MinSessionMaxBytes, MaxSessionMaxBytes)
	}
	return n, nil
}

// StartSession creates a session of the Agent and sends it the first message,
// when one is given; without one the session is only created and the caller
// sends its first turn itself.
// The controller refuses the create while the Agent has no ready revision.
// With a workspace grant the session gets its directory on the workspace's
// volume: the grant is checked to name the caller and to be unexpired, then
// passed to the controller as the admission token of the volume source it
// names; the controller's admission verifies it and refuses with its reason.
// A Session kagent gives providers is bound in muster to the caller before
// its first turn, so its sandbox acts as the caller towards them: a workspace
// Session's admitted providers, and muster itself on every Session where muster
// verifies the actor's own JWT, with or without a workspace. A bind muster
// refuses deletes the Session again.
func (s *Service) StartSession(ctx context.Context, in StartSession) (*SessionStart, error) {
	if in.Message != "" && strings.TrimSpace(in.Message) == "" {
		return nil, invalidf("message is blank: omit it to create the session without a first message")
	}
	if utf8.RuneCountInString(in.Message) > MaxSessionMessageLength {
		return nil, invalidf("message is longer than %d characters", MaxSessionMessageLength)
	}
	if len(in.RequestID) > maxRequestIDLength {
		return nil, invalidf("requestId is longer than %d characters", maxRequestIDLength)
	}
	if utf8.RuneCountInString(in.SessionName) > maxSessionNameLength {
		return nil, invalidf("sessionName is longer than %d characters", maxSessionNameLength)
	}
	ref, token, err := s.sessionCall(ctx, in.Namespace, in.Name)
	if err != nil {
		return nil, err
	}
	requestID := in.RequestID
	if requestID == "" {
		requestID = uuid.NewString()
	}
	create := kagent.SessionCreate{Agent: ref, RequestID: requestID, Name: in.SessionName, BindActor: s.cfg.Bindings != nil}
	var workspace *Workspace
	if in.WorkspaceGrant != "" {
		if workspace, err = workspaceGrant(ctx, in.WorkspaceGrant, time.Now()); err != nil {
			return nil, err
		}
		if s.cfg.Bindings == nil {
			return nil, errSessionBindingsUnsupported
		}
		create.VolumeSource, create.AdmissionToken = workspace.Volume, in.WorkspaceGrant
	}
	session, err := s.cfg.Sessions.CreateSession(ctx, token, create)
	if err != nil {
		return nil, kagentError(err, fmt.Sprintf("a session of agent %s", ref.Tenant()))
	}
	if s.cfg.Bindings != nil {
		if err := s.bindSession(ctx, token, session); err != nil {
			return nil, s.abandonSession(ctx, token, session, err)
		}
	}
	var turn *a2apb.StreamResponse
	if in.Message != "" {
		if turn, err = s.firstTurn(ctx, token, ref, session, requestID, in.Message); err != nil {
			return nil, err
		}
	}
	out := &SessionStart{}
	attrs := []any{identity.LogAttr(ctx), "agent", ref.Tenant(), "session", session.GetId()}
	if workspace != nil {
		out.Workspace = workspace.Name
		attrs = append(attrs, "workspace", workspace.Name)
	}
	s.log.InfoContext(ctx, "session started", attrs...)
	if out.Session, err = marshalProto(session); err != nil {
		return nil, err
	}
	if turn != nil {
		if out.Turn, err = marshalProto(turn); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// firstMessageIDs is the namespace of the first messages' ids: a start's
// first message has the id its request id names, so a retry finds it.
var firstMessageIDs = uuid.MustParse("6f1c4b5e-2d0a-4f43-9a8e-3b7d1c0e5a21")

// firstTurn sends a session's first message once per request id: the message
// id is derived from the request id, and when the session's first turn
// already carries that message (a retry of a start whose answer was lost) it
// answers that turn's latest state and sends nothing.
func (s *Service) firstTurn(ctx context.Context, token string, ref kagent.Ref, session *apiv1alpha1.Session, requestID, message string) (*a2apb.StreamResponse, error) {
	contextID := session.GetContextId()
	if contextID == "" {
		contextID = session.GetId()
	}
	messageID := uuid.NewSHA1(firstMessageIDs, []byte(requestID)).String()
	sent, err := s.sentFirstTurn(ctx, token, ref, contextID, messageID)
	if err != nil {
		return nil, fmt.Errorf("session %s was created but its turns could not be read to send the first message once (start again with requestId %q to reuse it): %w",
			session.GetId(), requestID, kagentError(err, fmt.Sprintf("session %s", session.GetId())))
	}
	if sent != nil {
		return &a2apb.StreamResponse{Payload: &a2apb.StreamResponse_Task{Task: sent}}, nil
	}
	turn, err := s.cfg.Sessions.SendMessage(ctx, token, ref, contextID, messageID, message)
	if err != nil {
		return nil, fmt.Errorf("session %s was created but its first message was not accepted (start again with requestId %q to reuse it): %w",
			session.GetId(), requestID, kagentError(err, fmt.Sprintf("session %s", session.GetId())))
	}
	return turn, nil
}

// sentFirstTurn is the session's first turn when it carries the message
// messageID, nil when the session has no turn or its first turn is another.
func (s *Service) sentFirstTurn(ctx context.Context, token string, ref kagent.Ref, contextID, messageID string) (*a2apb.Task, error) {
	none := int32(0)
	list, err := s.cfg.Sessions.ListSessionTasks(ctx, token, ref, contextID, kagent.TaskListOptions{HistoryLength: &none})
	if err != nil || len(list.Tasks) == 0 {
		return nil, err
	}
	first, err := s.cfg.Sessions.GetSessionTask(ctx, token, ref, list.Tasks[0].GetId())
	if err != nil {
		return nil, err
	}
	for _, m := range first.GetHistory() {
		if m.GetMessageId() == messageID {
			first.History, first.Artifacts = nil, nil
			return first, nil
		}
	}
	return nil, nil
}

// DeleteSession deletes one of the caller's sessions and its runtime state.
// The controller scopes the delete to the session's creator: a session of
// another person is refused as not found, as one that does not exist. While
// another operation on the session runs, the controller answers aborted and
// the delete is worth retrying.
func (s *Service) DeleteSession(ctx context.Context, sessionID string) (*SessionDelete, error) {
	if !s.SessionsAvailable() {
		return nil, errSessionsUnsupported
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil, invalidf("session id is required")
	}
	token, err := sessionToken(ctx)
	if err != nil {
		return nil, err
	}
	var binding *muster.Binding
	if s.cfg.Bindings != nil {
		current, err := s.cfg.Sessions.GetSession(ctx, token, sessionID)
		if err != nil {
			return nil, kagentError(err, fmt.Sprintf("session %s", sessionID))
		}
		if binding, err = sessionBinding(current); err != nil {
			return nil, err
		}
	}
	session, err := s.cfg.Sessions.DeleteSession(ctx, token, sessionID)
	if status.Code(err) == codes.Aborted {
		return nil, fmt.Errorf("%w (another operation on the session is running: retry the delete shortly)", kagentError(err, fmt.Sprintf("session %s", sessionID)))
	}
	if err != nil {
		return nil, kagentError(err, fmt.Sprintf("session %s", sessionID))
	}
	s.log.InfoContext(ctx, "session deleted", identity.LogAttr(ctx), "agent", kagent.Ref{
		Namespace: session.GetAgent().GetNamespace(), Name: session.GetAgent().GetName(),
	}.Tenant(), "session", session.GetId())
	out := &SessionDelete{}
	if binding != nil {
		if err := s.cfg.Bindings.Unbind(ctx, token, binding.Actor); err != nil {
			s.log.ErrorContext(ctx, "session deleted but not unbound in muster", identity.LogAttr(ctx), "session", session.GetId(), "actor", binding.Actor, "error", err)
			out.Warning = fmt.Sprintf("the session was deleted, but muster did not remove its sandbox's binding (it expires with the Session idle TTL): %v", err)
		}
	}
	if out.Session, err = marshalProto(session); err != nil {
		return nil, err
	}
	return out, nil
}

// actorAuthoritySuffix ends a Session's A2A authority,
// <actor>.<atespace>.actors.resources.substrate.ate.dev: the address kagent
// records for the Session's Substrate actor once it is created.
const actorAuthoritySuffix = ".actors.resources.substrate.ate.dev"

// providerInstance is the provider instance a Session provider names: its
// provider, or its audience on a Session kagent stored before provider
// existed.
func providerInstance(p *apiv1alpha1.SessionProvider) string {
	if p.GetProvider() != "" {
		return p.GetProvider()
	}
	//nolint:staticcheck // audience is the provider instance on a Session stored before provider existed.
	return p.GetAudience()
}

// sessionBinding is the binding of a Session's sandbox actor: its
// SPIFFE id from the Session's A2A authority and the provider instances
// kagent admitted it to. Where muster verifies the actor's own JWT, kagent
// lists the reserved provider muster among them, so the binding carries it
// and muster serves the sandbox's calls as the person; agent-manager never
// adds it. Nil for a Session without providers: muster has nothing to
// release to its sandbox.
func sessionBinding(session *apiv1alpha1.Session) (*muster.Binding, error) {
	var providers []string
	for _, p := range session.GetProviders() {
		if name := providerInstance(p); name != "" && !slices.Contains(providers, name) {
			providers = append(providers, name)
		}
	}
	if len(providers) == 0 {
		return nil, nil
	}
	name, ok := strings.CutSuffix(session.GetA2AAuthority(), actorAuthoritySuffix)
	actor, atespace, hasAtespace := strings.Cut(name, ".")
	if !ok || !hasAtespace || actor == "" || atespace == "" || strings.Contains(atespace, ".") {
		return nil, conflictf("session %s has no Substrate actor address kagent records (a2aAuthority %q), so its sandbox cannot be bound in muster", session.GetId(), session.GetA2AAuthority())
	}
	return &muster.Binding{
		Actor:     muster.ActorID(atespace, actor),
		Session:   session.GetAgent().GetNamespace() + "/" + session.GetId(),
		Providers: providers,
	}, nil
}

// bindSession binds a Session's sandbox actor to the caller in muster, as the
// caller, when kagent gave the Session providers.
func (s *Service) bindSession(ctx context.Context, token string, session *apiv1alpha1.Session) error {
	binding, err := sessionBinding(session)
	if err != nil {
		return err
	}
	if binding == nil {
		s.log.InfoContext(ctx, "session has no providers: nothing to bind in muster", identity.LogAttr(ctx), "session", session.GetId())
		return nil
	}
	if err := s.cfg.Bindings.Bind(ctx, token, *binding); err != nil {
		return bindingError(err, session.GetId())
	}
	s.log.InfoContext(ctx, "session bound in muster", identity.LogAttr(ctx), "session", session.GetId(), "actor", binding.Actor, "providers", binding.Providers)
	return nil
}

// abandonSession deletes a Session whose bind failed, so no Session with
// providers runs without its person, and answers the bind's error, naming the
// Session when the delete failed too.
func (s *Service) abandonSession(ctx context.Context, token string, session *apiv1alpha1.Session, cause error) error {
	if _, err := s.cfg.Sessions.DeleteSession(ctx, token, session.GetId()); err != nil {
		s.log.ErrorContext(ctx, "session left behind after a failed bind", identity.LogAttr(ctx), "session", session.GetId(), "error", err)
		return fmt.Errorf("%w (session %s could not be deleted again: delete it with delete_session: %v)", cause, session.GetId(), kagentError(err, fmt.Sprintf("session %s", session.GetId())))
	}
	s.log.InfoContext(ctx, "session deleted after a failed bind", identity.LogAttr(ctx), "session", session.GetId())
	return cause
}

// bindingError maps a refused bind onto the domain sentinels; every message
// names muster.
func bindingError(err error, sessionID string) error {
	what := fmt.Sprintf("binding session %s's sandbox to you in muster failed, so the session was not started", sessionID)
	switch {
	case errors.Is(err, muster.ErrForbidden):
		return fmt.Errorf("%w: %s: %v", ErrForbidden, what, err)
	case errors.Is(err, muster.ErrInvalid):
		return fmt.Errorf("%w: %s: %v", ErrInvalid, what, err)
	case errors.Is(err, muster.ErrUnauthenticated):
		return fmt.Errorf("%w: %s: %v", ErrUnauthenticated, what, err)
	}
	return fmt.Errorf("%s: %w", what, err)
}

var errSessionBindingsUnsupported = fmt.Errorf("%w: this installation has no muster session bindings (--muster-bindings-url), so a session cannot be started in a workspace: its sandbox would not act as you", ErrUnsupported)

var errSessionsUnsupported = fmt.Errorf("%w: this installation has no kagent controller target (--kagent-target), so the session tools are not offered", ErrUnsupported)

// sessionCall resolves the Agent of a session operation and the caller token
// it runs with.
func (s *Service) sessionCall(ctx context.Context, namespace, name string) (kagent.Ref, string, error) {
	if !s.SessionsAvailable() {
		return kagent.Ref{}, "", errSessionsUnsupported
	}
	if err := ValidateName(name); err != nil {
		return kagent.Ref{}, "", err
	}
	ns, err := s.Namespace(namespace)
	if err != nil {
		return kagent.Ref{}, "", err
	}
	token, err := sessionToken(ctx)
	if err != nil {
		return kagent.Ref{}, "", err
	}
	return kagent.Ref{Namespace: ns, Name: name}, token, nil
}

// sessionToken is the caller's IdP token: a session operation runs as the
// caller through agentgateway and never as the ServiceAccount.
func sessionToken(ctx context.Context) (string, error) {
	token, ok := identity.TokenFromContext(ctx)
	if !ok {
		return "", fmt.Errorf("%w: the session tools act as the caller towards kagent and the request carries no identity token (agent-manager runs without OAuth, or the caller's token could not be resolved)", ErrUnauthenticated)
	}
	return token, nil
}

// kagentError maps a gRPC status from the controller or agentgateway onto
// the domain sentinels, keeping the controller's message.
func kagentError(err error, what string) error {
	if errors.Is(err, kagent.ErrNoToken) {
		return fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}
	st, ok := status.FromError(err)
	if !ok {
		return fmt.Errorf("kagent: %s: %w", what, err)
	}
	var sentinel error
	switch st.Code() {
	case codes.NotFound:
		sentinel = ErrNotFound
	case codes.InvalidArgument, codes.OutOfRange:
		sentinel = ErrInvalid
	case codes.FailedPrecondition, codes.AlreadyExists, codes.Aborted:
		sentinel = ErrConflict
	case codes.PermissionDenied:
		sentinel = ErrForbidden
	case codes.Unauthenticated:
		sentinel = ErrUnauthenticated
	case codes.Unimplemented:
		sentinel = ErrUnsupported
	default:
		return fmt.Errorf("kagent: %s: %s: %s", what, st.Code(), st.Message())
	}
	return fmt.Errorf("%w: kagent: %s: %s", sentinel, what, st.Message())
}

func marshalProto(m proto.Message) (json.RawMessage, error) {
	data, err := protoJSON.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", m.ProtoReflect().Descriptor().FullName(), err)
	}
	return data, nil
}
