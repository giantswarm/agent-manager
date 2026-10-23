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
	"fmt"
	"regexp"

	"github.com/giantswarm/agent-manager/internal/mustermcp"
)

const (
	// toolAuthorizeWorkspace is the workspace-manager's tool, by its own name.
	toolAuthorizeWorkspace = "authorize_workspace"
	authLoginTool          = mustermcp.AuthLoginTool
	metaCallTool           = mustermcp.MetaCallTool
)

// Client calls the workspace-manager's tools through muster, named by the
// configured prefix (x_workspace-manager_) joined with the tool's own name.
type Client struct {
	muster *mustermcp.Client
}

// New is the client of the workspace-manager behind the muster MCP endpoint
// (http(s)://host[:port]/mcp) under the tool prefix. version names
// agent-manager in the MCP handshake.
func New(endpoint, prefix, version string) (*Client, error) {
	c, err := mustermcp.New(endpoint, prefix, version, "workspace-manager")
	if err != nil {
		return nil, err
	}
	return &Client{muster: c}, nil
}

// The errors of a call through muster.
type (
	// AuthRequiredError is muster's sign-in challenge for the
	// workspace-manager itself.
	AuthRequiredError = mustermcp.AuthRequiredError
	// ToolError is the workspace-manager (or muster for it) refusing the call.
	ToolError = mustermcp.ToolError
)

// ErrMalformed is an answer that is not the tool's documented result.
var ErrMalformed = mustermcp.ErrMalformed

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

// Authorize asks whether the caller, whose IdP token is bearer, may start a
// Session on the workspace name. An answer about the caller's access is an
// Authorization; an error is a failure to ask.
func (c *Client) Authorize(ctx context.Context, bearer, name string) (*Authorization, error) {
	tool := c.muster.Tool(toolAuthorizeWorkspace)
	res, err := c.muster.Call(ctx, bearer, tool, map[string]any{"workspace": name})
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
	if len(res.Structured) == 0 || json.Unmarshal(res.Structured, &out) != nil {
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
	return c.muster.SignInLink(ctx, bearer, server)
}
