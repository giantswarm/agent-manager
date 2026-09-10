package chart

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/agent-manager/internal/oci"
	"github.com/giantswarm/agent-manager/internal/oci/ocitest"
)

func newFake(t *testing.T) *ocitest.Registry {
	f := ocitest.New(t, "charts/giantswarm/agent")
	f.Tags = []string{"0.1.0", "0.6.1", "1.0.0-dev.kagent-v2.2026-09-11.01-00-00.habcdef0", "1.0.0", "1.2.3", "artifacthub.io", "2.0.0"}
	f.Files["1.2.3"] = map[string]string{"Chart.yaml": "name: agent\nversion: 1.2.3\n", "values.schema.json": `{"type":"object","properties":{"agent":{"type":"object"}}}`}
	f.Files["2.0.0"] = map[string]string{"Chart.yaml": "name: agent\nversion: 2.0.0\n", "values.schema.json": `{"type":"object"}`}
	return f
}

func TestLatestFollowsFluxSemverSemantics(t *testing.T) {
	tags := []string{"0.1.0", "0.6.1", "1.0.0-dev.kagent-v2.2026-09-11.01-00-00.habcdef0", "1.0.0", "1.2.3", "artifacthub.io", "2.0.0"}
	v, err := Latest(tags, "x.x.x")
	require.NoError(t, err)
	assert.Equal(t, "2.0.0", v, "x.x.x tracks every stable release")
	v, err = Latest(tags, "1.x")
	require.NoError(t, err)
	assert.Equal(t, "1.2.3", v, "1.x stays inside the major and skips pre-releases")
	v, err = Latest(tags, "0.x")
	require.NoError(t, err)
	assert.Equal(t, "0.6.1", v)
	_, err = Latest([]string{"1.0.0-dev.kagent-v2.2026-09-11.01-00-00.habcdef0"}, "1.x")
	assert.Error(t, err, "a branch build never satisfies 1.x: the lab pins the exact version instead")
	v, err = Latest([]string{"1.0.0-dev.kagent-v2.2026-09-11.01-00-00.habcdef0"}, ">=1.0.0-0 <2.0.0")
	require.NoError(t, err)
	assert.Equal(t, "1.0.0-dev.kagent-v2.2026-09-11.01-00-00.habcdef0", v, "a pre-release-aware range does")
	_, err = Latest([]string{"artifacthub.io"}, "x.x.x")
	assert.Error(t, err)
}

func TestResolverPrefersTheRegistryAndFallsBackToTheEmbeddedSchema(t *testing.T) {
	f := newFake(t)
	chartURL := "oci://" + f.Host() + "/" + f.Repo
	r, err := NewResolver(chartURL, "1.x", time.Hour, oci.NewRegistry(nil), nil)
	require.NoError(t, err)
	r.ref.Insecure = true

	s := r.Schema(context.Background())
	assert.Equal(t, SourceRegistry, s.Source)
	assert.Equal(t, "1.2.3", s.Version)
	info := r.Info(context.Background())
	assert.Equal(t, "1.2.3", info.LatestVersion)
	assert.Equal(t, "1.x", info.Semver)
	assert.Empty(t, info.Error)

	// A broken registry on a fresh resolver: the embedded copy validates.
	f.FailTags = true
	r2, err := NewResolver(chartURL, "1.x", time.Hour, oci.NewRegistry(nil), nil)
	require.NoError(t, err)
	r2.ref.Insecure = true
	s2 := r2.Schema(context.Background())
	assert.Equal(t, SourceEmbedded, s2.Source)
	assert.Equal(t, EmbeddedSchemaVersion, s2.Version)
	info2 := r2.Info(context.Background())
	assert.NotEmpty(t, info2.Error)
	assert.Empty(t, info2.LatestVersion)
	props, ok := s2.Document.(map[string]any)["properties"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, props, "modelConfig", "the embedded copy is the real agent chart schema")

	_, err = NewResolver(chartURL, "not a range", time.Hour, nil, nil)
	assert.Error(t, err)
}

// TestEmbeddedSchemaIsTheChart1xContract pins the shape of the fallback schema
// to the Generic chart 1.x values contract: the 0.x runtime keys are gone,
// skills are a list of pinned sources, muster has url and tools.
func TestEmbeddedSchemaIsTheChart1xContract(t *testing.T) {
	doc := EmbeddedSchema().Document.(map[string]any)
	assert.Equal(t, false, doc["additionalProperties"], "a stale composer sending a removed key fails, never silently loses a field")
	props := doc["properties"].(map[string]any)
	for _, removed := range []string{"replicas", "resources", "nodeSelector", "tolerations"} {
		assert.NotContains(t, props, removed)
	}
	agent := props["agent"].(map[string]any)["properties"].(map[string]any)
	assert.NotContains(t, agent, "runtime")
	for _, kept := range []string{"name", "displayName", "description", "iconUrl", "systemMessage"} {
		assert.Contains(t, agent, kept)
	}
	muster := props["muster"].(map[string]any)["properties"].(map[string]any)
	for _, removed := range []string{"serverRef", "allowedHeaders", "stsWellKnownUri", "toolNames"} {
		assert.NotContains(t, muster, removed)
	}
	for _, kept := range []string{"enabled", "tools", "url"} {
		assert.Contains(t, muster, kept)
	}
	skills := props["skills"].(map[string]any)
	assert.Equal(t, "array", skills["type"], "skills is a list of pinned sources, not the 0.x refs/gitRefs object")
	assert.Equal(t, "#/$defs/skill.schema.json", skills["items"].(map[string]any)["$ref"], "the chart bundles its skill schema")
	skill := doc["$defs"].(map[string]any)["skill.schema.json"].(map[string]any)["properties"].(map[string]any)
	assert.NotContains(t, skill, "gitAuthSecretRef")
	for _, kept := range []string{"name", "git", "oci", "path"} {
		assert.Contains(t, skill, kept)
	}
	assert.Contains(t, agent, "harness", "the Harness admission label's value")
	for _, kept := range []string{"toolset", "extraTools", "extraAgentSpec", "labels", "annotations", "modelConfig"} {
		assert.Contains(t, props, kept)
	}
}
