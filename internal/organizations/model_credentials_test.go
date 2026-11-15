package organizations

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestInstallationCredentialDirectory(t *testing.T) {
	for _, directory := range []string{"", ".", "..", "../models", "/models", "models/../other", "agent-platform/organizations", "agent-platform/organizations/team", "agent-platform"} {
		t.Run(directory, func(t *testing.T) {
			writer := &orgCommit{cfg: &CommitConfig{PlatformModelsPath: directory, ContentPrefix: "agent-platform/organizations"}}
			_, err := writer.credentialDirectory(Namespace{Name: "kagent", Scope: "platform"})
			require.Error(t, err)
		})
	}
	writer := &orgCommit{cfg: &CommitConfig{PlatformModelsPath: "agent-platform/models", ContentPrefix: "agent-platform/organizations"}}
	directory, err := writer.credentialDirectory(Namespace{Name: "kagent", Scope: "platform"})
	require.NoError(t, err)
	require.Equal(t, "agent-platform/models", directory.Path())
}

func TestVertexLocationAndProject(t *testing.T) {
	configure := func(location, project string) error {
		model := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"provider": "GeminiVertexAI"}}}
		return configureModel(model, ModelRequest{Location: location, ProjectID: project})
	}
	for _, location := range []string{"global", "us", "eu", "europe-west1", "us-east5"} {
		require.NoError(t, configure(location, "vertex-test"), location)
	}
	for _, location := range []string{"", "EU", "attacker.example/x?", "europe-west", "us-east5.", "eu "} {
		require.Error(t, configure(location, "vertex-test"), location)
	}
	for _, project := range []string{"", "short", "1project", "project-", "Project-id", "project/id", "a-project-id-that-is-far-too-long-x"} {
		require.Error(t, configure("eu", project), project)
	}
}

func TestGoogleCredentialSizeLimit(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	// #nosec G101 -- the private key is generated for this test, not a stored credential.
	document, err := json.Marshal(map[string]any{"type": "service_account", "project_id": "vertex-test", "client_email": "agent@vertex-test.iam.gserviceaccount.com", "private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}))})
	require.NoError(t, err)
	padded := func(size int) string {
		return string(document[:len(document)-1]) + strings.Repeat(" ", size-len(document)) + "}"
	}
	require.NoError(t, validateGoogleCredential(padded(64*1024), "vertex-test"))
	require.Error(t, validateGoogleCredential(padded(64*1024+1), "vertex-test"))
}
