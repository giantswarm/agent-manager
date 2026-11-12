package api

import (
	"context"
	"strings"
	"testing"

	"github.com/giantswarm/gitops-commit/commit"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agent-manager/internal/organizations"
)

// racingRemote is the fake forge where another writer commits file to the
// base branch once, when the write asks for an open pull request: after the
// object's file was checked and the change planned, before the branch is
// cut.
type racingRemote struct {
	*commit.Fake
	file    string
	content []byte
	done    bool
}

func (r *racingRemote) FindPullRequest(ctx context.Context, repo commit.Repository, head string) (commit.PullRequest, bool, error) {
	if !r.done && r.file != "" {
		r.done = true
		if err := r.Commit(ctx, repo, "main", "another writer adds the object", map[string][]byte{r.file: r.content}); err != nil {
			return commit.PullRequest{}, false, err
		}
	}
	return r.Fake.FindPullRequest(ctx, repo, head)
}

// commitRaceSubject is a set tool that creates an object of name, with the
// arguments of a new one, its file and the file another writer adds.
type commitRaceSubject struct {
	tool, secret, key, file, branch string
	arguments                       map[string]any
	other                           *unstructured.Unstructured
}

func commitRaceSubjects() []commitRaceSubject {
	subjects := []commitRaceSubject{{ // #nosec G101 -- a Secret's name, no credential
		tool: ToolSetOrganizationModel, secret: "tenant-credential-claude", key: "apiKey", file: paymentsPath + "/modelconfig-claude.yaml", branch: "agent-manager/model-claude-org-acme-payments",
		arguments: map[string]any{"namespace": orgNS, "name": "claude", "provider": "OpenAI", "model": "gpt-5"},
		other:     model("claude", true),
	}}
	branches := map[string]string{ToolSetOrganizationConnector: "agent-manager/connector-gh-org-acme-payments", ToolSetOrganizationEgress: "agent-manager/egress-github-org-acme-payments"}
	for _, s := range createOnlySubjects() {
		subjects = append(subjects, commitRaceSubject{tool: s.tool, secret: s.secret, key: "token", file: s.file, branch: branches[s.tool], arguments: s.arguments, other: s.existing()})
	}
	return subjects
}

// A commit-mode create whose file another writer adds to git after the
// check and before the branch is cut is refused on the branch: with create
// already_exists, without it conflict; the other writer's file is left as
// it is and no pull request is opened. Without the race the create opens
// its pull request.
func TestCommitModeCreateRefusesAFileAddedBeforeTheBranch(t *testing.T) {
	for _, subject := range commitRaceSubjects() {
		for _, scenario := range []struct {
			name   string
			create bool
			race   bool
			prefix string
		}{
			{"create, file added", true, true, "already_exists:"},
			{"no create, file added", false, true, "conflict:"},
			{"create, no race", true, false, ""},
		} {
			t.Run(subject.tool+"/"+scenario.name, func(t *testing.T) {
				otherFile := gitManifest(t, subject.other)
				remote := &racingRemote{}
				if scenario.race {
					remote.file, remote.content = subject.file, otherFile
				}
				fixture, forge := commitFixtureWith(t, nil, func(fake *commit.Fake) organizations.GitHubRemote {
					remote.Fake = fake
					return remote
				})
				seedSecret(t, fixture, subject.secret, subject.key)
				arguments := map[string]any{"mode": "commit", "create": scenario.create}
				for key, value := range subject.arguments {
					arguments[key] = value
				}
				text, failed := fixture.call(t, subject.tool, arguments)
				if !scenario.race {
					require.False(t, failed, text)
					require.Len(t, forge.PullRequests(), 1)
					return
				}
				require.True(t, failed, text)
				require.True(t, strings.HasPrefix(text, scenario.prefix), text)
				require.Empty(t, forge.PullRequests())
				require.Equal(t, otherFile, forge.Files(mcRepo, "main")[subject.file])
				// The branch is cut and left without a commit of the write.
				require.Equal(t, forge.Files(mcRepo, "main"), forge.Files(mcRepo, subject.branch))
			})
		}
	}
}
