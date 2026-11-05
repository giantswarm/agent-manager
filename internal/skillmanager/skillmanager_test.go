package skillmanager

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/agent-manager/internal/identity"
	"github.com/giantswarm/agent-manager/internal/skills"
)

const (
	repo   = "https://github.com/giantswarm/agent-platform"
	commit = "0123456789abcdef0123456789abcdef01234567"
	digest = "sha256:" + "ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12"
)

// fakeSkillManager serves skill-manager's REST API as the contract states
// it: POST /v1/resolve pins each item, GET /v1/skills answers the catalogue.
type fakeSkillManager struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   []map[string]any
	// fail answers every call with this status and body when set.
	fail     int
	failBody string
	// refs maps a git item's ref (with "@path" when it has a path) to its
	// commit; an unknown one is 404.
	refs map[string]string
}

func (f *fakeSkillManager) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.requests = append(f.requests, r)
		f.bodies = append(f.bodies, body)
		f.mu.Unlock()
		if f.fail != 0 {
			w.WriteHeader(f.fail)
			_, _ = w.Write([]byte(f.failBody))
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/resolve":
			f.resolve(w, body)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/skills":
			_, _ = w.Write([]byte(`{"repositories":[{"repoUrl":"` + repo + `","ref":"v1.2.0","commit":"` + commit + `","path":"skills","readOnly":true,"skills":[` +
				`{"name":"kubectl","description":"k","path":"skills/kubectl","tags":["kubernetes"]},` +
				`{"name":"pagerduty","description":"p","path":"skills/pagerduty","tags":["alerting"]}]}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeSkillManager) resolve(w http.ResponseWriter, body map[string]any) {
	items, _ := body["skills"].([]any)
	out := []any{}
	for i, raw := range items {
		item := raw.(map[string]any)
		if oci, ok := item["oci"].(map[string]any); ok {
			ref := oci["ref"].(string)
			out = append(out, map[string]any{"name": item["name"], "oci": map[string]any{"ref": ref + "@" + digest}})
			continue
		}
		git := item["git"].(map[string]any)
		key, _ := git["ref"].(string)
		if p, _ := item["path"].(string); p != "" {
			key += "@" + p
		}
		pin, ok := f.refs[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "ref " + key + " not found", "item": i})
			return
		}
		pinned := map[string]any{"name": item["name"], "git": map[string]any{"url": git["url"], "commit": pin}}
		if p, ok := item["path"]; ok {
			pinned["path"] = p
		}
		out = append(out, pinned)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"skills": out, "plugins": []any{}})
}

func (f *fakeSkillManager) last() (*http.Request, map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1], f.bodies[len(f.bodies)-1]
}

func newClient(t *testing.T, f *fakeSkillManager, token string) *Client {
	t.Helper()
	c, err := New(Config{URL: f.start(t).URL + "/", Token: token})
	require.NoError(t, err)
	return c
}

func TestGitHeadPinsThroughResolveAsTheCaller(t *testing.T) {
	f := &fakeSkillManager{refs: map[string]string{"main": commit}}
	c := newClient(t, f, "")
	ctx := identity.ContextWithToken(t.Context(), "id-token")

	got, err := c.GitHead(ctx, repo, "main")
	require.NoError(t, err)
	assert.Equal(t, commit, got)
	req, body := f.last()
	assert.Equal(t, "Bearer id-token", req.Header.Get("Authorization"))
	assert.Equal(t, "application/json", req.Header.Get("Content-Type"))
	item := body["skills"].([]any)[0].(map[string]any)
	assert.Equal(t, map[string]any{"url": repo, "ref": "main"}, item["git"])
	assert.NotEmpty(t, item["name"])
}

// An item with no ref is sent with its path: skill-manager gives an item
// below a configured source's path that source's ref.
func TestGitHeadAtSendsThePath(t *testing.T) {
	f := &fakeSkillManager{refs: map[string]string{"@skills/kubectl": commit}}
	c := newClient(t, f, "")

	got, err := c.GitHeadAt(identity.ContextWithToken(t.Context(), "id-token"), repo, "", "skills/kubectl")
	require.NoError(t, err)
	assert.Equal(t, commit, got)
	_, body := f.last()
	item := body["skills"].([]any)[0].(map[string]any)
	assert.Equal(t, "skills/kubectl", item["path"])
	assert.Equal(t, map[string]any{"url": repo}, item["git"], "no ref: skill-manager picks it")
	assert.Equal(t, "kubectl", item["name"])
}

// On the GitHub registration the bearer agent-manager received is the
// person's GitHub token, the ID token rides in muster's forwarded header:
// skill-manager is called the same way.
func TestTheGitHubRegistrationsBearerIsForwardedAsReceived(t *testing.T) {
	f := &fakeSkillManager{refs: map[string]string{"main": commit}}
	c := newClient(t, f, "static")
	ctx := identity.ContextWithToken(t.Context(), "id-token")
	ctx = identity.ContextWithGitHub(ctx, &identity.GitHub{Login: "alice", Token: "gh-token"})

	_, err := c.GitHead(ctx, repo, "main")
	require.NoError(t, err)
	req, _ := f.last()
	assert.Equal(t, "Bearer gh-token", req.Header.Get("Authorization"))
	assert.Equal(t, "id-token", req.Header.Get("X-Muster-Id-Token"))
}

// The migrate command has no caller: its configured token is the bearer.
func TestWithoutACallerTheConfiguredTokenIsTheBearer(t *testing.T) {
	f := &fakeSkillManager{refs: map[string]string{"main": commit}}
	c := newClient(t, f, "static")

	_, err := c.GitHead(t.Context(), repo, "main")
	require.NoError(t, err)
	req, _ := f.last()
	assert.Equal(t, "Bearer static", req.Header.Get("Authorization"))
}

func TestOCIDigestPinsThroughResolve(t *testing.T) {
	f := &fakeSkillManager{}
	c := newClient(t, f, "t")

	got, err := c.OCIDigest(t.Context(), "ghcr.io/giantswarm/skills/kubectl:1.4.0")
	require.NoError(t, err)
	assert.Equal(t, "ghcr.io/giantswarm/skills/kubectl:1.4.0@"+digest, got)
	_, body := f.last()
	item := body["skills"].([]any)[0].(map[string]any)
	assert.Equal(t, map[string]any{"ref": "ghcr.io/giantswarm/skills/kubectl:1.4.0"}, item["oci"])
}

// RequireReadable asks skill-manager to resolve the repository's default
// branch: it refuses a repository the caller cannot read with 403.
func TestRequireReadableIsSkillManagersCheck(t *testing.T) {
	f := &fakeSkillManager{refs: map[string]string{"": commit}}
	c := newClient(t, f, "t")
	require.NoError(t, c.RequireReadable(t.Context(), repo, "alice"))
	_, body := f.last()
	assert.Equal(t, map[string]any{"url": repo}, body["skills"].([]any)[0].(map[string]any)["git"])

	f.fail, f.failBody = http.StatusForbidden, `{"error":"alice cannot read giantswarm/private","item":0}`
	err := c.RequireReadable(t.Context(), repo, "alice")
	require.ErrorIs(t, err, skills.ErrNotReadable)
	assert.Contains(t, err.Error(), "alice cannot read giantswarm/private")
}

func TestSkillManagerErrorsAreMapped(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		is     error
		says   string
	}{
		{http.StatusBadRequest, `{"error":"skills[0].git.url must be a GitHub URL","item":0}`, skills.ErrUnresolvable, "must be a GitHub URL"},
		{http.StatusNotFound, `{"error":"ref nope not found","item":0}`, skills.ErrUnresolvable, "ref nope not found"},
		{http.StatusForbidden, `{"error":"not readable","item":0}`, skills.ErrNotReadable, "not readable"},
		{http.StatusUnauthorized, `{"error":"invalid token"}`, ErrUnauthenticated, "invalid token"},
		{http.StatusBadGateway, `{"error":"GitHub answered 500"}`, ErrUnavailable, "GitHub answered 500"},
		{http.StatusServiceUnavailable, `upstream down`, ErrUnavailable, "upstream down"},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			f := &fakeSkillManager{fail: tc.status, failBody: tc.body}
			c := newClient(t, f, "t")
			_, err := c.GitHead(t.Context(), repo, "nope")
			require.ErrorIs(t, err, tc.is)
			assert.Contains(t, err.Error(), tc.says)
			assert.Contains(t, err.Error(), "skill-manager")
		})
	}
}

func TestAnUnreachableSkillManagerIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c, err := New(Config{URL: url})
	require.NoError(t, err)
	_, err = c.GitHead(t.Context(), repo, "main")
	require.ErrorIs(t, err, ErrUnavailable)
}

func TestAMalformedPinIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"skills":[{"name":"x","git":{"url":"` + repo + `","commit":"main"}}]}`))
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{URL: srv.URL})
	require.NoError(t, err)
	_, err = c.GitHead(t.Context(), repo, "main")
	require.ErrorIs(t, err, ErrUnavailable)
}

func TestSkillsProxiesTheCatalogue(t *testing.T) {
	f := &fakeSkillManager{}
	c := newClient(t, f, "")
	ctx := identity.ContextWithToken(t.Context(), "id-token")

	got, err := c.Skills(ctx, repo, true)
	require.NoError(t, err)
	req, _ := f.last()
	assert.Equal(t, "Bearer id-token", req.Header.Get("Authorization"))
	assert.Equal(t, repo, req.URL.Query().Get("source"))
	assert.Equal(t, "true", req.URL.Query().Get("refresh"))
	repos := got["repositories"].([]any)
	require.Len(t, repos, 1)
	assert.Equal(t, true, repos[0].(map[string]any)["readOnly"])

	_, err = c.Skills(ctx, "", false)
	require.NoError(t, err)
	req, _ = f.last()
	assert.Empty(t, req.URL.RawQuery)
}

func TestSkillsErrorsAreMapped(t *testing.T) {
	f := &fakeSkillManager{fail: http.StatusBadRequest, failBody: `{"error":"source https://github.com/x/y is not configured"}`}
	c := newClient(t, f, "")
	_, err := c.Skills(t.Context(), "https://github.com/x/y", false)
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "is not configured")
}

func TestNewRefusesABadURL(t *testing.T) {
	for _, u := range []string{"", "skill-manager:8080", "ftp://skill-manager"} {
		_, err := New(Config{URL: u})
		require.Error(t, err, u)
	}
}

// ClaudeFormatPlugin is not part of skill-manager's API: the configured
// checker answers it; without one no plugin is taken for Claude-format, as
// with a pinner that cannot tell.
func TestClaudeFormatPluginIsDelegated(t *testing.T) {
	c, err := New(Config{URL: "http://skill-manager:8080", Formats: formatsFunc(func(_ context.Context, _, _, path string) (bool, error) {
		return strings.HasPrefix(path, "claude"), nil
	})})
	require.NoError(t, err)
	got, err := c.ClaudeFormatPlugin(t.Context(), repo, commit, "claude-format")
	require.NoError(t, err)
	assert.True(t, got)

	bare, err := New(Config{URL: "http://skill-manager:8080"})
	require.NoError(t, err)
	got, err = bare.ClaudeFormatPlugin(t.Context(), repo, commit, "claude-format")
	require.NoError(t, err)
	assert.False(t, got)
}

type formatsFunc func(ctx context.Context, repoURL, commit, path string) (bool, error)

func (f formatsFunc) ClaudeFormatPlugin(ctx context.Context, repoURL, commit, path string) (bool, error) {
	return f(ctx, repoURL, commit, path)
}
