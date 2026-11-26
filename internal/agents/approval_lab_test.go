package agents

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/agent-manager/internal/approvalgrants"
)

// Regression tests of the lab integration of tool approval.

// a create in an organization namespace whose agent chart source
// serves an older chart (the operator's copy) writes `toolset`, and refuses
// settings naming the source, instead of an install that fails on `tools`.
func TestACreateFollowsTheNamespacesChartSource(t *testing.T) {
	old := ociRepository("org-acme", "2.x")
	_ = unstructured.SetNestedField(old.Object, "0.9.0@sha256:0123", "status", "artifact", "revision")
	f := newFixture(t, []runtime.Object{modelConfig("org-acme", "default-model-config", "Anthropic", "claude-sonnet-4-6"), harness("org-acme", "kagent", "kagent"), old})
	withTenants(f, &fakeTenants{namespaces: []string{"org-acme"}})
	f.svc.chart = toolsChart{}
	f.svc.cfg.Bindings = &approvalBindings{}

	_, err := f.svc.Create(dana(t), Spec{Location: In("org-acme"), Name: "bot", ModelConfig: "default-model-config", Tools: ToolList{{Selector: "server:github", Setting: "Ask"}}})
	require.ErrorIs(t, err, ErrUnsupported)
	assert.Contains(t, err.Error(), "0.9.0 that OCIRepository org-acme/agent, the namespace's agent chart source, serves")

	_, err = f.svc.Create(dana(t), Spec{Location: In("org-acme"), Name: "bot", ModelConfig: "default-model-config", Tools: ToolList{{Selector: "server:github"}}})
	require.NoError(t, err)
	hr, err := f.dyn.Resource(hrGVR).Namespace("org-acme").Get(t.Context(), "bot", metav1.GetOptions{})
	require.NoError(t, err)
	values := mustValues(hr)
	assert.Equal(t, []any{"server:github"}, values[ToolsetValuesKey], "the older chart takes toolset")
	assert.NotContains(t, values, ToolsValuesKey)

	// Its release on the older chart reports the settings as not enforced.
	_ = unstructured.SetNestedSlice(hr.Object, []any{map[string]any{"chartVersion": "0.9.0"}}, "status", "history")
	_, err = f.dyn.Resource(hrGVR).Namespace("org-acme").UpdateStatus(t.Context(), hr, metav1.UpdateOptions{})
	require.NoError(t, err)
	agent, err := f.svc.Get(dana(t), In("org-acme"), "bot")
	require.NoError(t, err)
	assert.Equal(t, EnforcementNotEnforced, agent.ApprovalEnforcement)
	require.NotNil(t, agent.ApprovalEnforced)
	assert.False(t, *agent.ApprovalEnforced)
}

// A namespace's source on another chart URL (another registry or chart) is
// taken not to take `tools`, whatever it serves: settings are refused and the
// release gets `toolset` (D-GO-13).
func TestACreateOnASourceOfAnotherURLTakesNoTools(t *testing.T) {
	other := ociRepository("org-acme", "2.x")
	_ = unstructured.SetNestedField(other.Object, "oci://mirror.example.com/charts/agent", "spec", "url")
	f := newFixture(t, []runtime.Object{modelConfig("org-acme", "default-model-config", "Anthropic", "claude-sonnet-4-6"), harness("org-acme", "kagent", "kagent"), other})
	withTenants(f, &fakeTenants{namespaces: []string{"org-acme"}})
	f.svc.chart = toolsChart{}
	f.svc.cfg.Bindings = &approvalBindings{}

	_, err := f.svc.Create(dana(t), Spec{Location: In("org-acme"), Name: "bot", ModelConfig: "default-model-config", Tools: ToolList{{Selector: "server:github", Setting: "Ask"}}})
	require.ErrorIs(t, err, ErrUnsupported)
	assert.Contains(t, err.Error(), "oci://mirror.example.com/charts/agent that OCIRepository org-acme/agent, the namespace's agent chart source, serves")

	_, err = f.svc.Create(dana(t), Spec{Location: In("org-acme"), Name: "bot", ModelConfig: "default-model-config", Tools: ToolList{{Selector: "server:github"}}})
	require.NoError(t, err)
	hr, err := f.dyn.Resource(hrGVR).Namespace("org-acme").Get(t.Context(), "bot", metav1.GetOptions{})
	require.NoError(t, err)
	values := mustValues(hr)
	assert.Equal(t, []any{"server:github"}, values[ToolsetValuesKey])
	assert.NotContains(t, values, ToolsValuesKey)
}

// delete_agent's result names the grants it dropped.
func TestDeleteReportsTheDroppedGrants(t *testing.T) {
	a := newApprovalFixture(t, nil)
	a.grantMerge()
	res, err := a.svc.Delete(dana(t), Location{}, "alert-review", false, WriteOptions{})
	require.NoError(t, err)
	require.Len(t, res.Dropped, 1)
	assert.Equal(t, DroppedGrant{Entry: "tool:x_github_merge_pull_request", Server: "github", Reason: "agent deleted", connectorUID: "uid-github"}, res.Dropped[0])
}

// the sync log carries muster's counts.
func TestTheSyncLogCarriesMustersCounts(t *testing.T) {
	a := newApprovalFixture(t, nil)
	a.grantMerge()
	_, err := a.svc.UpdateAs(asPerson(t.Context(), "bob@example.com"), "update_agent", Update{Name: "alert-review", Description: new("x")})
	require.NoError(t, err)
	assert.Contains(t, a.logs.String(), `"msg":"approval grants removed from running sessions"`)
	assert.Contains(t, a.logs.String(), `"bindings":2,"dropped":1`)
}

// a release that has not reconciled yet reads `pending`, with
// approvalEnforced left out rather than false; once it ran, `enforced`.
func TestApprovalEnforcementIsPendingUntilTheReleaseRan(t *testing.T) {
	a := newApprovalFixture(t, nil)
	res, err := a.svc.Create(dana(t), Spec{Name: "fresh", ModelConfig: "default-model-config", Tools: ToolList{{Selector: "server:github", Setting: "Ask"}}})
	require.NoError(t, err)
	assert.Equal(t, EnforcementPending, res.Agent.ApprovalEnforcement, "the create answer")
	assert.Nil(t, res.Agent.ApprovalEnforced, "not false while pending")

	agent, err := a.svc.Get(dana(t), Location{}, "alert-review")
	require.NoError(t, err)
	assert.Equal(t, EnforcementEnforced, agent.ApprovalEnforcement)
	require.NotNil(t, agent.ApprovalEnforced)
	assert.True(t, *agent.ApprovalEnforced)
}

// list_approval_grants for a namespace the caller may not list
// HelmReleases in answers the list helmreleases refusal, before any chart
// source is read.
func TestListApprovalGrantsChecksListHelmReleasesFirst(t *testing.T) {
	a := newApprovalFixture(t, nil)
	a.denyListReleases = map[string]bool{"kagent": true}
	read := 0
	a.dyn.PrependReactor("list", "ocirepositories", func(k8stesting.Action) (bool, runtime.Object, error) {
		read++
		return false, nil, nil
	})
	_, err := a.svc.ListApprovalGrants(dana(t), "kagent", "")
	require.ErrorIs(t, err, ErrForbidden)
	assert.Contains(t, err.Error(), "list helmreleases in namespace kagent")
	assert.Zero(t, read, "no chart source read before the check")
}

var _ = approvalgrants.Agent{}
