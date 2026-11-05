package agents

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

// fakeSkillManager is skill-manager's REST API as the contract states it.
// An item with no ref below skills/ is the platform source's, pinned at
// tagHead; any other item without a ref is the default branch, mainHead.
type fakeSkillManager struct {
	mu      sync.Mutex
	items   []map[string]any
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
				Skills []map[string]any `json:"skills"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			out := []map[string]any{}
			for _, it := range body.Skills {
				f.mu.Lock()
				f.items = append(f.items, it)
				f.mu.Unlock()
				git := it["git"].(map[string]any)
				commit := mainHead
				path, _ := it["path"].(string)
				switch ref, _ := git["ref"].(string); {
				case ref == "feature":
					commit = featureHead
				case ref == "" && git["url"] == platformSkillsRepo && len(path) > len("skills/") && path[:len("skills/")] == "skills/":
					commit = tagHead
				}
				pinned := map[string]any{"name": it["name"], "git": map[string]any{"url": git["url"], "commit": commit}}
				if path != "" {
					pinned["path"] = path
				}
				out = append(out, pinned)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"skills": out, "plugins": []any{}})
		case "/v1/skills":
			_, _ = w.Write([]byte(`{"repositories":[{"repoUrl":"` + platformSkillsRepo + `","ref":"v1.2.0","commit":"` + tagHead + `","path":"skills","readOnly":true,"skills":[` +
				`{"name":"kubectl","path":"skills/kubectl","tags":["kubernetes"]},{"name":"pagerduty","path":"skills/pagerduty","tags":["alerting"]}]}]}`))
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
	assert.Contains(t, sm.items, map[string]any{"name": "kubectl", "path": "skills/kubectl", "git": map[string]any{"url": platformSkillsRepo}})
	for _, b := range sm.bearers {
		assert.Equal(t, "Bearer id-token", b)
	}
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
			sm := &fakeSkillManager{status: tc.status, body: `{"error":"the answer","item":0}`}
			f := withSkillManager(t, sm)
			_, err := f.svc.Create(t.Context(), Spec{Name: "sre", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"}, Skills: Skills{
				{Git: &GitSkill{URL: skillsRepo, Ref: "main"}, Path: "runbooks"},
			}})
			require.ErrorIs(t, err, tc.is)
			assert.NotErrorIs(t, err, tc.not)
			assert.Contains(t, err.Error(), "the answer")
			assert.Contains(t, err.Error(), "skill-manager")
		})
	}
}

// list_skills proxies skill-manager's catalogue, filtered by tags; get_info
// names skill-manager instead of skill repositories.
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
