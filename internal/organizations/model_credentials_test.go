package organizations

import (
	"testing"

	"github.com/stretchr/testify/require"
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
