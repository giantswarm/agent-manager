package agents

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// TestModelSettingsMapMatchesTheModelConfigCRD holds providerBlockKeys and
// modelSettingsBlocks against kagent's ModelConfig CRD that agent-manager is
// tested against (testdata/crds). The map can go wrong only when kagent
// changes a provider block, so a refresh of the CRD copy for a new kagent
// version fails here until the map agrees. A provider block is a spec
// property whose name is, in any case, a value of the spec.provider enum.
func TestModelSettingsMapMatchesTheModelConfigCRD(t *testing.T) {
	const crdFile = "testdata/crds/api.kagent.dev_modelconfigs.yaml"
	raw, err := os.ReadFile(crdFile)
	require.NoError(t, err)
	var crd apiextensionsv1.CustomResourceDefinition
	require.NoError(t, yaml.Unmarshal(raw, &crd))
	var spec apiextensionsv1.JSONSchemaProps
	for _, v := range crd.Spec.Versions {
		if v.Storage {
			spec = v.Schema.OpenAPIV3Schema.Properties["spec"]
		}
	}
	require.NotEmpty(t, spec.Properties, "%s: no spec properties in the storage version", crdFile)
	providers := spec.Properties["provider"].Enum
	require.NotEmpty(t, providers, "%s: spec.provider has no enum", crdFile)

	// The provider blocks of the CRD, by provider in lower case.
	crdBlocks := map[string]string{}
	for _, p := range providers {
		var provider string
		require.NoError(t, yaml.Unmarshal(p.Raw, &provider))
		for key := range spec.Properties {
			if strings.EqualFold(key, provider) {
				crdBlocks[strings.ToLower(provider)] = key
			}
		}
	}

	for _, provider := range sortedKeys(crdBlocks) {
		key := crdBlocks[provider]
		if got, ok := providerBlockKeys[provider]; !ok || got != key {
			t.Errorf("%s has provider block spec.%s; set providerBlockKeys[%q] = %q in model_settings.go", crdFile, key, provider, key)
		}
	}
	for _, provider := range sortedKeys(providerBlockKeys) {
		if _, ok := crdBlocks[provider]; !ok {
			t.Errorf("%s has no provider block for %q; remove providerBlockKeys[%q] from model_settings.go", crdFile, provider, provider)
		}
	}

	for _, provider := range sortedKeys(crdBlocks) {
		key := crdBlocks[provider]
		fields := spec.Properties[key].Properties
		_, maxTokens := fields["maxTokens"]
		_, caching := fields["promptCaching"]
		_, cacheTTL := fields["cacheTTL"]
		block, listed := modelSettingsBlocks[provider]
		if caching != cacheTTL {
			t.Errorf("%s: spec.%s has promptCaching %v and cacheTTL %v; ModelSettingsBlock.Caching stands for both, so model_settings.go needs a field for each before modelSettingsBlocks[%q] can follow it", crdFile, key, caching, cacheTTL, provider)
			continue
		}
		want := ModelSettingsBlock{Key: key, Caching: caching, MaxTokens: maxTokens}
		switch {
		case !listed && (want.Caching || want.MaxTokens):
			t.Errorf("%s: spec.%s has %s; add modelSettingsBlocks[%q] = %+v in model_settings.go", crdFile, key, settingNames(want), provider, want)
		case listed && !want.Caching && !want.MaxTokens:
			t.Errorf("%s: spec.%s has none of maxTokens, promptCaching and cacheTTL; remove modelSettingsBlocks[%q] from model_settings.go", crdFile, key, provider)
		case listed && block != want:
			t.Errorf("%s: spec.%s has %s; set modelSettingsBlocks[%q] = %+v in model_settings.go (it is %+v)", crdFile, key, settingNames(want), provider, want, block)
		}
	}
	for _, provider := range sortedKeys(modelSettingsBlocks) {
		if _, ok := crdBlocks[provider]; !ok {
			t.Errorf("%s has no provider block for %q; remove modelSettingsBlocks[%q] from model_settings.go", crdFile, provider, provider)
		}
	}
}

// settingNames names the settings b has, for a failure message.
func settingNames(b ModelSettingsBlock) string {
	var names []string
	if b.MaxTokens {
		names = append(names, "maxTokens")
	}
	if b.Caching {
		names = append(names, "promptCaching", "cacheTTL")
	}
	return strings.Join(names, ", ")
}
