package agents

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agent-manager/internal/approvalgrants"
	"github.com/giantswarm/agent-manager/internal/identity"
	apiv1alpha1 "github.com/giantswarm/agent-manager/internal/kagent/gen/kagent/api/v1alpha1"
	"github.com/giantswarm/agent-manager/internal/kube"
	"github.com/giantswarm/agent-manager/internal/muster"
)

// Regression tests of the first review round of the approval grants.

// get_agent keeps `tools` as the kagent MCP bindings; the tool list is
// toolSettings.
func TestGetAgentKeepsToolsAsTheBindings(t *testing.T) {
	a := newApprovalFixture(t, nil)
	agent, err := a.svc.Get(dana(t), Location{}, "alert-review")
	require.NoError(t, err)
	raw, err := json.Marshal(agent)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(raw, &doc))
	tools, ok := doc["tools"].([]any)
	require.True(t, ok, "tools is the bindings list: %s", raw)
	require.NotEmpty(t, tools)
	assert.Contains(t, tools[0].(map[string]any), "server", "a binding names its server")
	assert.Equal(t, []any{"server:github", map[string]any{"tool:x_github_merge_pull_request": "Allow"}}, doc["toolSettings"])
	assert.NotContains(t, doc, "toolBindings")
	summary, _ := json.Marshal(agent.Summary())
	assert.Contains(t, string(summary), `"toolSettings":`)
}

// without the sender (tool approval off) settings are refused,
// reported as not taken, and a release carrying some refuses a start.
func TestSettingsAreRefusedWhereNothingBindsThem(t *testing.T) {
	a := newApprovalFixture(t, map[string]any{
		"agent": map[string]any{"name": "alert-review"}, "modelConfig": map[string]any{"name": "default-model-config"},
		"tools": []any{map[string]any{"server:github": "Ask"}},
	})
	a.bindings.noSender = true
	_, err := a.svc.Create(dana(t), Spec{Name: "fresh", ModelConfig: "default-model-config", Tools: ToolList{{Selector: "server:github", Setting: "Block"}}})
	require.ErrorIs(t, err, ErrUnsupported)
	assert.Contains(t, err.Error(), "does not bind tool approval settings")
	_, err = a.svc.Create(dana(t), Spec{Name: "plain", ModelConfig: "default-model-config", Tools: ToolList{{Selector: "server:github"}}})
	require.NoError(t, err, "a list without settings is fine")
	_, err = a.svc.SetApproval(dana(t), SetApproval{Name: "alert-review", Settings: map[string]string{"server:github": "Block"}})
	require.ErrorIs(t, err, ErrUnsupported)
	agent, err := a.svc.Get(dana(t), Location{}, "alert-review")
	require.NoError(t, err)
	assert.False(t, agent.ApprovalSettings)

	err = a.startErr(t)
	require.ErrorIs(t, err, ErrApprovalUnavailable)
	assert.Contains(t, err.Error(), "does not bind them to sessions")
}

// startErr starts a session of alert-review and returns its error.
func (a *approvalFixture) startErr(t *testing.T) error {
	t.Helper()
	svc, ctrl, _ := newBoundSessionFixtureWithService(t, a.fixture)
	svc.cfg.Bindings = a.bindings
	ctrl.ActorJWTProviders = []*apiv1alpha1.SessionProvider{musterProvider()}
	_, err := svc.StartSession(dana(t), StartSession{Name: "alert-review", Message: "hello"})
	return err
}

// settings that may come from spec.valuesFrom, and a HelmRelease-
// managed agent whose release is gone, refuse a start.
func TestStartRefusesSettingsItCannotBind(t *testing.T) {
	a := newApprovalFixture(t, nil)
	hr, err := a.dyn.Resource(hrGVR).Namespace("kagent").Get(t.Context(), "alert-review", metav1.GetOptions{})
	require.NoError(t, err)
	require.NoError(t, unstructured.SetNestedSlice(hr.Object, []any{map[string]any{"kind": "ConfigMap", "name": "more"}}, "spec", "valuesFrom"))
	_, err = a.dyn.Resource(hrGVR).Namespace("kagent").Update(t.Context(), hr, metav1.UpdateOptions{})
	require.NoError(t, err)
	err = a.startErr(t)
	require.ErrorIs(t, err, ErrApprovalUnavailable)
	assert.Contains(t, err.Error(), "valuesFrom")

	require.NoError(t, a.dyn.Resource(hrGVR).Namespace("kagent").Delete(t.Context(), "alert-review", metav1.DeleteOptions{}))
	obj, err := a.dyn.Resource(agentGVR).Namespace("kagent").Get(t.Context(), "alert-review", metav1.GetOptions{})
	require.NoError(t, err)
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[HelmReleaseNameLabel] = "alert-review"
	obj.SetLabels(labels)
	_, err = a.dyn.Resource(agentGVR).Namespace("kagent").Update(t.Context(), obj, metav1.UpdateOptions{})
	require.NoError(t, err)
	err = a.startErr(t)
	require.ErrorIs(t, err, ErrApprovalUnavailable)
	assert.Contains(t, err.Error(), "cannot be found")
}

// a muster that predates approval settings refuses them as unknown
// fields: an agent without settings binds without them, one with settings
// is refused; a muster that took the bind but ignored the settings refuses
// the start too (and is unbound).
func TestBindFallsBackOnlyWithoutSettings(t *testing.T) {
	a := newApprovalFixture(t, map[string]any{
		"agent": map[string]any{"name": "alert-review"}, "modelConfig": map[string]any{"name": "default-model-config"},
		"tools": []any{"server:github", "tool:x_github_merge_pull_request"},
	})
	a.bindings.Legacy = true
	b := a.start(t)
	assert.Empty(t, b.Agent, "bound again without the agent")
	assert.Nil(t, b.Approval)

	a2 := newApprovalFixture(t, nil)
	a2.bindings.Legacy = true
	err := a2.startErr(t)
	require.ErrorIs(t, err, ErrApprovalUnavailable)
	assert.Contains(t, err.Error(), MusterApprovalVersionHint)

	a3 := newApprovalFixture(t, nil)
	a3.bindings.Settings = muster.SettingsIgnored
	err = a3.startErr(t)
	require.ErrorIs(t, err, ErrApprovalUnavailable)
	assert.Contains(t, err.Error(), "ignored")
	assert.NotEmpty(t, a3.bindings.Unbound, "the binding without settings is removed")
}

// a drop muster could not take is retried, reported, kept
// pending and sent again by the agent's next start.
func TestAFailedDropIsRetriedReportedAndSentAgain(t *testing.T) {
	a := newApprovalFixture(t, nil)
	a.grantMerge()
	a.bindings.dropFails = 100 // every attempt of this request fails
	res, err := a.svc.UpdateAs(asPerson(t.Context(), "bob@example.com"), "update_agent", Update{Name: "alert-review", Description: new("x")})
	require.NoError(t, err)
	require.NotNil(t, res.SessionsUpdated)
	assert.False(t, *res.SessionsUpdated)
	require.Len(t, res.Warnings, 1)
	assert.Contains(t, res.Warnings[0], "running sessions keep the grants")
	rec, err := a.grants.Get(t.Context(), alertAgent)
	require.NoError(t, err)
	assert.True(t, rec.PendingSessionSync, "the record stays to send it again")
	assert.Empty(t, rec.Grants)
	assert.Empty(t, a.bindings.drops)

	a.bindings.dropFails = 0
	_ = a.start(t)
	require.Len(t, a.bindings.drops, 1, "the next start sends the pending drop")
	assert.Empty(t, a.bindings.drops[0].Keep)
	rec, _ = a.grants.Get(t.Context(), alertAgent)
	assert.Empty(t, rec.Version, "synced: the empty record is gone")
}

func TestARetryGetsThrough(t *testing.T) {
	a := newApprovalFixture(t, nil)
	a.grantMerge()
	a.bindings.dropFails = 2
	res, err := a.svc.UpdateAs(asPerson(t.Context(), "bob@example.com"), "update_agent", Update{Name: "alert-review", Description: new("x")})
	require.NoError(t, err)
	require.NotNil(t, res.SessionsUpdated)
	assert.True(t, *res.SessionsUpdated, "the third attempt reached muster")
	assert.Empty(t, res.Warnings)
}

// delete_agent drops every mark of the agent; a failure keeps an empty
// pending record that a read of the namespace sends again and then removes.
func TestDeleteTellsMusterAndKeepsAPendingRecordOnFailure(t *testing.T) {
	a := newApprovalFixture(t, nil)
	a.grantMerge()
	a.bindings.dropFails = 100
	res, err := a.svc.Delete(dana(t), Location{}, "alert-review", false, WriteOptions{})
	require.NoError(t, err)
	require.NotNil(t, res.SessionsUpdated)
	assert.False(t, *res.SessionsUpdated)
	rec, _ := a.grants.Get(t.Context(), alertAgent)
	assert.True(t, rec.PendingSessionSync)

	a.bindings.dropFails = 0
	_, err = a.svc.ListApprovalGrants(dana(t), "", "")
	require.NoError(t, err)
	require.Len(t, a.bindings.drops, 1)
	assert.Equal(t, "kagent/alert-review", a.bindings.drops[0].Agent)
	assert.Empty(t, a.bindings.drops[0].Keep)
	rec, _ = a.grants.Get(t.Context(), alertAgent)
	assert.Empty(t, rec.Version, "the deleted agent's record is removed once muster has it")
}

// a grant names the tools it covered, its marks carry them, and the
// toolset bound with settings is the release's own list, not the live
// RemoteMCPServer's header.
func TestGrantMarksCarryTheirToolsAndTheReleaseToolset(t *testing.T) {
	a := newApprovalFixture(t, nil)
	a.grants.Put(alertAgent, approvalgrants.Grant{Entry: "tool:x_github_merge_pull_request", Server: "github", ConnectorNamespace: "muster", ConnectorUID: "uid-github", Generation: 7, Tools: []string{"merge_pull_request"}})
	server, err := a.dyn.Resource(serverGVR).Namespace("kagent").Get(t.Context(), "alert-review", metav1.GetOptions{})
	require.NoError(t, err)
	require.NoError(t, unstructured.SetNestedSlice(server.Object, []any{map[string]any{"name": ToolsetHeader, "value": "preset:full"}}, "spec", "headersFrom"))
	_, err = a.dyn.Resource(serverGVR).Namespace("kagent").Update(t.Context(), server, metav1.UpdateOptions{})
	require.NoError(t, err)
	b := a.start(t)
	assert.Equal(t, []string{"server:github", "tool:x_github_merge_pull_request"}, b.Toolset, "the release's list, not the edited header")
	require.Len(t, b.Approval.Grants, 1)
	assert.Equal(t, []string{"merge_pull_request"}, b.Approval.Grants[0].Tools)
}

// set_agent_approval writes nothing over a list that moved while it
// checked.
func TestSetAgentApprovalRefusesAListThatMoved(t *testing.T) {
	a := newApprovalFixture(t, nil)
	a.admins["muster"] = true
	a.reporter.onCall = func() {
		a.reporter.onCall = nil
		hr, err := a.dyn.Resource(hrGVR).Namespace("kagent").Get(context.Background(), "alert-review", metav1.GetOptions{})
		require.NoError(t, err)
		require.NoError(t, unstructured.SetNestedSlice(hr.Object, []any{"server:github"}, "spec", "values", "tools"))
		_, err = a.dyn.Resource(hrGVR).Namespace("kagent").Update(context.Background(), hr, metav1.UpdateOptions{})
		require.NoError(t, err)
	}
	_, err := a.svc.SetApproval(dana(t), SetApproval{Name: "alert-review", Settings: map[string]string{"server:github": "Ask", "tool:x_github_merge_pull_request": "Allow"}})
	require.ErrorIs(t, err, ErrConflict)
	assert.Equal(t, []any{"server:github"}, a.values(t)[ToolsValuesKey], "the other save's removal stands")
	assert.Empty(t, a.stored(t))
}

// a start that read the release before a save wrote it never
// drops the save's newer grant; it only leaves it unbound.
func TestAStartNeverDropsANewerGrant(t *testing.T) {
	a := newApprovalFixture(t, nil)
	a.grants.Put(alertAgent, approvalgrants.Grant{Entry: "tool:x_github_merge_pull_request", Server: "github", ConnectorNamespace: "muster", ConnectorUID: "uid-github", Generation: 8, Tools: []string{"merge_pull_request"}})
	b := a.start(t)
	assert.Empty(t, b.Approval.Grants, "not bound at generation 7")
	require.Len(t, a.stored(t), 1, "and not dropped")
	assert.NotContains(t, a.logs.String(), `"msg":"approval grant dropped"`)
}

// without the caller's own Kubernetes identity no one is a connector
// admin, whatever the review would answer for the ServiceAccount.
func TestNoConnectorAdminWithoutTheCallersIdentity(t *testing.T) {
	a := newApprovalFixture(t, nil)
	a.admins["muster"] = true
	a.svc.kube = a.svc.kube.(callerIdentity).Provider
	require.Equal(t, kube.IdentityServiceAccount, a.svc.kube.Identity())
	_, err := a.svc.SetApproval(dana(t), SetApproval{Name: "alert-review", Settings: map[string]string{"tool:x_github_merge_pull_request": "Allow"}})
	require.ErrorIs(t, err, ErrForbidden)
}

// a release still running a chart without `tools` keeps `toolset`, and
// settings wait until it runs one that takes them.
func TestAReleaseOnAnOlderChartKeepsItsToolset(t *testing.T) {
	a := newApprovalFixture(t, map[string]any{
		"agent": map[string]any{"name": "alert-review"}, "modelConfig": map[string]any{"name": "default-model-config"},
		"toolset": []any{"server:github"},
	})
	hr, err := a.dyn.Resource(hrGVR).Namespace("kagent").Get(t.Context(), "alert-review", metav1.GetOptions{})
	require.NoError(t, err)
	require.NoError(t, unstructured.SetNestedSlice(hr.Object, []any{map[string]any{"chartVersion": "0.0.1-old"}}, "status", "history"))
	_, err = a.dyn.Resource(hrGVR).Namespace("kagent").UpdateStatus(t.Context(), hr, metav1.UpdateOptions{})
	require.NoError(t, err)
	tools := ToolList{{Selector: "server:github"}, {Selector: "server:pagerduty"}}
	_, err = a.svc.UpdateAs(dana(t), "update_agent_access", Update{Name: "alert-review", Tools: &tools})
	require.NoError(t, err)
	values := a.values(t)
	assert.Equal(t, []any{"server:github", "server:pagerduty"}, values[ToolsetValuesKey])
	assert.NotContains(t, values, ToolsValuesKey)
	withSetting := ToolList{{Selector: "server:github", Setting: "Ask"}}
	_, err = a.svc.UpdateAs(dana(t), "update_agent_access", Update{Name: "alert-review", Tools: &withSetting})
	require.ErrorIs(t, err, ErrUnsupported)
	assert.Contains(t, err.Error(), "0.0.1-old")
}

// get_agent reads the one agent's grants record, not the namespace's.
func TestGetAgentReadsItsOwnGrantsRecord(t *testing.T) {
	a := newApprovalFixture(t, nil)
	a.grantMerge()
	counting := &listCounting{MemoryStore: a.grants}
	a.svc.cfg.Grants = counting
	agent, err := a.svc.Get(dana(t), Location{}, "alert-review")
	require.NoError(t, err)
	require.Len(t, agent.Grants, 1)
	assert.Zero(t, counting.lists)
}

type listCounting struct {
	*approvalgrants.MemoryStore
	lists int
}

func (l *listCounting) ListByNamespace(ctx context.Context, ns string) ([]approvalgrants.Record, error) {
	l.lists++
	return l.MemoryStore.ListByNamespace(ctx, ns)
}

// a credential in a skill or plugin URL is refused (without echoing
// it), and one in values written before is redacted in the access log.
func TestCredentialsInGitURLsAreRefusedAndRedacted(t *testing.T) {
	withCredential := "https://user:" + "s3cret" + "@example.com/repo.git"
	err := ValidateSkills(Skills{{Name: "x", Git: &GitSkill{URL: withCredential, Ref: "main"}}})
	require.ErrorIs(t, err, ErrInvalid)
	assert.NotContains(t, err.Error(), "s3cret")
	assert.Contains(t, err.Error(), "gitAuthSecretName")

	a := newApprovalFixture(t, nil)
	before := map[string]any{"skills": []any{map[string]any{"git": map[string]any{"url": withCredential}}}}
	a.svc.logAccessChanges(dana(t), "update_agent_access", "kagent/alert-review", before, map[string]any{})
	assert.NotContains(t, a.logs.String(), "s3cret")
	assert.Contains(t, a.logs.String(), "***@example.com")
}

// a grant names an unverified email's subject, not the email.
func TestGrantedByIsAVerifiedEmailOrTheSubject(t *testing.T) {
	a := newApprovalFixture(t, nil)
	a.admins["muster"] = true
	ctx := identity.ContextWithToken(identity.ContextWith(t.Context(), &identity.Identity{Subject: "dex-mallory", Email: "ceo@example.com"}), callerToken)
	res, err := a.svc.SetApproval(ctx, SetApproval{Name: "alert-review", Settings: map[string]string{"tool:x_github_merge_pull_request": "Allow"}})
	require.NoError(t, err)
	require.Len(t, res.Grants, 1)
	assert.Equal(t, "dex-mallory", res.Grants[0].GrantedBy)
}

// set_agent_approval answers with the list named as get_agent names it.
func TestSetAgentApprovalAnswersToolSettings(t *testing.T) {
	a := newApprovalFixture(t, nil)
	res, err := a.svc.SetApproval(dana(t), SetApproval{Name: "alert-review", Settings: map[string]string{"server:github": "Ask"}})
	require.NoError(t, err)
	raw, err := json.Marshal(res)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(raw, &doc))
	assert.Equal(t, []any{map[string]any{"server:github": "Ask"}, map[string]any{"tool:x_github_merge_pull_request": "Allow"}}, doc["toolSettings"])
	assert.NotContains(t, doc, "tools")
	assert.Contains(t, doc["before"], "toolSettings")
}

// get_agent reports the release generation; the update tools and
// set_agent_approval refuse a write when the release moved past the
// generation the caller read, accept a matching one, and an absent one
// changes nothing.
func TestExpectedGenerationRefusesAStaleWrite(t *testing.T) {
	a := newApprovalFixture(t, nil)
	agent, err := a.svc.Get(dana(t), Location{}, "alert-review")
	require.NoError(t, err)
	require.Equal(t, int64(7), agent.Generation)
	assert.Equal(t, int64(7), agent.Summary().Generation)

	stale := int64(6)
	_, err = a.svc.UpdateAs(dana(t), "update_agent", Update{Name: "alert-review", Description: new("x"), ExpectedGeneration: &stale})
	require.ErrorIs(t, err, ErrConflict)
	assert.Contains(t, err.Error(), "agent kagent/alert-review changed since it was read (generation 7 now, expectedGeneration 6): read it again with get_agent")
	_, err = a.svc.SetApproval(dana(t), SetApproval{Name: "alert-review", Settings: map[string]string{"server:github": "Ask"}, ExpectedGeneration: &stale})
	require.ErrorIs(t, err, ErrConflict)
	assert.NotContains(t, a.values(t), DefaultValuesKey)
	assert.Equal(t, []any{"server:github", map[string]any{"tool:x_github_merge_pull_request": "Allow"}}, a.values(t)[ToolsValuesKey], "nothing written")

	current := int64(7)
	_, err = a.svc.UpdateAs(dana(t), "update_agent", Update{Name: "alert-review", Description: new("matching"), ExpectedGeneration: &current})
	require.NoError(t, err)
	_, err = a.svc.UpdateAs(dana(t), "update_agent", Update{Name: "alert-review", Description: new("unconditional")})
	require.NoError(t, err)
}

// A drop that never reached muster is sent at startup, without a person's
// token (muster takes the sender token alone), and the record is cleared.
func TestStartupSweepSendsPendingDrops(t *testing.T) {
	a := newApprovalFixture(t, nil)
	a.grantMerge()
	a.bindings.dropFails = 100
	_, err := a.svc.UpdateAs(asPerson(t.Context(), "bob@example.com"), "update_agent", Update{Name: "alert-review", Description: new("x")})
	require.NoError(t, err)
	rec, _ := a.grants.Get(t.Context(), alertAgent)
	require.True(t, rec.PendingSessionSync)

	a.bindings.dropFails = 0
	sent, failed := a.svc.SweepPendingSessionSyncs(context.Background())
	assert.Equal(t, 1, sent)
	assert.Zero(t, failed)
	require.Len(t, a.bindings.drops, 1)
	assert.Empty(t, a.bindings.drops[0].Keep)
	rec, _ = a.grants.Get(t.Context(), alertAgent)
	assert.Empty(t, rec.Version, "synced: the empty record is gone")
	assert.Contains(t, a.logs.String(), `"msg":"pending approval grant syncs swept"`)
}
