package agents

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agent-manager/internal/identity"
	"github.com/giantswarm/agent-manager/internal/tenants"
)

// fakeTenants is the tenant reconciler as the service sees it.
type fakeTenants struct {
	namespaces []string
	status     tenants.Status
}

func (f *fakeTenants) Namespaces() []string   { return f.namespaces }
func (f *fakeTenants) ModelConfig() string    { return "default-model-config" }
func (f *fakeTenants) Status() tenants.Status { return f.status }

func withTenants(f *fixture, t *fakeTenants) {
	f.svc.cfg.Tenants = t
}

// A tenant namespace is managed beside the configured ones, and its first
// agent may name the platform's default ModelConfig before the reconciler
// copied it.
func TestTenantNamespaceIsManaged(t *testing.T) {
	f := seeded(t)
	withTenants(f, &fakeTenants{namespaces: []string{"org-a"}})
	ctx := context.Background()

	assert.Equal(t, []string{"kagent", "tenant", "org-a"}, f.svc.Managed())
	ns, err := f.svc.Namespace("org-a")
	require.NoError(t, err)
	assert.Equal(t, "org-a", ns)
	_, err = f.svc.Namespace("org-b")
	assert.ErrorIs(t, err, ErrInvalid)

	spec := Spec{Location: In("org-a"), Name: "sre", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"}}
	v, err := f.svc.ValidateCreate(ctx, spec)
	require.NoError(t, err)
	assert.True(t, v.Valid, "%v", v.Errors)
	_, err = f.svc.Create(ctx, spec)
	require.NoError(t, err)

	// Any other ModelConfig must exist there.
	_, err = f.svc.Create(ctx, Spec{Location: In("org-a"), Name: "other", ModelConfig: "qwen3-8-27b", Toolset: []string{"preset:read-only"}})
	assert.ErrorIs(t, err, ErrInvalid)
	// A configured namespace is not a tenant: no fallback.
	_, err = f.svc.Create(ctx, Spec{Location: In("tenant"), Name: "sre", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"}})
	assert.ErrorIs(t, err, ErrInvalid)
}

// get_info lists the managed namespaces in which the caller may create.
func TestInfoListsTheCreatableNamespaces(t *testing.T) {
	f := seeded(t)
	withTenants(f, &fakeTenants{namespaces: []string{"org-a", "org-b"}})
	allowCreate(f.typed, func(ns string) bool { return ns == "org-a" })
	assert.Equal(t, []string{"org-a"}, f.svc.Info(context.Background()).Namespaces.Managed)
}

// get_info is where an operator reads that the tenant reconciler failed
// while the API kept serving; without tenants it says nothing about one.
func TestInfoReportsTheTenantReconcilersFailure(t *testing.T) {
	f := seeded(t)
	assert.Nil(t, f.svc.Info(t.Context()).TenantReconciler)

	at := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	withTenants(f, &fakeTenants{status: tenants.Status{Error: "leader election lost", FailedAt: &at}})
	got := f.svc.Info(t.Context()).TenantReconciler
	require.NotNil(t, got)
	assert.Equal(t, "leader election lost", got.Error)
	assert.Equal(t, at, *got.FailedAt)
}

// The creator is recorded on create, on the HelmRelease and in the values
// the chart puts on the Agent; an update by someone else keeps it, and
// nobody can set or change it.
func TestCreatorAnnotation(t *testing.T) {
	f := seeded(t)
	alice := identity.ContextWith(context.Background(), &identity.Identity{Subject: "a", Email: "alice@example.com"})
	bob := identity.ContextWith(context.Background(), &identity.Identity{Subject: "b", Email: "bob@example.com"})

	_, err := f.svc.Create(alice, Spec{Name: "sre", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"},
		Annotations: map[string]string{CreatedByAnnotation: "mallory@example.com"}})
	assert.ErrorIs(t, err, ErrInvalid, "the caller cannot name the creator")

	res, err := f.svc.Create(alice, Spec{Name: "sre", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"}})
	require.NoError(t, err)
	assert.Equal(t, "alice@example.com", res.Agent.CreatedBy)
	assert.Contains(t, res.Manifests.HelmRelease, CreatedByAnnotation+": alice@example.com")
	hr, err := f.dyn.Resource(hrGVR).Namespace("kagent").Get(alice, "sre", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "alice@example.com", hr.GetAnnotations()[CreatedByAnnotation])
	assert.Equal(t, "alice@example.com", valuesCreator(t, hr.Object["spec"].(map[string]any)["values"].(map[string]any)), "the chart's annotations value")

	// Bob replaces the annotations: the creator stays.
	annotations := map[string]string{"team": "sre"}
	upd, err := f.svc.Update(bob, Update{Name: "sre", Annotations: &annotations})
	require.NoError(t, err)
	assert.Equal(t, "alice@example.com", valuesCreator(t, upd.After))
	assert.Equal(t, "sre", upd.After["annotations"].(map[string]any)["team"])
	hr, err = f.dyn.Resource(hrGVR).Namespace("kagent").Get(bob, "sre", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "alice@example.com", hr.GetAnnotations()[CreatedByAnnotation])

	// Clearing every annotation keeps it too; changing it is refused.
	empty := map[string]string{}
	upd, err = f.svc.Update(bob, Update{Name: "sre", Annotations: &empty})
	require.NoError(t, err)
	assert.Equal(t, "alice@example.com", valuesCreator(t, upd.After))
	steal := map[string]string{CreatedByAnnotation: "bob@example.com"}
	_, err = f.svc.Update(bob, Update{Name: "sre", Annotations: &steal})
	assert.ErrorIs(t, err, ErrInvalid)

	// An agent created before the annotation existed cannot be given one.
	_, err = f.svc.Update(bob, Update{Name: "verifier", Annotations: &steal})
	assert.ErrorIs(t, err, ErrInvalid)
}

// valuesCreator reads the creator where the chart reads it:
// annotations[CreatedByAnnotation] of the values.
func valuesCreator(t *testing.T, values map[string]any) string {
	t.Helper()
	creator, _, err := unstructured.NestedString(values, "annotations", CreatedByAnnotation)
	require.NoError(t, err)
	return creator
}

// The installation's skills credential exists in the configured namespaces
// only, so an agent in a tenant is not composed with it.
func TestTenantGetsNoInstallationSkillsCredential(t *testing.T) {
	f := seeded(t)
	withTenants(f, &fakeTenants{namespaces: []string{"org-a"}})
	f.svc.cfg.Compose.SkillsGitAuthSecretName = "skills-token"
	gitPlugin := Plugin{Path: "bundles/sre", Git: &GitSkill{URL: skillsRepo, Commit: tagHead}, Skills: []string{"triage"}}
	values := func(ns string) map[string]any {
		dry, err := f.svc.ValidateCreate(t.Context(), Spec{Location: In(ns), Name: "sre", ModelConfig: "default-model-config",
			Toolset: []string{"preset:read-only"}, Plugins: Plugins{gitPlugin}})
		require.NoError(t, err)
		require.True(t, dry.Valid, dry.Errors)
		return dry.Manifests.Values
	}
	assert.NotContains(t, values("org-a"), SkillsGitAuthValuesKey)
	assert.Equal(t, map[string]any{"name": "skills-token"}, values("kagent")[SkillsGitAuthValuesKey])
}
