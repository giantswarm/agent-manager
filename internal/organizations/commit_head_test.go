package organizations

import (
	"strings"
	"testing"

	"github.com/giantswarm/gitops-commit/commit"
	"github.com/giantswarm/gitops-commit/provenance"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/agent-manager/internal/agents"
)

// Each write gets a head branch of its own: the object's branch name, the
// separator and a suffix, valid for git and new on every call.
func TestHeadIsTheObjectNameAndASuffixOfItsOwn(t *testing.T) {
	object := branch("model-claude", "payments")
	first, second := head(object), head(object)
	require.NotEqual(t, first, second)
	for _, h := range []string{first, second} {
		suffix, ok := strings.CutPrefix(h, object+agents.HeadSeparator)
		require.True(t, ok, h)
		require.Len(t, suffix, 12)
		require.Equal(t, -1, strings.IndexFunc(suffix, func(r rune) bool { return !strings.ContainsRune("0123456789abcdef", r) }), suffix)
	}
}

// The guard finds an open pull request of the object by its branch name's
// prefix, and by the name itself, the head an older agent-manager opened.
// The prefix of one object never matches a head of another whose name
// starts with the same characters, and a closed pull request is not open.
func TestOpenPullRequestOfAnObject(t *testing.T) {
	repository := commit.Repository{Owner: "acme", Name: "config"}
	for _, scenario := range []struct {
		name, object string
		heads        []string
		closed       bool
		want         string
	}{
		{name: "none", object: "agent-manager/model-a-org-acme-payments"},
		{name: "a head of a write", object: "agent-manager/model-a-org-acme-payments", heads: []string{"agent-manager/model-a-org-acme-payments_0123456789ab"}, want: "agent-manager/model-a-org-acme-payments_0123456789ab"},
		{name: "the name of an older agent-manager", object: "agent-manager/model-a-org-acme-payments", heads: []string{"agent-manager/model-a-org-acme-payments"}, want: "agent-manager/model-a-org-acme-payments"},
		{name: "closed", object: "agent-manager/model-a-org-acme-payments", heads: []string{"agent-manager/model-a-org-acme-payments_0123456789ab", "agent-manager/model-a-org-acme-payments"}, closed: true},
		{name: "model-a and model-ab", object: "agent-manager/model-a", heads: []string{"agent-manager/model-ab_0123456789ab", "agent-manager/model-ab"}},
		{name: "organization payments and payments-eu", object: "agent-manager/update-org-acme-payments", heads: []string{"agent-manager/update-org-acme-payments-eu_0123456789ab", "agent-manager/update-org-acme-payments-eu"}},
		{name: "model-ab and model-a", object: "agent-manager/model-ab", heads: []string{"agent-manager/model-a_0123456789ab", "agent-manager/model-a"}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			remote := commit.NewFake()
			remote.AddBranch(repository, "main", map[string][]byte{})
			for _, h := range scenario.heads {
				require.NoError(t, remote.CreateBranch(t.Context(), repository, h, "main"))
				require.NoError(t, remote.Commit(t.Context(), repository, h, "change", map[string][]byte{"f.yaml": []byte(h)}))
				pr, err := remote.OpenPullRequest(t.Context(), repository, h, "main", "change", "")
				require.NoError(t, err)
				if scenario.closed {
					require.NoError(t, remote.Close(t.Context(), pr, false))
				}
			}
			c := &orgCommit{cfg: &CommitConfig{Objects: provenance.Location{Repository: repository, Branch: "main"}}, remote: remote, gh: &agents.GitHubUserAccessToken{}}
			pr, has, err := c.openPullRequest(t.Context(), scenario.object)
			require.NoError(t, err)
			require.Equal(t, scenario.want != "", has)
			if has {
				open, found, err := remote.FindPullRequest(t.Context(), repository, scenario.want)
				require.NoError(t, err)
				require.True(t, found)
				require.Equal(t, open.URL, pr.URL)
			}
		})
	}
}
