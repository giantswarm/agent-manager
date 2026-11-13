package api

import (
	"encoding/json"
	"net/http"
	"testing"

	apov1alpha1 "github.com/giantswarm/agent-platform-operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func markedModelConfig(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "api.kagent.dev/v1alpha3", "kind": "ModelConfig",
		"metadata": map[string]any{"name": name, "namespace": "kagent", "annotations": map[string]any{apov1alpha1.DefaultModelConfigAnnotation: "true"}},
		"spec":     map[string]any{"provider": "Anthropic", "model": "claude-sonnet-4-6"},
	}}
}

func TestCreateWithoutAModelConfigWritesTheDefault(t *testing.T) {
	svc, dyn := newService(t)
	_, err := dyn.Resource(mcGVR).Namespace("kagent").Create(t.Context(), markedModelConfig("claude"), metav1.CreateOptions{})
	require.NoError(t, err)
	srv := NewMCPServer(svc, "test")
	mux := http.NewServeMux()
	NewREST(svc, nil).Register(mux)

	text, isErr := callTool(t, srv, ToolCreateAgent, map[string]any{"name": "sre", "toolset": []string{"preset:read-only"}})
	require.False(t, isErr, text)
	var created map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &created))
	assert.Equal(t, "claude", created["modelConfig"])
	assert.Equal(t, true, created["modelConfigFromDefault"])

	code, body := do(t, mux, http.MethodPost, Prefix+"/agents", map[string]any{"name": "ops", "toolset": []string{"preset:read-only"}})
	require.Equal(t, http.StatusCreated, code, body)
	assert.Equal(t, "claude", body["modelConfig"])
	assert.Equal(t, true, body["modelConfigFromDefault"])

	for _, name := range []string{"sre", "ops"} {
		hr, err := dyn.Resource(hrGVR).Namespace("kagent").Get(t.Context(), name, metav1.GetOptions{})
		require.NoError(t, err)
		model, _, _ := unstructured.NestedString(hr.Object, "spec", "values", "modelConfig", "name")
		assert.Equal(t, "claude", model, name)
	}

	text, isErr = callTool(t, srv, ToolCreateAgent, map[string]any{"name": "named", "modelConfig": "default-model-config", "toolset": []string{"preset:read-only"}})
	require.False(t, isErr, text)
	var named map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &named))
	assert.Equal(t, "default-model-config", named["modelConfig"])
	assert.NotContains(t, named, "modelConfigFromDefault")
}

func TestCreateWithoutADefaultIsRefused(t *testing.T) {
	svc, dyn := newService(t)
	for _, name := range []string{"claude", "qwen"} {
		_, err := dyn.Resource(mcGVR).Namespace("kagent").Create(t.Context(), markedModelConfig(name), metav1.CreateOptions{})
		require.NoError(t, err)
	}
	const refusal = "modelConfig: namespace kagent has no default model, claude, qwen are all marked as the default; pass modelConfig, or have an admin mark one as the default (annotation " +
		apov1alpha1.DefaultModelConfigAnnotation + "=true). ModelConfigs: claude, default-model-config, qwen"

	text, isErr := callTool(t, NewMCPServer(svc, "test"), ToolCreateAgent, map[string]any{"name": "sre", "toolset": []string{"preset:read-only"}})
	assert.True(t, isErr)
	assert.Contains(t, text, "invalid_request:")
	assert.Contains(t, text, refusal)

	mux := http.NewServeMux()
	NewREST(svc, nil).Register(mux)
	code, body := do(t, mux, http.MethodPost, Prefix+"/agents", map[string]any{"name": "sre", "toolset": []string{"preset:read-only"}})
	assert.Equal(t, http.StatusBadRequest, code, body)
	assert.Contains(t, errorMessage(body), refusal)
}

func TestListModelConfigsReportsTheDefault(t *testing.T) {
	svc, dyn := newService(t)
	_, err := dyn.Resource(mcGVR).Namespace("kagent").Create(t.Context(), markedModelConfig("claude"), metav1.CreateOptions{})
	require.NoError(t, err)
	want := []any{
		map[string]any{"name": "claude", "namespace": "kagent", "provider": "Anthropic", "model": "claude-sonnet-4-6", "default": true, "accepted": nil, "resolvedRefs": nil, "promptCaching": false},
		map[string]any{"name": "default-model-config", "namespace": "kagent", "provider": "Anthropic", "model": "claude-sonnet-4-6", "default": false, "accepted": nil, "resolvedRefs": nil, "promptCaching": false},
	}

	text, isErr := callTool(t, NewMCPServer(svc, "test"), ToolListModelConfigs, nil)
	require.False(t, isErr, text)
	var listed map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &listed))
	assert.Equal(t, want, listed["modelConfigs"])

	mux := http.NewServeMux()
	NewREST(svc, nil).Register(mux)
	code, body := do(t, mux, http.MethodGet, Prefix+"/modelconfigs", nil)
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, want, body["modelConfigs"])
}
