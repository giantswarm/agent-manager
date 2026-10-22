package chart

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The embedded chart renders the Agent and the agent's RemoteMCPServer in the
// release namespace, named after the release, as helm-controller installs them.
func TestRenderEmbeddedChart(t *testing.T) {
	values := map[string]any{
		"agent":       map[string]any{"egress": []any{"tls://db.example.com:5432"}},
		"modelConfig": map[string]any{"name": "default-model-config"},
		"toolset":     []any{"preset:read-only"},
	}
	objs, err := EmbeddedSchema().Render(t.Context(), "dba", "kagent", values)
	require.NoError(t, err)
	require.Len(t, objs, 2)
	kinds := map[string]string{}
	for _, o := range objs {
		assert.Equal(t, "api.kagent.dev/v1alpha3", o.GetAPIVersion())
		assert.Equal(t, "kagent", o.GetNamespace())
		assert.Equal(t, "dba", o.GetName())
		kinds[o.GetKind()] = o.GetName()
	}
	assert.Contains(t, kinds, "Agent")
	assert.Contains(t, kinds, "RemoteMCPServer")
}

// The chart's own refusal while rendering (a template's fail) is the error.
func TestRenderReturnsTheChartsRefusal(t *testing.T) {
	_, err := EmbeddedSchema().Render(t.Context(), "dba", "kagent", map[string]any{"toolset": []any{}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "toolset is empty")
}

func TestRenderWithoutAnArchive(t *testing.T) {
	_, err := Schema{Version: "1.0.0", Source: SourceRegistry}.Render(t.Context(), "dba", "kagent", nil)
	assert.ErrorContains(t, err, "has no archive to render")
}
