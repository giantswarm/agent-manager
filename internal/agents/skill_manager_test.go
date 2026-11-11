package agents

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
	"github.com/giantswarm/agent-manager/internal/kube"
	"github.com/giantswarm/agent-manager/internal/skillmanager"
	"github.com/giantswarm/agent-manager/internal/tags"
	"github.com/giantswarm/agent-manager/internal/tags/tagstest"
)

const platformSkillsRepo = "https://github.com/giantswarm/agent-platform"

// fakeSkillManager is skill-manager's REST API as its docs/api.md states
// it. A git item without ref below skills/ of platformSkillsRepo is the
// platform source's (tagHead); ref feature is featureHead; any other without
// ref or commit the default branch (mainHead); a commit is kept;
// privateSkillsRepo is unreadable (403); a git plugin at claude-format is
// Claude-format.
type fakeSkillManager struct {
	mu      sync.Mutex
	skills  []skillmanager.Item
	plugins []skillmanager.Item
	calls   int
	bearers []string
	// status, when set, is every answer's status with body.
	status int
	body   string
	url    string
}

func (f *fakeSkillManager) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.bearers = append(f.bearers, r.Header.Get("Authorization"))
		f.mu.Unlock()
		if f.status != 0 {
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte(f.body))
			return
		}
		switch r.URL.Path {
		case "/v1/resolve":
			var body struct {
				Skills  []skillmanager.Item `json:"skills"`
				Plugins []skillmanager.Item `json:"plugins"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.calls++
			f.skills = append(f.skills, body.Skills...)
			f.plugins = append(f.plugins, body.Plugins...)
			f.mu.Unlock()
			out := map[string][]skillmanager.Item{"skills": {}, "plugins": {}}
			for list, items := range map[string][]skillmanager.Item{"skills": body.Skills, "plugins": body.Plugins} {
				for i, it := range items {
					if it.OCI != nil {
						it.OCI = &skillmanager.OCI{Ref: kubectlRef + "@" + kubectlSum}
						out[list] = append(out[list], it)
						continue
					}
					if it.Git.URL == privateSkillsRepo {
						w.WriteHeader(http.StatusForbidden)
						_ = json.NewEncoder(w).Encode(map[string]any{"error": "the caller cannot read " + privateSkillsRepo, "item": i, "list": list})
						return
					}
					g := *it.Git
					switch {
					case g.Commit != "":
					case g.Ref == "feature":
						g.Commit = featureHead
					case g.Ref == "" && g.URL == platformSkillsRepo && strings.HasPrefix(it.Path, "skills/"):
						g.Commit = tagHead
					default:
						g.Commit = mainHead
					}
					g.Ref = ""
					it.Git = &g
					if list == "plugins" {
						claude := it.Path == "claude-format"
						it.ClaudeFormat = &claude
					}
					out[list] = append(out[list], it)
				}
			}
			_ = json.NewEncoder(w).Encode(out)
		case "/v1/skills":
			_, _ = w.Write([]byte(`{"repositories":[{"repoUrl":"` + platformSkillsRepo + `","ref":"v1.2.0","commit":"` + tagHead + `","path":"skills","readOnly":true,"skills":[` +
				`{"name":"kubectl","path":"skills/kubectl","tags":["kubernetes"]},{"name":"pagerduty","path":"skills/pagerduty","tags":[" Alerting"]}]}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return srv
}

// withSkillManager is the seeded fixture pinning and listing through a fake
// skill-manager.
func withSkillManager(t *testing.T, sm *fakeSkillManager) *fixture {
	t.Helper()
	client, err := skillmanager.New(skillmanager.Config{URL: sm.start(t).URL})
	require.NoError(t, err)
	f := seeded(t)
	f.pinner = nil
	f.svc = New(kube.NewServiceAccountProvider(kube.FromInterfaces(f.dyn, f.typed, f.typed.Discovery())), embeddedChart{}, nil, client, Config{
		DefaultNamespace: "kagent", ManagedNamespaces: []string{"tenant"}, Version: "test", Tags: tagstest.Rule(), SkillManager: client,
	}, nil)
	return f
}

// A skill with no ref is pinned by skill-manager, as the caller, with its
// path: below a configured source's path it takes that source's ref.
func TestCreatePinsThroughSkillManager(t *testing.T) {
	sm := &fakeSkillManager{}
	f := withSkillManager(t, sm)
	ctx := identity.ContextWithToken(t.Context(), "id-token")

	res, err := f.svc.Create(ctx, Spec{Name: "sre", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"}, Skills: Skills{
		{Git: &GitSkill{URL: platformSkillsRepo}, Path: "skills/kubectl"},
		{Git: &GitSkill{URL: skillsRepo, Ref: "feature"}, Path: "runbooks"},
	}})
	require.NoError(t, err)
	assert.Equal(t, Skills{
		{Name: "kubectl", Path: "skills/kubectl", Git: &GitSkill{URL: platformSkillsRepo, Commit: tagHead}},
		{Name: "runbooks", Path: "runbooks", Git: &GitSkill{URL: skillsRepo, Commit: featureHead}},
	}, res.Agent.Skills)
	assert.Equal(t, 1, sm.calls, "one resolve for the whole list, readability included")
	assert.Contains(t, sm.skills, skillmanager.Item{Name: "kubectl", Path: "skills/kubectl", Git: &skillmanager.Git{URL: platformSkillsRepo}})
	for _, b := range sm.bearers {
		assert.Equal(t, "Bearer id-token", b)
	}
}

// A write with skills and plugins pins both in one resolve; a refused
// plugin is named by its index.
func TestSkillsAndPluginsArePinnedInOneResolve(t *testing.T) {
	sm := &fakeSkillManager{}
	f := withSkillManager(t, sm)
	ctx := identity.ContextWithToken(t.Context(), "id-token")

	res, err := f.svc.Create(ctx, Spec{Name: "sre", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"},
		Skills:  Skills{{Git: &GitSkill{URL: skillsRepo, Ref: "feature"}, Path: "runbooks"}},
		Plugins: Plugins{{Git: &GitSkill{URL: skillsRepo}, Path: "plugins/review"}},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, sm.calls, "skills and plugins in one call; the plugin's format comes with it")
	assert.Equal(t, Plugins{{Path: "plugins/review", Git: &GitSkill{URL: skillsRepo, Commit: mainHead}}}, res.Agent.Plugins)

	_, err = f.svc.Update(ctx, Update{Name: "sre", Plugins: &Plugins{
		{Git: &GitSkill{URL: skillsRepo}, Path: "plugins/review"},
		{Git: &GitSkill{URL: privateSkillsRepo}, Path: "plugins/secret"},
	}})
	require.ErrorIs(t, err, ErrForbidden)
	assert.Contains(t, err.Error(), "plugins[1]")
}

// refreshSkills re-pins through skill-manager too: a platform skill to its
// source's ref, any other to its default-branch head.
func TestRefreshSkillsPinsThroughSkillManager(t *testing.T) {
	sm := &fakeSkillManager{}
	f := withSkillManager(t, sm)
	ctx := identity.ContextWithToken(t.Context(), "id-token")
	_, err := f.svc.Update(ctx, Update{Name: "verifier", Skills: &Skills{
		{Git: &GitSkill{URL: platformSkillsRepo, Commit: featureHead}, Path: "skills/kubectl"},
		{Git: &GitSkill{URL: skillsRepo, Commit: featureHead}, Path: "runbooks"},
	}})
	require.NoError(t, err)

	res, err := f.svc.Update(ctx, Update{Name: "verifier", RefreshSkills: true})
	require.NoError(t, err)
	assert.Equal(t, []any{
		map[string]any{"name": "kubectl", "path": "skills/kubectl", "git": map[string]any{"url": platformSkillsRepo, "commit": tagHead}},
		map[string]any{"name": "runbooks", "path": "runbooks", "git": map[string]any{"url": skillsRepo, "commit": mainHead}},
	}, res.After["skills"])
}

// skill-manager's refusals keep their meaning: 403 is forbidden, 400 and
// 404 an invalid request, 401 unauthenticated, 5xx a backend failure.
func TestSkillManagerRefusalsAreMapped(t *testing.T) {
	for _, tc := range []struct {
		status int
		is     error
		not    error
	}{
		{http.StatusForbidden, ErrForbidden, ErrInvalid},
		{http.StatusNotFound, ErrInvalid, ErrForbidden},
		{http.StatusBadRequest, ErrInvalid, ErrForbidden},
		{http.StatusUnauthorized, ErrUnauthenticated, ErrInvalid},
		{http.StatusBadGateway, skillmanager.ErrUnavailable, ErrInvalid},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			sm := &fakeSkillManager{status: tc.status, body: `{"error":"the answer","item":1,"list":"skills"}`}
			f := withSkillManager(t, sm)
			_, err := f.svc.Create(t.Context(), Spec{Name: "sre", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"}, Skills: Skills{
				{Git: &GitSkill{URL: skillsRepo, Ref: "main"}, Path: "runbooks"},
				{Git: &GitSkill{URL: skillsRepo, Ref: "main"}, Path: "ops"},
			}})
			require.ErrorIs(t, err, tc.is)
			assert.Contains(t, err.Error(), "skills[1] (ops)", "the item skill-manager names")
			assert.NotErrorIs(t, err, tc.not)
			assert.Contains(t, err.Error(), "the answer")
			assert.Contains(t, err.Error(), "skill-manager")
		})
	}
}

// list_skills proxies skill-manager's catalogue, filtered by tags normalised
// as the in-process path does; get_info names skill-manager instead of skill
// repositories.
func TestListSkillsProxiesSkillManager(t *testing.T) {
	sm := &fakeSkillManager{}
	f := withSkillManager(t, sm)
	ctx := identity.ContextWithToken(t.Context(), "id-token")

	got, err := f.svc.ListSkills(ctx, "", "", false, tags.Filter{"alerting": {}})
	require.NoError(t, err)
	raw, err := json.Marshal(got)
	require.NoError(t, err)
	var out struct {
		Repositories []struct {
			RepoURL  string `json:"repoUrl"`
			ReadOnly bool   `json:"readOnly"`
			Skills   []struct {
				Name string `json:"name"`
			} `json:"skills"`
		} `json:"repositories"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	require.Len(t, out.Repositories, 1)
	assert.Equal(t, platformSkillsRepo, out.Repositories[0].RepoURL)
	assert.True(t, out.Repositories[0].ReadOnly)
	require.Len(t, out.Repositories[0].Skills, 1)
	assert.Equal(t, "pagerduty", out.Repositories[0].Skills[0].Name)
	assert.Equal(t, "Bearer id-token", sm.bearers[len(sm.bearers)-1])

	_, err = f.svc.ListSkills(ctx, "", "main", false, nil)
	require.ErrorIs(t, err, ErrInvalid, "a ref is skill-manager's configuration")

	info := f.svc.Info(ctx)
	assert.Equal(t, sm.url, info.SkillManager)
	assert.True(t, info.Capabilities["skills"])
	assert.Empty(t, info.SkillsRepositories)
}

// Plugins are pinned in the plugins list; the format skill-manager answers
// is what the Agent Plugins Harness guard refuses a Claude-format one by,
// with no other lookup.
func TestPluginsPinThroughSkillManager(t *testing.T) {
	sm := &fakeSkillManager{}
	f := withSkillManager(t, sm)
	ctx := identity.ContextWithToken(t.Context(), "id-token")

	res, err := f.svc.Create(ctx, Spec{Name: "sre", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"}, Plugins: Plugins{
		{Git: &GitSkill{URL: skillsRepo, Ref: "feature"}, Path: "plugins/ops", Skills: []string{"triage"}},
	}})
	require.NoError(t, err)
	assert.Equal(t, Plugins{{Path: "plugins/ops", Skills: []string{"triage"}, Git: &GitSkill{URL: skillsRepo, Commit: featureHead}}}, res.Agent.Plugins)
	assert.Equal(t, []skillmanager.Item{{Path: "plugins/ops", Skills: []string{"triage"}, Git: &skillmanager.Git{URL: skillsRepo, Ref: "feature"}}}, sm.plugins)
	assert.Empty(t, sm.skills)

	calls := sm.calls
	_, err = f.svc.Create(ctx, Spec{Name: "claude", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"}, Plugins: Plugins{
		{Git: &GitSkill{URL: skillsRepo}, Path: "claude-format"},
	}})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "Claude-format")
	assert.Equal(t, calls+1, sm.calls, "the format came with the pin")

	_, err = f.svc.Create(ctx, Spec{Name: "private", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"}, Plugins: Plugins{
		{Git: &GitSkill{URL: skillsRepo}, Path: "plugins/ops"}, {Git: &GitSkill{URL: privateSkillsRepo}},
	}})
	require.ErrorIs(t, err, ErrForbidden)
	assert.Contains(t, err.Error(), "plugins[1]")
}
