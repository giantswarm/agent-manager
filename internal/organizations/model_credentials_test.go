package organizations

import (
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
