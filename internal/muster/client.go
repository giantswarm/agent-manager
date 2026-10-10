// Package muster resolves toolsets with muster's filter_tools meta-tool, as
// the caller: every call carries the caller's IdP token as `Authorization:
// Bearer` and nothing else, so muster resolves the toolset within the
// caller's own catalogue, the tools the agent will see when it runs as that
// person. A call that cannot be made, or that muster answers with an error,
// is an error: the caller of this package fails closed on it.
package muster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

const (
	// filterTools is muster's discovery meta-tool; its toolset argument
	// resolves inline selectors against the caller's catalogue.
	filterTools = "filter_tools"
	// pageSize and maxPages bound how far a resolution is followed, so a
	// misbehaving truncated flag cannot loop forever.
	pageSize = 200
	maxPages = 20
	// callTimeout bounds one resolution, the session's initialize included.
	callTimeout = 20 * time.Second
)

// ErrNoToken is returned for a resolution without a caller token: muster
// resolves a toolset per person, and there is no other identity to ask with.
var ErrNoToken = errors.New("no caller token")

// Annotations are the MCP hints muster forwards from the tool's server; a
// hint the server did not set is nil.
type Annotations struct {
	ReadOnlyHint    *bool `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool `json:"destructiveHint,omitempty"`
}

// Tool is one tool a toolset resolves to.
type Tool struct {
	Name        string            `json:"name"`
	Server      string            `json:"server,omitempty"`
	Kind        string            `json:"kind,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations *Annotations      `json:"annotations,omitempty"`
}

// ReadOnly reports whether the tool's server marks it read-only.
func (t Tool) ReadOnly() bool {
	return t.Annotations != nil && t.Annotations.ReadOnlyHint != nil && *t.Annotations.ReadOnlyHint
}

// ServerAuth is a server whose tools the caller cannot list before signing in.
type ServerAuth struct {
	Name string `json:"name"`
}

// Resolution is what one toolset resolves to for the caller.
type Resolution struct {
	Tools []Tool
	// Unmatched are the selectors that select no tool.
	Unmatched []string
	// RequiringAuth are the servers the toolset names whose tools are unknown
	// until the caller signs in to them.
	RequiringAuth []string
}

// page is the part of filter_tools' answer a resolution reads.
type page struct {
	Tools                []Tool       `json:"tools"`
	Truncated            bool         `json:"truncated"`
	ToolsetUnmatched     []string     `json:"toolset_unmatched"`
	ToolsetRequiringAuth []ServerAuth `json:"toolset_requiring_auth"`
}

// Client resolves toolsets against a muster MCP endpoint. It holds no
// connection: every resolution opens its own session as its caller.
type Client struct {
	// Version is the client version announced at initialize.
	Version string
}

// ResolveToolsets resolves each toolset with filter_tools in one session on
// url, as the caller whose IdP token is token, in the order given.
func (c *Client) ResolveToolsets(ctx context.Context, url, token string, toolsets ...[]string) ([]Resolution, error) {
	if token == "" {
		return nil, ErrNoToken
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	mc, err := client.NewStreamableHttpClient(url, transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + token}))
	if err != nil {
		return nil, fmt.Errorf("muster %s: %w", url, err)
	}
	defer func() { _ = mc.Close() }()
	if err := mc.Start(ctx); err != nil {
		return nil, fmt.Errorf("muster %s: %w", url, err)
	}
	init := mcp.InitializeRequest{}
	init.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = mcp.Implementation{Name: "agent-manager", Version: c.Version}
	if _, err := mc.Initialize(ctx, init); err != nil {
		return nil, fmt.Errorf("muster %s: initialize: %w", url, err)
	}
	out := make([]Resolution, 0, len(toolsets))
	for _, ts := range toolsets {
		res, err := resolve(ctx, mc, ts)
		if err != nil {
			return nil, fmt.Errorf("muster %s: %s toolset [%s]: %w", url, filterTools, strings.Join(ts, ", "), err)
		}
		out = append(out, res)
	}
	return out, nil
}

// resolve walks filter_tools' pages for one toolset.
func resolve(ctx context.Context, mc *client.Client, selectors []string) (Resolution, error) {
	var res Resolution
	for i := range maxPages {
		req := mcp.CallToolRequest{}
		req.Params.Name = filterTools
		req.Params.Arguments = map[string]any{"toolset": selectors, "limit": pageSize, "offset": i * pageSize}
		result, err := mc.CallTool(ctx, req)
		if err != nil {
			return Resolution{}, err
		}
		text := resultText(result)
		if result.IsError {
			return Resolution{}, fmt.Errorf("muster answered an error: %s", text)
		}
		var p page
		if err := json.Unmarshal([]byte(text), &p); err != nil {
			return Resolution{}, fmt.Errorf("unreadable answer %q: %w", truncate(text, 200), err)
		}
		res.Tools = append(res.Tools, p.Tools...)
		if i == 0 {
			res.Unmatched = p.ToolsetUnmatched
			for _, s := range p.ToolsetRequiringAuth {
				res.RequiringAuth = append(res.RequiringAuth, s.Name)
			}
		}
		if !p.Truncated {
			return res, nil
		}
	}
	return Resolution{}, fmt.Errorf("more than %d tools; the resolution was not followed to its end", pageSize*maxPages)
}

func resultText(r *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range r.Content {
		if t, ok := c.(mcp.TextContent); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
