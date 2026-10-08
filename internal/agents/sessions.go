package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	ListSessionTasks(ctx context.Context, token string, agent kagent.Ref, contextID string) ([]*a2apb.Task, error)
	CreateSession(ctx context.Context, token string, agent kagent.Ref, requestID, name string) (*apiv1alpha1.Session, error)
	SendMessage(ctx context.Context, token string, agent kagent.Ref, contextID, messageID, text string) (*a2apb.StreamResponse, error)
}

// SessionList is the answer of list_sessions: the Agent's sessions as kagent
// reports them (proto3 JSON of kagent.api.v1alpha1.Session, state as
// RUNTIME_STATE_*).
type SessionList struct {
	Agent    AgentRef          `json:"agent"`
	Sessions []json.RawMessage `json:"sessions"`
}

// SessionDetail is the answer of get_session: the session and its
// conversation, every A2A task of the session's context oldest first (proto3
// JSON of lf.a2a.v1.Task: status, history, artifacts).
type SessionDetail struct {
	Session json.RawMessage   `json:"session"`
	Tasks   []json.RawMessage `json:"tasks"`
}

// SessionStart is the answer of start_session: the new session and the first
// event of its first turn (proto3 JSON of lf.a2a.v1.StreamResponse, usually
// {task} in TASK_STATE_SUBMITTED or WORKING). The turn keeps running; its
// answer is read with get_session.
type SessionStart struct {
	Session json.RawMessage `json:"session"`
	Turn    json.RawMessage `json:"turn"`
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
	// Message is the first user message of the conversation.
	Message string `json:"message"`
	// RequestID makes a retried start idempotent: the same id answers the
	// session the first attempt created. Empty: a new id per call.
	RequestID string `json:"requestId,omitempty"`
	// SessionName is the conversation's display name; empty leaves it unnamed.
	SessionName string `json:"sessionName,omitempty"`
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

// GetSession is one session and its conversation: the A2A tasks of the
// session's context, read from the session's Agent.
func (s *Service) GetSession(ctx context.Context, sessionID string) (*SessionDetail, error) {
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
	ref := kagent.Ref{Namespace: agent.GetNamespace(), Name: agent.GetName()}
	contextID := session.GetContextId()
	if contextID == "" {
		contextID = session.GetId()
	}
	tasks, err := s.cfg.Sessions.ListSessionTasks(ctx, token, ref, contextID)
	if err != nil {
		return nil, kagentError(err, fmt.Sprintf("conversation of session %s", sessionID))
	}
	out := &SessionDetail{Tasks: []json.RawMessage{}}
	if out.Session, err = marshalProto(session); err != nil {
		return nil, err
	}
	for _, task := range tasks {
		raw, err := marshalProto(task)
		if err != nil {
			return nil, err
		}
		out.Tasks = append(out.Tasks, raw)
	}
	return out, nil
}

// StartSession creates a session of the Agent and sends it the first message.
// The controller refuses the create while the Agent has no ready revision.
func (s *Service) StartSession(ctx context.Context, in StartSession) (*SessionStart, error) {
	if strings.TrimSpace(in.Message) == "" {
		return nil, invalidf("message is required")
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
	session, err := s.cfg.Sessions.CreateSession(ctx, token, ref, requestID, in.SessionName)
	if err != nil {
		return nil, kagentError(err, fmt.Sprintf("a session of agent %s", ref.Tenant()))
	}
	contextID := session.GetContextId()
	if contextID == "" {
		contextID = session.GetId()
	}
	turn, err := s.cfg.Sessions.SendMessage(ctx, token, ref, contextID, uuid.NewString(), in.Message)
	if err != nil {
		return nil, fmt.Errorf("session %s was created but its first message was not accepted (start again with requestId %q to reuse it): %w",
			session.GetId(), requestID, kagentError(err, fmt.Sprintf("session %s", session.GetId())))
	}
	s.log.InfoContext(ctx, "session started", identity.LogAttr(ctx), "agent", ref.Tenant(), "session", session.GetId())
	out := &SessionStart{}
	if out.Session, err = marshalProto(session); err != nil {
		return nil, err
	}
	if out.Turn, err = marshalProto(turn); err != nil {
		return nil, err
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
