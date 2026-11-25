package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/giantswarm/gitops-commit/commit"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	// cut are the branches the write cut.
	cut []string
}

func (r *racingRemote) FindPullRequestsByPrefix(ctx context.Context, repo commit.Repository, prefix string) ([]commit.PullRequest, error) {
	if !r.done && r.file != "" {
		r.done = true
		if err := r.Commit(ctx, repo, "main", "another writer adds the object", map[string][]byte{r.file: r.content}); err != nil {
			return nil, err
		}
	}
	return r.Fake.FindPullRequestsByPrefix(ctx, repo, prefix)
}

func (r *racingRemote) CreateBranch(ctx context.Context, repo commit.Repository, branch, base string) error {
	r.cut = append(r.cut, branch)
	return r.Fake.CreateBranch(ctx, repo, branch, base)
}

// commitRaceSubject is a set tool that creates an object of name, with the
// arguments of a new one, its file, the file another writer adds and the
// object's branch name, which starts each pull request's head branch.
type commitRaceSubject struct {
	tool, secret, key, file, branch string
	arguments                       map[string]any
	other                           *unstructured.Unstructured
}

func commitRaceSubjects() []commitRaceSubject {
	subjects := []commitRaceSubject{{ // #nosec G101 -- a Secret's name, no credential
		tool: ToolSetOrganizationModel, secret: "tenant-credential-claude", key: "apiKey", file: paymentsPath + "/modelconfig-claude.yaml", branch: "agent-manager/model-claude-payments",
		arguments: map[string]any{"organization": "payments", "name": "claude", "provider": "OpenAI", "model": "gpt-5"},
		other:     model("claude", true),
	}}
	branches := map[string]string{ToolSetOrganizationConnector: "agent-manager/connector-gh-payments", ToolSetOrganizationEgress: "agent-manager/egress-github-payments"}
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
				// The write's own branch is cut and left without a commit of
				// the write.
				require.Len(t, remote.cut, 1)
				require.True(t, strings.HasPrefix(remote.cut[0], subject.branch+"_"), remote.cut[0])
				require.Equal(t, forge.Files(mcRepo, "main"), forge.Files(mcRepo, remote.cut[0]))
			})
		}
	}
}

// A write of an object after its earlier pull request ended, closed without
// a merge or merged, opens a pull request on a new branch of its own, cut
// from the base: a commit that came onto the earlier branch after it ended
// never rides along, and the new pull request changes only the files the
// write reports.
func TestCommitModeWriteAfterAnEndedPullRequestUsesANewBranch(t *testing.T) {
	const stray = paymentsPath + "/stray.yaml"
	for _, subject := range commitRaceSubjects() {
		for _, merged := range []bool{false, true} {
			name := subject.tool + "/closed"
			if merged {
				if subject.tool != ToolSetOrganizationModel {
					// A second write after a merge changes the object; the
					// model's arguments carry a field to change.
					continue
				}
				name = subject.tool + "/merged"
			}
			t.Run(name, func(t *testing.T) {
				fixture, forge := commitFixture(t, nil)
				seedSecret(t, fixture, subject.secret, subject.key)
				arguments := map[string]any{"mode": "commit", "create": true}
				for key, value := range subject.arguments {
					arguments[key] = value
				}
				text, failed := fixture.call(t, subject.tool, arguments)
				require.False(t, failed, text)
				require.Len(t, forge.PullRequests(), 1)
				first := forge.PullRequests()[0]
				require.True(t, strings.HasPrefix(first.Head, subject.branch+"_"), first.Head)
				if merged {
					pr := first.PullRequest
					sha, err := forge.Head(t.Context(), mcRepo, first.Head)
					require.NoError(t, err)
					pr.HeadSHA = sha
					require.NoError(t, forge.Merge(t.Context(), pr))
					// Flux applies the merged file; the next write changes
					// the object.
					live := subject.other.DeepCopy()
					live.SetLabels(fromGitLabels("payments"))
					_, err = fixture.dyn.Resource(mcGVR).Namespace(orgNS).Create(t.Context(), live, metav1.CreateOptions{})
					require.NoError(t, err)
					arguments["create"], arguments["model"] = false, "gpt-5-mini"
				} else {
					require.NoError(t, forge.Close(t.Context(), first.PullRequest, false))
				}
				// A commit lands on the ended pull request's branch, which
				// GitHub keeps.
				require.NoError(t, forge.Commit(t.Context(), mcRepo, first.Head, "a late commit", map[string][]byte{stray: []byte("kind: ConfigMap\n")}))

				text, failed = fixture.call(t, subject.tool, arguments)
				require.False(t, failed, text)
				require.Len(t, forge.PullRequests(), 2)
				second := forge.PullRequests()[1]
				require.NotEqual(t, first.Head, second.Head)
				require.True(t, strings.HasPrefix(second.Head, subject.branch+"_"), second.Head)
				files := changed(forge, second.Head)
				require.NotContains(t, files, stray)
				var result struct {
					Commit struct {
						Branch string `json:"branch"`
						Files  []struct {
							Path string `json:"path"`
						} `json:"files"`
					} `json:"commit"`
				}
				require.NoError(t, json.Unmarshal([]byte(text), &result), text)
				require.Equal(t, second.Head, result.Commit.Branch)
				reported := map[string]bool{}
				for _, f := range result.Commit.Files {
					reported[f.Path] = true
				}
				for p := range files {
					require.True(t, reported[p], "the pull request changes %s, which the answer does not list", p)
				}
			})
		}
	}
}

// An organization whose object file another writer adds to git after the
// check and before the branch is cut is refused, with no pull request.
func TestCommitModeCreateOrganizationRefusesAFileAddedBeforeTheBranch(t *testing.T) {
	other := []byte("apiVersion: security.giantswarm.io/v1alpha1\nkind: Organization\nmetadata:\n  name: payments\n")
	remote := &racingRemote{file: paymentsFile, content: other}
	fixture, forge := commitFixtureWith(t, nil, func(fake *commit.Fake) organizations.GitHubRemote {
		remote.Fake = fake
		return remote
	})
	fixture.dropOrganization(t, "payments")
	text, failed := fixture.call(t, ToolCreateOrganization, map[string]any{"name": "payments", "adminGroups": []any{"payments-leads"}, "mode": "commit"})
	require.True(t, failed, text)
	require.True(t, strings.HasPrefix(text, "conflict:"), text)
	require.Contains(t, text, "already and not applied yet")
	require.Empty(t, forge.PullRequests())
	require.Equal(t, other, forge.Files(mcRepo, "main")[paymentsFile])
}

// A write of an object is refused while a pull request of the object is
// open: one of an earlier write on a head of its own, and one an older
// agent-manager opened on the object's branch name without a suffix. No
// second pull request opens.
func TestCommitModeWriteIsRefusedWhileAPullRequestOfTheObjectIsOpen(t *testing.T) {
	for _, subject := range commitRaceSubjects() {
		for _, older := range []bool{false, true} {
			name := subject.tool + "/earlier write"
			if older {
				name = subject.tool + "/older agent-manager"
			}
			t.Run(name, func(t *testing.T) {
				fixture, forge := commitFixture(t, nil)
				seedSecret(t, fixture, subject.secret, subject.key)
				arguments := map[string]any{"mode": "commit", "create": true}
				for key, value := range subject.arguments {
					arguments[key] = value
				}
				if older {
					require.NoError(t, forge.CreateBranch(t.Context(), mcRepo, subject.branch, "main"))
					require.NoError(t, forge.Commit(t.Context(), mcRepo, subject.branch, "an older write", map[string][]byte{subject.file: gitManifest(t, subject.other)}))
					_, err := forge.OpenPullRequest(t.Context(), mcRepo, subject.branch, "main", "an older write", "")
					require.NoError(t, err)
				} else {
					text, failed := fixture.call(t, subject.tool, arguments)
					require.False(t, failed, text)
				}
				require.Len(t, forge.PullRequests(), 1)
				open := forge.PullRequests()[0]

				text, failed := fixture.call(t, subject.tool, arguments)
				require.True(t, failed, text)
				require.True(t, strings.HasPrefix(text, "conflict:"), text)
				require.Contains(t, text, "the pull request "+open.URL+" for this object is open: merge or close it first")
				require.Len(t, forge.PullRequests(), 1)
			})
		}
	}
}
