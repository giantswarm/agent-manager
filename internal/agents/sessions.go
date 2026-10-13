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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/giantswarm/agent-manager/internal/identity"
	"github.com/giantswarm/agent-manager/internal/kagent"
	apiv1alpha1 "github.com/giantswarm/agent-manager/internal/kagent/gen/kagent/api/v1alpha1"
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
	ListSessionTasks(ctx context.Context, token string, agent kagent.Ref, contextID string, opts kagent.TaskListOptions) ([]*a2apb.Task, error)
	GetSessionTask(ctx context.Context, token string, agent kagent.Ref, taskID string) (*a2apb.Task, error)
	CreateSession(ctx context.Context, token string, in kagent.SessionCreate) (*apiv1alpha1.Session, error)
	SendMessage(ctx context.Context, token string, agent kagent.Ref, contextID, messageID, text string) (*a2apb.StreamResponse, error)
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
// JSON of lf.a2a.v1.Task: status, history, artifacts). The newest turns that
// fit the budget are kept; OmittedTurns counts the older ones left out.
type SessionDetail struct {
	Session      json.RawMessage   `json:"session"`
	Detail       string            `json:"detail"`
	TotalTurns   int               `json:"totalTurns"`
	Truncated    bool              `json:"truncated,omitempty"`
	OmittedTurns int               `json:"omittedTurns,omitempty"`
	Hint         string            `json:"hint,omitempty"`
	Turns        []Turn            `json:"turns,omitempty"`
	Tasks        []json.RawMessage `json:"tasks,omitempty"`
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
// messages from FromMessage on that fit the budget (Messages in the
// transcript view, History and Artifacts as proto3 JSON in the full view;
// NextMessage continues them) or, asked for one, a tool call whole.
type SessionTurn struct {
	SessionID        string            `json:"sessionId"`
	Detail           string            `json:"detail"`
	Index            int               `json:"index"`
	TotalTurns       int               `json:"totalTurns"`
	TaskID           string            `json:"taskId"`
	State            string            `json:"state"`
	Timestamp        string            `json:"timestamp,omitempty"`
	TotalMessages    int               `json:"totalMessages"`
	FromMessage      int               `json:"fromMessage"`
	NextMessage      int               `json:"nextMessage,omitempty"`
	Truncated        bool              `json:"truncated,omitempty"`
	Hint             string            `json:"hint,omitempty"`
	ToolCalls        []ToolCallStub    `json:"toolCalls,omitempty"`
	Messages         []TurnMessage     `json:"messages,omitempty"`
	History          []json.RawMessage `json:"history,omitempty"`
	Artifacts        []json.RawMessage `json:"artifacts,omitempty"`
	OmittedArtifacts int               `json:"omittedArtifacts,omitempty"`
	ToolCall         *ToolCallDetail   `json:"toolCall,omitempty"`
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
	// session the first attempt created. Empty: a new id per call.
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

var sessionTruncatedHint = fmt.Sprintf("older turns were left out to fit maxBytes: raise maxBytes (at most %d), pick turns with lastTurns, or read one turn with get_session_turn", MaxSessionMaxBytes)

// envelopeSlack is what an answer's fields set after its budget was taken
// (truncated, omitted counts, a short hint) may add.
const envelopeSlack = 128

// GetSession is one session and its conversation, the A2A tasks of the
// session's context read from the session's Agent, in the view q asks for
// and within its budget.
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
	tasks, err := s.cfg.Sessions.ListSessionTasks(ctx, sc.token, sc.agent, sc.contextID, kagent.TaskListOptions{IncludeArtifacts: detail == DetailFull})
	if err != nil {
		return nil, kagentError(err, fmt.Sprintf("conversation of session %s", q.SessionID))
	}
	out := &SessionDetail{Session: sc.raw, Detail: detail, TotalTurns: len(tasks)}
	budget := maxBytes - jsonSize(out) - len(sessionTruncatedHint) - envelopeSlack
	first := 0
	if q.LastTurns > 0 {
		first = max(len(tasks)-q.LastTurns, 0)
	}
	clipped := false
	if detail == DetailFull {
		var raws []json.RawMessage
		for _, task := range tasks[first:] {
			raw, err := marshalProto(task)
			if err != nil {
				return nil, err
			}
			raws = append(raws, raw)
		}
		out.Tasks, out.OmittedTurns = packNewest(raws, budget, nil)
	} else {
		var turns []Turn
		for i, task := range tasks[first:] {
			t := readTurn(first+i, task)
			if detail == DetailTranscript {
				turns = append(turns, t.transcript())
			} else {
				turns = append(turns, t.summary())
			}
		}
		out.Turns, out.OmittedTurns = packNewest(turns, budget, fitTurn)
		clipped = len(out.Turns) > 0 && out.Turns[len(out.Turns)-1].Clipped
	}
	if out.OmittedTurns > 0 || clipped {
		out.Truncated, out.Hint = true, sessionTruncatedHint
	}
	return out, nil
}

// GetSessionTurn is one turn of a session's conversation: its messages from
// q.FromMessage on in the view q asks for, or one of its tool calls whole,
// within q's budget.
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
	none := int32(0)
	tasks, err := s.cfg.Sessions.ListSessionTasks(ctx, sc.token, sc.agent, sc.contextID, kagent.TaskListOptions{HistoryLength: &none})
	if err != nil {
		return nil, kagentError(err, fmt.Sprintf("conversation of session %s", q.SessionID))
	}
	index := turnIndex(tasks, ref)
	if index < 0 {
		return nil, notFoundf("session %s has no turn %s (it has %d, indexed from 0)", q.SessionID, ref, len(tasks))
	}
	task, err := s.cfg.Sessions.GetSessionTask(ctx, sc.token, sc.agent, tasks[index].GetId())
	if err != nil {
		return nil, kagentError(err, fmt.Sprintf("turn %d of session %s", index, q.SessionID))
	}
	if task.GetContextId() != "" && task.GetContextId() != sc.contextID {
		return nil, notFoundf("session %s has no turn %s", q.SessionID, ref)
	}
	t := readTurn(index, task)
	out := &SessionTurn{
		SessionID: q.SessionID, Detail: detail, Index: index, TotalTurns: len(tasks), TaskID: task.GetId(),
		State: t.state(), Timestamp: t.timestamp(), TotalMessages: len(t.messages), FromMessage: q.FromMessage,
		ToolCalls: t.stubs(),
	}
	if q.ToolCall != "" {
		call := t.call(strings.TrimSpace(string(q.ToolCall)))
		if call == nil {
			return nil, notFoundf("turn %d of session %s has no tool call %s (it has %d, indexed from 0)", index, q.SessionID, q.ToolCall, len(t.calls))
		}
		out.ToolCalls = nil
		whole, clipped := t.toolCallDetail(call, maxBytes-jsonSize(out)-envelopeSlack)
		out.ToolCall, out.Truncated = &whole, clipped
		return out, nil
	}
	if q.FromMessage > len(t.messages) {
		return nil, invalidf("fromMessage %d is past the turn's %d messages", q.FromMessage, len(t.messages))
	}
	budget := maxBytes - jsonSize(out) - envelopeSlack
	next := 0
	if detail == DetailFull {
		used := 0
		if q.FromMessage == 0 {
			for _, artifact := range task.GetArtifacts() {
				raw, err := marshalProto(artifact)
				if err != nil {
					return nil, err
				}
				if size := len(raw) + 1; used+size <= budget/2 {
					out.Artifacts, used = append(out.Artifacts, raw), used+size
				} else {
					out.OmittedArtifacts++
				}
			}
		}
		out.History, next, err = packForward(len(t.messages), q.FromMessage, budget-used, func(i int) (json.RawMessage, error) {
			return marshalProto(t.messages[i])
		}, func(i int, raw json.RawMessage, _ int) json.RawMessage {
			msg := t.messages[i]
			placeholder, _ := json.Marshal(map[string]any{
				"messageId": msg.GetMessageId(), "role": msg.GetRole().String(),
				"omitted": fmt.Sprintf("%d bytes exceed maxBytes; read its tool calls with toolCall", len(raw)),
			})
			return placeholder
		})
		if err != nil {
			return nil, err
		}
	} else {
		out.Messages, next, _ = packForward(len(t.messages), q.FromMessage, budget, func(i int) (TurnMessage, error) {
			return t.messageView(i), nil
		}, func(_ int, m TurnMessage, budget int) TurnMessage { return fitMessage(m, budget) })
	}
	if next < len(t.messages) {
		out.NextMessage, out.Truncated, out.Hint = next, true, "continue with fromMessage: "+fmt.Sprint(next)
	}
	if out.OmittedArtifacts > 0 {
		out.Truncated = true
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
	create := kagent.SessionCreate{Agent: ref, RequestID: requestID, Name: in.SessionName}
	var workspace *Workspace
	if in.WorkspaceGrant != "" {
		if workspace, err = workspaceGrant(ctx, in.WorkspaceGrant, time.Now()); err != nil {
			return nil, err
		}
		create.VolumeSource, create.AdmissionToken = workspace.Volume, in.WorkspaceGrant
	}
	session, err := s.cfg.Sessions.CreateSession(ctx, token, create)
	if err != nil {
		return nil, kagentError(err, fmt.Sprintf("a session of agent %s", ref.Tenant()))
	}
	var turn *a2apb.StreamResponse
	if in.Message != "" {
		contextID := session.GetContextId()
		if contextID == "" {
			contextID = session.GetId()
		}
		turn, err = s.cfg.Sessions.SendMessage(ctx, token, ref, contextID, uuid.NewString(), in.Message)
		if err != nil {
			return nil, fmt.Errorf("session %s was created but its first message was not accepted (start again with requestId %q to reuse it): %w",
				session.GetId(), requestID, kagentError(err, fmt.Sprintf("session %s", session.GetId())))
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
