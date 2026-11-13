package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	apov1alpha1 "github.com/giantswarm/agent-platform-operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agent-manager/internal/agents"
)

// modelBlock is the provider block key of the ModelConfig obj, or nil.
func modelBlock(t *testing.T, obj *unstructured.Unstructured, key string) map[string]any {
	t.Helper()
	require.NotNil(t, obj)
	block, _, err := unstructured.NestedMap(obj.Object, "spec", key)
	require.NoError(t, err)
	return block
}

// withSettings is model claude with the platform model's settings: prompt
// caching for 5m, a token limit of 32000 and a display name.
func withSettings() *unstructured.Unstructured {
	m := model("claude", true)
	m.SetAnnotations(map[string]string{agents.DisplayNameAnnotation: "Claude Sonnet", apov1alpha1.DefaultModelConfigAnnotation: "true"})
	_ = unstructured.SetNestedField(m.Object, map[string]any{"promptCaching": true, "cacheTTL": "5m", "maxTokens": int64(32000)}, "spec", "anthropic")
	return m
}

// A new Anthropic model caches for 5m by default and takes the token limit
// and the display name; promptCaching false turns the default off.
func TestSetModelNewAnthropicModelCachesByDefault(t *testing.T) {
	fixture := newOrgFixture(t)
	seedSecret(t, fixture, "tenant-credential-claude", "apiKey")
	text, failed := fixture.call(t, ToolSetOrganizationModel, map[string]any{"namespace": orgNS, "name": "claude", "provider": "Anthropic", "model": "claude-sonnet-5-5", "maxTokens": 32000, "displayName": "Claude Sonnet"})
	require.False(t, failed, text)
	live := fixture.get(t, mcGVR, orgNS, "claude")
	assert.Equal(t, map[string]any{"promptCaching": true, "cacheTTL": "5m", "maxTokens": int64(32000)}, modelBlock(t, live, "anthropic"))
	assert.Equal(t, "Claude Sonnet", live.GetAnnotations()[agents.DisplayNameAnnotation])

	fixture = newOrgFixture(t)
	seedSecret(t, fixture, "tenant-credential-claude", "apiKey")
	text, failed = fixture.call(t, ToolSetOrganizationModel, map[string]any{"namespace": orgNS, "name": "claude", "provider": "Anthropic", "model": "claude-sonnet-5-5", "promptCaching": false})
	require.False(t, failed, text)
	assert.Equal(t, map[string]any{"promptCaching": false}, modelBlock(t, fixture.get(t, mcGVR, orgNS, "claude"), "anthropic"))
}

// An update that omits the settings keeps them, also caching that is off:
// only a new model gets the default. Each argument changes its own setting;
// "" and 0 remove the display name and the token limit.
func TestSetModelUpdateKeepsTheStoredSettings(t *testing.T) {
	fixture := newOrgFixture(t, withSettings())
	seedSecret(t, fixture, "tenant-credential-claude", "apiKey")
	text, failed := fixture.call(t, ToolSetOrganizationModel, map[string]any{"namespace": orgNS, "name": "claude", "model": "claude-opus-5"})
	require.False(t, failed, text)
	live := fixture.get(t, mcGVR, orgNS, "claude")
	assert.Equal(t, "claude-opus-5", liveModelSpec(t, fixture))
	assert.Equal(t, map[string]any{"promptCaching": true, "cacheTTL": "5m", "maxTokens": int64(32000)}, modelBlock(t, live, "anthropic"))
	assert.Equal(t, "Claude Sonnet", live.GetAnnotations()[agents.DisplayNameAnnotation])

	text, failed = fixture.call(t, ToolSetOrganizationModel, map[string]any{"namespace": orgNS, "name": "claude", "cacheTTL": "1h", "maxTokens": 0, "displayName": ""})
	require.False(t, failed, text)
	live = fixture.get(t, mcGVR, orgNS, "claude")
	assert.Equal(t, map[string]any{"promptCaching": true, "cacheTTL": "1h"}, modelBlock(t, live, "anthropic"))
	assert.NotContains(t, live.GetAnnotations(), agents.DisplayNameAnnotation)
	assert.True(t, marked(live), "the default mark stays")

	off := model("claude", true)
	_ = unstructured.SetNestedField(off.Object, false, "spec", "anthropic", "promptCaching")
	fixture = newOrgFixture(t, off)
	seedSecret(t, fixture, "tenant-credential-claude", "apiKey")
	text, failed = fixture.call(t, ToolSetOrganizationModel, map[string]any{"namespace": orgNS, "name": "claude", "maxTokens": 16000})
	require.False(t, failed, text)
	assert.Equal(t, map[string]any{"promptCaching": false, "maxTokens": int64(16000)}, modelBlock(t, fixture.get(t, mcGVR, orgNS, "claude"), "anthropic"))
}

// A provider without prompt caching gets no default and refuses the caching
// arguments; its token limit goes into its own block; a setting its block
// does not have and a negative limit are refused, and nothing is written.
func TestSetModelSettingsFollowTheProvider(t *testing.T) {
	fixture := newOrgFixture(t)
	seedSecret(t, fixture, "tenant-credential-claude", "apiKey")
	text, failed := createModel(t, fixture, map[string]any{"maxTokens": 4096})
	require.False(t, failed, text)
	live := fixture.get(t, mcGVR, orgNS, "claude")
	assert.Equal(t, map[string]any{"maxTokens": int64(4096)}, modelBlock(t, live, "openAI"))
	assert.Nil(t, modelBlock(t, live, "anthropic"))

	for name, extra := range map[string]map[string]any{
		"promptCaching on OpenAI":  {"promptCaching": true},
		"cacheTTL on OpenAI":       {"cacheTTL": "5m"},
		"maxTokens on Ollama":      {"provider": "Ollama", "maxTokens": 100},
		"negative maxTokens":       {"maxTokens": -1},
		"fractional maxTokens":     {"maxTokens": 1.5},
		"promptCaching not a bool": {"promptCaching": "yes"},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newOrgFixture(t)
			seedSecret(t, fixture, "tenant-credential-claude", "apiKey")
			text, failed := createModel(t, fixture, extra)
			require.True(t, failed, text)
			require.True(t, strings.HasPrefix(text, "invalid_request:"), text)
			require.Nil(t, fixture.get(t, mcGVR, orgNS, "claude"))
			require.Zero(t, consumerWrites(fixture))
		})
	}
}

// In commit mode the model's file in git carries the same settings as an
// apply: a new model's file has the default caching, the token limit and
// the display name; an update keeps what the file says.
func TestSetModelSettingsInCommitMode(t *testing.T) {
	fixture, forge := commitFixture(t, nil)
	seedSecret(t, fixture, "tenant-credential-haiku", "apiKey")
	text, failed := fixture.call(t, ToolSetOrganizationModel, map[string]any{"namespace": orgNS, "name": "haiku", "provider": "Anthropic", "model": "claude-haiku", "maxTokens": 8000, "displayName": "Claude Haiku", "mode": "commit"})
	require.False(t, failed, text)
	require.Len(t, forge.PullRequests(), 1)
	file := parseFile(t, forge.Files(mcRepo, forge.PullRequests()[0].Head)[paymentsPath+"/modelconfig-haiku.yaml"])
	assert.Equal(t, map[string]any{"promptCaching": true, "cacheTTL": "5m", "maxTokens": int64(8000)}, normalized(t, modelBlock(t, file, "anthropic")))
	assert.Equal(t, "Claude Haiku", file.GetAnnotations()[agents.DisplayNameAnnotation])
	assert.Nil(t, fixture.get(t, mcGVR, orgNS, "haiku"), "nothing live")

	path := paymentsPath + "/modelconfig-claude.yaml"
	live := withSettings()
	live.SetLabels(fromGitLabels("payments"))
	fixture, forge = commitFixture(t, map[string][]byte{path: gitManifest(t, withSettings())}, live)
	seedSecret(t, fixture, "tenant-credential-claude", "apiKey")
	text, failed = fixture.call(t, ToolSetOrganizationModel, map[string]any{"namespace": orgNS, "name": "claude", "model": "claude-opus-5", "mode": "commit"})
	require.False(t, failed, text)
	require.Len(t, forge.PullRequests(), 1)
	file = parseFile(t, forge.Files(mcRepo, forge.PullRequests()[0].Head)[path])
	model, _, _ := unstructured.NestedString(file.Object, "spec", "model")
	assert.Equal(t, "claude-opus-5", model)
	assert.Equal(t, map[string]any{"promptCaching": true, "cacheTTL": "5m", "maxTokens": int64(32000)}, normalized(t, modelBlock(t, file, "anthropic")))
	assert.Equal(t, "Claude Sonnet", file.GetAnnotations()[agents.DisplayNameAnnotation])
}

// normalized is block with its numbers as int64, whatever the YAML decoder
// gave.
func normalized(t *testing.T, block map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(block)
	require.NoError(t, err)
	out := map[string]any{}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	require.NoError(t, decoder.Decode(&out))
	for key, value := range out {
		if n, ok := value.(json.Number); ok {
			i, err := n.Int64()
			require.NoError(t, err)
			out[key] = i
		}
	}
	return out
}

// list_model_configs and GET /modelconfigs report each model's display
// name, token limit and prompt caching, so an organization's model compares
// with the platform's; a provider without caching reports none.
func TestListModelConfigsReportsTheSettings(t *testing.T) {
	svc, dyn := newService(t)
	platform := markedModelConfig("anthropic-sonnet-5-5")
	platform.SetAnnotations(map[string]string{agents.DisplayNameAnnotation: "Claude Sonnet 5.5"})
	_ = unstructured.SetNestedField(platform.Object, map[string]any{"promptCaching": true, "cacheTTL": "5m", "maxTokens": int64(32000)}, "spec", "anthropic")
	openai := markedModelConfig("gpt")
	openai.SetAnnotations(nil)
	_ = unstructured.SetNestedField(openai.Object, "OpenAI", "spec", "provider")
	_ = unstructured.SetNestedField(openai.Object, map[string]any{"maxTokens": int64(4096)}, "spec", "openAI")
	for _, obj := range []*unstructured.Unstructured{platform, openai} {
		_, err := dyn.Resource(mcGVR).Namespace("kagent").Create(t.Context(), obj, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	byName := func(list any) map[string]map[string]any {
		out := map[string]map[string]any{}
		for _, item := range list.([]any) {
			m := item.(map[string]any)
			out[m["name"].(string)] = m
		}
		return out
	}

	text, isErr := callTool(t, NewMCPServer(svc, "test"), ToolListModelConfigs, nil)
	require.False(t, isErr, text)
	var listed map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &listed))
	mux := http.NewServeMux()
	NewREST(svc, nil).Register(mux)
	code, body := do(t, mux, http.MethodGet, Prefix+"/modelconfigs", nil)
	require.Equal(t, http.StatusOK, code, body)

	for _, models := range []map[string]map[string]any{byName(listed["modelConfigs"]), byName(body["modelConfigs"])} {
		sonnet := models["anthropic-sonnet-5-5"]
		assert.Equal(t, "Claude Sonnet 5.5", sonnet["displayName"])
		assert.EqualValues(t, 32000, sonnet["maxTokens"])
		assert.Equal(t, true, sonnet["promptCaching"])
		assert.Equal(t, "5m", sonnet["cacheTTL"])
		gpt := models["gpt"]
		assert.EqualValues(t, 4096, gpt["maxTokens"])
		assert.NotContains(t, gpt, "promptCaching")
		assert.NotContains(t, gpt, "displayName")
		uncached := models["default-model-config"]
		assert.Equal(t, false, uncached["promptCaching"])
		assert.NotContains(t, uncached, "cacheTTL")
	}
}
