package agents

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// connectorFixture is seeded() with organization namespace org-a, its model
// and its connectors github and jira.
func connectorFixture(t *testing.T) *fixture {
	t.Helper()
	f := seeded(t)
	withTenants(f, &fakeTenants{namespaces: []string{"org-a"}})
	mustCreate(t, f, mcGVR, modelConfig("org-a", "own", "Anthropic", "claude-sonnet-4-6"))
	for _, name := range []string{"github", "jira"} {
		mustCreate(t, f, serverGVR, connector(name))
	}
	return f
}

// connector is a RemoteMCPServer of org-a as set_organization_connector
// writes it: no agent release renders it.
func connector(name string) *unstructured.Unstructured {
	server := remoteMCPServer("org-a", name)
	server.SetLabels(map[string]string{ManagedByLabel: ManagedByValue})
	_ = unstructured.SetNestedField(server.Object, "https://"+name+".example/mcp", "spec", "url")
	return server
}

func connectorSpec(name string, connectors ...string) Spec {
	return Spec{Location: In("org-a"), Name: name, ModelConfig: "own", Toolset: []string{"preset:read-only"}, Connectors: connectors}
}

// renderedServers are the servers the rendered Agent's template binds, in
// order: the chart's own render of the values.
func renderedServers(t *testing.T, f *fixture) []string {
	t.Helper()
	rendered := f.dryRuns.last("Agent")
	require.NotNil(t, rendered)
	var out []string
	for _, b := range toolBindingsOf(rendered) {
		out = append(out, b.Server)
	}
	return out
}

func orgRelease(t *testing.T, f *fixture, name string) *unstructured.Unstructured {
	t.Helper()
	hr, err := f.dyn.Resource(hrGVR).Namespace("org-a").Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	return hr
}

// connectors become one extraTools entry each, which the chart binds after
// the muster binding; get_agent reads them back. An update replaces them and
// keeps every other extraTools entry; an absent argument changes nothing; an
// empty list removes them.
func TestConnectorsOnCreateUpdateAndGet(t *testing.T) {
	f := connectorFixture(t)
	ctx := t.Context()

	dry, err := f.svc.ValidateCreate(ctx, connectorSpec("sre", "github", "jira"))
	require.NoError(t, err)
	require.True(t, dry.Valid, dry.Errors)
	assert.Equal(t, []string{"sre", "github", "jira"}, renderedServers(t, f), "muster first, then the connectors")

	_, err = f.svc.Create(ctx, connectorSpec("sre", "github", "jira"))
	require.NoError(t, err)
	assert.Equal(t, []any{
		map[string]any{"mcp": map[string]any{"server": map[string]any{"kind": "RemoteMCPServer", "name": "github"}}},
		map[string]any{"mcp": map[string]any{"server": map[string]any{"kind": "RemoteMCPServer", "name": "jira"}}},
	}, mustValues(orgRelease(t, f, "sre"))["extraTools"])
	got, err := f.svc.Get(ctx, In("org-a"), "sre")
	require.NoError(t, err)
	assert.Equal(t, []string{"github", "jira"}, got.Connectors)

	// A sub-agent binding written by hand stays through every change.
	hr := orgRelease(t, f, "sre")
	extra, _, _ := unstructured.NestedSlice(hr.Object, "spec", "values", "extraTools")
	subAgent := map[string]any{"subAgent": map[string]any{"name": "helper", "description": "helps", "templateRef": map[string]any{"name": "helper"}}}
	require.NoError(t, unstructured.SetNestedSlice(hr.Object, append([]any{subAgent}, extra...), "spec", "values", "extraTools"))
	_, err = f.dyn.Resource(hrGVR).Namespace("org-a").Update(ctx, hr, metav1.UpdateOptions{})
	require.NoError(t, err)

	res, err := f.svc.Update(ctx, Update{Location: In("org-a"), Name: "sre", Connectors: &[]string{"jira"}})
	require.NoError(t, err)
	assert.Equal(t, []any{subAgent, map[string]any{"mcp": map[string]any{"server": map[string]any{"kind": "RemoteMCPServer", "name": "jira"}}}}, res.After["extraTools"])

	desc := "reviews"
	res, err = f.svc.Update(ctx, Update{Location: In("org-a"), Name: "sre", Description: &desc})
	require.NoError(t, err)
	assert.Equal(t, res.Before["extraTools"], res.After["extraTools"], "no connectors argument changes nothing")

	res, err = f.svc.Update(ctx, Update{Location: In("org-a"), Name: "sre", Connectors: &[]string{}})
	require.NoError(t, err)
	assert.Equal(t, []any{subAgent}, res.After["extraTools"])
	got, err = f.svc.Get(ctx, In("org-a"), "sre")
	require.NoError(t, err)
	assert.Empty(t, got.Connectors)

	// The last binding gone, the list stays as an empty one: it still
	// overrides a list the release's valuesFrom may bring.
	_, err = f.svc.Create(ctx, connectorSpec("solo", "github"))
	require.NoError(t, err)
	res, err = f.svc.Update(ctx, Update{Location: In("org-a"), Name: "solo", Connectors: &[]string{}})
	require.NoError(t, err)
	assert.Equal(t, []any{}, res.After["extraTools"])
}

// Commit mode writes the same values into the pull request's file.
func TestConnectorsInCommitMode(t *testing.T) {
	f, fake, ctx := gitOpsFixture(t, true)
	withTenants(f, &fakeTenants{namespaces: []string{"org-a"}})
	mustCreate(t, f, mcGVR, modelConfig("org-a", "own", "Anthropic", "claude-sonnet-4-6"))
	mustCreate(t, f, serverGVR, connector("github"))
	spec := connectorSpec("sre", "github")
	spec.WriteOptions = WriteOptions{Mode: ModeCommit, Repository: "giantswarm/fleet", Branch: "main", Path: "orgs"}

	res, err := f.svc.Create(ctx, spec)
	require.NoError(t, err)
	require.Len(t, fake.PullRequests(), 1)
	file := string(fake.Files(fleetRepo, fake.PullRequests()[0].Head)["orgs/agent-manager/sre.yaml"])
	assert.Equal(t, res.Manifests.HelmRelease, file)
	assert.Contains(t, file, "extraTools:")
	assert.Equal(t, []string{"sre", "github"}, renderedServers(t, f))

	spec.Connectors = []string{"gitlab"}
	_, err = f.svc.Create(ctx, spec)
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), `RemoteMCPServer "gitlab" does not exist in namespace org-a`)
	assert.Len(t, fake.PullRequests(), 1, "a refused create opens nothing")
}

// A connector is one of the agent's own organization: a name that is not
// there, a name with a namespace or a kind, and an agent outside an
// organization namespace are refused before anything is written.
func TestConnectorsRefused(t *testing.T) {
	f := connectorFixture(t)
	ctx := t.Context()
	// helper is another agent's muster binding; vendor is a connector a
	// HelmRelease of another chart renders.
	helper := remoteMCPServer("org-a", "helper")
	helper.SetLabels(map[string]string{HelmReleaseNameLabel: "helper", HelmReleaseNamespaceLabel: "org-a", chartNameLabel: "agent"})
	mustCreate(t, f, serverGVR, helper)
	vendor := connector("vendor")
	vendor.SetLabels(map[string]string{HelmReleaseNameLabel: "vendor-mcp", HelmReleaseNamespaceLabel: "org-a", chartNameLabel: "vendor-mcp"})
	mustCreate(t, f, serverGVR, vendor)
	for _, tc := range []struct {
		name string
		spec Spec
		want string
	}{
		{"missing", connectorSpec("a", "github", "gitlab"), `connectors: RemoteMCPServer "gitlab" does not exist in namespace org-a`},
		{"other namespace", connectorSpec("a", "org-b/github"), `connectors: "org-b/github" names a namespace or a kind: a connector is always a RemoteMCPServer of the agent's own organization namespace`},
		{"other kind", connectorSpec("a", "server:github"), `connectors: "server:github" names a namespace or a kind`},
		{"named twice", connectorSpec("a", "github", "github"), `connectors: "github" is named twice`},
		{"own muster binding", connectorSpec("a", "a"), `connectors: "a" is the agent's own muster binding, no connector`},
		{"another agent's muster binding", connectorSpec("a", "helper"), `connectors: RemoteMCPServer "helper" is rendered by HelmRelease helper: it is an agent's muster binding, no connector`},
		{"platform namespace", Spec{Name: "a", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"}, Connectors: []string{"verifier"}}, "connectors: namespace kagent is no organization namespace"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.svc.Create(ctx, tc.spec)
			require.ErrorIs(t, err, ErrInvalid)
			assert.Contains(t, err.Error(), tc.want)
			dry, err := f.svc.ValidateCreate(ctx, tc.spec)
			require.NoError(t, err)
			assert.True(t, containsLine(dry.Errors, tc.want), dry.Errors)
		})
	}
	_, err := f.dyn.Resource(hrGVR).Namespace("org-a").Get(ctx, "a", metav1.GetOptions{})
	require.Error(t, err, "nothing is written")

	_, err = f.svc.Create(ctx, connectorSpec("vendored", "vendor"))
	require.NoError(t, err, "a connector of another chart's release is one")

	_, err = f.svc.Create(ctx, connectorSpec("sre"))
	require.NoError(t, err)
	_, err = f.svc.Update(ctx, Update{Location: In("org-a"), Name: "sre", Connectors: &[]string{"gitlab"}})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), `RemoteMCPServer "gitlab" does not exist`)
	assert.NotContains(t, mustValues(orgRelease(t, f, "sre")), "extraTools")

	// extraAgentSpec.tools replaces the rendered tool list, connectors and all.
	hr := orgRelease(t, f, "sre")
	require.NoError(t, unstructured.SetNestedSlice(hr.Object, []any{map[string]any{"mcp": map[string]any{"server": map[string]any{"kind": "RemoteMCPServer", "name": "sre"}}}}, "spec", "values", "extraAgentSpec", "tools"))
	_, err = f.dyn.Resource(hrGVR).Namespace("org-a").Update(ctx, hr, metav1.UpdateOptions{})
	require.NoError(t, err)
	_, err = f.svc.Update(ctx, Update{Location: In("org-a"), Name: "sre", Connectors: &[]string{"github"}})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "extraAgentSpec.tools, which replaces its whole tool list")

	_, err = f.svc.Update(ctx, Update{Name: "verifier", Connectors: &[]string{"verifier"}})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "namespace kagent is no organization namespace")
}

// An agent on a coding Harness takes no connector: at create, and on a move
// of an agent with connectors to a coding Harness.
func TestConnectorsRefusedOnCodingHarness(t *testing.T) {
	const want = "connectors github need an agent on a kagent Harness"
	f := connectorFixture(t)
	ctx := t.Context()
	mustCreate(t, f, f.svc.harnessGVR(), harness("org-a", "claude", "claude"))

	spec := connectorSpec("coder", "github")
	spec.Harness = "claude"
	_, err := f.svc.Create(ctx, spec)
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), want)

	_, err = f.svc.Create(ctx, connectorSpec("sre", "github"))
	require.NoError(t, err)
	claude := "claude"
	_, err = f.svc.Update(ctx, Update{Location: In("org-a"), Name: "sre", Harness: &claude})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), want)
}
