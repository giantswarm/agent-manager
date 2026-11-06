package agents

import (
	"testing"

	apov1alpha1 "github.com/giantswarm/agent-platform-operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

func markedModel(ns, name string) *unstructured.Unstructured {
	mc := modelConfig(ns, name, "Anthropic", "claude-sonnet-4-6")
	mc.SetAnnotations(map[string]string{apov1alpha1.DefaultModelConfigAnnotation: "true"})
	return mc
}

// modelFixture is a namespace kagent with its platform Harness and the given
// ModelConfigs.
func modelFixture(t *testing.T, models ...*unstructured.Unstructured) *fixture {
	t.Helper()
	objs := []runtime.Object{harness("kagent", "kagent", "kagent")}
	for _, mc := range models {
		objs = append(objs, mc)
	}
	return newFixture(t, objs)
}

func TestTheNamespaceDefaultModel(t *testing.T) {
	tests := []struct {
		name   string
		models []*unstructured.Unstructured
		// want is the default; empty: none.
		want    string
		refusal string
	}{
		{
			name:   "one marked",
			models: []*unstructured.Unstructured{markedModel("kagent", "claude"), modelConfig("kagent", "qwen", "OpenAI", "qwen3")},
			want:   "claude",
		},
		{
			name:    "none marked",
			models:  []*unstructured.Unstructured{modelConfig("kagent", "claude", "Anthropic", "claude-sonnet-4-6"), modelConfig("kagent", "qwen", "OpenAI", "qwen3")},
			refusal: "modelConfig: namespace kagent has no default model; pass modelConfig, or have an admin mark one as the default (annotation " + apov1alpha1.DefaultModelConfigAnnotation + "=true). ModelConfigs: claude, qwen",
		},
		{
			name:    "two marked",
			models:  []*unstructured.Unstructured{markedModel("kagent", "claude"), markedModel("kagent", "qwen")},
			refusal: "modelConfig: namespace kagent has no default model, claude, qwen are all marked as the default; pass modelConfig, or have an admin mark one as the default (annotation " + apov1alpha1.DefaultModelConfigAnnotation + "=true). ModelConfigs: claude, qwen",
		},
		{
			name:    "default-model-config unmarked",
			models:  []*unstructured.Unstructured{modelConfig("kagent", "default-model-config", "Anthropic", "claude-sonnet-4-6"), modelConfig("kagent", "qwen", "OpenAI", "qwen3")},
			refusal: "modelConfig: namespace kagent has no default model; pass modelConfig, or have an admin mark one as the default (annotation " + apov1alpha1.DefaultModelConfigAnnotation + "=true). ModelConfigs: default-model-config, qwen",
		},
		{
			name:    "no ModelConfigs",
			refusal: "modelConfig: namespace kagent has no ModelConfigs, so it has no default model; a platform admin adds one in the portal",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := modelFixture(t, tt.models...)
			ctx := t.Context()

			list, err := f.svc.ListModelConfigs(ctx, In("kagent"))
			require.NoError(t, err)
			var defaults []string
			for _, mc := range list {
				if mc.Default {
					defaults = append(defaults, mc.Name)
				}
			}
			if tt.want == "" {
				assert.Empty(t, defaults)
			} else {
				assert.Equal(t, []string{tt.want}, defaults)
			}

			res, err := f.svc.Create(ctx, Spec{Name: "sre", Toolset: []string{"preset:read-only"}})
			if tt.refusal != "" {
				require.ErrorIs(t, err, ErrInvalid)
				assert.Equal(t, "invalid request: "+tt.refusal, err.Error())
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, res.ModelConfig)
			assert.True(t, res.ModelConfigFromDefault)
			hr, err := f.dyn.Resource(hrGVR).Namespace("kagent").Get(ctx, "sre", metav1.GetOptions{})
			require.NoError(t, err)
			name, _, _ := unstructured.NestedString(hr.Object, "spec", "values", "modelConfig", "name")
			assert.Equal(t, tt.want, name, "the agent is pinned to the concrete name")
		})
	}
}

func TestListModelConfigsReportsResolvedRefs(t *testing.T) {
	unresolved := markedModel("kagent", "claude")
	unresolved.Object["status"] = map[string]any{"conditions": []any{
		map[string]any{"type": "Accepted", "status": "True"},
		map[string]any{"type": "ResolvedRefs", "status": "False", "message": "secret kagent-anthropic not found"},
	}}
	unreported := modelConfig("kagent", "qwen", "OpenAI", "qwen3")
	delete(unreported.Object, "status")
	f := modelFixture(t, unresolved, unreported, modelConfig("kagent", "sonnet", "Anthropic", "claude-sonnet-4-6"))

	list, err := f.svc.ListModelConfigs(t.Context(), In("kagent"))
	require.NoError(t, err)
	require.Len(t, list, 3)
	assert.Equal(t, ModelConfig{Name: "claude", Namespace: "kagent", Provider: "Anthropic", Model: "claude-sonnet-4-6", Default: true, Accepted: new(true), ResolvedRefs: new(false)}, list[0])
	assert.Equal(t, ModelConfig{Name: "qwen", Namespace: "kagent", Provider: "OpenAI", Model: "qwen3"}, list[1])
	assert.Equal(t, new(true), list[2].ResolvedRefs)
	assert.False(t, list[2].Default)
}

func TestValidateCreateReportsTheModelItWouldWrite(t *testing.T) {
	unresolved := markedModel("kagent", "claude")
	unresolved.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "ResolvedRefs", "status": "False"}}}
	f := modelFixture(t, unresolved, modelConfig("kagent", "qwen", "OpenAI", "qwen3"))
	ctx := t.Context()

	res, err := f.svc.ValidateCreate(ctx, Spec{Name: "sre", Toolset: []string{"preset:read-only"}})
	require.NoError(t, err)
	assert.True(t, res.Valid, res.Errors)
	assert.Equal(t, "claude", res.ModelConfig)
	assert.True(t, res.ModelConfigFromDefault)
	assert.Equal(t, []string{"modelConfig claude is not ResolvedRefs=True (False): the agent does not run until it is"}, res.Notes)
	assert.Equal(t, map[string]any{"name": "claude"}, res.Manifests.Values["modelConfig"])

	res, err = f.svc.ValidateCreate(ctx, Spec{Name: "sre", ModelConfig: "qwen", Toolset: []string{"preset:read-only"}})
	require.NoError(t, err)
	assert.Equal(t, "qwen", res.ModelConfig)
	assert.False(t, res.ModelConfigFromDefault)
	assert.Empty(t, res.Notes)
}

func TestCreateWithANamedModelKeepsIt(t *testing.T) {
	f := modelFixture(t, markedModel("kagent", "claude"), modelConfig("kagent", "qwen", "OpenAI", "qwen3"))

	res, err := f.svc.Create(t.Context(), Spec{Name: "sre", ModelConfig: "qwen", Toolset: []string{"preset:read-only"}})
	require.NoError(t, err)
	assert.Equal(t, "qwen", res.ModelConfig)
	assert.False(t, res.ModelConfigFromDefault)

	_, err = f.svc.Create(t.Context(), Spec{Name: "other", ModelConfig: "nope", Toolset: []string{"preset:read-only"}})
	require.EqualError(t, err, `invalid request: modelConfig "nope" does not exist in namespace kagent; valid: claude, qwen`)
}

func TestUpdateNeverResolvesTheDefault(t *testing.T) {
	f := modelFixture(t, modelConfig("kagent", "qwen", "OpenAI", "qwen3"))
	ctx := t.Context()
	_, err := f.svc.Create(ctx, Spec{Name: "sre", ModelConfig: "qwen", Toolset: []string{"preset:read-only"}})
	require.NoError(t, err)
	mustCreate(t, f, mcGVR, markedModel("kagent", "claude"))

	desc := "helps"
	res, err := f.svc.Update(ctx, Update{Name: "sre", Description: &desc})
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"name": "qwen"}, res.After["modelConfig"], "a new default does not move the agent")

	_, err = f.svc.Update(ctx, Update{Name: "sre", ModelConfig: new("")})
	require.EqualError(t, err, "invalid request: modelConfig is required")
}

// A tenant owns its ModelConfigs: without one, the refusal names who adds it.
func TestCreateInATenantWithoutModels(t *testing.T) {
	f := modelFixture(t)
	withTenants(f, &fakeTenants{namespaces: []string{"org-acme"}})

	_, err := f.svc.Create(t.Context(), Spec{Location: In("org-acme"), Name: "sre", Toolset: []string{"preset:none"}})
	require.EqualError(t, err, "invalid request: modelConfig: namespace org-acme has no ModelConfigs, so it has no default model; an admin of the organization adds one in the portal or with set_organization_model")

	_, err = f.svc.Create(t.Context(), Spec{Location: In("org-acme"), Name: "sre", ModelConfig: "default-model-config", Toolset: []string{"preset:none"}})
	require.EqualError(t, err, `invalid request: modelConfig "default-model-config" does not exist in namespace org-acme (no ModelConfigs there; an admin of the organization adds one in the portal or with set_organization_model)`)
}
