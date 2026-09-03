package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/api"
	"github.com/giantswarm/agent-manager/internal/chart"
	"github.com/giantswarm/agent-manager/internal/kube"
)

type embeddedChart struct{}

func (embeddedChart) Schema(context.Context) chart.Schema { return chart.EmbeddedSchema() }
func (embeddedChart) Info(context.Context) chart.Info     { return chart.Info{} }
func (embeddedChart) Name() string                        { return "agent" }
func (embeddedChart) OCIURL() string                      { return agents.DefaultChartOCIURL }
func (embeddedChart) SemverRange() string                 { return "x.x.x" }

func TestServerMountsHealthRESTAndMCP(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	typed := kubefake.NewClientset()
	svc := agents.New(kube.NewServiceAccountProvider(kube.FromInterfaces(dyn, typed, typed.Discovery())), embeddedChart{}, nil, agents.Config{Version: "test"}, nil)
	srv, err := New(Config{Addr: "127.0.0.1:0", MCPEnabled: true}, svc, api.NewMCPServer(svc, "test"), nil)
	require.NoError(t, err)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	get := func(path string) int {
		resp, err := http.Get(ts.URL + path)
		require.NoError(t, err)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	assert.Equal(t, http.StatusOK, get("/healthz"))
	assert.Equal(t, http.StatusOK, get("/readyz"))
	assert.Equal(t, http.StatusOK, get("/api/v1/info"))

	// The MCP endpoint answers an initialize.
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}
