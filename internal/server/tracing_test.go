package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/api"
	"github.com/giantswarm/agent-manager/internal/kube"
)

func TestTracesMCPToolCallsUnderTheInboundTrace(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter), sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.NeverSample())))
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})

	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	typed := kubefake.NewClientset()
	svc := agents.New(kube.NewServiceAccountProvider(kube.FromInterfaces(dyn, typed, typed.Discovery())), embeddedChart{}, nil, nil, agents.Config{Version: "test"}, nil)
	srv, err := New(Config{Addr: "127.0.0.1:0", MCPEnabled: true}, svc, api.NewMCPServer(svc, "test"), quiet())
	require.NoError(t, err)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	traceparent := "00-" + traceID + "-00f067aa0ba902b7-01"
	post := func(session, body string) *http.Response {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/mcp", strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("traceparent", traceparent)
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		return resp
	}
	resp := post("", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`)
	session := resp.Header.Get("Mcp-Session-Id")
	_ = resp.Body.Close()
	resp = post(session, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_info","arguments":{}}}`)
	_ = resp.Body.Close()

	probeReq, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/healthz", nil)
	require.NoError(t, err)
	probeReq.Header.Set("traceparent", traceparent)
	probe, err := http.DefaultClient.Do(probeReq)
	require.NoError(t, err)
	_ = probe.Body.Close()

	spans := map[string]tracetest.SpanStub{}
	for _, s := range exporter.GetSpans() {
		require.Equal(t, traceID, s.SpanContext.TraceID().String(), "span %s left the inbound trace", s.Name)
		require.NotContains(t, s.Name, "healthz")
		spans[s.Name] = s
	}

	httpSpan, ok := spans["POST /mcp"]
	require.True(t, ok, "spans: %v", names(spans))
	require.Equal(t, trace.SpanKindServer, httpSpan.SpanKind)

	call, ok := spans["mcp.tools/call"]
	require.True(t, ok, "spans: %v", names(spans))
	require.Equal(t, trace.SpanKindServer, call.SpanKind)
	require.Equal(t, httpSpan.SpanContext.SpanID(), call.Parent.SpanID())
	require.Contains(t, call.Attributes, attribute.String("gen_ai.tool.name", api.ToolGetInfo))
	require.Contains(t, call.Attributes, attribute.String("mcp.tool.name", api.ToolGetInfo))

	tool, ok := spans["tool."+api.ToolGetInfo]
	require.True(t, ok, "spans: %v", names(spans))
	require.Equal(t, call.SpanContext.SpanID(), tool.Parent.SpanID())
}

func names(spans map[string]tracetest.SpanStub) []string {
	out := make([]string, 0, len(spans))
	for n := range spans {
		out = append(out, n)
	}
	return out
}
