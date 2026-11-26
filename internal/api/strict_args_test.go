package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The split tools take only the arguments they declare, in their exact case: JSON binding would otherwise take "Toolset" or "EGRESS" for
// fields another tool owns.
func TestTheSplitToolsRefuseArgumentsInAnotherCase(t *testing.T) {
	svc, _ := newService(t)
	srv := NewMCPServer(svc, "test")
	for _, c := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{ToolUpdateAgent, map[string]any{"name": "sre", "Toolset": []string{"preset:full"}, "EGRESS": []string{"https://example.com"}}, "update_agent does not take EGRESS, Toolset"},
		{ToolUpdateAgentAccess, map[string]any{"name": "sre", "SystemMessage": "x"}, "update_agent_access does not take SystemMessage"},
		{ToolSetAgentApproval, map[string]any{"name": "sre", "Tools": []string{"preset:full"}}, "set_agent_approval does not take Tools"},
		{ToolValidateAgent, map[string]any{"name": "sre", "Harness": "claude"}, "validate_agent does not take Harness"},
	} {
		text, isErr := callTool(t, srv, c.tool, c.args)
		require.True(t, isErr, "%s: %s", c.tool, text)
		assert.Contains(t, text, c.want)
	}

	listReq, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	out, _ := json.Marshal(srv.HandleMessage(context.Background(), listReq))
	var listed struct {
		Result struct {
			Tools []struct {
				Name        string         `json:"name"`
				InputSchema map[string]any `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(out, &listed))
	strict := map[string]bool{ToolUpdateAgent: true, ToolUpdateAgentAccess: true, ToolSetAgentApproval: true, ToolValidateAgent: true}
	for _, tool := range listed.Result.Tools {
		if strict[tool.Name] {
			assert.Equal(t, false, tool.InputSchema["additionalProperties"], tool.Name)
		}
	}
}

// expectedGeneration is declared on the update tools and reaches the write.
func TestUpdateToolsTakeExpectedGeneration(t *testing.T) {
	svc, _ := newService(t)
	srv := NewMCPServer(svc, "test")
	text, isErr := callTool(t, srv, ToolUpdateAgent, map[string]any{"name": "sre", "description": "x", "expectedGeneration": 999})
	require.True(t, isErr, text)
	assert.NotContains(t, text, "does not take")
	assert.Contains(t, text, "not_found", "bound and passed on to the write")
}
