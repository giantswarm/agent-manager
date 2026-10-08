// Package kagenttest is a fake kagent controller for tests: SessionService and
// lf.a2a.v1.A2AService on an in-memory gRPC connection, addressed the way the
// A2A gateway routes (the Agent as tenant, the session as the message's
// context), recording every call's metadata.
package kagenttest

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strconv"
	"sync"
	"testing"

	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	apiv1alpha1 "github.com/giantswarm/agent-manager/internal/kagent/gen/kagent/api/v1alpha1"
)

// Creator is the creator the fake records on every session it creates.
const Creator = "admin@lab.local"

// Call is one RPC the fake served.
type Call struct {
	Method   string
	Metadata metadata.MD
	Request  proto.Message
}

// Controller is the fake's state. Seed it before the calls; read it after.
type Controller struct {
	apiv1alpha1.UnimplementedSessionServiceServer
	a2apb.UnimplementedA2AServiceServer

	// NotReady holds the tenants (<namespace>/<name>) whose CreateSession
	// answers FailedPrecondition, as an Agent without a ready revision does.
	NotReady map[string]bool
	// Fail makes a method (its bare name, e.g. "ListSessions") answer the error.
	Fail map[string]error

	mu        sync.Mutex
	sessions  []*apiv1alpha1.Session
	requests  map[string]string
	tasks     map[string][]*a2apb.Task
	calls     []Call
	turnEnded chan struct{}
}

// New starts the fake and answers a client connection to it. Both stop with
// the test.
func New(t *testing.T) (*Controller, *grpc.ClientConn) {
	t.Helper()
	c := &Controller{
		NotReady:  map[string]bool{},
		Fail:      map[string]error{},
		requests:  map[string]string{},
		tasks:     map[string][]*a2apb.Task{},
		turnEnded: make(chan struct{}, 16),
	}
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	apiv1alpha1.RegisterSessionServiceServer(srv, c)
	a2apb.RegisterA2AServiceServer(srv, c)
	go func() { _ = srv.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///kagent",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = conn.Close()
		srv.Stop()
	})
	return c, conn
}

// AddSession seeds a READY session of agent and its tasks; it answers the
// session.
func (c *Controller) AddSession(namespace, name string, tasks ...*a2apb.Task) *apiv1alpha1.Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.newSession(namespace, name, "")
	for _, task := range tasks {
		task.ContextId = s.GetContextId()
	}
	c.tasks[s.GetContextId()] = append(c.tasks[s.GetContextId()], tasks...)
	return s
}

// Calls answers the calls served so far, in order.
func (c *Controller) Calls() []Call {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.calls)
}

// Tasks answers the tasks of a session's context.
func (c *Controller) Tasks(contextID string) []*a2apb.Task {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.tasks[contextID])
}

// TurnEnded is signalled when a SendStreamingMessage stream ends: the client
// hung up, and the fake finishes the turn regardless, as the gateway does.
func (c *Controller) TurnEnded() <-chan struct{} { return c.turnEnded }

func (c *Controller) newSession(namespace, name, sessionName string) *apiv1alpha1.Session {
	id := uuid.NewString()
	s := &apiv1alpha1.Session{
		Id: id, ContextId: id, Creator: Creator, Name: sessionName,
		Agent: &apiv1alpha1.ResourceReference{Namespace: namespace, Name: name},
		State: apiv1alpha1.RuntimeState_RUNTIME_STATE_READY,
	}
	c.sessions = append(c.sessions, s)
	return s
}

func (c *Controller) record(ctx context.Context, method string, req proto.Message) error {
	md, _ := metadata.FromIncomingContext(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, Call{Method: method, Metadata: md, Request: req})
	return c.Fail[method]
}

func (c *Controller) session(id string) *apiv1alpha1.Session {
	for _, s := range c.sessions {
		if s.GetId() == id {
			return s
		}
	}
	return nil
}

func tenantOf(ref *apiv1alpha1.ResourceReference) string {
	return ref.GetNamespace() + "/" + ref.GetName()
}

// page answers items[token:token+limit] and the next token.
func page[T any](items []T, token string, limit int) ([]T, string, error) {
	start := 0
	if token != "" {
		n, err := strconv.Atoi(token)
		if err != nil || n < 0 || n > len(items) {
			return nil, "", status.Error(codes.InvalidArgument, "bad page token")
		}
		start = n
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	end := min(start+limit, len(items))
	next := ""
	if end < len(items) {
		next = strconv.Itoa(end)
	}
	return items[start:end], next, nil
}

// ListSessions implements SessionService.
func (c *Controller) ListSessions(ctx context.Context, req *apiv1alpha1.ListSessionsRequest) (*apiv1alpha1.ListSessionsResponse, error) {
	if err := c.record(ctx, "ListSessions", req); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var matched []*apiv1alpha1.Session
	for _, s := range c.sessions {
		if req.GetAgent() == nil || tenantOf(s.GetAgent()) == tenantOf(req.GetAgent()) {
			matched = append(matched, s)
		}
	}
	items, next, err := page(matched, req.GetPage().GetPageToken(), int(req.GetPage().GetLimit()))
	if err != nil {
		return nil, err
	}
	return &apiv1alpha1.ListSessionsResponse{Sessions: items, Page: &apiv1alpha1.PageResponse{NextPageToken: next}}, nil
}

// GetSession implements SessionService.
func (c *Controller) GetSession(ctx context.Context, req *apiv1alpha1.GetSessionRequest) (*apiv1alpha1.GetSessionResponse, error) {
	if err := c.record(ctx, "GetSession", req); err != nil {
		return nil, err
	}
	if _, err := uuid.Parse(req.GetSessionId()); err != nil {
		return nil, status.Error(codes.InvalidArgument, "session_id: value must be a valid UUID")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.session(req.GetSessionId())
	if s == nil {
		return nil, status.Error(codes.NotFound, "session not found")
	}
	return &apiv1alpha1.GetSessionResponse{Session: s}, nil
}

// CreateSession implements SessionService: idempotent on request_id, refused
// for a NotReady Agent.
func (c *Controller) CreateSession(ctx context.Context, req *apiv1alpha1.CreateSessionRequest) (*apiv1alpha1.CreateSessionResponse, error) {
	if err := c.record(ctx, "CreateSession", req); err != nil {
		return nil, err
	}
	if req.GetRequestId() == "" {
		return nil, status.Error(codes.InvalidArgument, "request_id: value length must be at least 1 characters")
	}
	tenant := tenantOf(req.GetAgent())
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.NotReady[tenant] {
		return nil, status.Errorf(codes.FailedPrecondition, "agent %s has no ready revision", tenant)
	}
	if id, ok := c.requests[req.GetRequestId()]; ok {
		return &apiv1alpha1.CreateSessionResponse{Session: c.session(id)}, nil
	}
	s := c.newSession(req.GetAgent().GetNamespace(), req.GetAgent().GetName(), req.GetName())
	c.requests[req.GetRequestId()] = s.GetId()
	return &apiv1alpha1.CreateSessionResponse{Session: s}, nil
}

// routed is the session a call addresses: the tenant must be the session's
// Agent, as the gateway resolves it.
func (c *Controller) routed(tenant, contextID string) (*apiv1alpha1.Session, error) {
	if tenant == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant is required")
	}
	s := c.session(contextID)
	if s == nil || tenantOf(s.GetAgent()) != tenant {
		return nil, status.Error(codes.NotFound, "session not found")
	}
	return s, nil
}

// ListTasks implements A2AService: the tasks of the context, through the
// session's Agent.
func (c *Controller) ListTasks(ctx context.Context, req *a2apb.ListTasksRequest) (*a2apb.ListTasksResponse, error) {
	if err := c.record(ctx, "ListTasks", req); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.routed(req.GetTenant(), req.GetContextId()); err != nil {
		return nil, err
	}
	all := c.tasks[req.GetContextId()]
	items, next, err := page(all, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, err
	}
	return &a2apb.ListTasksResponse{Tasks: items, NextPageToken: next, TotalSize: int32(len(all))}, nil // #nosec G115 -- a test fixture's task count
}

// SendStreamingMessage implements A2AService: the turn is accepted (a task
// in SUBMITTED), held open until the client hangs up, then finished with the
// agent's reply, as a turn outlives its observer.
func (c *Controller) SendStreamingMessage(req *a2apb.SendMessageRequest, stream grpc.ServerStreamingServer[a2apb.StreamResponse]) error {
	ctx := stream.Context()
	defer func() { c.turnEnded <- struct{}{} }()
	if err := c.record(ctx, "SendStreamingMessage", req); err != nil {
		return err
	}
	msg := req.GetMessage()
	c.mu.Lock()
	if _, err := c.routed(req.GetTenant(), msg.GetContextId()); err != nil {
		c.mu.Unlock()
		return err
	}
	task := &a2apb.Task{
		Id: uuid.NewString(), ContextId: msg.GetContextId(),
		Status:  &a2apb.TaskStatus{State: a2apb.TaskState_TASK_STATE_SUBMITTED},
		History: []*a2apb.Message{msg},
	}
	c.tasks[msg.GetContextId()] = append(c.tasks[msg.GetContextId()], task)
	accepted := proto.Clone(task).(*a2apb.Task)
	c.mu.Unlock()

	if err := stream.Send(&a2apb.StreamResponse{Payload: &a2apb.StreamResponse_Task{Task: accepted}}); err != nil {
		return err
	}
	<-ctx.Done()
	c.mu.Lock()
	defer c.mu.Unlock()
	task.Status = &a2apb.TaskStatus{State: a2apb.TaskState_TASK_STATE_COMPLETED, Message: &a2apb.Message{
		MessageId: uuid.NewString(), ContextId: msg.GetContextId(), TaskId: task.GetId(), Role: a2apb.Role_ROLE_AGENT,
		Parts: []*a2apb.Part{{Content: &a2apb.Part_Text{Text: fmt.Sprintf("echo: %s", msg.GetParts()[0].GetText())}}},
	}}
	return nil
}
