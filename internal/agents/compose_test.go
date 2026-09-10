package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/giantswarm/agent-manager/internal/chart"
	"github.com/giantswarm/agent-manager/internal/skills"
)

// The pins the fake resolver hands out.
const (
	mainHead    = "0123456789abcdef0123456789abcdef01234567"
	featureHead = "89abcdef0123456789abcdef0123456789abcdef"
	tagHead     = "fedcba9876543210fedcba9876543210fedcba98"
	kubectlRef  = "ghcr.io/giantswarm/skills/kubectl"
	kubectlSum  = "sha256:5b0bcabd1ed22e9fb1310cf6c2dec7cdef19f0ad69efa1f392e94a4333501270"
	skillsRepo  = "https://github.com/giantswarm/agent-skills"
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

// fakePinner resolves the refs of skillsRepo and the kubectl image; every
// other reference is unresolvable, the way a private repository without a
// token is.
type fakePinner struct {
	calls []string
}

func (p *fakePinner) GitHead(_ context.Context, repoURL, ref string) (string, error) {
	p.calls = append(p.calls, "git "+repoURL+"@"+ref)
	if repoURL != skillsRepo {
		return "", fmt.Errorf("%w: ref %s of %s could not be resolved: GitHub answered 404 without a token", skills.ErrUnresolvable, ref, repoURL)
	}
	switch ref {
	case "", "main":
		return mainHead, nil
	case "feature":
		return featureHead, nil
	case "v1.2.0":
		return tagHead, nil
	}
	if skills.CommitPattern.MatchString(ref) {
		return ref, nil
	}
	return "", fmt.Errorf("%w: ref %s of %s could not be resolved: unknown ref", skills.ErrUnresolvable, ref, repoURL)
}

func (p *fakePinner) OCIDigest(_ context.Context, ref string) (string, error) {
	p.calls = append(p.calls, "oci "+ref)
	if strings.HasPrefix(ref, kubectlRef) {
		return kubectlRef + "@" + kubectlSum, nil
	}
	return "", fmt.Errorf("%w: OCI skill %s could not be resolved to a digest", skills.ErrUnresolvable, ref)
}

func TestBuildValuesIsTheChart1xContract(t *testing.T) {
	cfg := ComposeConfig{ChartSemver: DefaultChartSemver}
	// The portal emits only what the user set: the chart's defaults cover the
	// rest, and an empty prompt means "the chart's default prompt".
	minimal := BuildValues(Spec{Name: "sre", ModelConfig: "default-model-config", Description: "  "}, cfg)
	assert.Equal(t, map[string]any{
		"agent":       map[string]any{"name": "sre", "harness": "kagent"},
		"modelConfig": map[string]any{"name": "default-model-config"},
	}, minimal, "the platform Harness is always composed, the caller's fields only when set")

	pinned := Skills{
		{Name: "runbooks", Path: "nested/runbooks", Git: &GitSkill{URL: skillsRepo, Commit: mainHead}},
		{Name: "kubectl", OCI: kubectlRef + "@" + kubectlSum},
	}
	full := BuildValues(Spec{
		Name: "sre", DisplayName: "SRE Assistant", Description: "helps", SystemMessage: "Be brief.", ModelConfig: "mc",
		IconURL: "https://avatars.example/v1/sre.png",
		Skills:  pinned,
		Toolset: []string{"preset:read-only", "workflow:incident-triage"},
		Labels:  map[string]string{"tenant": "sre"},
	}, ComposeConfig{MusterURL: "http://muster.agent-platform.svc.cluster.local:8090/mcp", HarnessName: "claude"})
	assert.Equal(t, map[string]any{
		"agent": map[string]any{
			"name": "sre", "displayName": "SRE Assistant", "description": "helps", "systemMessage": "Be brief.",
			"iconUrl": "https://avatars.example/v1/sre.png", "harness": "claude",
		},
		"modelConfig": map[string]any{"name": "mc"},
		"skills": []any{
			map[string]any{"name": "runbooks", "path": "nested/runbooks", "git": map[string]any{"url": skillsRepo, "commit": mainHead}},
			map[string]any{"name": "kubectl", "oci": kubectlRef + "@" + kubectlSum},
		},
		"toolset": []any{"preset:read-only", "workflow:incident-triage"},
		"muster":  map[string]any{"url": "http://muster.agent-platform.svc.cluster.local:8090/mcp"},
		"labels":  map[string]any{"tenant": "sre"},
	}, full)
	_, violations := ValidateValues(context.Background(), embeddedChart{}, full)
	assert.Empty(t, violations, "the composed values satisfy the Generic chart 1.x schema")

	// Nothing the 1.x contract removed is ever emitted.
	for _, values := range []map[string]any{minimal, full} {
		for _, p := range RemovedValuePaths {
			assertNoPath(t, values, p)
		}
		for from := range RenamedValuePaths {
			assertNoPath(t, values, from)
		}
	}
	// The read model reads its skills back from the values it wrote.
	assert.Equal(t, pinned, skillsFromValues(full))
}

func assertNoPath(t *testing.T, values map[string]any, dotted string) {
	t.Helper()
	cur := any(values)
	for _, seg := range strings.Split(dotted, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return
		}
		cur, ok = m[seg]
		if !ok {
			return
		}
	}
	t.Errorf("removed value %s is emitted: %v", dotted, cur)
}

func TestBuildHelmReleaseAndOCIRepositoryTrackTheChartRange(t *testing.T) {
	cfg := ComposeConfig{ChartOCIURL: DefaultChartOCIURL, ChartName: "agent", ChartSemver: DefaultChartSemver}
	values := BuildValues(Spec{Name: "sre", ModelConfig: "mc"}, cfg)

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
	assert.Equal(t, map[string]any{"interval": "30m", "url": DefaultChartOCIURL, "ref": map[string]any{"semver": "1.x"}}, got["spec"], "the range is 1.x, never x.x.x")
	assert.Equal(t, "1.x", DefaultChartSemver)
}

func TestValidateValuesReportsSchemaViolations(t *testing.T) {
	values := map[string]any{
		"agent":       map[string]any{"name": "Bad Name", "displayName": strings.Repeat("x", 64), "runtime": "go"},
		"modelConfig": map[string]any{"name": ""},
		"replicas":    2,
		"muster":      map[string]any{"serverRef": map[string]any{"name": "muster"}, "toolNames": []any{"a"}},
		"skills": []any{
			map[string]any{"name": "branch", "git": map[string]any{"url": skillsRepo, "commit": "main"}},
			map[string]any{"name": "tag", "oci": kubectlRef + ":1.4.0"},
			map[string]any{"name": "both", "git": map[string]any{"url": skillsRepo, "commit": mainHead}, "oci": kubectlRef + "@" + kubectlSum},
			map[string]any{"name": "climb", "path": "../etc", "git": map[string]any{"url": skillsRepo, "commit": mainHead}},
		},
	}
	sch, violations := ValidateValues(context.Background(), embeddedChart{}, values)
	assert.Equal(t, chart.SourceEmbedded, sch.Source)
	joined := strings.Join(violations, "\n")
	for _, want := range []string{"/agent/name", "/agent/displayName", "/modelConfig/name",
		"/agent: additional properties 'runtime'", "(root): additional properties 'replicas'",
		"/muster: additional properties", "/skills/0/git/commit", "/skills/1/oci", "/skills/2", "/skills/3/path"} {
		assert.Contains(t, joined, want)
	}
}

func TestValidateName(t *testing.T) {
	assert.NoError(t, ValidateName("sre-agent-1"))
	assert.ErrorIs(t, ValidateName(""), ErrInvalid)
	assert.ErrorIs(t, ValidateName("SRE"), ErrInvalid)
	assert.ErrorIs(t, ValidateName("-sre"), ErrInvalid)
	assert.ErrorIs(t, ValidateName("a.b"), ErrInvalid)
}

func TestValidateSkills(t *testing.T) {
	assert.NoError(t, ValidateSkills(nil))
	assert.NoError(t, ValidateSkills(Skills{
		{Git: &GitSkill{URL: skillsRepo, Ref: "main"}, Path: "runbooks"},
		{Git: &GitSkill{URL: skillsRepo, Commit: mainHead}},
		{OCI: kubectlRef + ":1.4.0"},
	}))
	for name, tc := range map[string]struct {
		list Skills
		want string
	}{
		"no source":            {Skills{{Name: "x"}}, "no source"},
		"both sources":         {Skills{{Git: &GitSkill{URL: skillsRepo}, OCI: kubectlRef}}, "both git and oci"},
		"not http":             {Skills{{Git: &GitSkill{URL: "git@github.com:o/r.git"}}}, "http(s) URL"},
		"short sha":            {Skills{{Git: &GitSkill{URL: skillsRepo, Commit: "abc123"}}}, "not a full commit id"},
		"no registry":          {Skills{{OCI: "kubectl:1.4.0"}}, "<registry>/<repository>"},
		"bare image name":      {Skills{{OCI: "skills/kubectl:1.4.0"}}, "registry host"},
		"oci with path":        {Skills{{OCI: kubectlRef + ":1", Path: "x"}}, "path is for git skills"},
		"absolute path":        {Skills{{Git: &GitSkill{URL: skillsRepo}, Path: "/etc"}}, "relative"},
		"climbing path":        {Skills{{Git: &GitSkill{URL: skillsRepo}, Path: "a/../b"}}, `".."`},
		"duplicate mount name": {Skills{{Git: &GitSkill{URL: skillsRepo}, Path: "a/runbooks"}, {Git: &GitSkill{URL: skillsRepo}, Path: "b/runbooks"}}, `both mount as "runbooks"`},
	} {
		t.Run(name, func(t *testing.T) {
			err := ValidateSkills(tc.list)
			require.ErrorIs(t, err, ErrInvalid)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestPinSkillsResolvesEveryMutableReference(t *testing.T) {
	ctx := context.Background()
	p := &fakePinner{}
	pinned, err := pinSkills(ctx, p, Skills{
		{Git: &GitSkill{URL: skillsRepo}, Path: "nested/runbooks"},               // default branch
		{Git: &GitSkill{URL: skillsRepo, Ref: "feature"}, Path: "a", Name: "fa"}, // a branch
		{Git: &GitSkill{URL: skillsRepo, Ref: "v1.2.0"}},                         // a tag
		{Git: &GitSkill{URL: skillsRepo, Commit: tagHead}, Name: "pinned"},       // already a pin: no lookup
		{OCI: kubectlRef + ":1.4.0"},                                             // an image tag
		{OCI: kubectlRef + "@" + kubectlSum, Name: "digest"},                     // already a digest: no lookup
	}, false)
	require.NoError(t, err)
	assert.Equal(t, Skills{
		{Name: "runbooks", Path: "nested/runbooks", Git: &GitSkill{URL: skillsRepo, Commit: mainHead}},
		{Name: "fa", Path: "a", Git: &GitSkill{URL: skillsRepo, Commit: featureHead}},
		{Name: "agent-skills", Git: &GitSkill{URL: skillsRepo, Commit: tagHead}},
		{Name: "pinned", Git: &GitSkill{URL: skillsRepo, Commit: tagHead}},
		{Name: "kubectl", OCI: kubectlRef + "@" + kubectlSum},
		{Name: "digest", OCI: kubectlRef + "@" + kubectlSum},
	}, pinned, "refs are gone, pins are what is written")
	assert.Equal(t, []string{"git " + skillsRepo + "@", "git " + skillsRepo + "@feature", "git " + skillsRepo + "@v1.2.0", "oci " + kubectlRef + ":1.4.0"}, p.calls, "a pin given is never looked up")

	// refresh: every git skill goes to the head of its ref, the default
	// branch unless the request names one; a commit given is replaced too.
	p = &fakePinner{}
	refreshed, err := pinSkills(ctx, p, Skills{
		{Name: "runbooks", Git: &GitSkill{URL: skillsRepo, Commit: tagHead}},
		{Name: "fa", Git: &GitSkill{URL: skillsRepo, Ref: "feature"}},
		{Name: "kubectl", OCI: kubectlRef + "@" + kubectlSum},
	}, true)
	require.NoError(t, err)
	assert.Equal(t, mainHead, refreshed[0].Git.Commit)
	assert.Equal(t, featureHead, refreshed[1].Git.Commit)
	assert.Equal(t, kubectlRef+"@"+kubectlSum, refreshed[2].OCI, "OCI skills are not touched by a refresh")

	// ref and commit together are ambiguous.
	_, err = pinSkills(ctx, p, Skills{{Git: &GitSkill{URL: skillsRepo, Ref: "main", Commit: mainHead}}}, false)
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "both ref")

	// A private repository nobody can read: the error names it, nothing crashes.
	_, err = pinSkills(ctx, p, Skills{{Git: &GitSkill{URL: "https://github.com/giantswarm/private-skills", Ref: "main"}}}, false)
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "https://github.com/giantswarm/private-skills")
	assert.Contains(t, err.Error(), "404")
	_, err = pinSkills(ctx, p, Skills{{OCI: "ghcr.io/nobody/skill:1"}}, false)
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "ghcr.io/nobody/skill:1")

	// Without a resolver only pins pass.
	_, err = pinSkills(ctx, nil, Skills{{Git: &GitSkill{URL: skillsRepo, Ref: "main"}}}, false)
	assert.ErrorIs(t, err, ErrInvalid)
	ok, err := pinSkills(ctx, nil, Skills{{Git: &GitSkill{URL: skillsRepo, Commit: mainHead}}}, false)
	require.NoError(t, err)
	assert.Equal(t, mainHead, ok[0].Git.Commit)
	empty, err := pinSkills(ctx, nil, nil, true)
	require.NoError(t, err)
	assert.Nil(t, empty)
}

func TestSkillsUnmarshalExplainsThe0xShape(t *testing.T) {
	var list Skills
	require.NoError(t, json.Unmarshal([]byte(`[{"name":"a","git":{"url":"https://github.com/o/r","ref":"main"},"path":"a"},{"oci":"ghcr.io/o/s:1"}]`), &list))
	require.Len(t, list, 2)
	assert.Equal(t, "main", list[0].Git.Ref)
	assert.Equal(t, "ghcr.io/o/s:1", list[1].OCI)

	err := json.Unmarshal([]byte(`{"gitRefs":[{"url":"https://github.com/o/r","ref":"main"}]}`), &list)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "skills is a list now")
	assert.Contains(t, err.Error(), "not the 0.x object")

	err = json.Unmarshal([]byte(`{"gitRefs":[],"gitAuthSecretName":"kagent-skills-token"}`), &list)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "skills.gitAuthSecretName is gone")
	assert.Contains(t, err.Error(), "no per-source skill credential")
	assert.Contains(t, err.Error(), "GITHUB_TOKEN")

	var spec Spec
	err = json.Unmarshal([]byte(`{"name":"sre","skills":{"refs":["x:1"]}}`), &spec)
	require.Error(t, err, "the list type is what Spec and Update decode")
	var upd Update
	require.NoError(t, json.Unmarshal([]byte(`{"name":"sre","skills":[],"refreshSkills":true}`), &upd))
	require.NotNil(t, upd.Skills)
	assert.Empty(t, *upd.Skills, "an empty list clears the skills; nil leaves them")
	assert.True(t, upd.RefreshSkills)
}

func TestSkillMountName(t *testing.T) {
	assert.Equal(t, "explicit", Skill{Name: "explicit", Path: "a/b"}.mountName())
	assert.Equal(t, "b", Skill{Git: &GitSkill{URL: "https://github.com/o/r"}, Path: "a/b/"}.mountName())
	assert.Equal(t, "r", Skill{Git: &GitSkill{URL: "https://github.com/o/r.git"}}.mountName())
	assert.Equal(t, "kubectl", Skill{OCI: kubectlRef + ":1.4.0"}.mountName())
	assert.Equal(t, "kubectl", Skill{OCI: kubectlRef + "@" + kubectlSum}.mountName())
	assert.Equal(t, "kubectl", Skill{OCI: "localhost:5000/kubectl@" + kubectlSum}.mountName())
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
