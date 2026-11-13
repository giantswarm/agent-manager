package api

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

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
		configuration.PlatformCredentialUIURL = "https://portal.example.com/model-credentials/{namespace}/{operation}"
	})
	result := prepareModelOperation(t, fixture, map[string]any{"namespace": "kagent"})
	confirmation := confirmOperation(t, fixture, result.CredentialOperation)
	require.Equal(t, "platform", confirmation.Organization.Scope)
	require.Empty(t, confirmation.Organization.Organization)
	require.Contains(t, result.Credentials[0].FormURL, "/kagent/")
	fixture.deny = func(namespace, verb, resource, name string) bool { return resource == "*" }
	status, _, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, "installation-test-key"), nil)
	require.Equal(t, http.StatusForbidden, status)
	require.Nil(t, fixture.get(t, secretGVR, "kagent", result.Credentials[0].Secret))
	_, failed := fixture.call(t, ToolSetOrganizationModel, map[string]any{"namespace": "kagent", "name": "another", "provider": "OpenAI", "model": "test"})
	require.True(t, failed)
}

func TestInstallationModelCredentialSave(t *testing.T) {
	fixture := newOrgFixtureWith(t, func(configuration *organizations.Config) {
		configuration.PlatformCredentialUIURL = "https://portal.example.com/model-credentials/{namespace}/{operation}"
	})
	result := prepareModelOperation(t, fixture, map[string]any{"namespace": "kagent", "provider": "GeminiVertexAI", "projectID": "vertex-test", "location": "global"})
	confirmation := confirmOperation(t, fixture, result.CredentialOperation)
	encoded, err := json.Marshal(vertexCredential(t))
	require.NoError(t, err)
	status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, string(encoded)), nil)
	require.Equal(t, http.StatusOK, status, answer)
	require.NotNil(t, fixture.get(t, mcGVR, "kagent", "claude"))
	text, failed := fixture.call(t, ToolReplaceOrganizationCredential, map[string]any{"namespace": "kagent", "secret": result.Credentials[0].Secret, "key": result.Credentials[0].Key})
	require.False(t, failed, text)
	var replacement organizations.ResourceResult
	require.NoError(t, json.Unmarshal([]byte(text), &replacement))
	require.Equal(t, "google-service-account-json", replacement.Credentials[0].InputFormat)
}
