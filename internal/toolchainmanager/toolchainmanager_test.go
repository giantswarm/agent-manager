package toolchainmanager

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

	"github.com/giantswarm/agent-manager/internal/mustermcp"
)

const prefix = "x_toolchain-manager_"

// fakeMuster serves muster's call_tool with answer's result for the target
// tool; a nil result is call_tool's own refusal.
type fakeMuster struct {
	mu      sync.Mutex
	bearers []string
	args    []map[string]any
	answer  func(tool string) *mcp.CallToolResult
}

func (f *fakeMuster) start(t *testing.T) *Client {
	t.Helper()
	s := mcpserver.NewMCPServer("muster", "test")
	s.AddTool(mcp.NewTool(mustermcp.MetaCallTool), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		tool, _ := args["name"].(string)
		inner, _ := args["arguments"].(map[string]any)
		f.mu.Lock()
		f.args = append(f.args, inner)
		f.mu.Unlock()
		res := f.answer(tool)
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

func TestOfferedAsTheCaller(t *testing.T) {
	f := &fakeMuster{answer: func(tool string) *mcp.CallToolResult {
		if tool != prefix+toolListToolchains {
			return nil
		}
		return mcp.NewToolResultStructured(map[string]any{
			"toolchains": []map[string]any{
				{"name": "go", "source": "acme", "versions": []string{"v0.9.0", "v0.10.0"}},
				{"name": "go", "source": "platform", "versions": []string{"v0.10.0", "v0.2.0"}},
				{"name": "gopls", "versions": []string{"v1.0.0"}},
			},
			"message": "3 toolchains",
		}, "3 toolchains")
	}}
	c := f.start(t)

	offer, err := c.Offered(t.Context(), "id-token", "go")
	require.NoError(t, err)
	assert.Equal(t, []string{"v0.10.0", "v0.9.0", "v0.2.0"}, offer.Versions, "every source's, newest first, once each")
	assert.Equal(t, []map[string]any{{"name": "go"}}, f.args)
	for _, b := range f.bearers {
		assert.Equal(t, "Bearer id-token", b)
	}

	none, err := c.Offered(t.Context(), "id-token", "node")
	require.NoError(t, err)
	assert.Empty(t, none.Versions)
}

func TestOfferedAnswers(t *testing.T) {
	for name, test := range map[string]struct {
		answer *mcp.CallToolResult
		check  func(t *testing.T, err error)
	}{
		"grant required": {
			answer: mcp.NewToolResultStructured(map[string]any{"toolchains": nil, "status": "grant_required", "apps": []string{"github"}, "message": "grant the App"}, "grant the App"),
			check: func(t *testing.T, err error) {
				var grant *GrantRequiredError
				require.ErrorAs(t, err, &grant)
				assert.Equal(t, []string{"github"}, grant.Apps)
				assert.Equal(t, "grant the App", grant.Error())
			},
		},
		"not behind muster": {
			check: func(t *testing.T, err error) {
				var toolErr *ToolError
				require.ErrorAs(t, err, &toolErr)
				assert.False(t, toolErr.Dispatched)
			},
		},
		"muster's sign-in": {
			answer: &mcp.CallToolResult{IsError: true, Content: []mcp.Content{mcp.NewTextContent("auth_required")},
				StructuredContent: map[string]any{"status": "auth_required", "server": "toolchain-manager", "authUrl": "https://muster.example/auth"}},
			check: func(t *testing.T, err error) {
				var challenge *AuthRequiredError
				require.ErrorAs(t, err, &challenge)
				assert.Equal(t, "https://muster.example/auth", challenge.LoginURL)
			},
		},
		"no JSON": {
			answer: mcp.NewToolResultText("3 toolchains"),
			check:  func(t *testing.T, err error) { require.ErrorIs(t, err, mustermcp.ErrMalformed) },
		},
		"JSON as text": {
			answer: mcp.NewToolResultText(`{"toolchains":[{"name":"go","versions":["v1.0.0"]}]}`),
			check:  func(t *testing.T, err error) { require.NoError(t, err) },
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := (&fakeMuster{answer: func(string) *mcp.CallToolResult { return test.answer }}).start(t)
			_, err := c.Offered(t.Context(), "id-token", "go")
			test.check(t, err)
		})
	}
}
