package agents

import (
	"context"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agent-manager/internal/identity"
)

// fakeTenants is the organization namespace finder as the service sees it.
type fakeTenants struct {
	namespaces []string
}

func (f *fakeTenants) Names(context.Context) ([]string, error) { return f.namespaces, nil }
func (f *fakeTenants) Holds(_ context.Context, ns string) (bool, error) {
	return slices.Contains(f.namespaces, ns), nil
}

func withTenants(f *fixture, t *fakeTenants) {
	f.svc.cfg.Tenants = t
}

// An organization namespace is managed beside the configured ones; with the
// tenant reconciler gone, its agents name a ModelConfig that exists there.
func TestTenantNamespaceIsManaged(t *testing.T) {
	f := seeded(t)
	withTenants(f, &fakeTenants{namespaces: []string{"org-a"}})
	ctx := context.Background()

	assert.Equal(t, []string{"kagent", "tenant", "org-a"}, f.svc.Managed(ctx))
	ns, err := f.svc.Namespace(ctx, "org-a")
	require.NoError(t, err)
	assert.Equal(t, "org-a", ns)
	_, err = f.svc.Namespace(ctx, "org-b")
	assert.ErrorIs(t, err, ErrInvalid)

	// No platform default is accepted before it exists there any more.
	_, err = f.svc.Create(ctx, Spec{Location: In("org-a"), Name: "sre", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"}})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "set_organization_model")

	mustCreate(t, f, mcGVR, modelConfig("org-a", "own", "Anthropic", "claude-sonnet-4-6"))
	_, err = f.svc.Create(ctx, Spec{Location: In("org-a"), Name: "sre", ModelConfig: "own", Toolset: []string{"preset:read-only"}})
	require.NoError(t, err)
}

// get_info lists the managed namespaces in which the caller may create.
func TestInfoListsTheCreatableNamespaces(t *testing.T) {
	f := seeded(t)
	withTenants(f, &fakeTenants{namespaces: []string{"org-a", "org-b"}})
	allowCreate(f.typed, func(ns string) bool { return ns == "org-a" })
	info := f.svc.Info(context.Background())
	assert.Equal(t, []string{"org-a"}, info.Namespaces.Managed)
	assert.True(t, info.Capabilities["organizations"])
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
	mustCreate(t, f, mcGVR, modelConfig("org-a", "default-model-config", "Anthropic", "claude-sonnet-4-6"))
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

// An agent in a tenant written before the credential step names the
// installation's skills credential, under its current or a previous name:
// admission refuses either there, so an update drops the reference and
// keeps a tenant's own credential.
func TestTenantUpdateDropsTheInstallationSkillsCredential(t *testing.T) {
	f := seeded(t)
	withTenants(f, &fakeTenants{namespaces: []string{"org-a"}})
	mustCreate(t, f, mcGVR, modelConfig("org-a", "default-model-config", "Anthropic", "claude-sonnet-4-6"))
	f.svc.cfg.Compose.SkillsGitAuthSecretName = "agent-manager-skills-token"
	f.svc.cfg.Compose.SkillsGitAuthPreviousSecretNames = []string{"tenant-credential-agent-manager-skills-token"} // #nosec G101 -- a Secret name, not a credential
	ctx := t.Context()
	gitPlugin := Plugin{Path: "bundles/sre", Git: &GitSkill{URL: skillsRepo, Commit: tagHead}, Skills: []string{"triage"}}
	_, err := f.svc.Create(ctx, Spec{Location: In("org-a"), Name: "sre", ModelConfig: "default-model-config",
		Toolset: []string{"preset:read-only"}, Plugins: Plugins{gitPlugin}})
	require.NoError(t, err)

	hrs := f.dyn.Resource(hrGVR).Namespace("org-a")
	for _, ref := range []string{"agent-manager-skills-token", "tenant-credential-agent-manager-skills-token", "tenant-credential-own"} {
		hr, err := hrs.Get(ctx, "sre", metav1.GetOptions{})
		require.NoError(t, err)
		require.NoError(t, unstructured.SetNestedField(hr.Object, map[string]any{"name": ref}, "spec", "values", SkillsGitAuthValuesKey))
		_, err = hrs.Update(ctx, hr, metav1.UpdateOptions{})
		require.NoError(t, err)

		desc := "updated with " + ref
		res, err := f.svc.Update(ctx, Update{Location: In("org-a"), Name: "sre", Description: &desc})
		require.NoError(t, err, ref)
		hr, err = hrs.Get(ctx, "sre", metav1.GetOptions{})
		require.NoError(t, err)
		if ref == "tenant-credential-own" {
			assert.Equal(t, map[string]any{"name": ref}, mustValues(hr)[SkillsGitAuthValuesKey], ref)
			continue
		}
		assert.NotContains(t, res.After, SkillsGitAuthValuesKey, ref)
		assert.NotContains(t, mustValues(hr), SkillsGitAuthValuesKey, ref)
	}
}
