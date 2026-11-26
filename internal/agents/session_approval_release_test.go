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

// listed registers hrNs/hrName as the platform's release for alert-review
// (muster.toolApproval.platformReleases).
func (a *approvalFixture) listed(hrNs, hrName string) {
	a.svc.cfg.PlatformReleases = []PlatformRelease{{Namespace: hrNs, Name: hrName, Agent: AgentRef{Namespace: "kagent", Name: "alert-review"}}}
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
			a.listed(tc.hrNs, tc.hrName)
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
	a.listed("agent-platform", "alert-review")
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
			a.listed("agent-platform", "default-agent")
			a.relabel(t, "agent-platform", "default-agent")
		}, "HelmRelease agent-platform/default-agent (its helm.toolkit.fluxcd.io/namespace and helm.toolkit.fluxcd.io/name labels), which cannot be found"},
		"labels name a release that renders another agent": {func(t *testing.T, a *approvalFixture) {
			hr := helmRelease("kagent", "other", map[string]any{"agent": map[string]any{"name": "other"}, "tools": []any{"server:github"}}, true, nil)
			mustCreate(t, a.fixture, hrGVR, hr)
			a.listed("kagent", "other")
			a.relabel(t, "kagent", "other")
		}, "names HelmRelease kagent/other as its renderer, which renders the Agent kagent/other instead"},
		"labels name a release that targets another namespace": {func(t *testing.T, a *approvalFixture) {
			hr := helmRelease("agent-platform", "alert-review", map[string]any{"tools": []any{"server:github"}}, true, nil)
			_ = unstructured.SetNestedField(hr.Object, "alert-review", "spec", "releaseName")
			mustCreate(t, a.fixture, hrGVR, hr)
			a.listed("agent-platform", "alert-review")
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

// The Agent's Flux labels are the Agent's, which its editors may change: they
// never point a Session's settings at a release agent-manager does not
// know as the agent's renderer (L-01).

// An Agent editor (no rights on the platform's release) creates a decoy
// release in the agent's namespace and points the Agent's labels at it: the
// platform's agent is refused, not bound to the decoy's entries and toolset.
func TestAPlatformAgentRelabelledToADecoyReleaseIsRefused(t *testing.T) {
	a := newApprovalFixture(t, nil)
	a.renderedBy(t, "agent-platform", "alert-review", askingValues(), func(hr *unstructured.Unstructured) {
		_ = unstructured.SetNestedField(hr.Object, "kagent", "spec", "targetNamespace")
		_ = unstructured.SetNestedField(hr.Object, "alert-review", "spec", "releaseName")
	})
	a.listed("agent-platform", "alert-review")
	decoy := helmRelease("kagent", "decoy", map[string]any{
		"agent": map[string]any{"name": "alert-review"},
		"tools": []any{"server:github", "server:kubernetes", map[string]any{"tool:noop": "Ask"}},
	}, true, nil)
	mustCreate(t, a.fixture, hrGVR, decoy)
	a.relabel(t, "kagent", "decoy")
	err := a.startErr(t)
	require.ErrorIs(t, err, ErrApprovalUnavailable)
	assert.Contains(t, err.Error(), "rendered by the platform's HelmRelease agent-platform/alert-review")
	assert.Empty(t, a.bindings.Bound, "nothing bound")
}

// A platform agent whose labels are removed is refused, not bound bare.
func TestAPlatformAgentWithoutItsLabelsIsRefused(t *testing.T) {
	a := newApprovalFixture(t, nil)
	a.renderedBy(t, "agent-platform", "alert-review", askingValues(), func(hr *unstructured.Unstructured) {
		_ = unstructured.SetNestedField(hr.Object, "kagent", "spec", "targetNamespace")
		_ = unstructured.SetNestedField(hr.Object, "alert-review", "spec", "releaseName")
	})
	a.listed("agent-platform", "alert-review")
	obj, err := a.dyn.Resource(agentGVR).Namespace("kagent").Get(t.Context(), "alert-review", metav1.GetOptions{})
	require.NoError(t, err)
	labels := obj.GetLabels()
	delete(labels, HelmReleaseNameLabel)
	delete(labels, HelmReleaseNamespaceLabel)
	obj.SetLabels(labels)
	_, err = a.dyn.Resource(agentGVR).Namespace("kagent").Update(t.Context(), obj, metav1.UpdateOptions{})
	require.NoError(t, err)
	err = a.startErr(t)
	require.ErrorIs(t, err, ErrApprovalUnavailable)
	assert.Empty(t, a.bindings.Bound, "nothing bound")
}

// Any other agent: only the release agent-manager creates (the agent's
// namespace, the agent's name) is accepted through the labels.
func TestAnAgentOutsideThePlatformListAcceptsOnlyItsOwnRelease(t *testing.T) {
	for name, tc := range map[string]struct {
		hrNs, hrName string
		refused      bool
	}{
		"decoy named otherwise in the agent's namespace": {"kagent", "decoy", true},
		"release of the platform's namespace":            {"agent-platform", "alert-review", true},
		"the release named after the agent, labelled":    {"kagent", "alert-review", false},
	} {
		t.Run(name, func(t *testing.T) {
			a := newApprovalFixture(t, nil)
			if tc.hrName != "alert-review" || tc.hrNs != "kagent" {
				mustCreate(t, a.fixture, hrGVR, helmRelease(tc.hrNs, tc.hrName, map[string]any{
					"agent": map[string]any{"name": "alert-review"},
					"tools": []any{"server:github", map[string]any{"tool:noop": "Ask"}},
				}, true, nil))
			}
			a.relabel(t, tc.hrNs, tc.hrName)
			if tc.refused {
				err := a.startErr(t)
				require.ErrorIs(t, err, ErrApprovalUnavailable)
				assert.Contains(t, err.Error(), "which agent-manager does not accept")
				assert.Empty(t, a.bindings.Bound, "nothing bound")
				return
			}
			b := a.start(t)
			require.NotNil(t, b.Approval)
			assert.Equal(t, []muster.ApprovalEntry{{Entry: "tool:x_github_merge_pull_request", Setting: "Allow"}}, b.Approval.Entries)
		})
	}
}

func TestParsePlatformReleases(t *testing.T) {
	got, err := ParsePlatformReleases(`[{"namespace":"agent-platform","name":"default-agent","agent":{"namespace":"kagent","name":"default-agent"}}]`)
	require.NoError(t, err)
	assert.Equal(t, []PlatformRelease{{Namespace: "agent-platform", Name: "default-agent", Agent: AgentRef{Namespace: "kagent", Name: "default-agent"}}}, got)
	none, err := ParsePlatformReleases("")
	require.NoError(t, err)
	assert.Empty(t, none)
	for name, raw := range map[string]string{
		"no agent":      `[{"namespace":"a","name":"b"}]`,
		"unknown field": `[{"namespace":"a","name":"b","agent":{"namespace":"k","name":"x"},"x":1}]`,
		"twice":         `[{"namespace":"a","name":"b","agent":{"namespace":"k","name":"x"}},{"namespace":"c","name":"d","agent":{"namespace":"k","name":"x"}}]`,
	} {
		_, err := ParsePlatformReleases(raw)
		assert.Error(t, err, name)
	}
}
