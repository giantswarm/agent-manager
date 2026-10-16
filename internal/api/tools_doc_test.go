package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/giantswarm/mcp-toolkit/tooldesc"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// TestToolsDocIsCurrent keeps docs/tools.md — the tool names, descriptions
// and input schemas the server registers, which tools/list answers — equal
// to the registered tools.
// TOOLS_DOC_UPDATE=1 rewrites the file (`make tools-doc`).
func TestToolsDocIsCurrent(t *testing.T) {
	svc, _ := newService(t)
	var tools []mcp.Tool
	for _, st := range NewMCPServer(svc, "test").ListTools() {
		tools = append(tools, st.Tool)
	}
	want := renderToolsDoc(tools)
	const path = "../../docs/tools.md"
	if os.Getenv("TOOLS_DOC_UPDATE") != "" {
		require.NoError(t, os.WriteFile(path, []byte(want), 0o600))
	}
	got, err := os.ReadFile(path)
	require.NoError(t, err, "run `make tools-doc`")
	require.Equal(t, want, string(got), "docs/tools.md is out of date: run `make tools-doc`")
}

// TestToolDescriptionBudgets holds the registered descriptions to the
// budget of giantswarm/giantswarm#38592.
func TestToolDescriptionBudgets(t *testing.T) {
	svc, _ := newService(t)
	require.NoError(t, tooldesc.CheckServer(NewMCPServer(svc, "test"), tooldesc.DefaultBudget))
}

func renderToolsDoc(ts []mcp.Tool) string {
	sort.Slice(ts, func(i, j int) bool { return ts[i].Name < ts[j].Name })
	var b strings.Builder
	b.WriteString("# The tools\n\n")
	b.WriteString("Generated from the registered tools (`make tools-doc`); through muster every tool is `x_agent-manager_<name>`.\n\n")
	b.WriteString("| Tool | Kind |\n|---|---|\n")
	for _, tool := range ts {
		kind := "read-only"
		if a := tool.Annotations; a.ReadOnlyHint == nil || !*a.ReadOnlyHint {
			kind = "write"
			if a.DestructiveHint != nil && *a.DestructiveHint {
				kind = "write, destructive"
			}
		}
		fmt.Fprintf(&b, "| `%s` | %s |\n", tool.Name, kind)
	}
	for _, tool := range ts {
		// A bare < starts an HTML tag on GitHub: <namespace> would vanish.
		fmt.Fprintf(&b, "\n## `%s`\n\n%s\n\n", tool.Name, strings.ReplaceAll(tool.Description, "<", `\<`))
		// mcp-go's own MarshalJSON escapes <, > and &; decode and encode
		// again without it.
		raw, err := json.Marshal(tool.InputSchema)
		var v any
		if err == nil {
			err = json.Unmarshal(raw, &v)
		}
		var schema bytes.Buffer
		enc := json.NewEncoder(&schema)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err == nil {
			err = enc.Encode(v)
		}
		if err != nil {
			return "encode " + tool.Name + ": " + err.Error()
		}
		fmt.Fprintf(&b, "```json\n%s```\n", schema.String())
	}
	return b.String()
}
