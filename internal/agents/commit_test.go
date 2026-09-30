package agents

import (
	"context"
	"strings"
	"testing"

	"github.com/giantswarm/gitops-commit/commit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/giantswarm/agent-manager/internal/identity"
)

var (
	ksGVR      = schema.GroupVersionResource{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Resource: "kustomizations"}
	gitRepoGVR = schema.GroupVersionResource{Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "gitrepositories"}
	fleetRepo  = commit.Repository{Owner: "giantswarm", Name: "fleet"}
)

// gitOpsFixture is seeded() with the namespace's agents applied from git:
// Kustomization flux-system/agents builds ./agents of giantswarm/fleet on
// main, and it applies the chart source and the release "gitops", whose file
// agent-manager's directory carries. The caller holds a GitHub grant.
func gitOpsFixture(t *testing.T, prune bool) (*fixture, *commit.Fake, context.Context) {
	t.Helper()
	f := seeded(t)
	fake := commit.NewFake()
	f.svc.cfg.GitHub = func(token string) (GitHubRemote, error) {
		require.Equal(t, "ghu_person", token)
		return fake, nil
	}
	mustCreate(t, f, ksGVR, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1", "kind": "Kustomization",
		"metadata": map[string]any{"name": "agents", "namespace": "flux-system"},
		"spec":     map[string]any{"path": "./agents", "prune": prune, "sourceRef": map[string]any{"kind": "GitRepository", "name": "fleet"}},
	}})
	mustCreate(t, f, gitRepoGVR, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "source.toolkit.fluxcd.io/v1", "kind": "GitRepository",
		"metadata": map[string]any{"name": "fleet", "namespace": "flux-system"},
		"spec":     map[string]any{"url": "https://github.com/giantswarm/fleet", "ref": map[string]any{"branch": "main"}},
	}})
	flux := map[string]any{KustomizationNameLabel: "agents", KustomizationNamespaceLabel: "flux-system"}
	src, err := f.dyn.Resource(ociGVR).Namespace("kagent").Get(context.Background(), "agent", metav1.GetOptions{})
	require.NoError(t, err)
	src.SetLabels(map[string]string{KustomizationNameLabel: "agents", KustomizationNamespaceLabel: "flux-system"})
	_, err = f.dyn.Resource(ociGVR).Namespace("kagent").Update(context.Background(), src, metav1.UpdateOptions{})
	require.NoError(t, err)
	values := map[string]any{"agent": map[string]any{"name": "gitops"}, "modelConfig": map[string]any{"name": "default-model-config"}, ToolsetValuesKey: []any{"preset:read-only"}}
	mustCreate(t, f, hrGVR, helmRelease("kagent", "gitops", values, true, flux))
	fake.AddBranch(fleetRepo, "main", map[string][]byte{
		"agents/kustomization.yaml":               []byte("resources:\n  - agent-manager\n"),
		"agents/agent-manager/kustomization.yaml": []byte("resources:\n  - agent.yaml\n  - gitops.yaml\n"),
		"agents/agent-manager/agent.yaml":         []byte(ToYAML(BuildOCIRepository("kagent", f.svc.cfg.Compose))),
		"agents/agent-manager/gitops.yaml":        []byte(ToYAML(BuildHelmRelease("gitops", "kagent", values, f.svc.cfg.Compose))),
	})
	ctx := identity.ContextWithGitHub(context.Background(), &identity.GitHub{Login: "jane", Token: "ghu_person"})
	return f, fake, ctx
}

func filesByPath(c *CommitResult) map[string]CommitFile {
	out := map[string]CommitFile{}
	for _, file := range c.Files {
		out[file.Path] = file
	}
	return out
}

func TestApplyDryRunWritesNothing(t *testing.T) {
	f := seeded(t)
	ctx := context.Background()
	dry := WriteOptions{DryRun: true}

	res, err := f.svc.Create(ctx, Spec{Name: "sre", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"}, WriteOptions: dry})
	require.NoError(t, err)
	assert.True(t, res.DryRun)
	assert.Equal(t, ModeApply, res.Mode)
	assert.True(t, res.Created.HelmRelease)
	assert.Contains(t, res.Manifests.HelmRelease, "name: sre")
	_, err = f.svc.Get(ctx, In(""), "sre")
	assert.ErrorIs(t, err, ErrNotFound, "a dry run creates nothing")

	upd, err := f.svc.Update(ctx, Update{Name: "verifier", Description: str("new"), WriteOptions: dry})
	require.NoError(t, err)
	assert.Equal(t, []string{"agent.description"}, upd.Changed)
	got, err := f.svc.Get(ctx, In(""), "verifier")
	require.NoError(t, err)
	assert.NotEqual(t, "new", got.Values["agent"].(map[string]any)["description"], "a dry run changes nothing")

	del, err := f.svc.Delete(ctx, In(""), "verifier", false, dry)
	require.NoError(t, err)
	assert.True(t, del.HelmReleaseDeleted)
	assert.True(t, del.OCIRepositoryDeleted)
	_, err = f.svc.Get(ctx, In(""), "verifier")
	require.NoError(t, err, "a dry run deletes nothing")
}

func TestModeArguments(t *testing.T) {
	f := seeded(t)
	ctx := context.Background()
	spec := Spec{Name: "sre", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"}}

	spec.WriteOptions = WriteOptions{Mode: "push"}
	_, err := f.svc.Create(ctx, spec)
	assert.ErrorIs(t, err, ErrInvalid)
	spec.WriteOptions = WriteOptions{Repository: "giantswarm/fleet", Branch: "main"}
	_, err = f.svc.Create(ctx, spec)
	assert.ErrorIs(t, err, ErrInvalid, "a commit target without mode commit")
	spec.WriteOptions = WriteOptions{Mode: ModeCommit}
	_, err = f.svc.Create(ctx, spec)
	assert.ErrorIs(t, err, ErrUnsupported, "commit mode without the GitHub pin")
	assert.False(t, f.svc.Info(ctx).Capabilities["commit"])
}

func TestCommitCreateOpensThePullRequestAsThePerson(t *testing.T) {
	f, fake, ctx := gitOpsFixture(t, true)
	assert.True(t, f.svc.Info(ctx).Capabilities["commit"])
	spec := Spec{Name: "sre", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"}, WriteOptions: WriteOptions{Mode: ModeCommit, DryRun: true}}

	dry, err := f.svc.Create(ctx, spec)
	require.NoError(t, err)
	require.NotNil(t, dry.Commit)
	assert.Equal(t, "giantswarm/fleet", dry.Commit.Repository)
	assert.Equal(t, "main", dry.Commit.Base)
	assert.Equal(t, "agents/agent-manager", dry.Commit.Directory)
	assert.Equal(t, "flux-system/agents", dry.Commit.Kustomization)
	assert.Equal(t, "agent-manager/create-kagent-sre", dry.Commit.Branch)
	assert.Empty(t, dry.Commit.PullRequest)
	files := filesByPath(dry.Commit)
	assert.Equal(t, "add", files["agents/agent-manager/sre.yaml"].Action)
	assert.Equal(t, dry.Manifests.HelmRelease, files["agents/agent-manager/sre.yaml"].Content, "the file is the dry run's manifest")
	assert.Equal(t, "unchanged", files["agents/agent-manager/agent.yaml"].Action, "the directory's chart source stays")
	assert.Equal(t, "update", files["agents/agent-manager/kustomization.yaml"].Action)
	assert.Empty(t, fake.PullRequests(), "a dry run opens nothing")

	spec.DryRun = false
	res, err := f.svc.Create(ctx, spec)
	require.NoError(t, err)
	require.Len(t, fake.PullRequests(), 1)
	pr := fake.PullRequests()[0]
	assert.Equal(t, pr.URL, res.Commit.PullRequest)
	assert.Equal(t, "jane", res.Commit.Author)
	assert.Equal(t, "feat(agents): add agent sre in kagent", pr.Title)
	assert.Contains(t, pr.Body, "@jane")
	head := fake.Files(fleetRepo, "agent-manager/create-kagent-sre")
	assert.Equal(t, dry.Manifests.HelmRelease, string(head["agents/agent-manager/sre.yaml"]))
	assert.Contains(t, string(head["agents/agent-manager/kustomization.yaml"]), "sre.yaml")
	assert.Equal(t, ManagedGitOps, res.Agent.Managed)
	_, err = f.svc.Get(ctx, In(""), "sre")
	assert.ErrorIs(t, err, ErrNotFound, "commit mode writes nothing live")
}

func TestCommitNeedsTheGitHubGrantAndATarget(t *testing.T) {
	f, _, ctx := gitOpsFixture(t, true)
	spec := Spec{Name: "sre", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"}, WriteOptions: WriteOptions{Mode: ModeCommit}}

	_, err := f.svc.Create(context.Background(), spec)
	require.ErrorIs(t, err, ErrAuthRequired)
	assert.Contains(t, err.Error(), "core_auth_login server=agent-manager")

	spec.Namespace = "tenant"
	_, err = f.svc.Create(ctx, spec)
	require.Error(t, err)
	mustCreate(t, f, mcGVR, modelConfig("tenant", "default-model-config", "Anthropic", "m"))
	_, err = f.svc.Create(ctx, spec)
	require.ErrorIs(t, err, ErrInvalid, "no Kustomization owns tenant")
	assert.Contains(t, err.Error(), "repository, branch and path")

	spec.Repository, spec.Branch, spec.Path = "giantswarm/fleet", "main", "tenants"
	spec.DryRun = true
	res, err := f.svc.Create(ctx, spec)
	require.NoError(t, err)
	assert.Equal(t, "tenants/agent-manager", res.Commit.Directory)
	files := filesByPath(res.Commit)
	assert.Equal(t, "add", files["tenants/agent-manager/agent.yaml"].Action, "a namespace without a chart source gets one")
	assert.Equal(t, "add", files["tenants/agent-manager/sre.yaml"].Action)

	spec.Name, spec.Namespace = "agent", ""
	spec.Repository, spec.Branch, spec.Path = "", "", ""
	_, err = f.svc.Create(ctx, spec)
	assert.ErrorIs(t, err, ErrInvalid, "an agent named like the chart source")
}

func TestApplyRefusesAGitOpsOwnedReleaseNamingTheTarget(t *testing.T) {
	f, _, ctx := gitOpsFixture(t, true)
	_, err := f.svc.Update(ctx, Update{Name: "gitops", Description: str("x")})
	require.ErrorIs(t, err, ErrGitOpsOwned)
	assert.Contains(t, err.Error(), "mode commit")
	assert.Contains(t, err.Error(), "giantswarm/fleet, directory agents/agent-manager on main")
	_, err = f.svc.Delete(ctx, In(""), "gitops", true, WriteOptions{})
	assert.ErrorIs(t, err, ErrGitOpsOwned)
}

func TestCommitUpdateRewritesTheReleaseFile(t *testing.T) {
	f, fake, ctx := gitOpsFixture(t, true)
	res, err := f.svc.Update(ctx, Update{Name: "gitops", Description: str("reviews"), WriteOptions: WriteOptions{Mode: ModeCommit}})
	require.NoError(t, err)
	assert.Equal(t, []string{"agent.description"}, res.Changed)
	require.Len(t, fake.PullRequests(), 1)
	assert.Equal(t, "chore(agents): update agent gitops in kagent", fake.PullRequests()[0].Title)
	head := fake.Files(fleetRepo, "agent-manager/update-kagent-gitops")
	assert.Equal(t, res.Manifests.HelmRelease, string(head["agents/agent-manager/gitops.yaml"]))
	assert.Contains(t, string(head["agents/agent-manager/gitops.yaml"]), "description: reviews")
	assert.Equal(t, "update", filesByPath(res.Commit)["agents/agent-manager/gitops.yaml"].Action)

	// A GitOps release defined elsewhere in the repository is refused.
	flux := map[string]any{KustomizationNameLabel: "agents", KustomizationNamespaceLabel: "flux-system"}
	mustCreate(t, f, hrGVR, helmRelease("kagent", "handmade", map[string]any{"agent": map[string]any{"name": "handmade"}}, true, flux))
	_, err = f.svc.Update(ctx, Update{Name: "handmade", Description: str("x"), WriteOptions: WriteOptions{Mode: ModeCommit}})
	require.ErrorIs(t, err, ErrConflict)
	assert.Contains(t, err.Error(), "not in agent-manager's directory")

	// A release applied live has no repository to commit to.
	_, err = f.svc.Update(ctx, Update{Name: "verifier", Description: str("x"), WriteOptions: WriteOptions{Mode: ModeCommit}})
	require.ErrorIs(t, err, ErrConflict)
	assert.Contains(t, err.Error(), "use mode apply")
}

func TestCommitDeleteOpensTheRemovingPullRequest(t *testing.T) {
	f, fake, ctx := gitOpsFixture(t, false)
	res, err := f.svc.Delete(ctx, In(""), "gitops", false, WriteOptions{Mode: ModeCommit})
	require.NoError(t, err)
	require.Len(t, fake.PullRequests(), 1)
	assert.Equal(t, "feat(agents): remove agent gitops in kagent", fake.PullRequests()[0].Title)
	files := filesByPath(res.Commit)
	assert.Equal(t, "remove", files["agents/agent-manager/gitops.yaml"].Action)
	_, keptSource := files["agents/agent-manager/agent.yaml"]
	assert.False(t, keptSource && files["agents/agent-manager/agent.yaml"].Action == "remove", "the verifier release still uses the chart source")
	assert.Contains(t, res.OCIRepositoryKept, "verifier")
	head := fake.Files(fleetRepo, "agent-manager/delete-kagent-gitops")
	_, still := head["agents/agent-manager/gitops.yaml"]
	assert.False(t, still)
	require.Len(t, res.Commit.LiveSteps, 1, "a Kustomization that does not prune leaves the release")
	assert.True(t, strings.Contains(res.Commit.LiveSteps[0], "kubectl delete helmrelease -n kagent gitops"))
	_, err = f.svc.Get(ctx, In(""), "gitops")
	require.NoError(t, err, "commit mode deletes nothing live")
}

func str(s string) *string { return &s }
