package workspaces

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const prefix = "x_workspace-manager_"

// fakeMuster serves muster's call_tool: it hands the target tool's call to
// answer and serialises the target's result into its own text, as muster
// does; a nil result is call_tool's own refusal.
type fakeMuster struct {
	mu      sync.Mutex
	bearers []string
	calls   []string
	answer  func(tool string, args map[string]any) *mcp.CallToolResult
}

func (f *fakeMuster) start(t *testing.T) *Client {
	t.Helper()
	s := mcpserver.NewMCPServer("muster", "test")
	s.AddTool(mcp.NewTool(metaCallTool), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		tool, _ := args["name"].(string)
		inner, _ := args["arguments"].(map[string]any)
		f.mu.Lock()
		f.calls = append(f.calls, tool)
		f.mu.Unlock()
		res := f.answer(tool, inner)
		if res == nil {
			return mcp.NewToolResultError("tool " + tool + " not found"), nil
		}
		raw, err := json.Marshal(res)
		if err != nil {
			return nil, err
		}
		return mcp.NewToolResultText(string(raw)), nil
	})
	handler := mcpserver.NewStreamableHTTPServer(s)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.bearers = append(f.bearers, r.Header.Get("Authorization"))
		f.mu.Unlock()
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL+"/mcp", prefix, "test")
	require.NoError(t, err)
	return c
}

func structured(v any, text string) *mcp.CallToolResult {
	return mcp.NewToolResultStructured(v, text)
}

func TestAuthorizeAsTheCaller(t *testing.T) {
	f := &fakeMuster{answer: func(tool string, args map[string]any) *mcp.CallToolResult {
		if tool != prefix+toolAuthorizeWorkspace || args["workspace"] != "platform" {
			return nil
		}
		return structured(map[string]any{"workspace": "platform", "outcome": "granted", "grant": "the-grant", "expiresAt": "2026-10-22T12:00:00Z"}, "Granted")
	}}
	c := f.start(t)

	a, err := c.Authorize(t.Context(), "id-token", "platform")
	require.NoError(t, err)
	assert.Equal(t, Granted, a.Outcome)
	assert.Equal(t, "the-grant", a.Grant)
	assert.Equal(t, []string{prefix + toolAuthorizeWorkspace}, f.calls)
	require.NotEmpty(t, f.bearers)
	for _, b := range f.bearers {
		assert.Equal(t, "Bearer id-token", b, "every request of the exchange is the caller's")
	}
}

func TestAuthorizeAnswers(t *testing.T) {
	for name, test := range map[string]struct {
		answer *mcp.CallToolResult
		check  func(t *testing.T, a *Authorization, err error)
	}{
		"refused": {
			answer: structured(map[string]any{"outcome": "refused", "reason": "you cannot read 1 repository", "unreadable": []string{"acme/billing"}}, "refused"),
			check: func(t *testing.T, a *Authorization, err error) {
				require.NoError(t, err)
				assert.Equal(t, Refused, a.Outcome)
				assert.Equal(t, []string{"acme/billing"}, a.Unreadable)
			},
		},
		"refused without repositories is unavailable": {
			answer: structured(map[string]any{"outcome": "refused", "reason": "workspace platform has not synced yet"}, "refused"),
			check: func(t *testing.T, a *Authorization, err error) {
				require.NoError(t, err)
				assert.Equal(t, Unavailable, a.Outcome)
				assert.Contains(t, a.Reason, "not synced")
			},
		},
		"connect required": {
			answer: structured(map[string]any{"outcome": "connect_required", "connect": []map[string]any{{"provider": "github", "signIn": "call core_auth_login server=github-app"}}}, "connect"),
			check: func(t *testing.T, a *Authorization, err error) {
				require.NoError(t, err)
				assert.Equal(t, ConnectRequired, a.Outcome)
				require.Len(t, a.Connect, 1)
				assert.Equal(t, "github-app", a.Connect[0].Server())
			},
		},
		"muster's sign-in challenge": {
			answer: &mcp.CallToolResult{
				Content:           []mcp.Content{mcp.NewTextContent("auth_required: sign in")},
				StructuredContent: map[string]any{"status": "auth_required", "authUrl": "https://muster.example/auth"},
				IsError:           true,
			},
			check: func(t *testing.T, _ *Authorization, err error) {
				var challenge *AuthRequiredError
				require.ErrorAs(t, err, &challenge)
				assert.Equal(t, "workspace-manager", challenge.Server, "the prefix's server when muster names none")
				assert.Equal(t, "https://muster.example/auth", challenge.LoginURL)
			},
		},
		"an unknown workspace": {
			answer: mcp.NewToolResultError(`authorize workspace: workspace "platform" not found`),
			check: func(t *testing.T, _ *Authorization, err error) {
				var toolErr *ToolError
				require.ErrorAs(t, err, &toolErr)
				assert.True(t, toolErr.NotFound())
			},
		},
		"a granted answer without a grant": {
			answer: structured(map[string]any{"outcome": "granted"}, "Granted"),
			check: func(t *testing.T, _ *Authorization, err error) {
				assert.ErrorIs(t, err, ErrMalformed)
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeMuster{answer: func(string, map[string]any) *mcp.CallToolResult { return test.answer }}
			a, err := f.start(t).Authorize(t.Context(), "id-token", "platform")
			test.check(t, a, err)
		})
	}
}

func TestAToolMusterDoesNotRunIsNotDispatched(t *testing.T) {
	f := &fakeMuster{answer: func(string, map[string]any) *mcp.CallToolResult { return nil }}
	_, err := f.start(t).Authorize(t.Context(), "id-token", "platform")
	var toolErr *ToolError
	require.ErrorAs(t, err, &toolErr)
	assert.False(t, toolErr.Dispatched)
	assert.False(t, toolErr.NotFound(), "muster's own refusal is no missing workspace")
}

func TestTheGrantIsInNoError(t *testing.T) {
	f := &fakeMuster{answer: func(string, map[string]any) *mcp.CallToolResult {
		return mcp.NewToolResultText(`{"grant":"the-grant"}`)
	}}
	_, err := f.start(t).Authorize(t.Context(), "id-token", "platform")
	require.ErrorIs(t, err, ErrMalformed)
	assert.NotContains(t, err.Error(), "the-grant")
}

func TestSignInLink(t *testing.T) {
	f := &fakeMuster{answer: func(tool string, args map[string]any) *mcp.CallToolResult {
		if tool != authLoginTool || args["server"] != "github-app" {
			return nil
		}
		return structured(map[string]any{"authUrl": "https://muster.example/auth/github-app"}, "sign in")
	}}
	link, err := f.start(t).SignInLink(t.Context(), "id-token", "github-app")
	require.NoError(t, err)
	assert.Equal(t, "https://muster.example/auth/github-app", link)
}

func TestNewChecksItsConfiguration(t *testing.T) {
	for _, test := range []struct{ url, prefix string }{
		{"muster:8090/mcp", prefix},
		{"http://muster/mcp?x=1", prefix},
		{"http://muster/mcp", "workspace-manager"},
		{"http://muster/mcp", "x__"},
	} {
		_, err := New(test.url, test.prefix, "test")
		assert.Error(t, err, test)
	}
	_, err := New("http://muster.agent-platform.svc.cluster.local:8090/mcp", prefix, "test")
	assert.NoError(t, err)
	_, err = (&Client{endpoint: "http://x/mcp", prefix: prefix}).call(t.Context(), "", "any", nil)
	assert.Error(t, err, "a call without the caller's token never leaves")
}
