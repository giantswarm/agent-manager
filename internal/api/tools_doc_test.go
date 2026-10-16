package api

import (
	"testing"

	"github.com/giantswarm/mcp-toolkit/tooldesc"
	"github.com/giantswarm/mcp-toolkit/tooldesc/tooldesctest"
	"github.com/stretchr/testify/require"
)

// TestToolsDocIsCurrent keeps docs/tools.md — every tool's description and
// input schema as a model reads them — equal to the registered tools.
// TOOLS_DOC_UPDATE=1 rewrites the file (`make tools-doc`).
func TestToolsDocIsCurrent(t *testing.T) {
	svc, _ := newService(t)
	tooldesctest.AssertServerDoc(t, NewMCPServer(svc, "test"),
		"# The tools\n\nGenerated from the registered tools (`make tools-doc`); through muster every tool is `x_agent-manager_<name>`.",
		"../../docs/tools.md")
}

// TestToolDescriptionBudgets holds the registered descriptions to the
// budget of giantswarm/giantswarm#38592.
func TestToolDescriptionBudgets(t *testing.T) {
	svc, _ := newService(t)
	require.NoError(t, tooldesc.CheckServer(NewMCPServer(svc, "test"), tooldesc.DefaultBudget))
}
