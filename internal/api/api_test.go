package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/chart"
	"github.com/giantswarm/agent-manager/internal/kube"
)

type embeddedChart struct{}

func (embeddedChart) Schema(context.Context) chart.Schema { return chart.EmbeddedSchema() }
func (embeddedChart) Info(context.Context) chart.Info {
	return chart.Info{OCIURL: agents.DefaultChartOCIURL, Semver: "x.x.x", SchemaVersion: chart.EmbeddedSchemaVersion, SchemaSource: chart.SourceEmbedded}
}
func (embeddedChart) Name() string        { return "agent" }
func (embeddedChart) OCIURL() string      { return agents.DefaultChartOCIURL }
func (embeddedChart) SemverRange() string { return "x.x.x" }

var (
	hrGVR  = schema.GroupVersionResource{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"}
	ociGVR = schema.GroupVersionResource{Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "ocirepositories"}
	agGVR  = schema.GroupVersionResource{Group: "kagent.dev", Version: "v1alpha2", Resource: "agents"}
	mcGVR  = schema.GroupVersionResource{Group: "kagent.dev", Version: "v1alpha2", Resource: "modelconfigs"}
)

func newService(t *testing.T) (*agents.Service, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	mc := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kagent.dev/v1alpha2", "kind": "ModelConfig",
		"metadata": map[string]any{"name": "default-model-config", "namespace": "kagent"},
		"spec":     map[string]any{"provider": "Anthropic", "model": "claude-sonnet-4-6"},
	}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		hrGVR: "HelmReleaseList", ociGVR: "OCIRepositoryList", agGVR: "AgentList", mcGVR: "ModelConfigList",
	}, mc)
	typed := kubefake.NewClientset()
	client := kube.FromInterfaces(dyn, typed, typed.Discovery())
	svc := agents.New(kube.NewServiceAccountProvider(client), embeddedChart{}, nil, agents.Config{DefaultNamespace: "kagent", Version: "test"}, nil)
	return svc, dyn
}

func do(t *testing.T, h http.Handler, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req := httptest.NewRequest(method, path, &buf)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 && strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), rec.Body.String())
	}
	return rec.Code, out
}

func TestRESTLifecycle(t *testing.T) {
	svc, _ := newService(t)
	mux := http.NewServeMux()
	NewREST(svc, nil).Register(mux)

	code, info := do(t, mux, http.MethodGet, Prefix+"/info", nil)
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, "test", info["version"])

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Prefix+"/openapi.yaml", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "x-mcp-tool: create_agent")

	readOnly := []string{"preset:read-only"}
	code, body := do(t, mux, http.MethodPost, Prefix+"/agents", map[string]any{"name": "sre", "modelConfig": "nope", "toolset": readOnly})
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, "invalid_request", body["error"].(map[string]any)["code"])

	code, body = do(t, mux, http.MethodPost, Prefix+"/agents", map[string]any{"name": "sre", "modelConfig": "default-model-config", "displayName": "SRE", "toolset": readOnly, "unknown": 1})
	assert.Equal(t, http.StatusBadRequest, code, "unknown fields are rejected: %v", body)

	// No toolset: 400 naming the presets. The removed toolNames: 400 with the reason.
	code, body = do(t, mux, http.MethodPost, Prefix+"/agents", map[string]any{"name": "sre", "modelConfig": "default-model-config"})
	assert.Equal(t, http.StatusBadRequest, code, body)
	msg := body["error"].(map[string]any)["message"].(string)
	for _, want := range []string{"toolset is required", "preset:read-only", "preset:none", "preset:infrastructure", "preset:agent-platform", "preset:full"} {
		assert.Contains(t, msg, want)
	}
	code, body = do(t, mux, http.MethodPost, Prefix+"/agents", map[string]any{"name": "sre", "modelConfig": "default-model-config", "toolset": readOnly, "toolNames": []string{"x_a_b"}})
	assert.Equal(t, http.StatusBadRequest, code, body)
	assert.Contains(t, body["error"].(map[string]any)["message"], "toolNames never narrowed anything against muster")

	code, body = do(t, mux, http.MethodPost, Prefix+"/agents/validate", map[string]any{"name": "sre", "modelConfig": "default-model-config"})
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, false, body["valid"], "a dry run without a toolset is invalid: %v", body["errors"])
	code, body = do(t, mux, http.MethodPost, Prefix+"/agents/validate", map[string]any{"name": "sre", "modelConfig": "default-model-config", "toolset": []string{}})
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, false, body["valid"])
	assert.Contains(t, body["errors"].([]any)[0], "preset:none")
	code, body = do(t, mux, http.MethodPost, Prefix+"/agents/validate", map[string]any{"name": "sre", "modelConfig": "default-model-config", "toolset": readOnly})
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, true, body["valid"], body["errors"])
	assert.Equal(t, []any{"preset:read-only"}, body["manifests"].(map[string]any)["values"].(map[string]any)["toolset"])

	code, body = do(t, mux, http.MethodPost, Prefix+"/agents", map[string]any{"name": "sre", "modelConfig": "default-model-config", "displayName": "SRE", "toolset": readOnly})
	require.Equal(t, http.StatusCreated, code, body)
	assert.Equal(t, true, body["created"].(map[string]any)["helmRelease"])
	assert.Equal(t, true, body["created"].(map[string]any)["ociRepository"])
	assert.Equal(t, []any{"preset:read-only"}, body["agent"].(map[string]any)["toolset"])
	_, implicit := body["agent"].(map[string]any)["implicitFullAccess"]
	assert.False(t, implicit)

	code, body = do(t, mux, http.MethodPost, Prefix+"/agents", map[string]any{"name": "sre", "modelConfig": "default-model-config", "toolset": readOnly})
	assert.Equal(t, http.StatusConflict, code, body)

	code, body = do(t, mux, http.MethodGet, Prefix+"/agents?namespace=kagent", nil)
	assert.Equal(t, http.StatusOK, code)
	assert.Len(t, body["agents"], 1)

	code, body = do(t, mux, http.MethodGet, Prefix+"/agents/kagent/sre", nil)
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, "SRE", body["displayName"])
	assert.Equal(t, "helmrelease", body["managed"])
	assert.Equal(t, []any{"preset:read-only"}, body["toolset"])

	code, body = do(t, mux, http.MethodPatch, Prefix+"/agents/kagent/sre", map[string]any{"description": "helps", "toolset": []string{"preset:read-only", "server:github"}})
	assert.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, []any{"agent.description", "toolset"}, body["changed"])
	assert.Equal(t, []any{"preset:read-only", "server:github"}, body["after"].(map[string]any)["toolset"])
	code, body = do(t, mux, http.MethodPatch, Prefix+"/agents/kagent/sre", map[string]any{"toolNames": []string{"x_a_b"}})
	assert.Equal(t, http.StatusBadRequest, code, body)
	assert.Contains(t, body["error"].(map[string]any)["message"], "toolNames never narrowed")
	code, body = do(t, mux, http.MethodPatch, Prefix+"/agents/kagent/sre", map[string]any{"toolset": []string{}})
	assert.Equal(t, http.StatusBadRequest, code, body)
	assert.Contains(t, body["error"].(map[string]any)["message"], "preset:none")
	code, body = do(t, mux, http.MethodPost, Prefix+"/agents/validate", map[string]any{"name": "sre", "update": true, "toolNames": []string{"x"}})
	assert.Equal(t, http.StatusBadRequest, code, body)

	code, body = do(t, mux, http.MethodGet, Prefix+"/agents/kagent/sre/status", nil)
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, "progressing", body["verdict"])

	code, body = do(t, mux, http.MethodGet, Prefix+"/modelconfigs", nil)
	assert.Equal(t, http.StatusOK, code)
	assert.Len(t, body["modelConfigs"], 1)

	code, body = do(t, mux, http.MethodGet, Prefix+"/skills", nil)
	assert.Equal(t, http.StatusNotImplemented, code, body)

	code, body = do(t, mux, http.MethodDelete, Prefix+"/agents/kagent/sre", nil)
	assert.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, true, body["helmReleaseDeleted"])
	assert.Equal(t, true, body["ociRepositoryDeleted"])

	code, _ = do(t, mux, http.MethodGet, Prefix+"/agents/kagent/sre", nil)
	assert.Equal(t, http.StatusNotFound, code)
}

func callTool(t *testing.T, srv interface {
	HandleMessage(ctx context.Context, message json.RawMessage) mcp.JSONRPCMessage
}, name string, args map[string]any) (string, bool) {
	t.Helper()
	req := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}}
	raw, err := json.Marshal(req)
	require.NoError(t, err)
	out, err := json.Marshal(srv.HandleMessage(context.Background(), raw))
	require.NoError(t, err)
	var parsed struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(out, &parsed))
	if parsed.Error != nil {
		return parsed.Error.Message, true
	}
	require.NotEmpty(t, parsed.Result.Content, string(out))
	return parsed.Result.Content[0].Text, parsed.Result.IsError
}

func TestMCPToolsMirrorREST(t *testing.T) {
	svc, dyn := newService(t)
	srv := NewMCPServer(svc, "test")

	listReq, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	out, _ := json.Marshal(srv.HandleMessage(context.Background(), listReq))
	var listed struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
				InputSchema struct {
					Properties map[string]any `json:"properties"`
					Required   []string       `json:"required"`
				} `json:"inputSchema"`
				Annotations struct {
					ReadOnlyHint    *bool `json:"readOnlyHint"`
					DestructiveHint *bool `json:"destructiveHint"`
				} `json:"annotations"`
			} `json:"tools"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(out, &listed))
	names := map[string]bool{}
	for _, tool := range listed.Result.Tools {
		names[tool.Name] = true
		_, hasToolNames := tool.InputSchema.Properties["toolNames"]
		assert.False(t, hasToolNames, "%s still declares toolNames", tool.Name)
		switch tool.Name {
		case ToolCreateAgent:
			assert.Contains(t, tool.InputSchema.Required, "toolset")
			assert.Contains(t, tool.InputSchema.Properties["toolset"].(map[string]any)["description"], "preset:none")
		case ToolUpdateAgent, ToolValidateAgent:
			assert.Contains(t, tool.InputSchema.Properties, "toolset")
			assert.NotContains(t, tool.InputSchema.Required, "toolset")
		}
		switch tool.Name {
		case ToolCreateAgent, ToolUpdateAgent, ToolDeleteAgent:
			assert.Contains(t, tool.Description, "WRITES", tool.Name)
			require.NotNil(t, tool.Annotations.ReadOnlyHint, tool.Name)
			assert.False(t, *tool.Annotations.ReadOnlyHint, tool.Name)
		default:
			assert.Contains(t, tool.Description, "Read-only", tool.Name)
			require.NotNil(t, tool.Annotations.ReadOnlyHint, tool.Name)
			assert.True(t, *tool.Annotations.ReadOnlyHint, tool.Name)
		}
		if tool.Name == ToolDeleteAgent {
			require.NotNil(t, tool.Annotations.DestructiveHint)
			assert.True(t, *tool.Annotations.DestructiveHint)
		}
	}
	for _, want := range ToolNames() {
		assert.True(t, names[want], "tool %s missing", want)
	}
	assert.Len(t, listed.Result.Tools, len(ToolNames()))

	text, isErr := callTool(t, srv, ToolGetInfo, nil)
	require.False(t, isErr, text)
	assert.Contains(t, text, `"identity": "serviceAccount"`)

	text, isErr = callTool(t, srv, ToolCreateAgent, map[string]any{"name": "sre", "modelConfig": "nope", "toolset": []string{"preset:read-only"}})
	assert.True(t, isErr)
	assert.True(t, strings.HasPrefix(text, "invalid_request:"), text)

	// Refused without a toolset, naming the presets; toolNames is explained.
	text, isErr = callTool(t, srv, ToolCreateAgent, map[string]any{"name": "sre", "modelConfig": "default-model-config"})
	assert.True(t, isErr)
	for _, want := range []string{"invalid_request:", "toolset is required", "preset:read-only", "preset:none", "preset:infrastructure", "preset:agent-platform", "preset:full"} {
		assert.Contains(t, text, want)
	}
	text, isErr = callTool(t, srv, ToolCreateAgent, map[string]any{"name": "sre", "modelConfig": "default-model-config", "toolNames": []string{"x_a_b"}})
	assert.True(t, isErr)
	assert.Contains(t, text, "toolNames never narrowed anything against muster")
	text, isErr = callTool(t, srv, ToolValidateAgent, map[string]any{"name": "sre", "modelConfig": "default-model-config", "toolset": []string{"toolset:shared"}})
	require.False(t, isErr, text)
	assert.Contains(t, text, `"valid": false`)
	assert.Contains(t, text, "reserved")

	text, isErr = callTool(t, srv, ToolCreateAgent, map[string]any{
		"name": "sre", "modelConfig": "default-model-config", "displayName": "SRE",
		"skills":  map[string]any{"gitRefs": []map[string]any{{"url": "https://github.com/giantswarm/agent-skills", "path": "runbooks", "ref": "main"}}},
		"toolset": []string{"preset:read-only", "workflow:incident-triage"},
	})
	require.False(t, isErr, text)
	var created map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &created))
	assert.Equal(t, true, created["created"].(map[string]any)["helmRelease"])
	assert.Equal(t, []any{"preset:read-only", "workflow:incident-triage"}, created["agent"].(map[string]any)["toolset"])

	hr, err := dyn.Resource(hrGVR).Namespace("kagent").Get(context.Background(), "sre", metav1.GetOptions{})
	require.NoError(t, err)
	values, _, _ := unstructured.NestedMap(hr.Object, "spec", "values")
	assert.Equal(t, map[string]any{"gitRefs": []any{map[string]any{"url": "https://github.com/giantswarm/agent-skills", "path": "runbooks", "ref": "main", "name": "runbooks"}}}, values["skills"])
	assert.Equal(t, []any{"preset:read-only", "workflow:incident-triage"}, values["toolset"])
	_, hasMuster := values["muster"]
	assert.False(t, hasMuster, "never muster.toolNames")

	// update: an explicit "" clears, absent leaves.
	text, isErr = callTool(t, srv, ToolUpdateAgent, map[string]any{"name": "sre", "displayName": "", "description": "helps"})
	require.False(t, isErr, text)
	assert.Contains(t, text, `"agent.description"`)
	assert.Contains(t, text, `"agent.displayName"`)

	// update replaces the toolset as a whole; [] and toolNames are refused.
	text, isErr = callTool(t, srv, ToolUpdateAgent, map[string]any{"name": "sre", "toolset": []string{"preset:none"}})
	require.False(t, isErr, text)
	assert.Contains(t, text, `"changed": [
    "toolset"
  ]`)
	text, isErr = callTool(t, srv, ToolUpdateAgent, map[string]any{"name": "sre", "toolset": []string{}})
	assert.True(t, isErr)
	assert.Contains(t, text, "preset:none")
	text, isErr = callTool(t, srv, ToolUpdateAgent, map[string]any{"name": "sre", "toolNames": []string{"x_a_b"}})
	assert.True(t, isErr)
	assert.Contains(t, text, "toolNames never narrowed")
	text, isErr = callTool(t, srv, ToolGetAgent, map[string]any{"name": "sre"})
	require.False(t, isErr, text)
	assert.Contains(t, text, `"toolset": [
    "preset:none"
  ]`)

	text, isErr = callTool(t, srv, ToolValidateAgent, map[string]any{"name": "sre", "update": true, "runtime": "rust"})
	require.False(t, isErr, text)
	assert.Contains(t, text, `"valid": false`)

	text, isErr = callTool(t, srv, ToolGetAgentStatus, map[string]any{"name": "sre"})
	require.False(t, isErr, text)
	assert.Contains(t, text, `"verdict": "progressing"`)

	text, isErr = callTool(t, srv, ToolListAgents, nil)
	require.False(t, isErr, text)
	assert.Contains(t, text, `"name": "sre"`)

	text, isErr = callTool(t, srv, ToolDeleteAgent, map[string]any{"name": "sre"})
	require.False(t, isErr, text)
	assert.Contains(t, text, `"ociRepositoryDeleted": true`)

	text, isErr = callTool(t, srv, ToolGetAgent, map[string]any{"name": "sre"})
	assert.True(t, isErr)
	assert.True(t, strings.HasPrefix(text, "not_found:"), text)
}

func TestStatusForMapsEverySentinel(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{agents.ErrNotFound, http.StatusNotFound, "not_found"},
		{agents.ErrInvalid, http.StatusBadRequest, "invalid_request"},
		{agents.ErrConflict, http.StatusConflict, "conflict"},
		{agents.ErrForbidden, http.StatusForbidden, "forbidden"},
		{agents.ErrUnauthenticated, http.StatusUnauthorized, "unauthenticated"},
		{agents.ErrUnsupported, http.StatusNotImplemented, "unsupported"},
		{errors.New("boom"), http.StatusBadGateway, "backend_error"},
	} {
		status, code := statusFor(fmt.Errorf("%w: detail", tc.err))
		assert.Equal(t, tc.status, status, tc.code)
		assert.Equal(t, tc.code, code)
	}
}
