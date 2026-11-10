package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agent-manager/internal/identity"
	"github.com/giantswarm/agent-manager/internal/kagent"
	apiv1alpha1 "github.com/giantswarm/agent-manager/internal/kagent/gen/kagent/api/v1alpha1"
	"github.com/giantswarm/agent-manager/internal/kagent/kagenttest"
	"github.com/giantswarm/agent-manager/internal/organizations"
)

func orgAgent(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "api.kagent.dev/v1alpha3", "kind": "Agent",
		"metadata": map[string]any{"name": name, "namespace": orgNS},
	}}
}

// The erase preview counts, per agent and across every creator, the
// sessions the erase deletes, listed as the caller.
func TestTheErasePreviewCountsTheSessions(t *testing.T) {
	for _, tc := range []struct {
		name        string
		seed        func(*kagenttest.Controller)
		sessions    []any
		unavailable string
	}{
		{
			name: "running and stopped sessions of two agents",
			seed: func(c *kagenttest.Controller) {
				c.AddSessionOf("bob@example.com", orgNS, "triage")
				c.AddSession(orgNS, "review")
				c.AddSession(orgNS, "review").State = apiv1alpha1.RuntimeState_RUNTIME_STATE_SUSPENDED
				c.AddSession(orgNS, "review").State = apiv1alpha1.RuntimeState_RUNTIME_STATE_DELETED
				c.AddSession("agents-billing-a1b2c", "triage")
			},
			sessions: []any{
				map[string]any{"agent": "review", "running": float64(1), "stopped": float64(1)},
				map[string]any{"agent": "triage", "running": float64(1), "stopped": float64(0)},
			},
		},
		{
			name:     "none",
			seed:     func(*kagenttest.Controller) {},
			sessions: []any{},
		},
		{
			name: "a refused list",
			seed: func(c *kagenttest.Controller) {
				c.Fail["ListSessions"] = status.Error(codes.PermissionDenied, "not allowed to list every creator's sessions")
			},
			unavailable: "PermissionDenied: not allowed to list every creator's sessions",
		},
		{
			name: "an unreachable runtime, named by its status code alone",
			seed: func(c *kagenttest.Controller) {
				c.Fail["ListSessions"] = status.Error(codes.Unavailable, "dial tcp 10.96.0.12:8080: connection refused")
			},
			unavailable: "the agent runtime answered Unavailable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl, conn := kagenttest.New(t)
			tc.seed(ctrl)
			f := newOrgFixtureWith(t, func(cfg *organizations.Config) { cfg.Sessions = kagent.NewClient(conn) },
				organization("org-acme", "payments", nil), orgAgent("review"), orgAgent("triage"))
			f.ctx = identity.ContextWithToken(f.ctx, "id-token-of-alice")

			text, isErr := f.call(t, ToolGetOrganization, map[string]any{"namespace": "org-acme", "name": "payments"})
			require.False(t, isErr, text)
			erases := decode(t, text)["erases"].(map[string]any)

			if tc.unavailable != "" {
				assert.Nil(t, erases["sessions"])
				assert.Contains(t, erases["sessionsUnavailable"], tc.unavailable)
				assert.NotContains(t, erases["sessionsUnavailable"], "10.96.0.12")
				return
			}
			assert.NotContains(t, erases, "sessionsUnavailable")
			assert.ElementsMatch(t, tc.sessions, erases["sessions"])
			var listed []string
			for _, call := range ctrl.Calls() {
				if call.Method == "ListSessions" {
					request := call.Request.(*apiv1alpha1.ListSessionsRequest)
					listed = append(listed, request.GetAgent().GetName())
					assert.Equal(t, orgNS, request.GetAgent().GetNamespace())
					assert.True(t, request.GetAllCreators(), "sessions are listed across creators")
					assert.Equal(t, []string{"Bearer id-token-of-alice"}, call.Metadata.Get("authorization"))
				}
			}
			assert.ElementsMatch(t, []string{"review", "triage"}, listed, "every agent of the organization is listed")
		})
	}

	t.Run("without an agent runtime target", func(t *testing.T) {
		f := newOrgFixture(t, organization("org-acme", "payments", nil), orgAgent("review"))
		text, isErr := f.call(t, ToolGetOrganization, map[string]any{"namespace": "org-acme", "name": "payments"})
		require.False(t, isErr, text)
		erases := decode(t, text)["erases"].(map[string]any)
		assert.Nil(t, erases["sessions"])
		assert.Contains(t, erases["sessionsUnavailable"], "--kagent-target")
	})

	t.Run("the delete preview carries the same count", func(t *testing.T) {
		ctrl, conn := kagenttest.New(t)
		ctrl.AddSession(orgNS, "review")
		f := newOrgFixtureWith(t, func(cfg *organizations.Config) { cfg.Sessions = kagent.NewClient(conn) },
			organization("org-acme", "payments", nil), orgAgent("review"))
		f.ctx = identity.ContextWithToken(f.ctx, "id-token-of-alice")

		text, isErr := f.call(t, ToolDeleteOrganization, map[string]any{"namespace": "org-acme", "name": "payments"})
		require.False(t, isErr, text)
		erases := decode(t, text)["erases"].(map[string]any)
		assert.Equal(t, []any{map[string]any{"agent": "review", "running": float64(1), "stopped": float64(0)}}, erases["sessions"])
	})
}
