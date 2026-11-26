package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	apov1alpha1 "github.com/giantswarm/agent-platform-operator/api/v1alpha1"
	orgv1alpha1 "github.com/giantswarm/organization-operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

// orphaned sets the operator's AgentPlatformOrphaned condition on org with
// reason.
func orphaned(t *testing.T, org *unstructured.Unstructured, reason string) {
	t.Helper()
	conditions, _, err := unstructured.NestedSlice(org.Object, "status", "conditions")
	require.NoError(t, err)
	conditions = append(conditions, map[string]any{"type": orgv1alpha1.AgentPlatformOrphaned, "status": "True", "reason": reason, "lastTransitionTime": "2026-11-12T00:00:00Z"})
	require.NoError(t, unstructured.SetNestedSlice(org.Object, conditions, "status", "conditions"))
}

// pendingErase is payments in Pending erase: its delete pull request merged.
func pendingErase(t *testing.T, labels map[string]string) *unstructured.Unstructured {
	t.Helper()
	org := organization("payments", labels)
	orphaned(t, org, orgv1alpha1.AgentPlatformOrphanedDeleteMerged)
	return org
}

// beingErased is payments deleted, its finalizer still on, the operator on
// the erasure step Namespace.
func beingErased() *unstructured.Unstructured {
	org := organization("payments", nil)
	now := metav1.Now()
	org.SetDeletionTimestamp(&now)
	org.SetFinalizers([]string{apov1alpha1.Finalizer})
	status := org.Object["status"].(map[string]any)
	status["conditions"] = append(status["conditions"].([]any), map[string]any{"type": orgv1alpha1.AgentPlatformErasing, "status": "True",
		"reason": orgv1alpha1.AgentPlatformErasingNamespace, "lastTransitionTime": "2026-11-12T00:00:00Z"})
	return org
}

// oneWriteOfEachKind is one write tool of each family of an organization's
// contents.
var oneWriteOfEachKind = map[string]struct {
	tool string
	args map[string]any
}{
	"members":         {ToolAddOrganizationMember, map[string]any{"role": "editor", "group": "payments-team"}},
	"models":          {ToolSetOrganizationModel, map[string]any{"name": "haiku", "provider": "Anthropic", "model": "claude-haiku"}},
	"connectors":      {ToolSetOrganizationConnector, map[string]any{"name": "gh", "url": "https://mcp.example.com/mcp", "noCredential": true}},
	"egress policies": {ToolSetOrganizationEgress, map[string]any{"name": "github", "destinations": []any{map[string]any{"origin": "https://api.github.com"}}}},
	"credentials":     {ToolReplaceOrganizationCredential, map[string]any{"secret": sharedSecret, "key": "token"}},
}

// requireNothingWritten fails when the caller sent a write, dry run or not.
func requireNothingWritten(t *testing.T, f *orgFixture) {
	t.Helper()
	for _, a := range f.dyn.Actions() {
		switch a.(type) {
		case k8stesting.CreateAction, k8stesting.UpdateAction, k8stesting.PatchAction, k8stesting.DeleteAction:
			t.Errorf("a %s %s was sent", a.GetVerb(), a.GetResource().Resource)
		}
	}
	assert.Empty(t, f.dryRuns.Received(), "nothing is dry-run either")
}

// Every write tool refuses a write into an organization in Pending erase, or
// one being erased, in both modes, with a conflict that names the state and
// the way forward; nothing is written and no pull request opens.
func TestWritesToAnOrganizationInPendingEraseAreRefused(t *testing.T) {
	states := map[string]struct {
		org      func(t *testing.T, labels map[string]string) *unstructured.Unstructured
		contains []string
	}{
		"pending erase": {pendingErase, []string{"Pending erase", "DeleteMerged", "Erase now", `confirm "payments"`, "restore its file"}},
		"being erased": {func(*testing.T, map[string]string) *unstructured.Unstructured { return beingErased() },
			[]string{"being erased", "(Namespace)"}},
	}
	for state, s := range states {
		for kind, w := range oneWriteOfEachKind {
			for _, mode := range []string{"apply", "commit"} {
				t.Run(state+"/"+kind+"/"+mode, func(t *testing.T) {
					args := map[string]any{"organization": "payments", "mode": mode}
					for k, v := range w.args {
						args[k] = v
					}
					var f *orgFixture
					pullRequests := func() int { return 0 }
					if mode == "commit" {
						g, fake := commitFixture(t, nil, s.org(t, fromGitLabels("flux")))
						f, pullRequests = g, func() int { return len(fake.PullRequests()) }
					} else {
						f = newOrgFixture(t, s.org(t, nil))
					}
					text, isErr := f.call(t, w.tool, args)
					require.True(t, isErr, text)
					assert.True(t, strings.HasPrefix(text, "conflict:"), text)
					assert.Contains(t, text, "organization payments")
					for _, c := range s.contains {
						assert.Contains(t, text, c)
					}
					if state == "pending erase" && mode == "commit" {
						assert.Contains(t, text, paymentsFile, "the file to restore")
					}
					requireNothingWritten(t, f)
					assert.Zero(t, pullRequests())
				})
			}
		}
	}
}

// A caller who may not read in the organization namespace gets forbidden,
// not the organization's state, which agent-manager reads with its own
// ServiceAccount.
func TestACallerWithoutRightsIsNotToldThePendingErase(t *testing.T) {
	writes := map[string]struct {
		tool string
		args map[string]any
	}{"delete an object": {ToolDeleteOrganizationObject, map[string]any{"kind": "model", "name": "claude"}}}
	for kind, w := range oneWriteOfEachKind {
		writes[kind] = w
	}
	for kind, w := range writes {
		t.Run(kind, func(t *testing.T) {
			f := newOrgFixture(t, pendingErase(t, nil), model("claude", true))
			for _, verb := range []string{"get", "list"} {
				f.dyn.PrependReactor(verb, "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
					if a.GetNamespace() != orgNS {
						return false, nil, nil
					}
					return true, nil, apierrors.NewForbidden(a.GetResource().GroupResource(), "", errors.New("RBAC: access denied"))
				})
			}
			args := map[string]any{"organization": "payments"}
			for k, v := range w.args {
				args[k] = v
			}
			text, isErr := f.call(t, w.tool, args)
			require.True(t, isErr, text)
			assert.True(t, strings.HasPrefix(text, "forbidden:"), text)
			assert.NotContains(t, text, "Pending erase")
		})
	}
}

// Orphaned for another reason than a merged delete is not Pending erase:
// the write goes on.
func TestAnOrganizationMissingFromGitIsStillWritten(t *testing.T) {
	org := organization("payments", nil)
	orphaned(t, org, orgv1alpha1.AgentPlatformOrphanedMissingFromGit)
	f := newOrgFixture(t, org)
	text, isErr := f.call(t, ToolAddOrganizationMember, map[string]any{"organization": "payments", "role": "editor", "group": "payments-team"})
	require.False(t, isErr, text)
	assert.NotNil(t, f.get(t, rbGVR, orgNS, "organization-editors"))
}

// In Pending erase the reads keep working, a write is refused, and Erase now
// still erases.
func TestPendingEraseKeepsReadsAndEraseNow(t *testing.T) {
	f, fake := gitOrganization(t)
	text, isErr := f.call(t, ToolDeleteOrganization, map[string]any{"name": "payments", "mode": "commit"})
	require.False(t, isErr, text)
	require.Len(t, fake.PullRequests(), 1)
	require.NoError(t, fake.Merge(context.Background(), fake.PullRequests()[0].PullRequest))
	// The operator reports the merged delete.
	live := f.get(t, orgGVR, "", "payments")
	orphaned(t, live, orgv1alpha1.AgentPlatformOrphanedDeleteMerged)
	_, err := f.dyn.Resource(orgGVR).UpdateStatus(t.Context(), live, metav1.UpdateOptions{})
	require.NoError(t, err)

	text, isErr = f.call(t, ToolSetOrganizationModel, map[string]any{"organization": "payments", "name": "haiku", "provider": "Anthropic", "model": "claude-haiku", "mode": "commit"})
	require.True(t, isErr, text)
	assert.Contains(t, text, "Pending erase")
	assert.Len(t, fake.PullRequests(), 1)

	for _, read := range []struct {
		tool string
		args map[string]any
	}{
		{ToolGetOrganization, map[string]any{"name": "payments"}},
		{ToolListOrganizationMembers, map[string]any{"organization": "payments"}},
		{ToolListOrganizationConnector, map[string]any{"organization": "payments"}},
	} {
		text, isErr = f.call(t, read.tool, read.args)
		require.False(t, isErr, "%s: %s", read.tool, text)
	}

	text, isErr = f.call(t, ToolDeleteOrganization, map[string]any{"name": "payments", "mode": "commit", "confirm": "payments"})
	require.False(t, isErr, text)
	assert.Equal(t, true, decode(t, text)["deleted"])
	assert.Nil(t, f.get(t, orgGVR, "", "payments"))
}

// update_organization refuses an organization in Pending erase, or one being
// erased, in both modes, with the content write tools' conflict: it names
// the state and the way forward. Nothing is written and no pull request
// opens.
func TestUpdateOrganizationInPendingEraseIsRefused(t *testing.T) {
	states := map[string]struct {
		org      func(t *testing.T, labels map[string]string) *unstructured.Unstructured
		contains []string
	}{
		"pending erase": {pendingErase, []string{"Pending erase", "DeleteMerged", "Erase now", `confirm "payments"`, "restore its file", "namespace " + orgNS}},
		"being erased": {func(*testing.T, map[string]string) *unstructured.Unstructured { return beingErased() },
			[]string{"being erased", "(Namespace)"}},
	}
	for state, s := range states {
		for _, mode := range []string{"apply", "commit"} {
			t.Run(state+"/"+mode, func(t *testing.T) {
				var f *orgFixture
				pullRequests := func() int { return 0 }
				if mode == "commit" {
					g, fake := commitFixture(t, nil, s.org(t, fromGitLabels("flux")))
					f, pullRequests = g, func() int { return len(fake.PullRequests()) }
				} else {
					f = newOrgFixture(t, s.org(t, nil))
				}
				text, isErr := f.call(t, ToolUpdateOrganization, map[string]any{"name": "payments", "displayName": "Payments", "mode": mode})
				require.True(t, isErr, text)
				assert.True(t, strings.HasPrefix(text, "conflict:"), text)
				assert.Contains(t, text, "organization payments")
				for _, c := range s.contains {
					assert.Contains(t, text, c)
				}
				if state == "pending erase" && mode == "commit" {
					assert.Contains(t, text, paymentsFile, "the file to restore")
				}
				requireNothingWritten(t, f)
				assert.Zero(t, pullRequests())
			})
		}
	}
}

// A caller who may not read the organization gets forbidden from
// update_organization, not its Pending erase.
func TestUpdateOrganizationWithoutRightsIsNotToldThePendingErase(t *testing.T) {
	f := newOrgFixture(t, pendingErase(t, nil))
	f.dyn.PrependReactor("get", "organizations", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(a.GetResource().GroupResource(), "payments", errors.New("RBAC: access denied"))
	})
	text, isErr := f.call(t, ToolUpdateOrganization, map[string]any{"name": "payments", "displayName": "Payments"})
	require.True(t, isErr, text)
	assert.True(t, strings.HasPrefix(text, "forbidden:"), text)
	assert.NotContains(t, text, "Pending erase")
	requireNothingWritten(t, f)
}

// Orphaned for another reason than a merged delete is not Pending erase:
// update_organization goes on.
func TestUpdateOrganizationMissingFromGitIsStillWritten(t *testing.T) {
	org := organization("payments", nil)
	orphaned(t, org, orgv1alpha1.AgentPlatformOrphanedMissingFromGit)
	f := newOrgFixture(t, org)
	text, isErr := f.call(t, ToolUpdateOrganization, map[string]any{"name": "payments", "displayName": "Payments"})
	require.False(t, isErr, text)
	assert.Equal(t, []any{"displayName"}, decode(t, text)["changed"])
}
