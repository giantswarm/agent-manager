package agents

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/giantswarm/agent-manager/internal/chart"
)

// embeddedChart validates against the compiled-in agent chart schema.
type embeddedChart struct{}

func (embeddedChart) Schema(context.Context) chart.Schema { return chart.EmbeddedSchema() }
func (embeddedChart) Info(context.Context) chart.Info {
	return chart.Info{OCIURL: DefaultChartOCIURL, Semver: DefaultChartSemver, SchemaVersion: chart.EmbeddedSchemaVersion, SchemaSource: chart.SourceEmbedded}
}
func (embeddedChart) Name() string        { return "agent" }
func (embeddedChart) OCIURL() string      { return DefaultChartOCIURL }
func (embeddedChart) SemverRange() string { return DefaultChartSemver }

func TestBuildValuesMirrorsComposeManifests(t *testing.T) {
	// The portal emits only what the user set: the chart's defaults cover the
	// rest, and an empty prompt means "the chart's default prompt".
	minimal := BuildValues(Spec{Name: "sre", ModelConfig: "default-model-config", Description: "  "})
	assert.Equal(t, map[string]any{
		"agent":       map[string]any{"name": "sre"},
		"modelConfig": map[string]any{"name": "default-model-config"},
	}, minimal)

	full := BuildValues(Spec{
		Name: "sre", DisplayName: "SRE Assistant", Description: "helps", SystemMessage: "Be brief.", ModelConfig: "mc",
		IconURL: "https://avatars.example/v1/sre.png", Runtime: "python",
		Skills:    &Skills{GitRefs: []SkillGitRef{{URL: "https://github.com/giantswarm/agent-skills", Path: "nested/runbooks", Ref: "main"}}, Refs: []string{"registry/skill:1"}},
		ToolNames: []string{"x_mcp-kubernetes_get_pods"},
		Labels:    map[string]string{"tenant": "sre"},
	})
	assert.Equal(t, map[string]any{
		"agent": map[string]any{
			"name": "sre", "displayName": "SRE Assistant", "description": "helps", "systemMessage": "Be brief.",
			"iconUrl": "https://avatars.example/v1/sre.png", "runtime": "python",
		},
		"modelConfig": map[string]any{"name": "mc"},
		"skills": map[string]any{
			"refs":    []any{"registry/skill:1"},
			"gitRefs": []any{map[string]any{"url": "https://github.com/giantswarm/agent-skills", "path": "nested/runbooks", "ref": "main", "name": "runbooks"}},
		},
		"muster": map[string]any{"toolNames": []any{"x_mcp-kubernetes_get_pods"}},
		"labels": map[string]any{"tenant": "sre"},
	}, full)

	_, violations := ValidateValues(context.Background(), embeddedChart{}, full)
	assert.Empty(t, violations, "the composed values satisfy the agent chart schema")
}

func TestSkillName(t *testing.T) {
	assert.Equal(t, "explicit", SkillName(SkillGitRef{Name: "explicit", Path: "a/b"}))
	assert.Equal(t, "b", SkillName(SkillGitRef{URL: "https://github.com/o/r", Path: "a/b/"}))
	assert.Equal(t, "r", SkillName(SkillGitRef{URL: "https://github.com/o/r.git"}))
}

func TestBuildHelmReleaseAndOCIRepositoryMatchThePortal(t *testing.T) {
	cfg := ComposeConfig{ChartOCIURL: DefaultChartOCIURL, ChartName: "agent", ChartSemver: "x.x.x"}
	values := BuildValues(Spec{Name: "sre", ModelConfig: "mc"})

	hr := BuildHelmRelease("sre", "kagent", values, cfg)
	var got map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(ToYAML(hr)), &got))
	assert.Equal(t, "helm.toolkit.fluxcd.io/v2", got["apiVersion"])
	assert.Equal(t, "HelmRelease", got["kind"])
	spec := got["spec"].(map[string]any)
	assert.Equal(t, "10m", spec["interval"])
	assert.Equal(t, map[string]any{"kind": "OCIRepository", "name": "agent", "namespace": "kagent"}, spec["chartRef"])
	assert.Equal(t, values, spec["values"])
	_, hasSA := spec["serviceAccountName"]
	assert.False(t, hasSA, "serviceAccountName is omitted unless configured")

	cfg.ServiceAccountName = "tenant"
	withSA := BuildHelmRelease("sre", "kagent", values, cfg)
	sa, _, _ := nestedString(withSA.Object, "spec", "serviceAccountName")
	assert.Equal(t, "tenant", sa)

	repo := BuildOCIRepository("kagent", cfg)
	require.NoError(t, yaml.Unmarshal([]byte(ToYAML(repo)), &got))
	assert.Equal(t, "source.toolkit.fluxcd.io/v1", got["apiVersion"])
	assert.Equal(t, "OCIRepository", got["kind"])
	assert.Equal(t, map[string]any{"name": "agent", "namespace": "kagent", "labels": map[string]any{ManagedByLabel: ManagedByValue}}, got["metadata"])
	assert.Equal(t, map[string]any{"interval": "30m", "url": DefaultChartOCIURL, "ref": map[string]any{"semver": "x.x.x"}}, got["spec"])
}

func TestValidateValuesReportsSchemaViolations(t *testing.T) {
	long := ""
	for i := 0; i < 64; i++ {
		long += "x"
	}
	values := map[string]any{
		"agent":       map[string]any{"name": "Bad Name", "displayName": long, "runtime": "rust"},
		"modelConfig": map[string]any{"name": ""},
		"unknownKey":  true,
	}
	sch, violations := ValidateValues(context.Background(), embeddedChart{}, values)
	assert.Equal(t, chart.SourceEmbedded, sch.Source)
	joined := ""
	for _, v := range violations {
		joined += v + "\n"
	}
	assert.Contains(t, joined, "/agent/name")
	assert.Contains(t, joined, "/agent/displayName")
	assert.Contains(t, joined, "/agent/runtime")
	assert.Contains(t, joined, "/modelConfig/name")
	assert.Contains(t, joined, "unknownKey")
}

func TestValidateName(t *testing.T) {
	assert.NoError(t, ValidateName("sre-agent-1"))
	assert.ErrorIs(t, ValidateName(""), ErrInvalid)
	assert.ErrorIs(t, ValidateName("SRE"), ErrInvalid)
	assert.ErrorIs(t, ValidateName("-sre"), ErrInvalid)
	assert.ErrorIs(t, ValidateName("a.b"), ErrInvalid)
}

func nestedString(obj map[string]any, fields ...string) (string, bool, error) {
	cur := any(obj)
	for _, f := range fields {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false, nil
		}
		cur, ok = m[f]
		if !ok {
			return "", false, nil
		}
	}
	s, ok := cur.(string)
	return s, ok, nil
}
