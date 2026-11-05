package skillmanager

import (
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
	repo        = "https://github.com/giantswarm/agent-platform"
	privateRepo = "https://github.com/giantswarm/private-skills"
	mainHead    = "0123456789abcdef0123456789abcdef01234567"
	tagHead     = "89abcdef0123456789abcdef0123456789abcdef"
	digest      = "sha256:" + "ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12"
)

// fakeSkillManager serves skill-manager's REST API as its docs/api.md
// states it. A git item without ref below skills/ of repo is the platform
// source's (tagHead), any other the default branch's (mainHead); a commit is
// kept; privateRepo is unreadable (403); a path "missing" is 404; a git
// plugin below claude/ is Claude-format.
type fakeSkillManager struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   []resolveBody
	// fail answers every call with this status and body when set.
	fail     int
	failBody string
}

func (f *fakeSkillManager) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body resolveBody
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
			_, _ = w.Write([]byte(`{"repositories":[{"repoUrl":"` + repo + `","ref":"v1.2.0","commit":"` + tagHead + `","path":"skills","readOnly":true,"skills":[` +
				`{"name":"kubectl","description":"k","path":"skills/kubectl","tags":["kubernetes"]}]}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func refuse(w http.ResponseWriter, status int, list string, i int, msg string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": msg, "item": i, "list": list})
}

func (f *fakeSkillManager) resolve(w http.ResponseWriter, body resolveBody) {
	out := resolveBody{Skills: []Item{}, Plugins: []Item{}}
	for _, list := range []string{ListSkills, ListPlugins} {
		items := body.Skills
		if list == ListPlugins {
			items = body.Plugins
		}
		for i, it := range items {
			switch {
			case it.OCI != nil:
				it.OCI = &OCI{Ref: strings.SplitN(it.OCI.Ref, ":", 2)[0] + "@" + digest}
			case it.Git.URL == privateRepo:
				refuse(w, http.StatusForbidden, list, i, "the caller cannot read "+privateRepo)
				return
			case it.Path == "missing":
				refuse(w, http.StatusNotFound, list, i, "path missing not found")
				return
			default:
				g := *it.Git
				switch {
				case g.Commit != "":
				case g.Ref == "" && g.URL == repo && strings.HasPrefix(it.Path, "skills/"):
					g.Commit = tagHead
				default:
					g.Commit = mainHead
				}
				g.Ref = ""
				it.Git = &g
				if list == ListPlugins {
					claude := strings.HasPrefix(it.Path, "claude")
					if it.ClaudeFormat != nil && *it.ClaudeFormat && !claude {
						refuse(w, http.StatusBadRequest, list, i, "not a Claude Code plugin")
						return
					}
					it.ClaudeFormat = &claude
				}
			}
			if list == ListPlugins {
				out.Plugins = append(out.Plugins, it)
			} else {
				out.Skills = append(out.Skills, it)
			}
		}
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (f *fakeSkillManager) last() (*http.Request, resolveBody) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1], f.bodies[len(f.bodies)-1]
}

func (f *fakeSkillManager) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func newClient(t *testing.T, f *fakeSkillManager, token string) *Client {
	t.Helper()
	c, err := New(Config{URL: f.start(t).URL + "/", Token: token})
	require.NoError(t, err)
	return c
}

// Skills and plugins are pinned in one call, as the caller: no ref below a
// source's path takes its ref, a commit is kept, an image tag its digest;
// a plugin is answered with its format.
func TestResolvePinsSkillsAndPluginsInOneCall(t *testing.T) {
	f := &fakeSkillManager{}
	c := newClient(t, f, "")
	ctx := identity.ContextWithToken(t.Context(), "id-token")

	gotSkills, gotPlugins, err := c.Resolve(ctx,
		[]Item{
			{Name: "kubectl", Path: "skills/kubectl", Git: &Git{URL: repo}},
			{Name: "runbooks", Path: "runbooks", Git: &Git{URL: repo, Commit: tagHead}},
			{Name: "img", OCI: &OCI{Ref: "ghcr.io/giantswarm/skills/kubectl:1.4.0"}},
		},
		[]Item{{Path: "claude/review", Git: &Git{URL: repo, Ref: "main"}}})
	require.NoError(t, err)
	assert.Equal(t, 1, f.calls())
	assert.Equal(t, &Git{URL: repo, Commit: tagHead}, gotSkills[0].Git)
	assert.Equal(t, &Git{URL: repo, Commit: tagHead}, gotSkills[1].Git)
	assert.Equal(t, &OCI{Ref: "ghcr.io/giantswarm/skills/kubectl@" + digest}, gotSkills[2].OCI)
	assert.Equal(t, &Git{URL: repo, Commit: mainHead}, gotPlugins[0].Git)
	require.NotNil(t, gotPlugins[0].ClaudeFormat)
	assert.True(t, *gotPlugins[0].ClaudeFormat)

	req, body := f.last()
	assert.Equal(t, "Bearer id-token", req.Header.Get("Authorization"))
	assert.Equal(t, "application/json", req.Header.Get("Content-Type"))
	assert.Equal(t, Item{Name: "kubectl", Path: "skills/kubectl", Git: &Git{URL: repo}}, body.Skills[0])
	assert.Len(t, body.Plugins, 1, "plugins go in the plugins list")

	// The format resolve reported answers ClaudeFormatPlugin without a call.
	claude, err := c.ClaudeFormatPlugin(ctx, repo, mainHead, "claude/review")
	require.NoError(t, err)
	assert.True(t, claude)
	assert.Equal(t, 1, f.calls())
	// One it has not seen is resolved at its commit.
	claude, err = c.ClaudeFormatPlugin(ctx, repo, tagHead, "plugins/ops")
	require.NoError(t, err)
	assert.False(t, claude)
	_, body = f.last()
	assert.Equal(t, []Item{{Path: "plugins/ops", Git: &Git{URL: repo, Commit: tagHead}}}, body.Plugins)
}

// On the GitHub registration the bearer agent-manager received is the
// person's GitHub token, the ID token rides in muster's forwarded header:
// skill-manager is called the same way.
func TestTheGitHubRegistrationsBearerIsForwardedAsReceived(t *testing.T) {
	f := &fakeSkillManager{}
	c := newClient(t, f, "service")
	ctx := identity.ContextWithToken(t.Context(), "id-token")
	ctx = identity.ContextWithGitHub(ctx, &identity.GitHub{Login: "alice", Token: "gh-token"})

	_, err := c.GitHead(ctx, repo, "main")
	require.NoError(t, err)
	req, _ := f.last()
	assert.Equal(t, "Bearer gh-token", req.Header.Get("Authorization"))
	assert.Equal(t, "id-token", req.Header.Get("X-Muster-Id-Token"))
}

// The migrate command has no caller: the service token is the bearer.
func TestWithoutACallerTheServiceTokenIsTheBearer(t *testing.T) {
	f := &fakeSkillManager{}
	c := newClient(t, f, "service")

	got, err := c.GitHead(t.Context(), repo, "main")
	require.NoError(t, err)
	assert.Equal(t, mainHead, got)
	req, _ := f.last()
	assert.Equal(t, "Bearer service", req.Header.Get("Authorization"))
}

func TestOCIDigestPinsThroughResolve(t *testing.T) {
	f := &fakeSkillManager{}
	c := newClient(t, f, "t")

	got, err := c.OCIDigest(t.Context(), "ghcr.io/giantswarm/skills/kubectl:1.4.0")
	require.NoError(t, err)
	assert.Equal(t, "ghcr.io/giantswarm/skills/kubectl@"+digest, got)
	_, body := f.last()
	assert.Equal(t, &OCI{Ref: "ghcr.io/giantswarm/skills/kubectl:1.4.0"}, body.Skills[0].OCI)
}

// Readability is skill-manager's, inside resolve: RequireReadable asks
// nothing.
func TestRequireReadableAsksNothing(t *testing.T) {
	f := &fakeSkillManager{}
	c := newClient(t, f, "t")
	require.NoError(t, c.RequireReadable(t.Context(), privateRepo, "alice"))
	assert.Zero(t, f.calls())

	_, _, err := c.Resolve(t.Context(), nil, []Item{{Git: &Git{URL: privateRepo}}})
	require.ErrorIs(t, err, skills.ErrNotReadable)
	var refused *ResolveError
	require.ErrorAs(t, err, &refused)
	assert.Equal(t, ListPlugins, refused.List)
	assert.Equal(t, 0, refused.Item)
	assert.Contains(t, err.Error(), "cannot read")
}

func TestResolveErrorsAreMapped(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		is     error
		list   string
		item   int
	}{
		{http.StatusBadRequest, `{"error":"not a Claude Code plugin","item":2,"list":"plugins"}`, skills.ErrUnresolvable, ListPlugins, 2},
		{http.StatusNotFound, `{"error":"ref nope not found","item":1,"list":"skills"}`, skills.ErrUnresolvable, ListSkills, 1},
		{http.StatusForbidden, `{"error":"not readable","item":0,"list":"skills"}`, skills.ErrNotReadable, ListSkills, 0},
		{http.StatusUnauthorized, `{"error":"invalid token"}`, ErrUnauthenticated, "", -1},
		{http.StatusBadGateway, `{"error":"GitHub answered 500","item":0,"list":"skills"}`, ErrUnavailable, ListSkills, 0},
		{http.StatusServiceUnavailable, `upstream down`, ErrUnavailable, "", -1},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			f := &fakeSkillManager{fail: tc.status, failBody: tc.body}
			c := newClient(t, f, "t")
			_, _, err := c.Resolve(t.Context(), []Item{{Git: &Git{URL: repo}}}, nil)
			require.ErrorIs(t, err, tc.is)
			var refused *ResolveError
			require.ErrorAs(t, err, &refused)
			assert.Equal(t, tc.list, refused.List)
			assert.Equal(t, tc.item, refused.Item)
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
		_, _ = w.Write([]byte(`{"skills":[{"name":"x","git":{"url":"` + repo + `","commit":"main"}}],"plugins":[]}`))
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
