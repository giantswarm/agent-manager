// Package kagent calls the kagent controller's SessionService and the A2A
// service it serves, through agentgateway, as the caller: every call carries
// the caller's IdP token as `authorization: Bearer` and nothing else that
// identifies anyone. agentgateway verifies the token on the controller route
// and derives the identity header the controller reads from it, replacing any
// value a client sent; a client that set one would be dead weight through the
// gateway and impersonation without it.
//
// A2A calls are addressed the way the gateway routes them: the Agent as the
// request's tenant (<namespace>/<name>) and the session as the message's
// context id (the controller keeps a session's A2A context equal to its id).
// The x-kagent-agent-instance-id header is the gateway's to the runtime and is
// never sent from here.
package kagent

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"

	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	apiv1alpha1 "github.com/giantswarm/agent-manager/internal/kagent/gen/kagent/api/v1alpha1"
)

// Target schemes: plaintext h2c (the in-cluster agentgateway Service) and TLS
// (a public hostname).
const (
	SchemePlaintext = "grpc"
	SchemeTLS       = "grpcs"
)

const (
	// pageSize is the controller's cap for ListSessions and ListTasks.
	pageSize = 100
	// maxSessionPages and maxTaskPages bound how far a listing is followed, so
	// a misbehaving next_page_token cannot loop forever.
	maxSessionPages = 20
	maxTaskPages    = 10
	// maxMessageBytes bounds one response: a session's tasks carry its whole
	// history and artifacts.
	maxMessageBytes = 32 << 20
)

// ErrNoToken is returned for a call without a caller token: there is no
// other identity to make it with.
var ErrNoToken = errors.New("no caller token")

// Ref names an Agent.
type Ref struct {
	Namespace string
	Name      string
}

// Tenant is the A2A tenant the gateway resolves the Agent from.
func (r Ref) Tenant() string { return r.Namespace + "/" + r.Name }

// Client speaks to one kagent controller. It is safe for concurrent use.
type Client struct {
	sessions  apiv1alpha1.SessionServiceClient
	a2a       a2apb.A2AServiceClient
	closeConn func() error
}

// ParseTarget splits a grpc:// or grpcs:// target into the host:port the gRPC
// client dials and whether the connection uses TLS. A grpcs target without a
// port is 443; a grpc target must name its port.
func ParseTarget(target string) (hostPort string, useTLS bool, err error) {
	u, err := url.Parse(target)
	if err != nil {
		return "", false, fmt.Errorf("kagent target %q: %w", target, err)
	}
	switch u.Scheme {
	case SchemePlaintext:
	case SchemeTLS:
		useTLS = true
	default:
		return "", false, fmt.Errorf("kagent target %q must use the %s:// or %s:// scheme", target, SchemePlaintext, SchemeTLS)
	}
	if u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
		return "", false, fmt.Errorf("kagent target %q must be %s://host[:port] with no path", target, u.Scheme)
	}
	if u.Port() != "" {
		return u.Host, useTLS, nil
	}
	if !useTLS {
		return "", false, fmt.Errorf("kagent target %q must name a port", target)
	}
	return net.JoinHostPort(u.Hostname(), "443"), true, nil
}

// Dial builds a Client for target. The connection is established lazily on
// the first call.
func Dial(target string) (*Client, error) {
	hostPort, useTLS, err := ParseTarget(target)
	if err != nil {
		return nil, err
	}
	creds := insecure.NewCredentials()
	if useTLS {
		creds = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	}
	conn, err := grpc.NewClient(hostPort,
		grpc.WithTransportCredentials(creds),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxMessageBytes)),
	)
	if err != nil {
		return nil, fmt.Errorf("kagent: dial %s: %w", target, err)
	}
	c := NewClient(conn)
	c.closeConn = conn.Close
	return c, nil
}

// NewClient builds a Client on an existing connection (tests use bufconn).
func NewClient(conn grpc.ClientConnInterface) *Client {
	return &Client{
		sessions:  apiv1alpha1.NewSessionServiceClient(conn),
		a2a:       a2apb.NewA2AServiceClient(conn),
		closeConn: func() error { return nil },
	}
}

// Close releases the connection Dial opened.
func (c *Client) Close() error { return c.closeConn() }

// asCaller is ctx carrying the caller's bearer as the call's only identity.
func asCaller(ctx context.Context, token string) (context.Context, error) {
	if token == "" {
		return nil, ErrNoToken
	}
	return metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+token)), nil
}

// ListSessions is SessionService/ListSessions for one Agent, walked to the
// end. allCreators asks for every person's sessions, which the controller
// grants only to a caller authorized for it.
func (c *Client) ListSessions(ctx context.Context, token string, agent Ref, allCreators bool) ([]*apiv1alpha1.Session, error) {
	ctx, err := asCaller(ctx, token)
	if err != nil {
		return nil, err
	}
	var out []*apiv1alpha1.Session
	pageToken := ""
	for range maxSessionPages {
		resp, err := c.sessions.ListSessions(ctx, &apiv1alpha1.ListSessionsRequest{
			Agent:       &apiv1alpha1.ResourceReference{Namespace: agent.Namespace, Name: agent.Name},
			AllCreators: allCreators,
			Page:        &apiv1alpha1.PageRequest{Limit: pageSize, PageToken: pageToken},
		})
		if err != nil {
			return nil, err
		}
		out = append(out, resp.GetSessions()...)
		pageToken = resp.GetPage().GetNextPageToken()
		if pageToken == "" {
			break
		}
	}
	return out, nil
}

// GetSession is SessionService/GetSession.
func (c *Client) GetSession(ctx context.Context, token, sessionID string) (*apiv1alpha1.Session, error) {
	ctx, err := asCaller(ctx, token)
	if err != nil {
		return nil, err
	}
	resp, err := c.sessions.GetSession(ctx, &apiv1alpha1.GetSessionRequest{SessionId: sessionID})
	if err != nil {
		return nil, err
	}
	return resp.GetSession(), nil
}

// ListSessionTasks is A2AService/ListTasks addressed to the session's Agent
// and filtered by its context, walked to the end: the conversation, oldest
// task first, with every task's history and artifacts.
func (c *Client) ListSessionTasks(ctx context.Context, token string, agent Ref, contextID string) ([]*a2apb.Task, error) {
	ctx, err := asCaller(ctx, token)
	if err != nil {
		return nil, err
	}
	var out []*a2apb.Task
	pageToken := ""
	size := int32(pageSize)
	includeArtifacts := true
	for range maxTaskPages {
		resp, err := c.a2a.ListTasks(ctx, &a2apb.ListTasksRequest{
			Tenant:           agent.Tenant(),
			ContextId:        contextID,
			PageSize:         &size,
			PageToken:        pageToken,
			IncludeArtifacts: &includeArtifacts,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, resp.GetTasks()...)
		pageToken = resp.GetNextPageToken()
		if pageToken == "" {
			break
		}
	}
	return out, nil
}

// CreateSession is SessionService/CreateSession for agent. requestID makes a
// retry idempotent: the controller keys it on (creator, request_id).
func (c *Client) CreateSession(ctx context.Context, token string, agent Ref, requestID, name string) (*apiv1alpha1.Session, error) {
	ctx, err := asCaller(ctx, token)
	if err != nil {
		return nil, err
	}
	resp, err := c.sessions.CreateSession(ctx, &apiv1alpha1.CreateSessionRequest{
		Agent:     &apiv1alpha1.ResourceReference{Namespace: agent.Namespace, Name: agent.Name},
		RequestId: requestID,
		Name:      name,
	})
	if err != nil {
		return nil, err
	}
	return resp.GetSession(), nil
}

// SendMessage opens A2AService/SendStreamingMessage with one user text message
// to the session (the Agent as tenant, the session as the message's context)
// and answers the first event, the gateway's acknowledgement that the runtime
// took the turn, then closes the stream: disconnecting an observer does not
// cancel the turn, and the session's tasks show it finish.
func (c *Client) SendMessage(ctx context.Context, token string, agent Ref, contextID, messageID, text string) (*a2apb.StreamResponse, error) {
	ctx, err := asCaller(ctx, token)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := c.a2a.SendStreamingMessage(ctx, &a2apb.SendMessageRequest{
		Tenant: agent.Tenant(),
		Message: &a2apb.Message{
			MessageId: messageID,
			ContextId: contextID,
			Role:      a2apb.Role_ROLE_USER,
			Parts:     []*a2apb.Part{{Content: &a2apb.Part_Text{Text: text}}},
		},
	})
	if err != nil {
		return nil, err
	}
	first, err := stream.Recv()
	if errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("kagent: the message stream ended before the turn was accepted")
	}
	return first, err
}
