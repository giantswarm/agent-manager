package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/agent-manager/internal/agents"
)

func TestMCPHTTPReturnsReadableAgentWhenFluxIsForbidden(t *testing.T) {
	service, dynamic := newService(t)
	_, err := dynamic.Resource(agentGVR).Namespace("kagent").Create(t.Context(), &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "api.kagent.dev/v1alpha3", "kind": "Agent",
		"metadata": map[string]any{"name": "verifier", "namespace": "kagent"},
	}}, metav1.CreateOptions{})
	require.NoError(t, err)
	for _, resource := range []string{"ocirepositories", "helmreleases"} {
		dynamic.PrependReactor("*", resource, func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(hrGVR.GroupResource(), "", errors.New("denied"))
		})
	}
	server := httptest.NewServer(mcpserver.NewStreamableHTTPServer(NewMCPServer(service, "test"),
		mcpserver.WithStateLess(true), mcpserver.WithDisableStreaming(true)))
	t.Cleanup(server.Close)

	for _, tool := range []string{ToolListAgents, ToolGetAgent, ToolGetAgentStatus} {
		t.Run(tool, func(t *testing.T) {
			arguments := map[string]any{}
			if tool != ToolListAgents {
				arguments["name"] = "verifier"
			}
			body, err := json.Marshal(map[string]any{
				"jsonrpc": "2.0", "id": 1, "method": "tools/call",
				"params": map[string]any{"name": tool, "arguments": arguments},
			})
			require.NoError(t, err)
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/mcp", bytes.NewReader(body))
			require.NoError(t, err)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Accept", "application/json, text/event-stream")
			request.Header.Set("MCP-Protocol-Version", mcp.ProtocolVersion20251125)
			response, err := server.Client().Do(request)
			require.NoError(t, err)
			defer func() { require.NoError(t, response.Body.Close()) }()
			require.Equal(t, http.StatusOK, response.StatusCode)
			var message struct {
				Result *mcp.CallToolResult `json:"result"`
				Error  json.RawMessage     `json:"error"`
			}
			require.NoError(t, json.NewDecoder(response.Body).Decode(&message))
			require.Empty(t, message.Error)
			require.NotNil(t, message.Result)
			require.False(t, message.Result.IsError)
			require.Len(t, message.Result.Content, 1)
			content, ok := message.Result.Content[0].(mcp.TextContent)
			require.True(t, ok)
			switch tool {
			case ToolListAgents:
				var list struct {
					Agents []agents.AgentSummary `json:"agents"`
				}
				require.NoError(t, json.Unmarshal([]byte(content.Text), &list))
				require.Len(t, list.Agents, 1)
				assert.Equal(t, agents.ManagedUnknown, list.Agents[0].Managed)
				assert.NotEmpty(t, list.Agents[0].ReleaseWithheld)
			case ToolGetAgent:
				var agent agents.Agent
				require.NoError(t, json.Unmarshal([]byte(content.Text), &agent))
				assert.Equal(t, agents.ManagedUnknown, agent.Managed)
				assert.Nil(t, agent.HelmRelease)
				assert.Nil(t, agent.Values)
				assert.NotEmpty(t, agent.ReleaseWithheld)
			case ToolGetAgentStatus:
				var status agents.Status
				require.NoError(t, json.Unmarshal([]byte(content.Text), &status))
				assert.Contains(t, status.Summary, "HelmRelease state was not read")
				assert.Nil(t, status.HelmRelease)
				assert.NotEmpty(t, status.ReleaseWithheld)
			}
		})
	}
}
