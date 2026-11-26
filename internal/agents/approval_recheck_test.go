package agents

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/agent-manager/internal/approvalgrants"
	apiv1alpha1 "github.com/giantswarm/agent-manager/internal/kagent/gen/kagent/api/v1alpha1"
	"github.com/giantswarm/agent-manager/internal/kube"
	"github.com/giantswarm/agent-manager/internal/kube/kubetest"
	"github.com/giantswarm/agent-manager/internal/muster"
)

// Regression tests of the second review round of the approval grants.

// saReader is agent-manager's ServiceAccount reader whose HelmRelease reads
// are refused (an organization namespace the chart grants no reader Role in).
type saReader struct {
	kube.Client
	dyn dynamic.Interface
}

func (r saReader) Dynamic() dynamic.Interface { return r.dyn }

func forbiddenServiceAccountReader(t *testing.T, a *approvalFixture) saReader {
	t.Helper()
	base, err := a.svc.kube.Client(dana(t))
	require.NoError(t, err)
	dyn := kubetest.NewDynamicClient()
	dyn.PrependReactor("get", "helmreleases", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, forbiddenErr("helmreleases")
	})
	return saReader{Client: base, dyn: dyn}
}

// where the ServiceAccount may not read the release, it is read as the
// person starting the Session; only when neither may the start is refused.
func TestStartReadsTheReleaseAsTheCallerWhereTheServiceAccountMayNot(t *testing.T) {
	a := newApprovalFixture(t, map[string]any{
		"agent": map[string]any{"name": "alert-review"}, "modelConfig": map[string]any{"name": "default-model-config"},
		"tools": []any{map[string]any{"server:github": "Ask"}},
	})
	a.svc.cfg.ApprovalReader = forbiddenServiceAccountReader(t, a)
	b := a.start(t)
	require.NotNil(t, b.Approval, "bound with the settings the caller read")
	assert.Equal(t, []muster.ApprovalEntry{{Entry: "server:github", Setting: "Ask"}}, b.Approval.Entries)

	a.dyn.PrependReactor("get", "helmreleases", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, forbiddenErr("helmreleases")
	})
	err := a.startErr(t)
	require.ErrorIs(t, err, ErrForbidden)
	assert.Contains(t, err.Error(), "neither by agent-manager's ServiceAccount nor by you")
}

// without a namespace the listing covers the organization namespaces
// too; an organization the caller may not read is skipped without its name,
// one that fails otherwise is counted, a configured one is named.
func TestListApprovalGrantsCoversTheOrganizations(t *testing.T) {
	a := newApprovalFixture(t, nil)
	a.grantMerge()
	withTenants(a.fixture, &fakeTenants{namespaces: []string{"org-a", "org-b"}})
	a.dyn.PrependReactor("list", "helmreleases", func(act k8stesting.Action) (bool, runtime.Object, error) {
		switch act.GetNamespace() {
		case "org-a":
			return true, nil, forbiddenErr("helmreleases")
		case "org-b":
			return true, nil, errors.New("etcd unavailable")
		}
		return false, nil, nil
	})
	got, err := a.svc.ListApprovalGrants(dana(t), "", "")
	require.NoError(t, err)
	assert.Len(t, got.Grants, 1)
	assert.Equal(t, 1, got.UnreadableCount, "org-b counted, org-a (not a member) skipped")
	assert.Empty(t, got.Unreadable, "no organization is named")

	a.dyn.PrependReactor("list", "helmreleases", func(act k8stesting.Action) (bool, runtime.Object, error) {
		if act.GetNamespace() == "kagent" {
			return true, nil, forbiddenErr("helmreleases")
		}
		return false, nil, nil
	})
	got, err = a.svc.ListApprovalGrants(dana(t), "", "")
	require.NoError(t, err)
	assert.Equal(t, []string{"kagent"}, got.Unreadable, "a configured namespace is named")
}

// the startup sweep covers the organization namespaces' records.
func TestTheSweepCoversTheOrganizations(t *testing.T) {
	a := newApprovalFixture(t, nil)
	withTenants(a.fixture, &fakeTenants{namespaces: []string{"org-a"}})
	gone := approvalgrants.Agent{UID: "uid-org", Namespace: "org-a", Name: "bot"}
	require.NoError(t, a.grants.Replace(context.Background(), approvalgrants.Record{Agent: gone, PendingSessionSync: true}))
	sent, failed := a.svc.SweepPendingSessionSyncs(context.Background())
	assert.Equal(t, 1, sent)
	assert.Zero(t, failed)
	require.Len(t, a.bindings.drops, 1)
	assert.Equal(t, "org-a/bot", a.bindings.drops[0].Agent)
}

// a deleted agent's pending empty record, re-sent
// while an agent of the same name lives under another release UID, tells
// muster that agent's grants, never "keep none".
func TestAGoneAgentsRecordKeepsTheRecreatedAgentsMarks(t *testing.T) {
	a := newApprovalFixture(t, nil)
	old := approvalgrants.Agent{UID: "old-uid", Namespace: "kagent", Name: "alert-review"}
	require.NoError(t, a.grants.Replace(context.Background(), approvalgrants.Record{Agent: old, PendingSessionSync: true}))
	a.grants.Put(alertAgent, approvalgrants.Grant{Entry: "tool:x_github_merge_pull_request", Server: "github", ConnectorNamespace: "muster", ConnectorUID: "uid-github", Generation: 7, Tools: []string{"merge_pull_request"}})

	sent, _ := a.svc.SweepPendingSessionSyncs(context.Background())
	assert.Equal(t, 1, sent)
	require.Len(t, a.bindings.drops, 1)
	assert.Equal(t, []muster.Grant{{Entry: "tool:x_github_merge_pull_request", Server: "github", UID: "uid-github", Tools: []string{"merge_pull_request"}}}, a.bindings.drops[0].Keep,
		"the live agent keeps its grant; the keep marks carry their tools")
	assert.Len(t, a.stored(t), 1)
	rec, _ := a.grants.Get(context.Background(), old)
	assert.Empty(t, rec.Version, "the gone agent's record is removed once sent")
}

// Only the recreated agent's grants at its release's current generation are
// kept: a stale grant of an earlier generation is not sent (D-GO-12).
func TestAGoneAgentsRecordKeepsOnlyTheRecreatedAgentsCurrentMarks(t *testing.T) {
	a := newApprovalFixture(t, nil)
	old := approvalgrants.Agent{UID: "old-uid", Namespace: "kagent", Name: "alert-review"}
	require.NoError(t, a.grants.Replace(context.Background(), approvalgrants.Record{Agent: old, PendingSessionSync: true}))
	a.grants.Put(alertAgent,
		approvalgrants.Grant{Entry: "tool:x_github_merge_pull_request", Server: "github", ConnectorNamespace: "muster", ConnectorUID: "uid-github", Generation: 7, Tools: []string{"merge_pull_request"}},
		approvalgrants.Grant{Entry: "tool:x_github_delete_repository", Server: "github", ConnectorNamespace: "muster", ConnectorUID: "uid-github", Generation: 6, Tools: []string{"delete_repository"}},
	)

	sent, _ := a.svc.SweepPendingSessionSyncs(context.Background())
	assert.Equal(t, 1, sent)
	require.Len(t, a.bindings.drops, 1)
	assert.Equal(t, []muster.Grant{{Entry: "tool:x_github_merge_pull_request", Server: "github", UID: "uid-github", Tools: []string{"merge_pull_request"}}}, a.bindings.drops[0].Keep,
		"the grant written at generation 6 is not the release's 7: never kept")
}

// A record of the live agent itself (same release UID) without grants is no
// recreated agent: muster is told to keep none, even when the store meanwhile
// holds grants a later save syncs itself (D-GO-12).
func TestARecordOfTheLiveAgentIsNoRecreatedAgent(t *testing.T) {
	a := newApprovalFixture(t, nil)
	a.grants.Put(alertAgent, approvalgrants.Grant{Entry: "tool:x_github_merge_pull_request", Server: "github", ConnectorNamespace: "muster", ConnectorUID: "uid-github", Generation: 7, Tools: []string{"merge_pull_request"}})

	a.svc.syncSessions(dana(t), "update_agent", approvalgrants.Record{Agent: alertAgent, PendingSessionSync: true}, false)
	require.Len(t, a.bindings.drops, 1)
	assert.Empty(t, a.bindings.drops[0].Keep, "the record sent keeps no grant; the same UID is not a successor")
}

// settings that only the rendered header shows (a release with
// valuesFrom and no inline settings) refuse a start.
func TestValuesFromWithSettingsOnlyInTheRenderedHeaderRefusesAStart(t *testing.T) {
	a := newApprovalFixture(t, map[string]any{
		"agent": map[string]any{"name": "alert-review"}, "modelConfig": map[string]any{"name": "default-model-config"},
		"tools": []any{"server:github"},
	})
	hr, err := a.dyn.Resource(hrGVR).Namespace("kagent").Get(t.Context(), "alert-review", metav1.GetOptions{})
	require.NoError(t, err)
	require.NoError(t, unstructured.SetNestedSlice(hr.Object, []any{map[string]any{"kind": "ConfigMap", "name": "more"}}, "spec", "valuesFrom"))
	_, err = a.dyn.Resource(hrGVR).Namespace("kagent").Update(t.Context(), hr, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, a.startErr(t), "valuesFrom without settings starts")

	server, err := a.dyn.Resource(serverGVR).Namespace("kagent").Get(t.Context(), "alert-review", metav1.GetOptions{})
	require.NoError(t, err)
	headers, _, _ := unstructured.NestedSlice(server.Object, "spec", "headersFrom")
	headers = append(headers, map[string]any{"name": ToolApprovalHeader, "value": "server:github=Block"})
	require.NoError(t, unstructured.SetNestedSlice(server.Object, headers, "spec", "headersFrom"))
	_, err = a.dyn.Resource(serverGVR).Namespace("kagent").Update(t.Context(), server, metav1.UpdateOptions{})
	require.NoError(t, err)
	err = a.startErr(t)
	require.ErrorIs(t, err, ErrApprovalUnavailable)
	assert.Contains(t, err.Error(), "valuesFrom")
}

// get_agent and list_agents send a pending sync again.
func TestReadsSendAPendingSyncAgain(t *testing.T) {
	for name, read := range map[string]func(a *approvalFixture) error{
		"get_agent":   func(a *approvalFixture) error { _, err := a.svc.Get(dana(t), Location{}, "alert-review"); return err },
		"list_agents": func(a *approvalFixture) error { _, err := a.svc.List(dana(t), Location{}, nil); return err },
	} {
		a := newApprovalFixture(t, nil)
		a.grantMerge()
		rec, _ := a.grants.Get(context.Background(), alertAgent)
		rec.PendingSessionSync = true
		require.NoError(t, a.grants.Replace(context.Background(), rec))
		require.NoError(t, read(a), name)
		require.Len(t, a.bindings.drops, 1, name)
		rec, _ = a.grants.Get(context.Background(), alertAgent)
		assert.False(t, rec.PendingSessionSync, name)
	}
}

// Start runs the sweep (cmd/serve.go calls it).
func TestStartSweeps(t *testing.T) {
	a := newApprovalFixture(t, nil)
	rec := approvalgrants.Record{Agent: approvalgrants.Agent{UID: "gone", Namespace: "kagent", Name: "gone"}, PendingSessionSync: true}
	require.NoError(t, a.grants.Replace(context.Background(), rec))
	<-a.svc.Start(context.Background())
	require.Len(t, a.bindings.drops, 1)
	assert.Equal(t, "kagent/gone", a.bindings.drops[0].Agent)
}

// removing a stale grant at a start keeps a newer one.
func TestRemovingAStaleGrantKeepsANewerOne(t *testing.T) {
	a := newApprovalFixture(t, nil)
	a.grants.Put(alertAgent,
		approvalgrants.Grant{Entry: "tool:x_github_merge_pull_request", Server: "github", ConnectorNamespace: "muster", ConnectorUID: "uid-github", Generation: 6},
		approvalgrants.Grant{Entry: "default", Server: "pagerduty", ConnectorNamespace: "connectors", ConnectorUID: "uid-pd", Generation: 8})
	_ = a.start(t)
	stored := a.stored(t)
	require.Len(t, stored, 1)
	assert.Equal(t, int64(8), stored[0].Generation)
}

// a regrant that no longer needs a connector reports its drop.
func TestARegrantReportsTheConnectorItLeavesOut(t *testing.T) {
	a := newApprovalFixture(t, nil)
	a.admins["muster"] = true
	a.grants.Put(alertAgent,
		approvalgrants.Grant{Entry: "tool:x_github_merge_pull_request", Server: "github", ConnectorNamespace: "muster", ConnectorUID: "uid-github", Generation: 7},
		approvalgrants.Grant{Entry: "tool:x_github_merge_pull_request", Server: "pagerduty", ConnectorNamespace: "connectors", ConnectorUID: "uid-pd", Generation: 7})
	res, err := a.svc.SetApproval(dana(t), SetApproval{Name: "alert-review", Settings: map[string]string{"tool:x_github_merge_pull_request": "Allow"}})
	require.NoError(t, err)
	require.Len(t, res.Dropped, 1)
	assert.Equal(t, "pagerduty", res.Dropped[0].Server)
	assert.Contains(t, res.Dropped[0].Reason, "regranted without this connector")
}

// Nit: a bind whose mark's tools were narrowed meanwhile is noticed.
func TestARecheckComparesTheMarksTools(t *testing.T) {
	a := newApprovalFixture(t, nil)
	a.grants.Put(alertAgent, approvalgrants.Grant{Entry: "tool:x_github_merge_pull_request", Server: "github", ConnectorNamespace: "muster", ConnectorUID: "uid-github", Generation: 7, Tools: []string{"merge_pull_request", "squash"}})
	a.bindings.onBind = func() {
		a.bindings.onBind = nil
		a.grants.Put(alertAgent, approvalgrants.Grant{Entry: "tool:x_github_merge_pull_request", Server: "github", ConnectorNamespace: "muster", ConnectorUID: "uid-github", Generation: 7, Tools: []string{"merge_pull_request"}})
	}
	_ = a.start(t)
	require.Len(t, a.bindings.drops, 1, "the narrowed mark is sent")
	assert.Equal(t, []string{"merge_pull_request"}, a.bindings.drops[0].Keep[0].Tools)
}

var _ = apiv1alpha1.SessionProvider{}
