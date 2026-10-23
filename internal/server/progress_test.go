package server

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/api"
	"github.com/giantswarm/agent-manager/internal/kube"
)

// A tool's progress reaches a caller that passed a progress token as
// notifications/progress on that token, through the whole HTTP stack: muster
// restarts its tool-call timeout on each.
func TestToolProgressReachesTheCaller(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	typed := kubefake.NewClientset()
	svc := agents.New(kube.NewServiceAccountProvider(kube.FromInterfaces(dyn, typed, typed.Discovery())), embeddedChart{}, nil, nil, agents.Config{Version: "test"}, nil)
	mcpSrv := api.NewMCPServer(svc, "test")
	mcpSrv.AddTool(mcp.NewTool("slow"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		agents.ProgressFrom(ctx)("first")
		agents.ProgressFrom(ctx)("second")
		return mcp.NewToolResultText("done"), nil
	})
	srv, err := New(Config{Addr: "127.0.0.1:0", MCPEnabled: true}, svc, mcpSrv, nil)
	require.NoError(t, err)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	c, err := mcpclient.NewStreamableHttpClient(ts.URL + "/mcp")
	require.NoError(t, err)
	defer func() { _ = c.Close() }()
	var (
		mu       sync.Mutex
		received []map[string]any
	)
	c.OnNotification(func(n mcp.JSONRPCNotification) {
		if n.Method != string(mcp.MethodNotificationProgress) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		received = append(received, n.Params.AdditionalFields)
	})
	ctx := t.Context()
	require.NoError(t, c.Start(ctx))
	_, err = c.Initialize(ctx, mcp.InitializeRequest{Params: mcp.InitializeParams{ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION, ClientInfo: mcp.Implementation{Name: "t", Version: "0"}}})
	require.NoError(t, err)

	// Without a token the call reports nothing.
	_, err = c.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "slow"}})
	require.NoError(t, err)
	mu.Lock()
	assert.Empty(t, received)
	mu.Unlock()

	res, err := c.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "slow", Meta: &mcp.Meta{ProgressToken: "call-1"}}})
	require.NoError(t, err)
	assert.False(t, res.IsError)
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, received, 2)
	for i, want := range []string{"first", "second"} {
		assert.Equal(t, "call-1", received[i]["progressToken"])
		assert.EqualValues(t, i+1, received[i]["progress"], "progress counts up")
		assert.Equal(t, want, received[i]["message"])
	}
}
