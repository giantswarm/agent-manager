package agents

import (
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// DefaultCacheTTL is the prompt cache time a new organization model with
// prompt caching gets: kagent's own default for the field, written out so
// that the model's file in git says it too.
const DefaultCacheTTL = "5m"

// ModelSettingsBlock says where kagent's ModelConfig keeps the settings of a
// provider: the key of the provider's block in the spec (openAI, not
// openai), and whether that block has promptCaching and cacheTTL (Anthropic
// and Bedrock) and maxTokens (Anthropic, AnthropicVertexAI, AzureOpenAI,
// Mistral and OpenAI). The API server prunes a field the block does not
// have, so a writer refuses it instead of losing it in silence. The
// provider is matched in any case; the API server judges its spelling.
type ModelSettingsBlock struct {
	Key       string
	Caching   bool
	MaxTokens bool
}

var modelSettingsBlocks = map[string]ModelSettingsBlock{
	"anthropic":         {Key: "anthropic", Caching: true, MaxTokens: true},
	"bedrock":           {Key: "bedrock", Caching: true},
	"anthropicvertexai": {Key: "anthropicVertexAI", MaxTokens: true},
	"azureopenai":       {Key: "azureOpenAI", MaxTokens: true},
	"mistral":           {Key: "mistral", MaxTokens: true},
	"openai":            {Key: "openAI", MaxTokens: true},
}

// providerBlockKeys are the keys of kagent's ModelConfigSpec provider
// blocks, by provider in lower case.
var providerBlockKeys = map[string]string{
	"anthropic": "anthropic", "anthropicvertexai": "anthropicVertexAI", "azureopenai": "azureOpenAI",
	"bedrock": "bedrock", "foundry": "foundry", "gemini": "gemini", "geminivertexai": "geminiVertexAI",
	"mistral": "mistral", "ollama": "ollama", "openai": "openAI", "sapaicore": "sapAICore",
}

// ProviderBlockKey is the key of provider's block in a ModelConfig spec, in
// any case; "" for a provider kagent does not know.
func ProviderBlockKey(provider string) string { return providerBlockKeys[strings.ToLower(provider)] }

// ProviderBlockKeys are the keys of every provider block, sorted.
func ProviderBlockKeys() []string {
	keys := make([]string, 0, len(providerBlockKeys))
	for _, key := range providerBlockKeys {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// SettingsBlockOf is the settings block of provider; ok is false for a
// provider that has none of the settings.
func SettingsBlockOf(provider string) (ModelSettingsBlock, bool) {
	b, ok := modelSettingsBlocks[strings.ToLower(provider)]
	return b, ok
}

// IsAnthropic says whether provider is kagent's Anthropic provider, in any
// case.
func IsAnthropic(provider string) bool { return strings.EqualFold(provider, "Anthropic") }

// modelSettings reads the display name, the token limit and the prompt
// caching of a ModelConfig into mc. PromptCaching stays nil for a provider
// whose block has no such field; unset on one that has it, it is false,
// kagent's default.
func modelSettings(item *unstructured.Unstructured, mc *ModelConfig) {
	mc.DisplayName = item.GetAnnotations()[DisplayNameAnnotation]
	block, ok := SettingsBlockOf(mc.Provider)
	if !ok {
		return
	}
	spec, _, _ := unstructured.NestedMap(item.Object, "spec", block.Key)
	if block.MaxTokens {
		mc.MaxTokens = IntValue(spec["maxTokens"])
	}
	if block.Caching {
		caching, _ := spec["promptCaching"].(bool)
		mc.PromptCaching = &caching
		if caching {
			mc.CacheTTL, _ = spec["cacheTTL"].(string)
		}
	}
}

// IntValue is a JSON number of an unstructured object as an int64: the API
// server's decoder gives int64, a YAML or test object may give another type.
func IntValue(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case int32:
		return int64(n)
	case float64:
		return int64(n)
	}
	return 0
}
