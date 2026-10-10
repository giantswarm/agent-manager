package muster

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type authKey struct{}

// fakeMuster serves filter_tools over streamable HTTP: answer builds the
// page for a toolset and offset, and every call's Authorization is recorded.
type fakeMuster struct {
	mu     sync.Mutex
	auth   []string
	answer func(toolset []string, offset int) (*mcp.CallToolResult, error)
}

func (f *fakeMuster) start(t *testing.T) string {
	t.Helper()
	s := server.NewMCPServer("muster", "test")
	s.AddTool(mcp.NewTool(filterTools), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		f.mu.Lock()
		f.auth = append(f.auth, ctx.Value(authKey{}).(string))
		f.mu.Unlock()
		var toolset []string
		for _, v := range req.GetArguments()["toolset"].([]any) {
			toolset = append(toolset, v.(string))
		}
		return f.answer(toolset, req.GetInt("offset", 0))
	})
	h := server.NewStreamableHTTPServer(s, server.WithHTTPContextFunc(func(ctx context.Context, r *http.Request) context.Context {
		return context.WithValue(ctx, authKey{}, r.Header.Get("Authorization"))
	}))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL + "/mcp"
}

func pageJSON(t *testing.T, p map[string]any) *mcp.CallToolResult {
	t.Helper()
	b, err := json.Marshal(p)
	require.NoError(t, err)
	return mcp.NewToolResultText(string(b))
}

func TestResolveToolsetsAsTheCaller(t *testing.T) {
	yes := true
	f := &fakeMuster{}
	f.answer = func(toolset []string, offset int) (*mcp.CallToolResult, error) {
		switch {
		case toolset[0] == "preset:read-only" && offset == 0:
			return pageJSON(t, map[string]any{"truncated": true, "tools": []any{
				map[string]any{"name": "x_kubernetes_get", "server": "kubernetes", "annotations": map[string]any{"readOnlyHint": true}},
			}, "toolset_requiring_auth": []any{map[string]any{"server": "github"}}, "toolset_unmatched": []any{"server:nothing"}}), nil
		case toolset[0] == "preset:read-only":
			return pageJSON(t, map[string]any{"tools": []any{map[string]any{"name": "x_kubernetes_delete"}}}), nil
		default:
			return pageJSON(t, map[string]any{"tools": []any{}}), nil
		}
	}
	url := f.start(t)

	res, err := (&Client{Version: "test"}).ResolveToolsets(t.Context(), url, "caller-token", []string{"preset:read-only", "server:nothing"}, []string{"preset:agent-platform"})
	require.NoError(t, err)
	require.Len(t, res, 2)
	assert.Equal(t, []Tool{
		{Name: "x_kubernetes_get", Server: "kubernetes", Annotations: &Annotations{ReadOnlyHint: &yes}},
		{Name: "x_kubernetes_delete"},
	}, res[0].Tools, "every page is followed")
	assert.True(t, res[0].Tools[0].ReadOnly())
	assert.False(t, res[0].Tools[1].ReadOnly(), "a tool without readOnlyHint is not read-only")
	assert.Equal(t, []string{"github"}, res[0].RequiringAuth)
	assert.Equal(t, []string{"server:nothing"}, res[0].Unmatched)
	assert.Empty(t, res[1].Tools)
	for _, a := range f.auth {
		assert.Equal(t, "Bearer caller-token", a)
	}
	assert.Len(t, f.auth, 3)
}

func TestResolveToolsetsFailsClosed(t *testing.T) {
	c := &Client{Version: "test"}
	ts := []string{"preset:read-only"}

	_, err := c.ResolveToolsets(t.Context(), "http://127.0.0.1:1/mcp", "caller-token", ts)
	require.Error(t, err, "an unreachable muster")

	_, err = c.ResolveToolsets(t.Context(), "http://127.0.0.1:1/mcp", "", ts)
	require.ErrorIs(t, err, ErrNoToken)

	f := &fakeMuster{answer: func([]string, int) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultError("unknown preset \"read-only\""), nil
	}}
	_, err = c.ResolveToolsets(t.Context(), f.start(t), "caller-token", ts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "muster answered an error: unknown preset")

	f = &fakeMuster{answer: func([]string, int) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("No tools available to filter"), nil
	}}
	_, err = c.ResolveToolsets(t.Context(), f.start(t), "caller-token", ts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unreadable answer")

	f = &fakeMuster{answer: func([]string, int) (*mcp.CallToolResult, error) {
		return pageJSON(t, map[string]any{"truncated": true, "tools": []any{}}), nil
	}}
	_, err = c.ResolveToolsets(t.Context(), f.start(t), "caller-token", ts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), fmt.Sprintf("more than %d tools", pageSize*maxPages))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	_, err = c.ResolveToolsets(t.Context(), srv.URL+"/mcp", "caller-token", ts)
	require.Error(t, err, "muster refusing the caller")
	assert.True(t, strings.Contains(err.Error(), "initialize"), err.Error())
}
