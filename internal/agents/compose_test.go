package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	testCommit = "0123456789abcdef0123456789abcdef01234567"
	testDigest = "sha256:5b0bcabd1ed22e9fb1310cf6c2dec7cdef19f0ad69efa1f392e94a4333501270"
)

var testCompose = ComposeConfig{APIVersion: DefaultAPIVersion, MusterServer: DefaultMusterServer, DefaultHarness: DefaultHarness}

// platformServer is the connectivity chart's muster RemoteMCPServer, the
// object every toolset carrier is copied from. extraHeaders are appended to
// spec.headersFrom (tests of what a carrier keeps and drops).
func platformServer(ns string, extraHeaders ...map[string]any) *unstructured.Unstructured {
	spec := map[string]any{
		"description":      "Shared muster MCP gateway for platform agents",
		"url":              "http://muster.agent-platform.svc.cluster.local:8090/mcp",
		"protocol":         "STREAMABLE_HTTP",
		"timeout":          "30s",
		"terminateOnClose": true,
	}
	if len(extraHeaders) > 0 {
		headers := make([]any, 0, len(extraHeaders))
		for _, h := range extraHeaders {
			headers = append(headers, h)
		}
		spec["headersFrom"] = headers
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": DefaultAPIVersion, "kind": kindRemoteMCPServer,
		"metadata": map[string]any{"name": "muster", "namespace": ns, "labels": map[string]any{DiscoveryLabel: "disabled"}},
		"spec":     spec,
		"status":   map[string]any{"conditions": []any{map[string]any{"type": "Accepted", "status": "True", "reason": "DiscoveryDisabled", "message": "Tool discovery is disabled by the kagent.dev/discovery=disabled label"}}},
	}}
}

// golden compares got with testdata/<name>; UPDATE_GOLDEN=1 rewrites the file.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if os.Getenv("UPDATE_GOLDEN") != "" {
		require.NoError(t, os.MkdirAll("testdata", 0o750))
		require.NoError(t, os.WriteFile(path, []byte(got), 0o600))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "run with UPDATE_GOLDEN=1 to (re)write %s", path)
	assert.Equal(t, string(want), got, "%s differs from the golden file (UPDATE_GOLDEN=1 rewrites it)", path)
}

func fullSpec() Spec {
	return Spec{
		Namespace: "kagent", Name: "sre", DisplayName: "SRE Assistant", Description: "helps", SystemMessage: "Be brief.",
		ModelConfig: "default-model-config", Harness: "claude",
		Skills: &Skills{
			GitRefs: []SkillGitRef{{URL: "https://github.com/giantswarm/agent-skills", Path: "nested/runbooks/", Ref: testCommit}},
			Refs:    []string{"ghcr.io/giantswarm/skills/kubectl@" + testDigest},
		},
		Toolset:     []string{"preset:read-only", "workflow:incident-triage"},
		Labels:      map[string]string{"tenant": "sre"},
		Annotations: map[string]string{"team": "bumblebee"},
	}
}

func TestBuildAgentTemplateMatchesTheContract(t *testing.T) {
	tpl, err := BuildAgentTemplate(declaration{Spec: fullSpec()}, testCompose, "admin@lab.local")
	require.NoError(t, err)
	golden(t, "agenttemplate-full.yaml", ToYAML(tpl))

	// The minimal template: only what was set, the default Harness, the
	// binding to the carrier; no caller without OAuth.
	minimal, err := BuildAgentTemplate(declaration{Spec: Spec{Namespace: "kagent", Name: "sre", ModelConfig: "mc", Toolset: []string{"preset:none"}, Description: "  "}}, testCompose, "")
	require.NoError(t, err)
	golden(t, "agenttemplate-minimal.yaml", ToYAML(minimal))
	assert.Equal(t, DefaultHarness, minimal.GetLabels()[HarnessLabel])
	_, hasRequestedBy := minimal.GetAnnotations()[RequestedByAnnotation]
	assert.False(t, hasRequestedBy)

	// A template read back with implicit full access binds the platform
	// server; one without tools binds nothing.
	implicit, err := BuildAgentTemplate(declaration{Spec: Spec{Namespace: "kagent", Name: "old", ModelConfig: "mc"}, bindsPlatformServer: true}, testCompose, "")
	require.NoError(t, err)
	tools, _, _ := unstructured.NestedSlice(implicit.Object, "spec", "tools")
	assert.Equal(t, []any{mcpBinding("muster")}, tools)
	none, err := BuildAgentTemplate(declaration{Spec: Spec{Namespace: "kagent", Name: "quiet", ModelConfig: "mc"}}, testCompose, "")
	require.NoError(t, err)
	_, hasTools, _ := unstructured.NestedSlice(none.Object, "spec", "tools")
	assert.False(t, hasTools)

	// The Harness is chosen with the argument, never a conflicting label.
	spec := fullSpec()
	spec.Labels[HarnessLabel] = "kagent"
	_, err = BuildAgentTemplate(declaration{Spec: spec}, testCompose, "")
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "conflicts with harness")
}

func TestBuildToolsetCarrierCopiesThePlatformServer(t *testing.T) {
	platform := platformServer("kagent",
		map[string]any{"name": "Authorization", "valueFrom": map[string]any{"type": "Secret", "name": "muster-token", "key": "token"}},
		map[string]any{"name": "X-Custom", "value": "kept"},
		map[string]any{"name": "X-Muster-Toolset", "value": "preset:full"},
	)
	carrier, dropped := BuildToolsetCarrier("sre", []string{"preset:read-only", "workflow:incident-triage"}, platform, testCompose, "admin@lab.local")
	golden(t, "toolset-carrier.yaml", ToYAML(carrier))
	assert.Equal(t, []string{"Authorization"}, dropped, "a static Authorization would override the caller's bearer")
	assert.Equal(t, "muster-sre", carrier.GetName())
	assert.Equal(t, "disabled", carrier.GetLabels()[DiscoveryLabel], "the discovery opt-out travels with the copy")
	assert.Equal(t, "sre", carrier.GetLabels()[AgentLabel])
	assert.Equal(t, "preset:read-only,workflow:incident-triage", carrierHeader(carrier))
	headers, _, _ := unstructured.NestedSlice(carrier.Object, "spec", "headersFrom")
	assert.Len(t, headers, 2, "the platform's own toolset header is replaced, the custom one kept, Authorization dropped")

	// The platform object is not modified.
	headers, _, _ = unstructured.NestedSlice(platform.Object, "spec", "headersFrom")
	assert.Len(t, headers, 3)

	plain, dropped := BuildToolsetCarrier("sre", []string{"preset:none"}, platformServer("kagent"), testCompose, "")
	assert.Empty(t, dropped)
	assert.Equal(t, []string{"preset:none"}, carrierToolset(plain))
	_, hasAnnotations := plain.Object["metadata"].(map[string]any)["annotations"]
	assert.False(t, hasAnnotations)
}

func TestSkillsMustBeImmutable(t *testing.T) {
	for name, tc := range map[string]struct {
		skills *Skills
		want   string
	}{
		"a branch is refused":  {&Skills{GitRefs: []SkillGitRef{{URL: "https://github.com/o/r", Ref: "main"}}}, "full git commit id"},
		"an empty ref too":     {&Skills{GitRefs: []SkillGitRef{{URL: "https://github.com/o/r"}}}, "full git commit id"},
		"a short sha too":      {&Skills{GitRefs: []SkillGitRef{{URL: "https://github.com/o/r", Ref: "0123456"}}}, "full git commit id"},
		"a non-http url":       {&Skills{GitRefs: []SkillGitRef{{URL: "git@github.com:o/r.git", Ref: testCommit}}}, "http(s) URL"},
		"a path escaping":      {&Skills{GitRefs: []SkillGitRef{{URL: "https://github.com/o/r", Ref: testCommit, Path: "../etc"}}}, "'..'"},
		"an OCI tag":           {&Skills{Refs: []string{"ghcr.io/o/skill:1.0"}}, "digest-pinned"},
		"two skills one name":  {&Skills{GitRefs: []SkillGitRef{{URL: "https://github.com/o/r", Ref: testCommit, Path: "a/x"}, {URL: "https://github.com/o/s", Ref: testCommit, Path: "b/x"}}}, `named "x"`},
		"git and oci one name": {&Skills{GitRefs: []SkillGitRef{{URL: "https://github.com/o/r", Ref: testCommit, Path: "kubectl"}}, Refs: []string{"ghcr.io/o/kubectl@" + testDigest}}, `named "kubectl"`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := skillsSpec(tc.skills)
			require.ErrorIs(t, err, ErrInvalid)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
	ok, err := skillsSpec(&Skills{
		GitRefs: []SkillGitRef{{URL: "https://github.com/o/r", Ref: strings.ToUpper(testCommit), Name: "explicit"}, {URL: "https://github.com/o/r.git", Ref: testCommit}},
		Refs:    []string{"registry.example/team/kubectl:v1@" + testDigest},
	})
	require.NoError(t, err)
	assert.Equal(t, []any{
		map[string]any{"name": "explicit", "source": map[string]any{"git": map[string]any{"url": "https://github.com/o/r", "commit": strings.ToUpper(testCommit)}}},
		map[string]any{"name": "r", "source": map[string]any{"git": map[string]any{"url": "https://github.com/o/r.git", "commit": testCommit}}},
		map[string]any{"name": "kubectl", "source": map[string]any{"oci": "registry.example/team/kubectl:v1@" + testDigest}},
	}, ok)
	empty, err := skillsSpec(&Skills{})
	require.NoError(t, err)
	assert.Nil(t, empty)
}

func TestSkillName(t *testing.T) {
	assert.Equal(t, "explicit", SkillName(SkillGitRef{Name: "explicit", Path: "a/b"}))
	assert.Equal(t, "b", SkillName(SkillGitRef{URL: "https://github.com/o/r", Path: "a/b/"}))
	assert.Equal(t, "r", SkillName(SkillGitRef{URL: "https://github.com/o/r.git"}))
	assert.Equal(t, "kubectl", ociSkillName("ghcr.io/o/kubectl@"+testDigest))
	assert.Equal(t, "kubectl", ociSkillName("ghcr.io/o/kubectl:v1@"+testDigest))
}

// TestDeclarationRoundTrip: what BuildAgentTemplate composes reads back as the
// same declaration, so an update re-composes from the served objects.
func TestDeclarationRoundTrip(t *testing.T) {
	spec := fullSpec()
	tpl, err := BuildAgentTemplate(declaration{Spec: spec}, testCompose, "admin@lab.local")
	require.NoError(t, err)
	carrier, _ := BuildToolsetCarrier(spec.Name, spec.Toolset, platformServer("kagent"), testCompose, "admin@lab.local")
	// Tooling stamps survive on the served object without becoming the caller's.
	tpl.SetLabels(mergeStringMaps(tpl.GetLabels(), map[string]string{KustomizationNameLabel: "flux-system"}))
	tpl.SetAnnotations(mergeStringMaps(tpl.GetAnnotations(), map[string]string{"kubectl.kubernetes.io/last-applied-configuration": "{}"}))

	d, unmanaged := declarationOf(tpl, carrier, testCompose)
	assert.Empty(t, unmanaged)
	want := spec
	want.Skills.GitRefs[0].Path = "nested/runbooks"
	want.Skills.GitRefs[0].Name = "runbooks"
	assert.Equal(t, want, d.Spec)
	assert.False(t, d.bindsPlatformServer)

	// Bound to the platform server without a carrier: implicit full access,
	// no toolset; bound to nothing: neither.
	implicit, _ := BuildAgentTemplate(declaration{Spec: Spec{Namespace: "kagent", Name: "old", ModelConfig: "mc"}, bindsPlatformServer: true}, testCompose, "")
	d, unmanaged = declarationOf(implicit, nil, testCompose)
	assert.Empty(t, unmanaged)
	assert.True(t, d.bindsPlatformServer)
	assert.Nil(t, d.Toolset)
	quiet, _ := BuildAgentTemplate(declaration{Spec: Spec{Namespace: "kagent", Name: "quiet", ModelConfig: "mc"}}, testCompose, "")
	d, _ = declarationOf(quiet, nil, testCompose)
	assert.False(t, d.bindsPlatformServer)
	assert.Nil(t, d.Toolset)

	// A carrier binding whose carrier is gone is reported, not invented.
	d, unmanaged = declarationOf(tpl, nil, testCompose)
	assert.Nil(t, d.Toolset)
	require.Len(t, unmanaged, 1)
	assert.Contains(t, unmanaged[0], "muster-sre, which does not exist")

	// What the request shape cannot express is listed as unmanaged.
	foreign := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": DefaultAPIVersion, "kind": kindAgentTemplate,
		"metadata": map[string]any{"name": "foreign", "namespace": "kagent", "labels": map[string]any{"kagent.dev/e2e-runtime": "kagent"}},
		"spec": map[string]any{
			"systemPromptFrom": map[string]any{"name": "prompts", "key": "sre"},
			"plugins":          []any{map[string]any{"source": map[string]any{"oci": "ghcr.io/o/p@" + testDigest}}},
			"promptTemplate":   map[string]any{},
			"skills": []any{
				map[string]any{"name": "s3", "source": map[string]any{"bucket": map[string]any{"s3": map[string]any{}}}},
				map[string]any{"name": "oci-dir", "source": map[string]any{"oci": "ghcr.io/o/x@" + testDigest, "path": "sub"}},
				map[string]any{"name": "git", "source": map[string]any{"git": map[string]any{"url": "https://github.com/o/r", "commit": testCommit}, "path": "a"}},
			},
			"tools": []any{
				map[string]any{"agent": map[string]any{"name": "helper", "description": "d", "templateRef": map[string]any{"name": "helper"}}},
				map[string]any{"mcp": map[string]any{"server": map[string]any{"kind": "RemoteMCPServer", "name": "github"}}},
				map[string]any{"mcp": map[string]any{"server": map[string]any{"kind": "RemoteMCPServer", "name": "muster"}, "tools": []any{"x_a_b"}}},
			},
		},
	}}
	d, unmanaged = declarationOf(foreign, nil, testCompose)
	assert.Equal(t, []string{
		"plugins", "promptTemplate", "skills[oci-dir].source", "skills[s3].source", "systemPromptFrom",
		"tools[0] (agent binding)", "tools[1].mcp.server github", "tools[2].mcp.tools (per-tool selection of muster)",
	}, unmanaged)
	assert.Equal(t, map[string]string{"kagent.dev/e2e-runtime": "kagent"}, d.Labels, "a foreign selector label is the caller's to keep")
	assert.Equal(t, "", d.Harness)
	require.NotNil(t, d.Skills)
	assert.Equal(t, []SkillGitRef{{URL: "https://github.com/o/r", Path: "a", Ref: testCommit, Name: "git"}}, d.Skills.GitRefs)
	assert.False(t, d.bindsPlatformServer, "a per-tool selection is not the whole-server binding")
}

func TestDeclarationMapAndChangedPaths(t *testing.T) {
	before := declarationMap(declaration{Spec: fullSpec()})
	spec := fullSpec()
	spec.Description = "helps more"
	spec.Toolset = []string{"preset:none"}
	spec.Labels = nil
	spec.Skills.Refs = nil
	after := declarationMap(declaration{Spec: spec})
	assert.Equal(t, []string{"description", "labels.tenant", "skills.refs", "toolset"}, changedPaths("", before, after))
	assert.Equal(t, map[string]any{
		"displayName": "SRE Assistant", "description": "helps more", "systemMessage": "Be brief.", "modelConfig": "default-model-config", "harness": "claude",
		"skills":      map[string]any{"gitRefs": []any{map[string]any{"url": "https://github.com/giantswarm/agent-skills", "path": "nested/runbooks/", "ref": testCommit, "name": "runbooks"}}},
		"toolset":     []any{"preset:none"},
		"annotations": map[string]any{"team": "bumblebee"},
	}, after)
	assert.Empty(t, changedPaths("", before, before))
}

func TestValidateNameAndHarness(t *testing.T) {
	assert.NoError(t, ValidateName("sre-agent-1"))
	assert.ErrorIs(t, ValidateName(""), ErrInvalid)
	assert.ErrorIs(t, ValidateName("SRE"), ErrInvalid)
	assert.ErrorIs(t, ValidateName("-sre"), ErrInvalid)
	assert.ErrorIs(t, ValidateName("a.b"), ErrInvalid)
	assert.NoError(t, ValidateHarness("claude"))
	assert.ErrorIs(t, ValidateHarness("Claude"), ErrInvalid)
	assert.Equal(t, "muster-sre", CarrierName("", "sre"))
	assert.Equal(t, "gateway-sre", CarrierName("gateway", "sre"))
}

func mergeStringMaps(a, b map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}
