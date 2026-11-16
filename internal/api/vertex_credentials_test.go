package api

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/organizations"
)

func vertexCredential(t *testing.T) map[string]any {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	// #nosec G101 -- the private key is generated for this test, not a stored credential.
	return map[string]any{"type": "service_account", "project_id": "vertex-test", "client_email": "agent@vertex-test.iam.gserviceaccount.com", "private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded})), "token_uri": "https://oauth2.googleapis.com/token"}
}

func TestInstallationModelEndpointRejectsCredentials(t *testing.T) {
	for _, endpoint := range []string{
		"https://user:synthetic-value@models.example/v1",
		"https://models.example/v1?api_key=synthetic-value",
		"https://models.example/v1?%61pi_key=synthetic-value",
		"https://models.example/v1?api_key_synthetic-value",
		"https://models.example/v1?safe=value;api_key=synthetic-value",
		"https://models.example/v1#synthetic-value",
	} {
		t.Run(endpoint, func(t *testing.T) {
			fixture := newOrgFixtureWith(t, func(configuration *organizations.Config) {
				configuration.Namespaces = nil
				configuration.PlatformNamespace = "kagent"
				configuration.CredentialUIURL = "https://portal.example.com/model-credentials/{namespace}/{operation}"
			})
			answer, failed := fixture.call(t, ToolSetOrganizationModel, map[string]any{
				"namespace": "kagent", "name": "new-model", "provider": "OpenAI", "model": "test-model", "endpoint": endpoint, "noCredential": true,
			})
			require.True(t, failed)
			require.NotContains(t, answer, "synthetic-value")
			require.Nil(t, fixture.get(t, mcGVR, "kagent", "new-model"))
		})
	}
}

func TestInstallationDefaultMovePreservesExternalOwnership(t *testing.T) {
	for _, owner := range []string{"Helm", "agentlab", "another-controller", "helm-annotations"} {
		t.Run(owner, func(t *testing.T) {
			previous := model("managed-default", true)
			previous.SetNamespace("kagent")
			if owner == "helm-annotations" {
				annotations := previous.GetAnnotations()
				annotations["meta.helm.sh/release-name"] = "platform-models"
				previous.SetAnnotations(annotations)
			} else {
				previous.SetLabels(map[string]string{agents.ManagedByLabel: owner})
			}
			fixture := newOrgFixtureWith(t, func(configuration *organizations.Config) {
				configuration.Namespaces = nil
				configuration.PlatformNamespace = "kagent"
				configuration.CredentialUIURL = "https://portal.example.com/model-credentials/{namespace}/{operation}"
			}, previous)
			answer, failed := fixture.call(t, ToolSetOrganizationModel, map[string]any{
				"namespace": "kagent", "name": "new-model", "provider": "OpenAI", "model": "test-model", "noCredential": true, "default": true,
			})
			require.True(t, failed, answer)
			require.True(t, marked(fixture.get(t, mcGVR, "kagent", "managed-default")))
			require.Nil(t, fixture.get(t, mcGVR, "kagent", "new-model"))
		})
	}
}

func TestInstallationDefaultMoveAllowsOwnedAndUnmanagedModels(t *testing.T) {
	for _, owner := range []string{"", agents.ManagedByValue, agents.PortalManagedByValue} {
		t.Run(owner, func(t *testing.T) {
			previous := model("previous-default", true)
			previous.SetNamespace("kagent")
			previous.SetLabels(map[string]string{agents.ManagedByLabel: owner})
			fixture := newOrgFixtureWith(t, func(configuration *organizations.Config) {
				configuration.Namespaces = nil
				configuration.PlatformNamespace = "kagent"
				configuration.CredentialUIURL = "https://portal.example.com/model-credentials/{namespace}/{operation}"
			}, previous)
			answer, failed := fixture.call(t, ToolSetOrganizationModel, map[string]any{
				"namespace": "kagent", "name": "new-model", "provider": "OpenAI", "model": "test-model", "noCredential": true, "default": true,
				"endpoint": "https://models.example/v1?api-version=2026-01-01",
			})
			require.False(t, failed, answer)
			require.False(t, marked(fixture.get(t, mcGVR, "kagent", "previous-default")))
			require.True(t, marked(fixture.get(t, mcGVR, "kagent", "new-model")))
		})
	}
}

func TestInstallationModelsNeedANamespaceInTheCredentialUIURL(t *testing.T) {
	for name, template := range map[string]string{
		"no form":           "",
		"organization form": "https://portal.example.com/organizations/test/{parent}/{organization}/credentials/{operation}",
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newOrgFixtureWith(t, func(configuration *organizations.Config) {
				configuration.PlatformNamespace = "kagent"
				configuration.CredentialUIURL = template
			})
			answer, failed := fixture.call(t, ToolSetOrganizationModel, map[string]any{
				"namespace": "kagent", "name": "keyless", "provider": "Ollama", "model": "test-model", "noCredential": true,
				"endpoint": "https://models.example/v1",
			})
			require.True(t, failed, answer)
			require.Contains(t, answer, "{namespace}")
			require.Nil(t, fixture.get(t, mcGVR, "kagent", "keyless"))
		})
	}
}

func TestVertexCredentialOperations(t *testing.T) {
	for _, provider := range []string{"AnthropicVertexAI", "GeminiVertexAI"} {
		t.Run(provider, func(t *testing.T) {
			fixture := newOrgFixture(t)
			result := prepareModelOperation(t, fixture, map[string]any{"provider": provider, "projectID": "vertex-test", "location": "global"})
			confirmation := confirmOperation(t, fixture, result.CredentialOperation)
			require.Equal(t, "google-service-account-json", confirmation.Result.Credentials[0].InputFormat)
			require.Equal(t, "GOOGLE_APPLICATION_CREDENTIALS", confirmation.Result.Credentials[0].Key)
			key, err := json.Marshal(vertexCredential(t))
			require.NoError(t, err)
			status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, string(key)), nil)
			require.Equal(t, http.StatusOK, status, answer)
			require.NotContains(t, answer, "PRIVATE KEY")
			model := fixture.get(t, mcGVR, orgNS, "claude")
			require.NotNil(t, model)
			stored, _, _ := unstructured.NestedString(model.Object, "spec", "provider")
			require.Equal(t, provider, stored)
			text, failed := fixture.call(t, ToolReplaceOrganizationCredential, map[string]any{"namespace": orgNS, "secret": result.Credentials[0].Secret, "key": result.Credentials[0].Key})
			require.False(t, failed, text)
			var replacement organizations.ResourceResult
			require.NoError(t, json.Unmarshal([]byte(text), &replacement))
			require.Equal(t, "google-service-account-json", replacement.Credentials[0].InputFormat)
		})
	}
}

func TestVertexCredentialValidationBeforeWrites(t *testing.T) {
	for _, scenario := range []string{"wrong project", "wrong type", "foreign token endpoint", "missing email", "invalid private key", "invalid JSON"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newOrgFixture(t)
			result := prepareModelOperation(t, fixture, map[string]any{"provider": "GeminiVertexAI", "projectID": "vertex-test", "location": "europe-west4"})
			confirmation := confirmOperation(t, fixture, result.CredentialOperation)
			key := vertexCredential(t)
			switch scenario {
			case "wrong project":
				key["project_id"] = "another-project"
			case "wrong type":
				key["type"] = "external_account"
			case "foreign token endpoint":
				key["token_uri"] = "https://attacker.example/token"
			case "missing email":
				delete(key, "client_email")
			case "invalid private key":
				key["private_key"] = "sensitive-invalid-key"
			}
			encoded, err := json.Marshal(key)
			require.NoError(t, err)
			if scenario == "invalid JSON" {
				encoded = []byte("sensitive-invalid-json")
			}
			status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, string(encoded)), nil)
			require.Equal(t, http.StatusBadRequest, status, answer)
			require.NotContains(t, answer, "sensitive-")
			require.Nil(t, fixture.get(t, mcGVR, orgNS, "claude"))
			require.Nil(t, fixture.get(t, secretGVR, orgNS, result.Credentials[0].Secret))
		})
	}
}

func TestVertexCredentialSavePreservesConsumerStatus(t *testing.T) {
	for _, provider := range []string{"AnthropicVertexAI", "GeminiVertexAI"} {
		t.Run(provider, func(t *testing.T) {
			fixture := newOrgFixture(t)
			arguments := map[string]any{"provider": provider, "projectID": "vertex-test", "location": "global"}
			result := prepareModelOperation(t, fixture, arguments)
			confirmation := confirmOperation(t, fixture, result.CredentialOperation)
			encoded, err := json.Marshal(vertexCredential(t))
			require.NoError(t, err)
			status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, string(encoded)), nil)
			require.Equal(t, http.StatusOK, status, answer)
			require.NoError(t, fixture.dyn.Resource(secretGVR).Namespace(orgNS).Delete(t.Context(), result.Credentials[0].Secret, metav1.DeleteOptions{}))
			arguments["model"] = "vertex-model-edited"
			result = prepareModelOperation(t, fixture, arguments)
			confirmation = confirmOperation(t, fixture, result.CredentialOperation)
			model := fixture.get(t, mcGVR, orgNS, "claude")
			require.NoError(t, unstructured.SetNestedField(model.Object, "Ready", "status", "phase"))
			_, err = fixture.dyn.Resource(mcGVR).Namespace(orgNS).UpdateStatus(t.Context(), model, metav1.UpdateOptions{})
			require.NoError(t, err)
			status, answer, _ = operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, string(encoded)), nil)
			require.Equal(t, http.StatusOK, status, answer)
			stored := fixture.get(t, mcGVR, orgNS, "claude")
			modelID, _, _ := unstructured.NestedString(stored.Object, "spec", "model")
			require.Equal(t, "vertex-model-edited", modelID)
			phase, _, _ := unstructured.NestedString(stored.Object, "status", "phase")
			require.Equal(t, "Ready", phase)
		})
	}
}

func TestInstallationModelCredentialAuthorization(t *testing.T) {
	fixture := newOrgFixtureWith(t, func(configuration *organizations.Config) {
		configuration.Namespaces = nil
		configuration.PlatformNamespace = "kagent"
		configuration.CredentialUIURL = "https://portal.example.com/model-credentials/{namespace}/{operation}"
	})
	result := prepareModelOperation(t, fixture, map[string]any{"namespace": "kagent"})
	confirmation := confirmOperation(t, fixture, result.CredentialOperation)
	require.Equal(t, "platform", confirmation.Organization.Scope)
	require.Empty(t, confirmation.Organization.Organization)
	require.Contains(t, result.Credentials[0].FormURL, "/kagent/")
	fixture.deny = func(namespace, verb, resource, name string) bool { return resource == "*" }
	readStatus, _, _ := operationHTTP(t, fixture, http.MethodGet, result.CredentialOperation, "", "", nil)
	require.Equal(t, http.StatusForbidden, readStatus)
	status, _, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, "installation-test-key"), nil)
	require.Equal(t, http.StatusForbidden, status)
	require.Nil(t, fixture.get(t, secretGVR, "kagent", result.Credentials[0].Secret))
	_, failed := fixture.call(t, ToolSetOrganizationModel, map[string]any{"namespace": "kagent", "name": "another", "provider": "OpenAI", "model": "test"})
	require.True(t, failed)
}

func TestInstallationModelCredentialSave(t *testing.T) {
	fixture := newOrgFixtureWith(t, func(configuration *organizations.Config) {
		configuration.Namespaces = nil
		configuration.PlatformNamespace = "kagent"
		configuration.CredentialUIURL = "https://portal.example.com/model-credentials/{namespace}/{operation}"
	})
	result := prepareModelOperation(t, fixture, map[string]any{"namespace": "kagent", "provider": "GeminiVertexAI", "projectID": "vertex-test", "location": "global"})
	confirmation := confirmOperation(t, fixture, result.CredentialOperation)
	encoded, err := json.Marshal(vertexCredential(t))
	require.NoError(t, err)
	status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, string(encoded)), nil)
	require.Equal(t, http.StatusOK, status, answer)
	require.NotNil(t, fixture.get(t, mcGVR, "kagent", "claude"))
	edit, failed := fixture.call(t, ToolSetOrganizationModel, map[string]any{"namespace": "kagent", "name": "claude", "model": "gemini-2.5-flash", "default": true})
	require.False(t, failed, edit)
	text, failed := fixture.call(t, ToolReplaceOrganizationCredential, map[string]any{"namespace": "kagent", "secret": result.Credentials[0].Secret, "key": result.Credentials[0].Key})
	require.False(t, failed, text)
	var replacement organizations.ResourceResult
	require.NoError(t, json.Unmarshal([]byte(text), &replacement))
	require.Equal(t, "google-service-account-json", replacement.Credentials[0].InputFormat)
	confirmation = confirmOperation(t, fixture, replacement.CredentialOperation)
	status, answer, _ = operationHTTP(t, fixture, http.MethodPost, replacement.CredentialOperation, confirmation.Version, valuesBody(t, replacement, string(encoded)), nil)
	require.Equal(t, http.StatusOK, status, answer)
	deleted, failed := fixture.call(t, ToolDeleteOrganizationObject, map[string]any{"namespace": "kagent", "kind": "model", "name": "claude"})
	require.False(t, failed, deleted)
	require.Nil(t, fixture.get(t, mcGVR, "kagent", "claude"))
	require.NotNil(t, fixture.get(t, secretGVR, "kagent", result.Credentials[0].Secret))
}

func TestInstallationModelToolsKeepOrganizationManagementDisabled(t *testing.T) {
	fixture := newOrgFixtureWith(t, func(configuration *organizations.Config) {
		configuration.Namespaces = nil
		configuration.PlatformNamespace = "kagent"
		configuration.CredentialUIURL = "https://portal.example.com/model-credentials/{namespace}/{operation}"
	})
	answer, failed := fixture.call(t, ToolListOrganizations, nil)
	require.False(t, failed, answer)
	require.JSONEq(t, `{"organizations":[]}`, answer)
	for _, tool := range []string{ToolCreateOrganization, ToolSetOrganizationConnector, ToolSetOrganizationEgress} {
		answer, failed := fixture.call(t, tool, map[string]any{"namespace": orgNS, "name": "blocked"})
		require.True(t, failed, answer)
	}
	answer, failed = fixture.call(t, ToolSetOrganizationModel, map[string]any{"namespace": orgNS, "name": "blocked", "provider": "Ollama", "model": "test", "noCredential": true})
	require.True(t, failed, answer)
	require.Empty(t, fixture.dyn.Actions())
}

func TestInstallationCredentialWritesOnlyOwnedSecrets(t *testing.T) {
	external := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata": map[string]any{"name": "kagent-vertex-credentials", "namespace": "kagent", "labels": map[string]any{"app.kubernetes.io/managed-by": "Helm"}},
		"data":     map[string]any{"credentials.json": "e30=", "other": "e30="},
	}}
	fixture := newOrgFixtureWith(t, func(configuration *organizations.Config) {
		configuration.Namespaces = nil
		configuration.PlatformNamespace = "kagent"
		configuration.CredentialUIURL = "https://portal.example.com/model-credentials/{namespace}/{operation}"
	}, external)
	vertex := map[string]any{"namespace": "kagent", "provider": "GeminiVertexAI", "model": "gemini-2.5-flash", "projectID": "vertex-test", "location": "europe-west1"}
	withSecret := func(name, secret string) map[string]any {
		arguments := map[string]any{"name": name, "credentialSecret": secret, "credentialKey": "credentials.json"}
		for key, value := range vertex {
			arguments[key] = value
		}
		return arguments
	}

	answer, failed := fixture.call(t, ToolSetOrganizationModel, withSecret("gitops-model", "kagent-vertex-credentials"))
	require.False(t, failed, answer)
	var result organizations.ResourceResult
	require.NoError(t, json.Unmarshal([]byte(answer), &result))
	require.Empty(t, result.CredentialOperation)
	require.NotNil(t, fixture.get(t, mcGVR, "kagent", "gitops-model"))

	answer, failed = fixture.call(t, ToolReplaceOrganizationCredential, map[string]any{"namespace": "kagent", "secret": "kagent-vertex-credentials", "key": "credentials.json"}) // #nosec G101 -- Secret and key names, no value.
	require.True(t, failed, answer)
	require.Contains(t, answer, "where it is managed")

	answer, failed = fixture.call(t, ToolSetOrganizationModel, withSecret("missing-model", "external-missing"))
	require.True(t, failed, answer)
	require.Contains(t, answer, "where it is managed")
	require.Nil(t, fixture.get(t, mcGVR, "kagent", "missing-model"))
	require.Equal(t, external.Object["data"], fixture.get(t, secretGVR, "kagent", "kagent-vertex-credentials").Object["data"])
}

func TestOrganizationModelsRefuseInstallationSettings(t *testing.T) {
	for name, extra := range map[string]map[string]any{
		"endpoint":              {"endpoint": "https://models.example/v1"},
		"insecureSkipTLSVerify": {"insecureSkipTLSVerify": true},
		"noCredential":          {"noCredential": true},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newOrgFixture(t)
			arguments := map[string]any{"namespace": orgNS, "name": "claude", "provider": "Anthropic", "model": "claude-sonnet-4-6"}
			for key, value := range extra {
				arguments[key] = value
			}
			answer, failed := fixture.call(t, ToolSetOrganizationModel, arguments)
			require.True(t, failed, answer)
			require.Contains(t, answer, "apply only to installation models")
			require.Nil(t, fixture.get(t, mcGVR, orgNS, "claude"))
		})
	}
	fixture := newOrgFixture(t)
	result := prepareModelOperation(t, fixture, map[string]any{"endpoint": "", "insecureSkipTLSVerify": false, "noCredential": false})
	require.NotEmpty(t, result.CredentialOperation)
}

func TestInstallationKeylessModelProviderChangeAsksForACredential(t *testing.T) {
	keyless := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "api.kagent.dev/v1alpha3", "kind": "ModelConfig",
		"metadata": map[string]any{"name": "local", "namespace": "kagent"},
		"spec":     map[string]any{"provider": "Ollama", "model": "qwen3:0.6b", "ollama": map[string]any{"host": "http://ollama.models:11434"}},
	}}
	fixture := newOrgFixtureWith(t, func(configuration *organizations.Config) {
		configuration.Namespaces = nil
		configuration.PlatformNamespace = "kagent"
		configuration.CredentialUIURL = "https://portal.example.com/model-credentials/{namespace}/{operation}"
	}, keyless)

	answer, failed := fixture.call(t, ToolSetOrganizationModel, map[string]any{"namespace": "kagent", "name": "local", "displayName": "Local model"})
	require.False(t, failed, answer)
	require.Contains(t, fixture.logs.String(), `"msg":"organization model written"`)
	require.Contains(t, fixture.logs.String(), `"credential":false`)

	answer, failed = fixture.call(t, ToolSetOrganizationModel, map[string]any{"namespace": "kagent", "name": "local", "provider": "OpenAI", "model": "gpt-5"})
	require.False(t, failed, answer)
	var result organizations.ResourceResult
	require.NoError(t, json.Unmarshal([]byte(answer), &result))
	require.NotEmpty(t, result.CredentialOperation, answer)
	require.Equal(t, organizations.CredentialPrefix+"local", result.Credentials[0].Secret)
	provider, _, _ := unstructured.NestedString(fixture.get(t, mcGVR, "kagent", "local").Object, "spec", "provider")
	require.Equal(t, "Ollama", provider)
}

func TestVertexCredentialOperationsStayInTheirOrganization(t *testing.T) {
	const billingNS = "agents-billing-a1b2c"
	fixture := newOrgFixture(t)
	fixture.deny = func(namespace, _, _, _ string) bool { return namespace == billingNS }
	answer, failed := fixture.call(t, ToolSetOrganizationModel, map[string]any{
		"namespace": billingNS, "name": "gemini", "provider": "GeminiVertexAI", "model": "gemini-2.5-flash", "projectID": "vertex-test", "location": "europe-west1",
	})
	require.True(t, failed, answer)
	require.Nil(t, fixture.get(t, mcGVR, billingNS, "gemini"))

	result := prepareModelOperation(t, fixture, map[string]any{"provider": "GeminiVertexAI", "projectID": "vertex-test", "location": "europe-west1"})
	confirmation := confirmOperation(t, fixture, result.CredentialOperation)
	fixture.deny = func(namespace, _, _, _ string) bool { return namespace == orgNS }
	encoded, err := json.Marshal(vertexCredential(t))
	require.NoError(t, err)
	status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, string(encoded)), nil)
	require.Equal(t, http.StatusForbidden, status, answer)
	require.NotContains(t, answer, "PRIVATE KEY")
	require.Nil(t, fixture.get(t, mcGVR, orgNS, "claude"))
	require.Nil(t, fixture.get(t, secretGVR, orgNS, result.Credentials[0].Secret))
}

func keylessModel(namespace, provider string) *unstructured.Unstructured {
	spec := map[string]any{"provider": provider, "model": "m"}
	switch provider {
	case "GeminiVertexAI":
		spec["geminiVertexAI"] = map[string]any{"projectID": "vertex-test", "location": "europe-west1"}
	case "AnthropicVertexAI":
		spec["anthropicVertexAI"] = map[string]any{"projectID": "vertex-test", "location": "europe-west1"}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "api.kagent.dev/v1alpha3", "kind": "ModelConfig",
		"metadata": map[string]any{"name": "claude", "namespace": namespace},
		"spec":     spec,
	}}
}

func TestKeylessEditsOpenAFormOutsideInstallationModels(t *testing.T) {
	for _, test := range []struct{ namespace, provider string }{
		{orgNS, "GeminiVertexAI"},
		{orgNS, "AnthropicVertexAI"},
		{orgNS, "Anthropic"},
		{"kagent", "GeminiVertexAI"},
	} {
		t.Run(test.namespace+"/"+test.provider, func(t *testing.T) {
			fixture := newOrgFixtureWith(t, func(configuration *organizations.Config) {
				if test.namespace == "kagent" {
					configuration.Namespaces = nil
					configuration.PlatformNamespace = "kagent"
					configuration.CredentialUIURL = "https://portal.example.com/model-credentials/{namespace}/{operation}"
				}
			}, keylessModel(test.namespace, test.provider))
			answer, failed := fixture.call(t, ToolSetOrganizationModel, map[string]any{"namespace": test.namespace, "name": "claude", "displayName": "Edited"})
			require.False(t, failed, answer)
			var result organizations.ResourceResult
			require.NoError(t, json.Unmarshal([]byte(answer), &result))
			require.NotEmpty(t, result.CredentialOperation, answer)
			require.Equal(t, organizations.CredentialPrefix+"claude", result.Credentials[0].Secret)
			stored := fixture.get(t, mcGVR, test.namespace, "claude")
			secret, _, _ := unstructured.NestedString(stored.Object, "spec", "apiKeySecret")
			require.Empty(t, secret)
			require.Empty(t, stored.GetAnnotations()["ui.giantswarm.io/display-name"])
		})
	}
}

func TestVertexProjectChangeNeedsAKeyCheckedAgainstTheNewProject(t *testing.T) {
	fixture := newOrgFixture(t)
	result := prepareModelOperation(t, fixture, map[string]any{"provider": "GeminiVertexAI", "projectID": "vertex-test", "location": "europe-west1"})
	confirmation := confirmOperation(t, fixture, result.CredentialOperation)
	key := vertexCredential(t)
	encoded, err := json.Marshal(key)
	require.NoError(t, err)
	status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, string(encoded)), nil)
	require.Equal(t, http.StatusOK, status, answer)
	seedSecret(t, fixture, "tenant-credential-other", "GOOGLE_APPLICATION_CREDENTIALS")

	for _, secret := range []string{"", result.Credentials[0].Secret, "tenant-credential-other"} {
		arguments := map[string]any{"namespace": orgNS, "name": "claude", "projectID": "vertex-other"}
		if secret != "" {
			arguments["credentialSecret"] = secret
		}
		answer, failed := fixture.call(t, ToolSetOrganizationModel, arguments)
		require.True(t, failed, secret+": "+answer)
	}
	project, _, _ := unstructured.NestedString(fixture.get(t, mcGVR, orgNS, "claude").Object, "spec", "geminiVertexAI", "projectID")
	require.Equal(t, "vertex-test", project)

	moved := prepareModelOperation(t, fixture, map[string]any{"provider": "", "model": "", "projectID": "vertex-other", "credentialSecret": "tenant-credential-claude-other"}) // #nosec G101 -- a Secret name, no value.
	confirmation = confirmOperation(t, fixture, moved.CredentialOperation)
	status, answer, _ = operationHTTP(t, fixture, http.MethodPost, moved.CredentialOperation, confirmation.Version, valuesBody(t, moved, string(encoded)), nil)
	require.Equal(t, http.StatusBadRequest, status, answer)
	require.Nil(t, fixture.get(t, secretGVR, orgNS, "tenant-credential-claude-other"))
	key["project_id"] = "vertex-other"
	encoded, err = json.Marshal(key)
	require.NoError(t, err)
	status, answer, _ = operationHTTP(t, fixture, http.MethodPost, moved.CredentialOperation, confirmation.Version, valuesBody(t, moved, string(encoded)), nil)
	require.Equal(t, http.StatusOK, status, answer)
	project, _, _ = unstructured.NestedString(fixture.get(t, mcGVR, orgNS, "claude").Object, "spec", "geminiVertexAI", "projectID")
	require.Equal(t, "vertex-other", project)
}

func installationVertexOperation(t *testing.T, configure func(*organizations.Config)) (*orgFixture, organizations.ResourceResult, string) {
	t.Helper()
	fixture := newOrgFixtureWith(t, func(configuration *organizations.Config) {
		configuration.Namespaces = nil
		configuration.PlatformNamespace = "kagent"
		configuration.CredentialUIURL = "https://portal.example.com/model-credentials/{namespace}/{operation}"
		if configure != nil {
			configure(configuration)
		}
	})
	result := prepareModelOperation(t, fixture, map[string]any{"namespace": "kagent", "provider": "GeminiVertexAI", "projectID": "vertex-test", "location": "europe-west1"})
	encoded, err := json.Marshal(vertexCredential(t))
	require.NoError(t, err)
	return fixture, result, valuesBody(t, result, string(encoded))
}

func TestInstallationVertexCredentialSizeLimitOverHTTP(t *testing.T) {
	fixture, result, _ := installationVertexOperation(t, nil)
	confirmation := confirmOperation(t, fixture, result.CredentialOperation)
	key := vertexCredential(t)
	key["padding"] = strings.Repeat("x", 64*1024)
	encoded, err := json.Marshal(key)
	require.NoError(t, err)
	status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, string(encoded)), nil)
	require.Equal(t, http.StatusBadRequest, status, answer)
	require.NotContains(t, answer, "PRIVATE KEY")
	require.Nil(t, fixture.get(t, mcGVR, "kagent", "claude"))
	require.Nil(t, fixture.get(t, secretGVR, "kagent", result.Credentials[0].Secret))
}

func TestInstallationVertexCredentialReplayWritesOnce(t *testing.T) {
	fixture, result, body := installationVertexOperation(t, nil)
	confirmation := confirmOperation(t, fixture, result.CredentialOperation)
	status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, body, nil)
	require.Equal(t, http.StatusOK, status, answer)
	require.NotNil(t, fixture.get(t, mcGVR, "kagent", "claude"))
	require.NotNil(t, fixture.get(t, secretGVR, "kagent", result.Credentials[0].Secret))
	before := len(fixture.dyn.Actions())
	status, answer, _ = operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, body, nil)
	require.Equal(t, http.StatusOK, status, answer)
	for _, action := range fixture.dyn.Actions()[before:] {
		require.NotContains(t, []string{"create", "update", "patch", "delete"}, action.GetVerb())
	}
	retry := prepareModelOperation(t, fixture, map[string]any{"namespace": "kagent", "provider": "GeminiVertexAI", "projectID": "vertex-test", "location": "europe-west1"})
	require.Equal(t, result.CredentialOperation, retry.CredentialOperation)
	require.Equal(t, organizations.CredentialPresent, retry.Credentials[0].State)
}

func TestInstallationVertexCredentialRecoversAnUnrecordedSave(t *testing.T) {
	store := &refuseCredentialCompletion{ConfigMapInterface: operationMaps(t)}
	fixture, result, body := installationVertexOperation(t, func(configuration *organizations.Config) {
		configuration.CredentialOperations = organizations.NewCredentialOperations(store)
	})
	confirmation := confirmOperation(t, fixture, result.CredentialOperation)
	status, _, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, body, nil)
	require.GreaterOrEqual(t, status, 400)
	require.True(t, store.refused)
	require.NotNil(t, fixture.get(t, secretGVR, "kagent", result.Credentials[0].Secret))
	confirmation = confirmOperation(t, fixture, result.CredentialOperation)
	require.Equal(t, "saving", confirmation.State)
	fixture.dyn.ClearActions()
	status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, body, nil)
	require.Equal(t, http.StatusOK, status, answer)
	for _, action := range fixture.dyn.Actions() {
		if action.GetResource().Resource == "secrets" {
			require.NotContains(t, []string{"create", "update", "patch", "delete"}, action.GetVerb())
		}
	}
	confirmation = confirmOperation(t, fixture, result.CredentialOperation)
	require.Equal(t, "complete", confirmation.State)
	require.NotNil(t, fixture.get(t, mcGVR, "kagent", "claude"))
}
