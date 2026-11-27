// Package mustermcp calls another platform server's tools through muster's
// MCP endpoint as the caller: every call carries the caller's IdP token as
// `Authorization: Bearer`, so muster sees the person and the tool runs under
// the person's own grants. One MCP session per call, through muster's
// call_tool, whose envelope carries the target tool's own result.
package mustermcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/giantswarm/mcp-toolkit/urlelicitation"
	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

const (
	// AuthLoginTool is muster's core tool answering the link that starts a
	// person's grant for one of its servers.
	AuthLoginTool = "core_auth_login"
	// MetaCallTool is muster's meta-tool that runs any aggregated or core
	// tool; the target's result comes back serialised in its text content.
	MetaCallTool = "call_tool"

	callTimeout = 45 * time.Second
)

// Client calls one server's tools through muster, named by the configured
// prefix (x_<server>_) joined with the tool's own name.
type Client struct {
	endpoint string
	prefix   string
	version  string
}

// New is the client of the server behind the muster MCP endpoint
// (http(s)://host[:port]/mcp) under the tool prefix. version names
// agent-manager in the MCP handshake; what names the server in errors.
func New(endpoint, prefix, version, what string) (*Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("muster MCP URL: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("muster MCP URL %q: want http(s)://host[:port]/path without a query", endpoint)
	}
	if !strings.HasPrefix(prefix, "x_") || !strings.HasSuffix(prefix, "_") || len(prefix) < 4 {
		return nil, fmt.Errorf("%s tool prefix %q: want x_<server>_", what, prefix)
	}
	return &Client{endpoint: endpoint, prefix: prefix, version: version}, nil
}

// Tool is the server's tool name as muster aggregates it.
func (c *Client) Tool(name string) string { return c.prefix + name }

// AuthRequiredError is muster's sign-in challenge for the server itself: it
// holds no grant for the caller there yet.
type AuthRequiredError struct {
	Server   string
	LoginURL string
}

func (e *AuthRequiredError) Error() string {
	if e.LoginURL == "" {
		return fmt.Sprintf("muster needs your sign-in to %s", e.Server)
	}
	return fmt.Sprintf("muster needs your sign-in to %s: %s", e.Server, e.LoginURL)
}

// ToolError is the server (or muster for it) refusing the call: an unknown
// name, a failure on its side. Message is the tool's own.
type ToolError struct {
	Tool    string
	Message string
	// Dispatched is false when muster refused the call itself: the tool is
	// not behind muster under the prefix, or outside the caller's toolset.
	Dispatched bool
}

func (e *ToolError) Error() string {
	if !e.Dispatched {
		return fmt.Sprintf("muster did not run %s: %s", e.Tool, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Tool, e.Message)
}

// NotFound reports whether the server answered that what was named does not
// exist.
func (e *ToolError) NotFound() bool {
	return e.Dispatched && strings.Contains(strings.ToLower(e.Message), "not found")
}

// ErrMalformed is an answer that is not the tool's documented result.
var ErrMalformed = errors.New("malformed answer")

// Result is a target tool's answer.
type Result struct {
	Text       string
	Structured json.RawMessage
}

// JSON is the result's structured content, else its text when that is a
// JSON object: a server that answers its JSON as text.
func (r *Result) JSON() json.RawMessage {
	if len(r.Structured) > 0 {
		return r.Structured
	}
	if t := strings.TrimSpace(r.Text); strings.HasPrefix(t, "{") && json.Valid([]byte(t)) {
		return json.RawMessage(t)
	}
	return nil
}

// SignInLink is the link that starts the caller's grant of the muster server
// server: muster's core_auth_login as the caller. "" without an error when
// muster answers no https link (the grant is held already).
func (c *Client) SignInLink(ctx context.Context, bearer, server string) (string, error) {
	res, err := c.Call(ctx, bearer, AuthLoginTool, map[string]any{"server": server})
	if err != nil {
		return "", err
	}
	var out struct {
		AuthURL string `json:"authUrl"`
	}
	if len(res.Structured) == 0 || json.Unmarshal(res.Structured, &out) != nil {
		return "", nil
	}
	if u, err := url.Parse(out.AuthURL); err != nil || u.Scheme != "https" || u.Host == "" {
		return "", nil
	}
	return out.AuthURL, nil
}

// Call runs a tool through muster. URL input requests retain the producer
// message and URL; legacy auth_required results remain AuthRequiredError.
func (c *Client) Call(ctx context.Context, bearer, tool string, args map[string]any) (*Result, error) {
	if bearer == "" {
		return nil, errors.New("muster: the request carries no identity token")
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	t, err := transport.NewStreamableHTTP(c.endpoint,
		transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + bearer}))
	if err != nil {
		return nil, fmt.Errorf("muster MCP client: %w", err)
	}
	// A server/discover slower than mcp-go's probe bound would fall back to
	// the 2025-11-25 handshake, which carries no InputRequiredResult; the
	// call's own deadline bounds it.
	client := mcpclient.NewClient(t, mcpclient.WithDiscoverTimeout(-1), mcpclient.WithManualInputRoundTrips())
	defer func() { _ = client.Close() }()
	host := c.host()
	if err := client.Start(ctx); err != nil {
		return nil, fmt.Errorf("muster (%s): %w", host, err)
	}
	init := mcp.InitializeRequest{}
	init.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = mcp.Implementation{Name: "agent-manager", Version: c.version}
	if _, err := client.Initialize(ctx, init); err != nil {
		return nil, fmt.Errorf("muster (%s): initialize: %w", host, err)
	}
	req := mcp.CallToolRequest{}
	req.Params.Name = MetaCallTool
	req.Params.Arguments = map[string]any{"name": tool, "arguments": args}
	relay := relayFrom(ctx)
	target, err := urlelicitation.RequestBinding(req)
	if err != nil {
		return nil, err
	}
	if relay != nil {
		ctx = mcpclient.WithRequestClientCapabilities(ctx, urlElicitation)
		req.Params.MultiRoundTripParams = relay.answer(target)
	}
	outer, err := client.CallTool(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("muster (%s): call %s: %w", host, tool, err)
	}
	if outer.NeedsInput() {
		relayed, err := urlelicitation.FromInputRequired(outer)
		if err != nil {
			return nil, fmt.Errorf("muster (%s): call %s: %w", host, tool, err)
		}
		if relay != nil {
			relay.record(target, relayed)
		}
		return nil, fmt.Errorf("muster requires URL input: %s Open %s and then retry", relayed.Message, relayed.URL)
	}
	inner, dispatched := unwrap(outer)
	if !inner.IsError {
		return &Result{Text: textOf(inner), Structured: structuredOf(inner)}, nil
	}
	if challenge := authChallenge(inner); challenge != nil {
		if challenge.Server == "" {
			challenge.Server = c.server()
		}
		return nil, challenge
	}
	return nil, &ToolError{Tool: tool, Message: textOf(inner), Dispatched: dispatched}
}

// server is the muster server the prefix names.
func (c *Client) server() string {
	return strings.TrimSuffix(strings.TrimPrefix(c.prefix, "x_"), "_")
}

func (c *Client) host() string {
	u, _ := url.Parse(c.endpoint)
	return u.Host
}

// unwrap reads the target tool's own result out of call_tool's text. A
// result that is no envelope is call_tool's own refusal (an unknown tool,
// one outside the caller's toolset): not dispatched.
func unwrap(outer *mcp.CallToolResult) (*mcp.CallToolResult, bool) {
	var env mcp.CallToolResult
	if err := json.Unmarshal([]byte(textOf(outer)), &env); err != nil || len(env.Content) == 0 {
		return outer, false
	}
	return &env, true
}

func textOf(r *mcp.CallToolResult) string {
	var texts []string
	for _, c := range r.Content {
		if t, ok := mcp.AsTextContent(c); ok && t.Text != "" {
			texts = append(texts, t.Text)
		}
	}
	return strings.Join(texts, "\n")
}

func structuredOf(r *mcp.CallToolResult) json.RawMessage {
	if len(r.RawStructuredContent) > 0 && string(r.RawStructuredContent) != "null" {
		return r.RawStructuredContent
	}
	if r.StructuredContent == nil {
		return nil
	}
	raw, err := json.Marshal(r.StructuredContent)
	if err != nil {
		return nil
	}
	return raw
}

// authChallenge reads muster's auth_required answer: an error result whose
// structured content names the server and the sign-in link.
func authChallenge(r *mcp.CallToolResult) *AuthRequiredError {
	var challenge struct {
		Status  string `json:"status"`
		Server  string `json:"server"`
		AuthURL string `json:"authUrl"`
	}
	if raw := structuredOf(r); len(raw) > 0 && json.Unmarshal(raw, &challenge) == nil && challenge.Status == "auth_required" {
		return &AuthRequiredError{Server: challenge.Server, LoginURL: challenge.AuthURL}
	}
	if strings.HasPrefix(textOf(r), "auth_required") {
		return &AuthRequiredError{}
	}
	return nil
}

// urlElicitation is what a call on a context from WithRelay declares to
// muster.
var urlElicitation = mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapability{URL: &struct{}{}}}

// Relay binds the first URL elicitation to its originating call and sends
// the caller's answer only to that call, once per handler invocation.
type Relay struct {
	mu          sync.Mutex
	retry       *urlelicitation.Retry
	answered    bool
	elicitation *Elicitation
}

// Elicitation is a URL interaction requested by muster.
type Elicitation struct {
	// Target names the call: the tool and a digest of its arguments.
	Target string
	// Relayed is muster's URL elicitation and requestState.
	Relayed *urlelicitation.Relayed
}

type relayKey struct{}

// WithRelay records URL elicitations and forwards a matching retry once.
func WithRelay(ctx context.Context, retry *urlelicitation.Retry) (context.Context, *Relay) {
	r := &Relay{retry: retry}
	return context.WithValue(ctx, relayKey{}, r), r
}

func relayFrom(ctx context.Context) *Relay {
	r, _ := ctx.Value(relayKey{}).(*Relay)
	return r
}

// Elicitation returns the first pending URL interaction, or nil.
func (r *Relay) Elicitation() *Elicitation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.elicitation
}

// answer is the caller's answer for the call target, once.
func (r *Relay) answer(target string) mcp.MultiRoundTripParams {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.retry == nil || r.answered || r.retry.Target != target {
		return mcp.MultiRoundTripParams{}
	}
	r.answered = true
	return r.retry.Params
}

func (r *Relay) record(target string, relayed *urlelicitation.Relayed) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.elicitation == nil {
		r.elicitation = &Elicitation{Target: target, Relayed: relayed}
	}
}
