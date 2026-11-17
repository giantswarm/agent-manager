package agents

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/giantswarm/gitops-commit/commit"
	"github.com/stretchr/testify/require"
)

const gitopsUpdate = "agent-manager/update-kagent-gitops"

// changedFiles are the paths whose content on branch differs from main.
func changedFiles(fake *commit.Fake, branch string) []string {
	base, head := fake.Files(fleetRepo, "main"), fake.Files(fleetRepo, branch)
	var out []string
	for p, content := range head {
		if string(base[p]) != string(content) {
			out = append(out, p)
		}
	}
	for p := range base {
		if _, ok := head[p]; !ok {
			out = append(out, p)
		}
	}
	return out
}

// earlierPullRequest opens a pull request of the gitops agent's update on
// branch, with a file the next write does not touch.
func earlierPullRequest(t *testing.T, fake *commit.Fake, branch string) commit.PullRequest {
	t.Helper()
	require.NoError(t, fake.CreateBranch(t.Context(), fleetRepo, branch, "main"))
	require.NoError(t, fake.Commit(t.Context(), fleetRepo, branch, "earlier", map[string][]byte{"agents/stray.yaml": []byte("stray")}))
	pr, err := fake.OpenPullRequest(t.Context(), fleetRepo, branch, "main", "earlier", "")
	require.NoError(t, err)
	return pr
}

func updateGitops(t *testing.T, f *fixture, ctx context.Context, description string) (*UpdateResult, error) {
	t.Helper()
	return f.svc.Update(ctx, Update{Name: "gitops", Description: str(description), WriteOptions: WriteOptions{Mode: ModeCommit}})
}

// After a pull request of the agent was closed or merged, a write cuts a
// head of its own from the base: the agent's branch name, an underscore and
// a suffix, and its pull request holds only the new write's files.
func TestAgentWriteAfterAClosedOrMergedPullRequestHasAHeadOfItsOwn(t *testing.T) {
	for _, end := range []string{"closed", "merged"} {
		for _, earlier := range []string{gitopsUpdate, Head(gitopsUpdate)} {
			t.Run(end+"/"+earlier, func(t *testing.T) {
				f, fake, ctx := gitOpsFixture(t, true)
				pr := earlierPullRequest(t, fake, earlier)
				if end == "closed" {
					require.NoError(t, fake.Close(ctx, pr, false))
				} else {
					require.NoError(t, fake.Merge(ctx, pr))
					// A later change of the base takes the earlier file out again.
					require.NoError(t, fake.Commit(ctx, fleetRepo, "main", "later", map[string][]byte{"agents/stray.yaml": nil}))
				}
				res, err := updateGitops(t, f, ctx, "reviews")
				require.NoError(t, err)
				require.True(t, strings.HasPrefix(res.Commit.Branch, gitopsUpdate+HeadSeparator), res.Commit.Branch)
				require.NotEqual(t, earlier, res.Commit.Branch)
				require.Equal(t, []string{"agents/agent-manager/gitops.yaml"}, changedFiles(fake, res.Commit.Branch))
				prs := fake.PullRequests()
				require.Len(t, prs, 2)
				require.Equal(t, res.Commit.Branch, prs[1].Head)
				require.Equal(t, prs[1].URL, res.Commit.PullRequest)
			})
		}
	}
}

// While a pull request of the agent is open, on a head of a write or on the
// name an older agent-manager used, a second write of it adds its commit to
// that pull request, as before every write had a head of its own.
func TestAgentWriteWhileAPullRequestIsOpenAddsToIt(t *testing.T) {
	for _, earlier := range []string{gitopsUpdate, Head(gitopsUpdate)} {
		t.Run(earlier, func(t *testing.T) {
			f, fake, ctx := gitOpsFixture(t, true)
			pr := earlierPullRequest(t, fake, earlier)
			res, err := updateGitops(t, f, ctx, "reviews")
			require.NoError(t, err)
			require.Equal(t, earlier, res.Commit.Branch)
			require.Equal(t, pr.URL, res.Commit.PullRequest)
			require.Len(t, fake.PullRequests(), 1)
			head := fake.Files(fleetRepo, earlier)
			require.Contains(t, string(head["agents/agent-manager/gitops.yaml"]), "description: reviews")
			require.Contains(t, head, "agents/stray.yaml", "the open pull request keeps its own commits")
		})
	}
}

// The prefix of one agent never matches the branch of another whose name
// starts with the same characters, nor the other way round.
func TestAgentPullRequestOfASimilarNameIsNotItsOwn(t *testing.T) {
	for _, other := range []string{
		"agent-manager/update-kagent-gitops-eu", Head("agent-manager/update-kagent-gitops-eu"),
		"agent-manager/update-kagent-gitop", Head("agent-manager/update-kagent-gitop"),
	} {
		t.Run(other, func(t *testing.T) {
			f, fake, ctx := gitOpsFixture(t, true)
			earlierPullRequest(t, fake, other)
			res, err := updateGitops(t, f, ctx, "reviews")
			require.NoError(t, err)
			require.True(t, strings.HasPrefix(res.Commit.Branch, gitopsUpdate+HeadSeparator), res.Commit.Branch)
			require.Len(t, fake.PullRequests(), 2)
			require.Equal(t, []string{"agents/agent-manager/gitops.yaml"}, changedFiles(fake, res.Commit.Branch))
		})
	}
}

// racingForge opens another pull request of the agent once, when this
// write cuts its own head: after its lookup found none open.
type racingForge struct {
	*commit.Fake
	object string
	other  commit.PullRequest
}

func (r *racingForge) CreateBranch(ctx context.Context, repo commit.Repository, branch, base string) error {
	if r.other.Number == 0 && strings.HasPrefix(branch, r.object+HeadSeparator) {
		h := Head(r.object)
		if err := r.Fake.CreateBranch(ctx, repo, h, base); err != nil {
			return err
		}
		if err := r.Commit(ctx, repo, h, "other", map[string][]byte{"agents/other.yaml": []byte("other")}); err != nil {
			return err
		}
		pr, err := r.OpenPullRequest(ctx, repo, h, base, "other", "")
		if err != nil {
			return err
		}
		r.other = pr
	}
	return r.Fake.CreateBranch(ctx, repo, branch, base)
}

// Two writes of the agent at almost the same time leave one open pull
// request: the write that opened the higher number closes its own.
func TestTwoAgentWritesAtTheSameTimeLeaveOnePullRequest(t *testing.T) {
	f, fake, ctx := gitOpsFixture(t, true)
	forge := &racingForge{Fake: fake, object: gitopsUpdate}
	f.svc.cfg.GitHub = func(string) (GitHubRemote, error) { return forge, nil }
	_, err := updateGitops(t, f, ctx, "reviews")
	require.ErrorIs(t, err, ErrConflict)
	require.ErrorContains(t, err, forge.other.URL)
	require.ErrorContains(t, err, "call again to add this change to it")
	prs := fake.PullRequests()
	require.Len(t, prs, 2)
	require.False(t, prs[0].Closed, "the other write's pull request stays open")
	require.True(t, prs[1].Closed, "this write closed its own")
}

// closingForge closes the open pull request of a branch when this write
// brings that branch up to date: a reviewer closed it after the lookup.
type closingForge struct {
	*commit.Fake
	closing commit.PullRequest
}

func (r *closingForge) CreateBranch(ctx context.Context, repo commit.Repository, branch, base string) error {
	if branch == r.closing.Head {
		if err := r.Close(ctx, r.closing, false); err != nil {
			return err
		}
	}
	return r.Fake.CreateBranch(ctx, repo, branch, base)
}

// A pull request found open and closed before the write adds to it: the
// change lands in a new pull request on its branch, which can carry the
// closed one's files. Nothing proves this call opened it, so the write
// closes nothing and answers conflict naming both; once that pull request is
// closed, a call again cuts a head of its own.
func TestAgentWriteToAPullRequestClosedMeanwhileIsNamed(t *testing.T) {
	f, fake, ctx := gitOpsFixture(t, true)
	pr := earlierPullRequest(t, fake, Head(gitopsUpdate))
	forge := &closingForge{Fake: fake, closing: pr}
	f.svc.cfg.GitHub = func(string) (GitHubRemote, error) { return forge, nil }
	_, err := updateGitops(t, f, ctx, "reviews")
	require.ErrorIs(t, err, ErrConflict)
	prs := fake.PullRequests()
	require.Len(t, prs, 2)
	require.ErrorContains(t, err, prs[0].URL)
	require.ErrorContains(t, err, prs[1].URL)
	require.False(t, prs[1].Closed, "the write closes no pull request it cannot prove it opened")

	require.NoError(t, fake.Close(ctx, prs[1].PullRequest, false))
	res, err := updateGitops(t, f, ctx, "reviews")
	require.NoError(t, err)
	require.NotEqual(t, pr.Head, res.Commit.Branch)
	require.Equal(t, []string{"agents/agent-manager/gitops.yaml"}, changedFiles(fake, res.Commit.Branch))
}

// failingLookupForge fails every prefix lookup after the write opened its
// pull request.
type failingLookupForge struct{ *commit.Fake }

func (r *failingLookupForge) OpenPullRequest(ctx context.Context, repo commit.Repository, head, base, title, body string) (commit.PullRequest, error) {
	pr, err := r.Fake.OpenPullRequest(ctx, repo, head, base, title, body)
	r.Fail[commit.OpFindByPrefix] = errors.New("GitHub is down")
	return pr, err
}

// A failed check for a second pull request keeps the write's own: the
// write is done.
func TestAgentWriteKeepsItsPullRequestWhenTheSecondLookupFails(t *testing.T) {
	f, fake, ctx := gitOpsFixture(t, true)
	f.svc.cfg.GitHub = func(string) (GitHubRemote, error) { return &failingLookupForge{fake}, nil }
	res, err := updateGitops(t, f, ctx, "reviews")
	require.NoError(t, err)
	require.Len(t, fake.PullRequests(), 1)
	require.Equal(t, fake.PullRequests()[0].URL, res.Commit.PullRequest)
	require.False(t, fake.PullRequests()[0].Closed)
}
