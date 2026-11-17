package organizations

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/giantswarm/gitops-commit/commit"
	"github.com/giantswarm/gitops-commit/provenance"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/identity"
)

// racingRemote is the forge where another write of the same object opens
// its pull request once, at a step of this write: before this write cuts
// its branch (after its guard found none open), or after this write opened
// its pull request.
type racingRemote struct {
	*commit.Fake
	object string
	// beforeCut opens the other pull request in CreateBranch; otherwise
	// OpenPullRequest opens it after this write's.
	beforeCut bool
	done      bool
	other     commit.PullRequest
}

func (r *racingRemote) race(ctx context.Context, repo commit.Repository) error {
	if r.done {
		return nil
	}
	r.done = true
	h := agents.Head(r.object)
	if err := r.Fake.CreateBranch(ctx, repo, h, "main"); err != nil {
		return err
	}
	if err := r.Commit(ctx, repo, h, "the other write", map[string][]byte{"other.yaml": []byte("other")}); err != nil {
		return err
	}
	pr, err := r.Fake.OpenPullRequest(ctx, repo, h, "main", "the other write", "")
	r.other = pr
	return err
}

func (r *racingRemote) CreateBranch(ctx context.Context, repo commit.Repository, branch, base string) error {
	if r.beforeCut && strings.HasPrefix(branch, r.object+agents.HeadSeparator) {
		if err := r.race(ctx, repo); err != nil {
			return err
		}
	}
	return r.Fake.CreateBranch(ctx, repo, branch, base)
}

func (r *racingRemote) OpenPullRequest(ctx context.Context, repo commit.Repository, head, base, title, body string) (commit.PullRequest, error) {
	pr, err := r.Fake.OpenPullRequest(ctx, repo, head, base, title, body)
	if err == nil && !r.beforeCut && strings.HasPrefix(head, r.object+agents.HeadSeparator) {
		err = r.race(ctx, repo)
	}
	return pr, err
}

func openOf(fake *commit.Fake, object string) []commit.FakePullRequest {
	var out []commit.FakePullRequest
	for _, pr := range fake.PullRequests() {
		if !pr.Merged && !pr.Closed && strings.HasPrefix(pr.Head, object+agents.HeadSeparator) {
			out = append(out, pr)
		}
	}
	return out
}

// Two writes of the same object at almost the same time leave one open
// pull request of it: the one with the lower number. The write that opened
// the higher one closes its own and answers the guard's conflict; a pull
// request this write did not open is never closed. A failed close is named,
// with both pull requests.
func TestTwoWritesOfAnObjectAtTheSameTimeLeaveOnePullRequest(t *testing.T) {
	repository := commit.Repository{Owner: "acme", Name: "config"}
	object := branch("model-claude", "org-acme", "payments")
	setup := func(beforeCut bool) (*racingRemote, *orgCommit, []fileChange) {
		fake := commit.NewFake()
		fake.AddBranch(repository, "main", map[string][]byte{})
		remote := &racingRemote{Fake: fake, object: object, beforeCut: beforeCut}
		cfg := &CommitConfig{Objects: provenance.Location{Repository: repository, Branch: "main", Directory: "organizations"}, ContentPrefix: DefaultContentPrefix}
		c := &orgCommit{cfg: cfg, remote: remote, gh: &identity.GitHub{Login: "jane"}}
		changes := []fileChange{{dir: c.objectDir("org-acme"), write: map[string][]byte{c.objectFile("org-acme", "payments"): []byte("kind: AgentPlatformOrganization\n")}}}
		return remote, c, changes
	}

	t.Run("the other write opened its pull request first", func(t *testing.T) {
		remote, c, changes := setup(true)
		_, err := c.open(t.Context(), changes, object, "change", "body", false)
		require.ErrorIs(t, err, agents.ErrConflict)
		require.ErrorContains(t, err, "the pull request "+remote.other.URL+" for this object is open: merge or close it first")
		open := openOf(remote.Fake, object)
		require.Len(t, open, 1)
		require.Equal(t, remote.other.Number, open[0].Number, "the other write's pull request stays open")
		prs := remote.PullRequests()
		require.Len(t, prs, 2)
		require.True(t, prs[1].Closed, "this write closed its own pull request")
		require.ErrorContains(t, err, prs[1].URL)
	})

	t.Run("this write opened its pull request first", func(t *testing.T) {
		remote, c, changes := setup(false)
		res, err := c.open(t.Context(), changes, object, "change", "body", false)
		require.NoError(t, err)
		require.Less(t, res.Number, remote.other.Number)
		require.Len(t, openOf(remote.Fake, object), 2, "a write never closes a pull request it did not open; the other write closes its own")
	})

	t.Run("the close fails", func(t *testing.T) {
		remote, c, changes := setup(true)
		remote.Fail[commit.OpClose] = errors.New("GitHub is down")
		_, err := c.open(t.Context(), changes, object, "change", "body", false)
		require.ErrorIs(t, err, agents.ErrConflict)
		prs := remote.PullRequests()
		require.Len(t, prs, 2)
		require.ErrorContains(t, err, remote.other.URL)
		require.ErrorContains(t, err, prs[1].URL)
		require.ErrorContains(t, err, "GitHub is down")
		require.ErrorContains(t, err, "close "+prs[1].URL+" yourself")
	})
}
