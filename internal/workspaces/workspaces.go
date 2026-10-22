// Package workspaces asks the workspace-manager, through muster's MCP
// endpoint and as the caller, whether the caller may start a Session on a
// workspace: its authorize_workspace, the call the surfaces make before they
// pass the grant to start_session. Every call carries the caller's IdP token
// as `Authorization: Bearer`, so muster sees the person and the tool runs
// under the person's own grants. agent-manager keeps nothing it answers: the
// grant goes on to kagent and appears in no log line, error or response.
package workspaces

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

const (
	// toolAuthorizeWorkspace is the workspace-manager's tool, by its own name.
	toolAuthorizeWorkspace = "authorize_workspace"
	// authLoginTool is muster's core tool answering the link that starts a
	// person's grant for one of its servers.
	authLoginTool = "core_auth_login"
	// metaCallTool is muster's meta-tool that runs any aggregated or core
	// tool; the target's result comes back serialised in its text content.
	metaCallTool = "call_tool"

	callTimeout = 45 * time.Second
)

// Client calls the workspace-manager's tools through muster, named by the
// configured prefix (x_workspace-manager_) joined with the tool's own name.
type Client struct {
	endpoint string
	prefix   string
	version  string
}

// New is the client of the workspace-manager behind the muster MCP endpoint
// (http(s)://host[:port]/mcp) under the tool prefix. version names
// agent-manager in the MCP handshake.
func New(endpoint, prefix, version string) (*Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("muster MCP URL: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("muster MCP URL %q: want http(s)://host[:port]/path without a query", endpoint)
	}
	if !strings.HasPrefix(prefix, "x_") || !strings.HasSuffix(prefix, "_") || len(prefix) < 4 {
		return nil, fmt.Errorf("workspace-manager tool prefix %q: want x_<server>_", prefix)
	}
	return &Client{endpoint: endpoint, prefix: prefix, version: version}, nil
}

// Outcome is how an authorization ended.
type Outcome string

const (
	// Granted: Authorization.Grant admits the caller's Session.
	Granted Outcome = "granted"
	// Refused: the caller cannot read Authorization.Unreadable.
	Refused Outcome = "refused"
	// Unavailable: no Session starts on the workspace yet, whoever asks
	// (not synced, being deleted); Authorization.Reason says why.
	Unavailable Outcome = "unavailable"
	// ConnectRequired: the caller has not granted the providers in
	// Authorization.Connect in muster yet.
	ConnectRequired Outcome = "connect_required"
)

// Connect is a provider whose App the caller grants in muster first.
type Connect struct {
	Provider string
	// SignIn says how, in the workspace-manager's words; it names muster's
	// core_auth_login server=<name> for a provider muster pins.
	SignIn string
}

// Server is the muster server a Connect's SignIn names, "" when it names none.
func (c Connect) Server() string {
	if m := signInServerRe.FindStringSubmatch(c.SignIn); m != nil {
		return m[1]
	}
	return ""
}

var signInServerRe = regexp.MustCompile(`\b` + authLoginTool + `\s+server=([^\s,;]+)`)

// Authorization is the workspace-manager's answer.
type Authorization struct {
	Outcome Outcome
	Reason  string
	// Grant is the workspace grant; set when Granted, never logged.
	Grant      string
	Unreadable []string
	Connect    []Connect
}

// AuthRequiredError is muster's sign-in challenge for the workspace-manager
// itself: it holds no grant for the caller there yet.
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

// ToolError is the workspace-manager (or muster for it) refusing the call:
// the workspace unknown, a failure on its side. Message is the tool's own.
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

// NotFound reports whether the workspace-manager answered that the workspace
// does not exist.
func (e *ToolError) NotFound() bool {
	return e.Dispatched && strings.Contains(strings.ToLower(e.Message), "not found")
}

// ErrMalformed is an answer that is not the tool's documented result.
var ErrMalformed = errors.New("malformed answer")

// Authorize asks whether the caller, whose IdP token is bearer, may start a
// Session on the workspace name. An answer about the caller's access is an
// Authorization; an error is a failure to ask.
func (c *Client) Authorize(ctx context.Context, bearer, name string) (*Authorization, error) {
	tool := c.prefix + toolAuthorizeWorkspace
	res, err := c.call(ctx, bearer, tool, map[string]any{"workspace": name})
	if err != nil {
		return nil, err
	}
	var out struct {
		Outcome    string   `json:"outcome"`
		Reason     string   `json:"reason"`
		Grant      string   `json:"grant"`
		Unreadable []string `json:"unreadable"`
		Connect    []struct {
			Provider string `json:"provider"`
			SignIn   string `json:"signIn"`
		} `json:"connect"`
	}
	if len(res.structured) == 0 || json.Unmarshal(res.structured, &out) != nil {
		// The text may carry the grant: it is never quoted.
		return nil, fmt.Errorf("%s: %w: no structured result", tool, ErrMalformed)
	}
	a := &Authorization{Outcome: Outcome(out.Outcome), Reason: out.Reason}
	switch a.Outcome {
	case Granted:
		if out.Grant == "" {
			return nil, fmt.Errorf("%s: %w: granted without a grant", tool, ErrMalformed)
		}
		a.Grant = out.Grant
	case Refused:
		a.Unreadable = out.Unreadable
		if len(a.Unreadable) == 0 {
			a.Outcome = Unavailable
		}
	case ConnectRequired:
		if len(out.Connect) == 0 {
			return nil, fmt.Errorf("%s: %w: connect_required without a provider", tool, ErrMalformed)
		}
		for _, p := range out.Connect {
			a.Connect = append(a.Connect, Connect{Provider: p.Provider, SignIn: p.SignIn})
		}
	default:
		return nil, fmt.Errorf("%s: %w: unknown outcome %q", tool, ErrMalformed, out.Outcome)
	}
	return a, nil
}

// SignInLink is the link that starts the caller's grant of the muster server
// server: muster's core_auth_login as the caller. "" without an error when
// muster answers no https link (the grant is held already).
func (c *Client) SignInLink(ctx context.Context, bearer, server string) (string, error) {
	res, err := c.call(ctx, bearer, authLoginTool, map[string]any{"server": server})
	if err != nil {
		return "", err
	}
	var out struct {
		AuthURL string `json:"authUrl"`
	}
	if len(res.structured) == 0 || json.Unmarshal(res.structured, &out) != nil {
		return "", nil
	}
	if u, err := url.Parse(out.AuthURL); err != nil || u.Scheme != "https" || u.Host == "" {
		return "", nil
	}
	return out.AuthURL, nil
}

// result is a target tool's answer.
type result struct {
	text       string
	structured json.RawMessage
}

// call runs tool through muster's call_tool as the caller, one MCP session
// per call. A tool's error result is a ToolError or, for muster's sign-in
// challenge, an AuthRequiredError.
func (c *Client) call(ctx context.Context, bearer, tool string, args map[string]any) (*result, error) {
	if bearer == "" {
		return nil, errors.New("muster: the request carries no identity token")
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	client, err := mcpclient.NewStreamableHttpClient(c.endpoint,
		transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + bearer}))
	if err != nil {
		return nil, fmt.Errorf("muster MCP client: %w", err)
	}
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
	req.Params.Name = metaCallTool
	req.Params.Arguments = map[string]any{"name": tool, "arguments": args}
	outer, err := client.CallTool(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("muster (%s): call %s: %w", host, tool, err)
	}
	inner, dispatched := unwrap(outer)
	if !inner.IsError {
		return &result{text: textOf(inner), structured: structuredOf(inner)}, nil
	}
	if challenge := authChallenge(inner); challenge != nil {
		if challenge.Server == "" {
			challenge.Server = strings.TrimSuffix(strings.TrimPrefix(c.prefix, "x_"), "_")
		}
		return nil, challenge
	}
	return nil, &ToolError{Tool: tool, Message: textOf(inner), Dispatched: dispatched}
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
