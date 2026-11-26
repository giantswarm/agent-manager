package agents

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	apiv1alpha1 "github.com/giantswarm/agent-manager/internal/kagent/gen/kagent/api/v1alpha1"
	"github.com/giantswarm/agent-manager/internal/muster"
)

// A Session binds the settings of the release that renders its agent, found
// by the Agent's Flux provenance labels: the platform's default agent is
// rendered by agent-platform/default-agent into kagent, not by a release
// kagent/<agent name>.

const renderedUID = "uid-rendering-release"

// renderedBy moves alert-review's settings to the release hrNs/hrName (spec
// edited by edit) and points the Agent's provenance labels at it; the
// release named after the agent is gone.
func (a *approvalFixture) renderedBy(t *testing.T, hrNs, hrName string, values map[string]any, edit func(hr *unstructured.Unstructured)) {
	t.Helper()
	require.NoError(t, a.dyn.Resource(hrGVR).Namespace("kagent").Delete(t.Context(), "alert-review", metav1.DeleteOptions{}))
	hr := helmRelease(hrNs, hrName, values, true, nil)
	hr.SetUID(renderedUID)
	hr.SetGeneration(3)
	if edit != nil {
		edit(hr)
	}
	mustCreate(t, a.fixture, hrGVR, hr)
	a.relabel(t, hrNs, hrName)
}

// relabel sets the Agent's provenance labels.
func (a *approvalFixture) relabel(t *testing.T, hrNs, hrName string) {
	t.Helper()
	obj, err := a.dyn.Resource(agentGVR).Namespace("kagent").Get(t.Context(), "alert-review", metav1.GetOptions{})
	require.NoError(t, err)
	labels := obj.GetLabels()
	labels[HelmReleaseNameLabel], labels[HelmReleaseNamespaceLabel] = hrName, hrNs
	obj.SetLabels(labels)
	_, err = a.dyn.Resource(agentGVR).Namespace("kagent").Update(t.Context(), obj, metav1.UpdateOptions{})
	require.NoError(t, err)
}

func askingValues() map[string]any {
	return map[string]any{
		"modelConfig": map[string]any{"name": "default-model-config"},
		"tools":       []any{map[string]any{"server:github": "Ask"}, map[string]any{"tool:x_github_merge_pull_request": "Block"}},
	}
}

func TestASessionBindsTheSettingsOfTheReleaseThatRendersItsAgent(t *testing.T) {
	for name, tc := range map[string]struct {
		hrNs, hrName string
		edit         func(hr *unstructured.Unstructured)
	}{
		"release named otherwise, agent.name set": {"kagent", "alert-review-release", func(hr *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(hr.Object, "alert-review", "spec", "values", "agent", "name")
		}},
		"release named otherwise, releaseName set": {"kagent", "alert-review-release", func(hr *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(hr.Object, "alert-review", "spec", "releaseName")
		}},
		// The platform's default agent: agent-platform/default-agent with
		// targetNamespace kagent and releaseName default-agent.
		"release in another namespace targeting the agent's": {"agent-platform", "alert-review", func(hr *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(hr.Object, "kagent", "spec", "targetNamespace")
			_ = unstructured.SetNestedField(hr.Object, "alert-review", "spec", "releaseName")
		}},
		"release in another namespace, Flux's default release name": {"agent-platform", "review", func(hr *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(hr.Object, "kagent", "spec", "targetNamespace")
			_ = unstructured.SetNestedField(hr.Object, "alert-review", "spec", "values", "agent", "name")
		}},
	} {
		t.Run(name, func(t *testing.T) {
			a := newApprovalFixture(t, nil)
			a.renderedBy(t, tc.hrNs, tc.hrName, askingValues(), tc.edit)
			b := a.start(t)
			assert.Equal(t, "kagent/alert-review", b.Agent)
			require.NotNil(t, b.Approval)
			assert.Equal(t, []muster.ApprovalEntry{{Entry: "server:github", Setting: "Ask"}, {Entry: "tool:x_github_merge_pull_request", Setting: "Block"}}, b.Approval.Entries)
			assert.Equal(t, []string{"server:github", "tool:x_github_merge_pull_request"}, b.Toolset, "the rendering release's own list")
		})
	}
}

// The platform's own explicit state: a release that renders the agent
// without settings binds the agent with none.
func TestAnAgentWhoseReleaseHasNoSettingsBindsWithNone(t *testing.T) {
	a := newApprovalFixture(t, nil)
	a.renderedBy(t, "agent-platform", "alert-review", map[string]any{
		"modelConfig": map[string]any{"name": "default-model-config"},
		"toolset":     []any{"server:github"},
	}, func(hr *unstructured.Unstructured) {
		_ = unstructured.SetNestedField(hr.Object, "kagent", "spec", "targetNamespace")
		_ = unstructured.SetNestedField(hr.Object, "alert-review", "spec", "releaseName")
	})
	b := a.start(t)
	assert.Equal(t, "kagent/alert-review", b.Agent)
	require.NotNil(t, b.Approval)
	assert.Empty(t, b.Approval.Entries)
	assert.Empty(t, b.Approval.Default)
	assert.Empty(t, b.Approval.Grants)
}

func TestAStartIsRefusedWhenTheRendererIsUnknown(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(t *testing.T, a *approvalFixture)
		want  string
	}{
		"labels name a release in another namespace that is gone": {func(t *testing.T, a *approvalFixture) {
			a.relabel(t, "agent-platform", "default-agent")
		}, "HelmRelease agent-platform/default-agent (its helm.toolkit.fluxcd.io/namespace and helm.toolkit.fluxcd.io/name labels), which cannot be found"},
		"labels name a release that renders another agent": {func(t *testing.T, a *approvalFixture) {
			hr := helmRelease("kagent", "other", map[string]any{"agent": map[string]any{"name": "other"}, "tools": []any{"server:github"}}, true, nil)
			mustCreate(t, a.fixture, hrGVR, hr)
			a.relabel(t, "kagent", "other")
		}, "names HelmRelease kagent/other as its renderer, which renders the Agent kagent/other instead"},
		"labels name a release that targets another namespace": {func(t *testing.T, a *approvalFixture) {
			hr := helmRelease("agent-platform", "alert-review", map[string]any{"tools": []any{"server:github"}}, true, nil)
			_ = unstructured.SetNestedField(hr.Object, "alert-review", "spec", "releaseName")
			mustCreate(t, a.fixture, hrGVR, hr)
			a.relabel(t, "agent-platform", "alert-review")
		}, "which renders the Agent agent-platform/alert-review instead"},
	} {
		t.Run(name, func(t *testing.T) {
			a := newApprovalFixture(t, nil)
			tc.setup(t, a)
			err := a.startErr(t)
			require.ErrorIs(t, err, ErrApprovalUnavailable)
			assert.Contains(t, err.Error(), tc.want)
			assert.Empty(t, a.bindings.Bound, "nothing bound")
		})
	}
}

// A start refused after kagent created its Session deletes the Session,
// which spends the request id at kagent: its retry is told to use a new one,
// and a new one starts the Session once the cause is gone.
func TestARetryOfARefusedStartNeedsANewRequestID(t *testing.T) {
	a := newApprovalFixture(t, nil)
	a.relabel(t, "agent-platform", "default-agent")
	svc, ctrl, _ := newBoundSessionFixtureWithService(t, a.fixture)
	svc.cfg.Bindings = a.bindings
	ctrl.ActorJWTProviders = []*apiv1alpha1.SessionProvider{musterProvider()}

	_, err := svc.StartSession(dana(t), StartSession{Name: "alert-review", RequestID: "req-1"})
	require.ErrorIs(t, err, ErrApprovalUnavailable)

	a.relabel(t, "kagent", "alert-review")
	_, err = svc.StartSession(dana(t), StartSession{Name: "alert-review", RequestID: "req-1"})
	require.ErrorIs(t, err, ErrConflict)
	assert.Contains(t, err.Error(), `requestId "req-1" belongs to a session that no longer exists`)
	assert.Contains(t, err.Error(), "start again with a new requestId")

	out, err := svc.StartSession(dana(t), StartSession{Name: "alert-review", RequestID: "req-2"})
	require.NoError(t, err)
	assert.NotEmpty(t, out.Session)
}
