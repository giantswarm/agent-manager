package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// liveModelField is a spec field of the live ModelConfig claude.
func liveModelField(t *testing.T, fixture *orgFixture, field string) string {
	t.Helper()
	value, _, err := unstructured.NestedString(fixture.get(t, mcGVR, orgNS, "claude").Object, "spec", field)
	require.NoError(t, err)
	return value
}

// A change of the provider of an existing model that keeps its credential
// Secret is refused with conflict and names the safe path; nothing is
// written: not the model, not the Secret, no credential operation. The
// Secret is kept when the call names none, when it names the same one, and
// when the Secret is missing; a dry run is refused too.
func TestSetModelRefusesAProviderChangeThatKeepsTheSecret(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		extra  map[string]any
		seeded bool
	}{
		{"no credentialSecret", nil, true},
		{"the same credentialSecret", map[string]any{"credentialSecret": "tenant-credential-claude"}, true}, // #nosec G101 -- a Secret's name, no credential
		{"the same Secret, another key", map[string]any{"credentialKey": "token"}, true},
		{"the Secret is missing", nil, false},
		{"dry run", map[string]any{"dryRun": true}, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newOrgFixture(t, model("claude", true))
			if scenario.seeded {
				seedSecret(t, fixture, "tenant-credential-claude", "apiKey")
			}
			arguments := map[string]any{"namespace": orgNS, "name": "claude", "provider": "OpenAI", "model": "gpt-5"}
			for key, value := range scenario.extra {
				arguments[key] = value
			}
			text, failed := fixture.call(t, ToolSetOrganizationModel, arguments)
			require.True(t, failed, text)
			require.True(t, strings.HasPrefix(text, "conflict:"), text)
			require.Contains(t, text, "new credentialSecret")
			require.Contains(t, text, "new model")
			require.NotContains(t, text, "credentialOperation")
			require.Equal(t, "Anthropic", liveModelField(t, fixture, "provider"))
			require.Equal(t, "m", liveModelField(t, fixture, "model"))
			require.Zero(t, consumerWrites(fixture))
			if !scenario.seeded {
				requireNoSecret(t, fixture, "tenant-credential-claude")
			}
		})
	}
}

// In commit mode a change that keeps the Secret is refused with conflict and
// opens no pull request. The provider compared is the one in the model's
// file in git, also when the live object Flux applied still has another.
func TestSetModelInCommitModeRefusesAProviderChangeThatKeepsTheSecret(t *testing.T) {
	file := paymentsPath + "/modelconfig-claude.yaml"
	for _, scenario := range []struct {
		name, inGit, asked string
	}{
		{"git and live agree", "Anthropic", "OpenAI"},
		{"git has a provider not applied yet", "OpenAI", "Anthropic"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			live := model("claude", true)
			live.SetLabels(fromGitLabels("payments"))
			inGit := model("claude", true)
			require.NoError(t, unstructured.SetNestedField(inGit.Object, scenario.inGit, "spec", "provider"))
			manifest := gitManifest(t, inGit)
			fixture, forge := commitFixture(t, map[string][]byte{file: manifest}, live)
			seedSecret(t, fixture, "tenant-credential-claude", "apiKey")
			text, failed := fixture.call(t, ToolSetOrganizationModel, map[string]any{"namespace": orgNS, "name": "claude", "provider": scenario.asked, "model": "gpt-5", "mode": "commit"})
			require.True(t, failed, text)
			require.True(t, strings.HasPrefix(text, "conflict:"), text)
			require.Contains(t, text, "new credentialSecret")
			require.Empty(t, forge.PullRequests())
			require.Equal(t, manifest, forge.Files(mcRepo, "main")[file])
		})
	}
}

// A provider change with a new credentialSecret that does not exist yet
// prepares a credential operation and writes nothing; its save writes the
// new Secret and then the provider and the reference in one write.
func TestSetModelProviderChangeWithANewSecret(t *testing.T) {
	fixture := newOrgFixture(t, model("claude", true))
	seedSecret(t, fixture, "tenant-credential-claude", "apiKey")
	result := prepareModelOperation(t, fixture, map[string]any{"provider": "OpenAI", "model": "gpt-5", "credentialSecret": "tenant-credential-openai"}) // #nosec G101 -- a Secret's name, no credential
	require.Equal(t, "Anthropic", liveModelField(t, fixture, "provider"))
	require.Zero(t, consumerWrites(fixture))
	requireNoSecret(t, fixture, "tenant-credential-openai")

	confirmation := confirmOperation(t, fixture, result.CredentialOperation)
	status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, "openai-value"), nil)
	require.Equal(t, http.StatusOK, status, answer)
	require.NotNil(t, fixture.get(t, secretGVR, orgNS, "tenant-credential-openai"))
	require.Equal(t, "OpenAI", liveModelField(t, fixture, "provider"))
	require.Equal(t, "gpt-5", liveModelField(t, fixture, "model"))
	require.Equal(t, "tenant-credential-openai", liveModelField(t, fixture, "apiKeySecret"))
	require.Equal(t, 1, consumerWrites(fixture))
}

// A keyless model keeps no credential reference when its provider changes.
func TestSetModelProviderChangeWithoutAStoredSecretIsNotRefused(t *testing.T) {
	existing := model("claude", true)
	unstructured.RemoveNestedField(existing.Object, "spec", "apiKeySecret")
	unstructured.RemoveNestedField(existing.Object, "spec", "apiKeySecretKey")
	fixture := newOrgFixture(t, existing)
	seedSecret(t, fixture, "tenant-credential-claude", "apiKey")
	text, failed := fixture.call(t, ToolSetOrganizationModel, map[string]any{"namespace": orgNS, "name": "claude", "provider": "OpenAI", "model": "gpt-5"})
	require.False(t, failed, text)
	require.Equal(t, "OpenAI", liveModelField(t, fixture, "provider"))
	require.Empty(t, liveModelField(t, fixture, "apiKeySecret"))
}

// The stored provider in another case is no provider change: the refusal
// does not answer it (admission judges the spelling).
func TestSetModelProviderInAnotherCaseIsNotAProviderChange(t *testing.T) {
	fixture := newOrgFixture(t, model("claude", true))
	seedSecret(t, fixture, "tenant-credential-claude", "apiKey")
	text, _ := fixture.call(t, ToolSetOrganizationModel, map[string]any{"namespace": orgNS, "name": "claude", "provider": "anthropic"})
	require.False(t, strings.HasPrefix(text, "conflict:"), text)
}

// A change of the model id alone, with the same provider or none given,
// keeps the Secret and is written as before.
func TestSetModelChangesTheModelIDOnTheSameProvider(t *testing.T) {
	for name, extra := range map[string]map[string]any{
		"same provider":      {"provider": "Anthropic"},
		"provider not given": nil,
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newOrgFixture(t, model("claude", true))
			seedSecret(t, fixture, "tenant-credential-claude", "apiKey")
			arguments := map[string]any{"namespace": orgNS, "name": "claude", "model": "claude-opus-4-1"}
			for key, value := range extra {
				arguments[key] = value
			}
			text, failed := fixture.call(t, ToolSetOrganizationModel, arguments)
			require.False(t, failed, text)
			require.Equal(t, "Anthropic", liveModelField(t, fixture, "provider"))
			require.Equal(t, "claude-opus-4-1", liveModelField(t, fixture, "model"))
			require.Equal(t, "tenant-credential-claude", liveModelField(t, fixture, "apiKeySecret"))
		})
	}
}
