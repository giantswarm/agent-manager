package skills

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fakeGitHub(t *testing.T) (*httptest.Server, *int) {
	hits := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/giantswarm/agent-skills", func(w http.ResponseWriter, r *http.Request) {
		hits++
		assert.Equal(t, "Bearer secret", r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(map[string]any{"default_branch": "main"})
	})
	mux.HandleFunc("/repos/giantswarm/agent-skills/git/trees/main", func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_ = json.NewEncoder(w).Encode(map[string]any{"truncated": false, "tree": []map[string]any{
			{"path": "README.md", "type": "blob"},
			{"path": "agent-self-awareness/SKILL.md", "type": "blob"},
			{"path": "nested/runbooks/SKILL.md", "type": "blob"},
			{"path": "noname/SKILL.md", "type": "blob"},
			{"path": "agent-self-awareness", "type": "tree"},
		}})
	})
	mux.HandleFunc("/repos/giantswarm/agent-skills/contents/", func(w http.ResponseWriter, r *http.Request) {
		hits++
		assert.Equal(t, "application/vnd.github.raw+json", r.Header.Get("Accept"))
		assert.Equal(t, "main", r.URL.Query().Get("ref"))
		switch strings.TrimPrefix(r.URL.Path, "/repos/giantswarm/agent-skills/contents/") {
		case "agent-self-awareness/SKILL.md":
			_, _ = w.Write([]byte("---\nname: self-awareness\ndescription: Knows what it is.\n---\n# body\n"))
		case "nested/runbooks/SKILL.md":
			_, _ = w.Write([]byte("---\nname: runbooks\ndescription: |\n  Operates things.\n---\n"))
		case "noname/SKILL.md":
			_, _ = w.Write([]byte("# no frontmatter at all\n"))
		default:
			http.NotFound(w, r)
		}
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, &hits
}

func TestDiscoverMirrorsThePortalSemantics(t *testing.T) {
	ts, hits := fakeGitHub(t)
	d := New(Config{Repositories: []string{"https://github.com/giantswarm/agent-skills.git"}, APIURL: ts.URL, Token: "secret", CacheTTL: time.Minute}, nil)

	res, err := d.List(context.Background(), "", "", false)
	require.NoError(t, err)
	require.Len(t, res.Repositories, 1)
	repo := res.Repositories[0]
	assert.Equal(t, "https://github.com/giantswarm/agent-skills", repo.RepoURL, "the URL is canonicalized")
	assert.Equal(t, "main", repo.Ref)
	assert.False(t, repo.Truncated)
	assert.Empty(t, repo.Error)
	require.Len(t, res.Skills, 3)
	assert.Equal(t, []Skill{
		{Name: "self-awareness", Description: "Knows what it is.", RepoURL: repo.RepoURL, Path: "agent-self-awareness", Ref: "main"},
		{Name: "runbooks", Description: "Operates things.", RepoURL: repo.RepoURL, Path: "nested/runbooks", Ref: "main"},
		{Name: "noname", Description: "", RepoURL: repo.RepoURL, Path: "noname", Ref: "main"},
	}, res.Skills, "sorted by path; the directory names a skill without frontmatter")

	gitRef := res.Skills[0].GitRef()
	assert.Equal(t, map[string]string{"url": repo.RepoURL, "path": "agent-self-awareness", "ref": "main", "name": "agent-self-awareness"}, gitRef)

	before := *hits
	_, err = d.List(context.Background(), "", "", false)
	require.NoError(t, err)
	assert.Equal(t, before, *hits, "the second list is served from the cache")
	_, err = d.List(context.Background(), "", "", true)
	require.NoError(t, err)
	assert.Greater(t, *hits, before, "refresh bypasses the cache")
}

func TestListRejectsNonGitHubRepositoriesAndReportsUnreadableOnes(t *testing.T) {
	ts, _ := fakeGitHub(t)
	d := New(Config{APIURL: ts.URL, Token: "secret"}, nil)
	_, err := d.List(context.Background(), "https://gitlab.com/x/y", "", false)
	assert.Error(t, err)

	res, err := d.List(context.Background(), "https://github.com/giantswarm/does-not-exist", "", false)
	require.NoError(t, err)
	require.Len(t, res.Repositories, 1)
	assert.NotEmpty(t, res.Repositories[0].Error)
	assert.Empty(t, res.Skills)
}

func TestParseFrontmatter(t *testing.T) {
	assert.Equal(t, map[string]string{"name": "a", "description": "b c"}, parseFrontmatter("---\nname: a\ndescription: b c\nlist: [1]\n---\nrest"))
	assert.Empty(t, parseFrontmatter("no frontmatter"))
	assert.Empty(t, parseFrontmatter("---\nname: unterminated\n"))
}
