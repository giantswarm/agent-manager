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

	"github.com/giantswarm/agent-manager/internal/oci"
	"github.com/giantswarm/agent-manager/internal/oci/ocitest"
)

// The commits the fake's refs resolve to.
const (
	headCommit    = "0123456789abcdef0123456789abcdef01234567"
	featureCommit = "89abcdef0123456789abcdef0123456789abcdef"
	tagCommit     = "fedcba9876543210fedcba9876543210fedcba98"
)

func fakeGitHub(t *testing.T) (*httptest.Server, *int) {
	hits := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/giantswarm/agent-skills", func(w http.ResponseWriter, r *http.Request) {
		hits++
		assert.Equal(t, "Bearer secret", r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(map[string]any{"default_branch": "main"})
	})
	mux.HandleFunc("/repos/giantswarm/agent-skills/commits/", func(w http.ResponseWriter, r *http.Request) {
		hits++
		switch strings.TrimPrefix(r.URL.Path, "/repos/giantswarm/agent-skills/commits/") {
		case "main":
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": headCommit})
		case "feature":
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": featureCommit})
		case "v1.2.0":
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": tagCommit})
		default:
			http.Error(w, `{"message":"No commit found for SHA: nope"}`, http.StatusUnprocessableEntity)
		}
	})
	mux.HandleFunc("/repos/giantswarm/agent-skills/git/trees/"+headCommit, func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_ = json.NewEncoder(w).Encode(map[string]any{"truncated": false, "tree": []map[string]any{
			{"path": "README.md", "type": "blob"},
			{"path": "agent-self-awareness/SKILL.md", "type": "blob"},
			{"path": "nested/runbooks/SKILL.md", "type": "blob"},
			{"path": "noname/SKILL.md", "type": "blob"},
			{"path": "agent-self-awareness", "type": "tree"},
		}})
	})
	mux.HandleFunc("/repos/giantswarm/agent-skills/git/trees/"+featureCommit, func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_ = json.NewEncoder(w).Encode(map[string]any{"truncated": false, "tree": []map[string]any{{"path": "README.md", "type": "blob"}}})
	})
	mux.HandleFunc("/repos/giantswarm/agent-skills/contents/", func(w http.ResponseWriter, r *http.Request) {
		hits++
		assert.Equal(t, "application/vnd.github.raw+json", r.Header.Get("Accept"))
		assert.Equal(t, headCommit, r.URL.Query().Get("ref"), "files are read at the resolved commit, not the moving branch")
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
	// A private repository looks like a missing one to an anonymous caller.
	mux.HandleFunc("/repos/giantswarm/private-skills", func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Header.Get("Authorization") == "" {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"default_branch": "trunk"})
	})
	mux.HandleFunc("/repos/giantswarm/private-skills/commits/trunk", func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_ = json.NewEncoder(w).Encode(map[string]any{"sha": tagCommit})
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
	assert.Equal(t, headCommit, repo.Commit, "the branch is resolved to the commit an agent pins")
	assert.False(t, repo.Truncated)
	assert.Empty(t, repo.Error)
	require.Len(t, res.Skills, 3)
	assert.Equal(t, []Skill{
		{Name: "self-awareness", Description: "Knows what it is.", RepoURL: repo.RepoURL, Path: "agent-self-awareness", Ref: "main", Commit: headCommit},
		{Name: "runbooks", Description: "Operates things.", RepoURL: repo.RepoURL, Path: "nested/runbooks", Ref: "main", Commit: headCommit},
		{Name: "noname", Description: "", RepoURL: repo.RepoURL, Path: "noname", Ref: "main", Commit: headCommit},
	}, res.Skills, "sorted by path; the directory names a skill without frontmatter")

	assert.Equal(t, map[string]any{"name": "agent-self-awareness", "path": "agent-self-awareness", "git": map[string]any{"url": repo.RepoURL, "commit": headCommit}}, res.Skills[0].Entry(), "the entry pins the commit, ready for create_agent")

	before := *hits
	_, err = d.List(context.Background(), "", "", false)
	require.NoError(t, err)
	assert.Equal(t, before, *hits, "the second list is served from the cache")
	_, err = d.List(context.Background(), "", "", true)
	require.NoError(t, err)
	assert.Greater(t, *hits, before, "refresh bypasses the cache")

	// An explicit ref is read at that ref's head.
	res, err = d.List(context.Background(), "", "feature", false)
	require.NoError(t, err)
	assert.Equal(t, featureCommit, res.Repositories[0].Commit)
	assert.Equal(t, "feature", res.Repositories[0].Ref)
	assert.Empty(t, res.Skills, "the feature branch carries no skill")
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

func TestResolverPinsGitRefs(t *testing.T) {
	ts, _ := fakeGitHub(t)
	ctx := context.Background()
	r := NewResolver(ts.URL, "secret", nil, nil)
	repo := "https://github.com/giantswarm/agent-skills"

	for ref, want := range map[string]string{"": headCommit, "main": headCommit, "feature": featureCommit, "v1.2.0": tagCommit, tagCommit: tagCommit} {
		got, err := r.GitHead(ctx, repo, ref)
		require.NoError(t, err, ref)
		assert.Equal(t, want, got, "ref %q", ref)
	}
	_, err := r.GitHead(ctx, repo, "nope")
	require.ErrorIs(t, err, ErrUnresolvable)
	assert.Contains(t, err.Error(), repo)
	_, err = r.GitHead(ctx, "https://gitlab.com/x/y", "main")
	assert.ErrorIs(t, err, ErrUnresolvable)

	// A private repository without a token: an error naming the repository
	// and the missing credential, never a crash; with a token it resolves.
	anonymous := NewResolver(ts.URL, "", nil, nil)
	_, err = anonymous.GitHead(ctx, "https://github.com/giantswarm/private-skills", "")
	require.ErrorIs(t, err, ErrUnresolvable)
	assert.Contains(t, err.Error(), "https://github.com/giantswarm/private-skills")
	assert.Contains(t, err.Error(), "GITHUB_TOKEN")
	got, err := r.GitHead(ctx, "https://github.com/giantswarm/private-skills", "")
	require.NoError(t, err)
	assert.Equal(t, tagCommit, got)
}

func TestResolverPinsOCITags(t *testing.T) {
	f := ocitest.New(t, "giantswarm/skills/kubectl")
	f.Tags = []string{"1.4.0", "latest"}
	ctx := context.Background()
	r := NewResolver("", "", nil, oci.NewRegistry(nil))
	insecure := func(ref string) string { return f.Host() + "/" + f.Repo + ref }

	// The fake speaks plain HTTP; ParseImageReference yields a secure
	// reference, so resolve through an insecure one the same way.
	pinned, err := r.resolveInsecure(ctx, insecure(":1.4.0"))
	require.NoError(t, err)
	assert.Equal(t, insecure("@"+ocitest.Digest("1.4.0")), pinned)
	pinned, err = r.resolveInsecure(ctx, insecure(""))
	require.NoError(t, err)
	assert.Equal(t, insecure("@"+ocitest.Digest("latest")), pinned, "no tag means latest")
	_, err = r.resolveInsecure(ctx, insecure(":missing"))
	require.ErrorIs(t, err, ErrUnresolvable)

	digest := "sha256:5b0bcabd1ed22e9fb1310cf6c2dec7cdef19f0ad69efa1f392e94a4333501270"
	pinned, err = r.OCIDigest(ctx, "ghcr.io/giantswarm/skills/kubectl:1.4.0@"+digest)
	require.NoError(t, err)
	assert.Equal(t, "ghcr.io/giantswarm/skills/kubectl@"+digest, pinned, "a digest needs no registry call")
	_, err = r.OCIDigest(ctx, "kubectl:1.4.0")
	assert.ErrorIs(t, err, ErrUnresolvable, "the registry host is required")
}

// resolveInsecure is OCIDigest against a plain-HTTP registry (tests only).
func (r *Resolver) resolveInsecure(ctx context.Context, ref string) (string, error) {
	img, err := oci.ParseImageReference(ref)
	if err != nil {
		return "", err
	}
	img.Insecure = true
	tag := img.Tag
	if tag == "" {
		tag = "latest"
	}
	digest, err := r.registry.ManifestDigest(ctx, img.Reference, tag)
	if err != nil {
		return "", ErrUnresolvable
	}
	img.Digest = digest
	return img.Pinned(), nil
}

func TestParseFrontmatter(t *testing.T) {
	assert.Equal(t, map[string]string{"name": "a", "description": "b c"}, parseFrontmatter("---\nname: a\ndescription: b c\nlist: [1]\n---\nrest"))
	assert.Empty(t, parseFrontmatter("no frontmatter"))
	assert.Empty(t, parseFrontmatter("---\nname: unterminated\n"))
}
