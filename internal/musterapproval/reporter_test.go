package musterapproval

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/mustermcp"
)

type fakeMuster struct {
	pages []string
	args  []map[string]any
	err   error
}

func (f *fakeMuster) MetaCall(_ context.Context, bearer, tool string, args map[string]any) (*mustermcp.Result, error) {
	if f.err != nil {
		return nil, f.err
	}
	if bearer != "tok" || tool != FilterTool {
		return nil, errors.New("unexpected call")
	}
	f.args = append(f.args, args)
	page := f.pages[0]
	f.pages = f.pages[1:]
	return &mustermcp.Result{Text: page}, nil
}

func TestAgentToolsPagesAndReadsTheDecidingEntry(t *testing.T) {
	m := &fakeMuster{pages: []string{
		`{"truncated": true, "tools": [{"name": "x_github_merge", "approval": {"setting": "ask", "source": "tool", "entry": "tool:x_github_merge", "admin": {"setting": "ask", "source": "tool"}}, "connectors": [{"server": "github", "namespace": "muster", "name": "github", "uid": "u1"}]}]}`,
		`{"truncated": false, "tools": [{"name": "x_github_get", "approval": {"setting": "allow", "source": "tool"}}]}`,
	}}
	got, err := New(m).AgentTools(context.Background(), "tok", agents.AgentToolsQuery{Tools: agents.ToolList{{Selector: "tool:x_github_merge", Setting: "Allow"}, {Selector: "server:github"}}, Default: "Ask"})
	require.NoError(t, err)
	assert.Equal(t, []agents.ReportedTool{
		{Name: "x_github_merge", Entry: "tool:x_github_merge", Admin: "ask", Connectors: []agents.Connector{{Server: "github", Namespace: "muster", Name: "github", UID: "u1"}}},
		{Name: "x_github_get"},
	}, got)
	require.Len(t, m.args, 2)
	assert.Equal(t, map[string]any{"tools": []any{map[string]any{"tool:x_github_merge": "Allow"}, "server:github"}, "default": "Ask"}, m.args[0]["agentTools"])
	assert.Equal(t, 1, m.args[1]["offset"])
}

func TestAMusterThatDoesNotEvaluateAgentListsIsUnavailable(t *testing.T) {
	m := &fakeMuster{pages: []string{`{"tools": [{"name": "x", "approval": {"setting": "ask", "source": "agent", "entry": "server:a"}}]}`}}
	_, err := New(m).AgentTools(context.Background(), "tok", agents.AgentToolsQuery{})
	require.ErrorIs(t, err, agents.ErrApprovalUnavailable)

	m = &fakeMuster{err: &mustermcp.ToolError{Tool: FilterTool, Message: "unknown argument agentTools"}}
	_, err = New(m).AgentTools(context.Background(), "tok", agents.AgentToolsQuery{})
	require.ErrorIs(t, err, agents.ErrApprovalUnavailable)
}
