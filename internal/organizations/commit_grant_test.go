package organizations

import (
	"net/http"
	"testing"

	"github.com/giantswarm/gitops-commit/commit"
	"github.com/giantswarm/gitops-commit/provenance"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/identity"
)

func TestOrganizationCommitGitHubRefusals(t *testing.T) {
	for _, operation := range []string{commit.OpCreateBranch, commit.OpReadFile, commit.OpCommit, commit.OpOpenPullRequest} {
		for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
			t.Run(operation+"/"+http.StatusText(status), func(t *testing.T) {
				repository := commit.Repository{Owner: "acme", Name: "config"}
				remote := commit.NewFake()
				remote.AddBranch(repository, "main", map[string][]byte{})
				remote.Fail = map[string]error{operation: &commit.AuthError{Op: operation, Status: status}}
				location := provenance.Location{Repository: repository, Branch: "main"}
				writer := &orgCommit{
					cfg:    &CommitConfig{Objects: location},
					remote: remote,
					gh:     &identity.GitHub{App: "agent-manager"},
				}
				_, err := writer.land(t.Context(), []commit.Change{{Location: location, Files: map[string][]byte{"organization.yaml": []byte("kind: AgentPlatformOrganization\n")}}}, []string{"organization.yaml"}, "change", "Change organization", "Change organization")
				if status == http.StatusUnauthorized {
					var required *agents.GrantRequiredError
					require.ErrorAs(t, err, &required)
					require.Equal(t, "agent-manager", required.App)
				} else {
					require.ErrorIs(t, err, agents.ErrForbidden)
				}
				require.Empty(t, remote.PullRequests())
			})
		}
	}
}
