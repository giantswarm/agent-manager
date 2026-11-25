package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/tags/tagstest"
)

// withAgentTools gives the fixture the agent tools, acting as the same
// caller with the organization namespaces managed.
func withAgentTools(t *testing.T, f *orgFixture) {
	t.Helper()
	svc := agents.New(f.provider, embeddedChart{}, nil, agents.InProcessPinner(pinner{}), agents.Config{
		DefaultNamespace: "kagent", Version: "test", Tags: tagstest.Rule(), Tenants: f.cfg.Namespaces,
	}, nil)
	f.srv = NewMCPServer(svc, "test", WithOrganizations(f.orgs), WithLifecycle(t.Context()))
}

// ownModel is a ModelConfig of the organization payments.
func ownModel() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "api.kagent.dev/v1alpha3", "kind": "ModelConfig",
		"metadata": map[string]any{"name": "own", "namespace": orgNS},
		"spec":     map[string]any{"provider": "Anthropic", "model": "claude-sonnet-4-6"},
	}}
}

// Every tool finds an organization by its name, from its Organization: one
// that does not exist and one the caller has no right in answer alike, one
// with the Agent Platform switched off and one whose namespace is not
// reported (or is not the one the operator labelled for it) each with their
// own refusal, and nothing is written in any case. An organization tool and
// an agent tool answer the same.
func TestAnOrganizationIsResolvedFromItsOrganization(t *testing.T) {
	const (
		notFound   = "not_found: organization payments not found; list_organizations lists yours"
		disabled   = "conflict: the Agent Platform is not enabled for organization payments (its Organization's spec.agentPlatform.enabled is not true); nothing was written"
		notReady   = "conflict: organization payments is not ready yet: agent-platform-operator has not reported its namespace (status.agentPlatform.namespace); nothing was written, try again shortly"
		otherLabel = notReady
	)
	reporting := func(ns string) *unstructured.Unstructured {
		u := organization("payments", nil)
		if ns == "" {
			unstructured.RemoveNestedField(u.Object, "status", "agentPlatform", "namespace")
		} else {
			u.Object["status"] = readyStatus(ns)
		}
		return u
	}
	switchedOff := organization("payments", nil)
	require.NoError(t, unstructured.SetNestedField(switchedOff.Object, false, "spec", "agentPlatform", "enabled"))

	for _, tc := range []struct {
		name string
		org  *unstructured.Unstructured
		// stranger: the caller has no right in the organization.
		stranger bool
		want     string
	}{
		{name: "not found", want: notFound},
		{name: "no right in it", org: organization("payments", nil), stranger: true, want: notFound},
		{name: "switched off", org: switchedOff, want: disabled},
		{name: "no namespace yet", org: reporting(""), want: notReady},
		{name: "another organization's namespace", org: reporting(billingNS), want: otherLabel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOrgFixture(t, ownModel())
			f.dropOrganization(t, "payments")
			if tc.org != nil {
				// Past the fixture's operator, which would report it Ready.
				require.NoError(t, f.dyn.Tracker().Add(tc.org))
			}
			if tc.stranger {
				f.deny = func(ns, verb, resource, name string) bool {
					return ns == orgNS || resource == "organizations" && name == "payments"
				}
			}
			withAgentTools(t, f)
			f.dyn.ClearActions()

			text, isErr := f.call(t, ToolSetOrganizationConnector, map[string]any{"organization": "payments", "name": "gh", "url": "https://mcp.example.com/mcp", "noCredential": true})
			require.True(t, isErr, text)
			assert.Equal(t, tc.want, text)

			text, isErr = f.call(t, ToolCreateAgent, map[string]any{"organization": "payments", "name": "sre", "modelConfig": "own", "toolset": []string{"preset:read-only"}})
			require.True(t, isErr, text)
			assert.Equal(t, tc.want, text)

			for _, a := range f.dyn.Actions() {
				assert.Contains(t, []string{"get", "list", "watch"}, a.GetVerb(), "nothing is written, not even a dry run: %s %s", a.GetVerb(), a.GetResource().Resource)
			}
		})
	}
}

// The agent tools take the organization's name, never its namespace: an
// agent is created and read by the organization, whose name every answer
// carries; the namespace with the organization, the organization's
// namespace without it, and a cluster without an organization are refused,
// the second naming the organization to pass.
func TestTheAgentToolsTakeTheOrganizationNotItsNamespace(t *testing.T) {
	f := newOrgFixture(t, ownModel())
	withAgentTools(t, f)

	text, isErr := f.call(t, ToolCreateAgent, map[string]any{"organization": "payments", "name": "sre", "modelConfig": "own", "toolset": []string{"preset:read-only"}})
	require.False(t, isErr, text)
	assert.Equal(t, "payments", decode(t, text)["organization"])
	assert.NotNil(t, f.get(t, hrGVR, orgNS, "sre"), "the HelmRelease is in the organization's namespace")

	text, isErr = f.call(t, ToolGetAgent, map[string]any{"organization": "payments", "name": "sre"})
	require.False(t, isErr, text)
	got := decode(t, text)
	assert.Equal(t, "payments", got["organization"])
	assert.Equal(t, orgNS, got["namespace"], "the namespace is a detail of the answer")

	text, isErr = f.call(t, ToolListAgents, map[string]any{"organization": "payments"})
	require.False(t, isErr, text)
	assert.Equal(t, "payments", decode(t, text)["organization"])

	text, isErr = f.call(t, ToolListOrganizationMembers, map[string]any{"organization": "payments"})
	require.False(t, isErr, text)
	assert.Equal(t, "payments", decode(t, text)["organization"])

	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"namespace": orgNS, "name": "sre"}, `invalid_request: namespace ` + orgNS + ` is the namespace of organization payments: pass organization "payments" instead of namespace`},
		{map[string]any{"organization": "payments", "namespace": orgNS, "name": "sre"}, "invalid_request: namespace " + orgNS + " with organization payments: the organization decides the namespace; pass organization alone (namespace is only for the namespace on a workload cluster, with cluster)"},
		{map[string]any{"cluster": "wc1", "name": "sre"}, "invalid_request: cluster needs organization, the name of the organization whose workload cluster it is"},
	} {
		text, isErr := f.call(t, ToolGetAgent, tc.args)
		require.True(t, isErr, text)
		assert.Equal(t, tc.want, text)
	}
}
