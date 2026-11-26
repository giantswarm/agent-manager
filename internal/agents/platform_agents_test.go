package agents

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/agent-manager/internal/kagent"
	"github.com/giantswarm/agent-manager/internal/kube/kubetest"
	"github.com/giantswarm/agent-manager/internal/muster"
)

// With a remote target the platform's default agent is rendered by a
// release on the management cluster, agent-manager runs on the target: the
// release cannot be read, so its approval values come with agent-manager's
// configuration (PlatformAgent), checked against the live Agent's labels
// and the agent chart's X-Muster-Tool-Approval header.

// onRemoteTarget turns alert-review into such an agent: its release is gone
// from this cluster, the Agent's labels name the release on the management
// cluster, and agent-manager is configured with values.
func (a *approvalFixture) onRemoteTarget(t *testing.T, values map[string]any) PlatformAgent {
	t.Helper()
	require.NoError(t, a.dyn.Resource(hrGVR).Namespace("kagent").Delete(t.Context(), "alert-review", metav1.DeleteOptions{}))
	a.relabel(t, "flux-giantswarm", "wc1-default-agent")
	p := PlatformAgent{Namespace: "kagent", Name: "alert-review", Release: ReleaseRef{Namespace: "flux-giantswarm", Name: "wc1-default-agent"}, Values: values}
	a.svc.cfg.PlatformAgents = []PlatformAgent{p}
	return p
}

// setApprovalHeader renders the agent chart's X-Muster-Tool-Approval header
// on the agent's RemoteMCPServer.
func (a *approvalFixture) setApprovalHeader(t *testing.T, value string) {
	t.Helper()
	obj, err := a.dyn.Resource(serverGVR).Namespace("kagent").Get(t.Context(), "alert-review", metav1.GetOptions{})
	require.NoError(t, err)
	headers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "headersFrom")
	headers = append(headers, map[string]any{"name": ToolApprovalHeader, "value": value})
	require.NoError(t, unstructured.SetNestedSlice(obj.Object, headers, "spec", "headersFrom"))
	_, err = a.dyn.Resource(serverGVR).Namespace("kagent").Update(t.Context(), obj, metav1.UpdateOptions{})
	require.NoError(t, err)
}

func remoteAskingValues() map[string]any {
	return map[string]any{
		"tools":   []any{map[string]any{"server:github": "Ask"}, map[string]any{"tool:x_github_merge_pull_request": "Block"}},
		"default": "Ask",
	}
}

func TestASessionOfAnAgentOnARemoteTargetBindsTheConfiguredSettings(t *testing.T) {
	for name, header := range map[string]string{
		"the chart renders the header":   "server:github=Ask,tool:x_github_merge_pull_request=Block,default=Ask",
		"header items in another order":  "default=Ask,tool:x_github_merge_pull_request=Block,server:github=Ask",
		"a chart that renders no header": "",
	} {
		t.Run(name, func(t *testing.T) {
			a := newApprovalFixture(t, nil)
			a.onRemoteTarget(t, remoteAskingValues())
			if header != "" {
				a.setApprovalHeader(t, header)
			}
			b := a.start(t)
			assert.Equal(t, "kagent/alert-review", b.Agent)
			require.NotNil(t, b.Approval)
			assert.Equal(t, []muster.ApprovalEntry{{Entry: "server:github", Setting: "Ask"}, {Entry: "tool:x_github_merge_pull_request", Setting: "Block"}}, b.Approval.Entries)
			assert.Equal(t, "Ask", b.Approval.Default)
			assert.Empty(t, b.Approval.Grants, "grants live with a release agent-manager can read")
			assert.Equal(t, []string{"server:github", "tool:x_github_merge_pull_request"}, b.Toolset)
		})
	}
}

// The default agent ships without settings: it binds with none, and no
// release is read (agent-manager could not).
func TestAPlatformAgentWithoutSettingsBindsWithNone(t *testing.T) {
	a := newApprovalFixture(t, nil)
	a.onRemoteTarget(t, map[string]any{"toolset": []any{"preset:full"}})
	reads := 0
	a.dyn.PrependReactor("get", "helmreleases", func(k8stesting.Action) (bool, runtime.Object, error) {
		reads++
		return false, nil, nil
	})
	b := a.start(t)
	assert.Equal(t, "kagent/alert-review", b.Agent)
	require.NotNil(t, b.Approval)
	assert.Empty(t, b.Approval.Entries)
	assert.Empty(t, b.Approval.Default)
	assert.Zero(t, reads, "no HelmRelease is read for a platform agent")
}

// muster.requireApproval (agent chart 2.x) binds as default Ask.
func TestAPlatformAgentWithRequireApprovalAsks(t *testing.T) {
	a := newApprovalFixture(t, nil)
	a.onRemoteTarget(t, map[string]any{"toolset": []any{"server:github"}, "muster": map[string]any{"requireApproval": true}})
	b := a.start(t)
	require.NotNil(t, b.Approval)
	assert.Equal(t, "Ask", b.Approval.Default)
}

// Tampered or missing data never loosens: every inconsistency refuses.
func TestAPlatformAgentStartIsRefusedWhenTheLiveObjectsDisagree(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(t *testing.T, a *approvalFixture)
		want  string
	}{
		"labels removed from the Agent": {func(t *testing.T, a *approvalFixture) {
			obj, err := a.dyn.Resource(agentGVR).Namespace("kagent").Get(t.Context(), "alert-review", metav1.GetOptions{})
			require.NoError(t, err)
			obj.SetLabels(map[string]string{})
			_, err = a.dyn.Resource(agentGVR).Namespace("kagent").Update(t.Context(), obj, metav1.UpdateOptions{})
			require.NoError(t, err)
		}, `but its helm.toolkit.fluxcd.io/namespace and helm.toolkit.fluxcd.io/name labels name ""/""`},
		"labels name another release": {func(t *testing.T, a *approvalFixture) {
			a.relabel(t, "kagent", "alert-review")
		}, `labels name "kagent"/"alert-review"`},
		"header without the configured Ask (release loosened past the configuration, or edited)": {func(t *testing.T, a *approvalFixture) {
			a.setApprovalHeader(t, "tool:x_github_merge_pull_request=Block,default=Ask")
		}, "renders other tool approval settings than agent-manager is configured with"},
		"header stricter than the configuration (release tightened, agent-manager not yet upgraded)": {func(t *testing.T, a *approvalFixture) {
			a.setApprovalHeader(t, "server:github=Block,tool:x_github_merge_pull_request=Block,default=Ask")
		}, "rendered default=Ask,server:github=Block"},
		"unreadable header": {func(t *testing.T, a *approvalFixture) {
			a.setApprovalHeader(t, "server:github")
		}, "unreadable header item"},
	} {
		t.Run(name, func(t *testing.T) {
			a := newApprovalFixture(t, nil)
			a.onRemoteTarget(t, remoteAskingValues())
			tc.setup(t, a)
			err := a.startErr(t)
			require.ErrorIs(t, err, ErrApprovalUnavailable)
			assert.Contains(t, err.Error(), tc.want)
			assert.Empty(t, a.bindings.Bound, "nothing bound")
		})
	}
}

// A platform agent whose objects neither agent-manager nor the caller may
// read refuses: its labels and header cannot be checked.
func TestAPlatformAgentStartIsRefusedWhenItsObjectsAreUnreadable(t *testing.T) {
	for _, resource := range []string{"agents", "remotemcpservers"} {
		t.Run(resource, func(t *testing.T) {
			a := newApprovalFixture(t, nil)
			p := a.onRemoteTarget(t, remoteAskingValues())
			client, err := a.svc.kube.Client(dana(t))
			require.NoError(t, err)
			denied := kubetest.NewDynamicClient()
			denied.PrependReactor("get", "*", func(act k8stesting.Action) (bool, runtime.Object, error) {
				obj, err := client.Dynamic().Resource(act.GetResource()).Namespace(act.GetNamespace()).Get(t.Context(), act.(k8stesting.GetAction).GetName(), metav1.GetOptions{})
				return true, obj, err
			})
			denied.PrependReactor("get", resource, func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, forbiddenErr(resource)
			})
			_, bound, err := a.svc.platformAgentApproval(dana(t), []dynamic.Interface{denied, denied}, kagent.Ref{Namespace: "kagent", Name: "alert-review"}, p, true, nil)
			require.ErrorIs(t, err, ErrForbidden)
			assert.Contains(t, err.Error(), "neither by agent-manager's ServiceAccount nor by you (get "+resource+")")
			assert.Nil(t, bound)
		})
	}
}

// With tool approval off, a platform agent with settings is refused, one
// without starts unbound.
func TestAPlatformAgentWithoutApprovalBinding(t *testing.T) {
	a := newApprovalFixture(t, nil)
	a.onRemoteTarget(t, remoteAskingValues())
	a.bindings.noSender = true
	err := a.startErr(t)
	require.ErrorIs(t, err, ErrApprovalUnavailable)
	assert.Contains(t, err.Error(), "does not bind them to sessions")
}

func TestParsePlatformAgents(t *testing.T) {
	got, err := ParsePlatformAgents(`[{"namespace":"kagent","name":"default-agent","release":{"namespace":"flux-giantswarm","name":"wc1-default-agent"},"values":{"toolset":["preset:full"],"default":"Ask","muster":{"requireApproval":false}}}]`)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, ReleaseRef{Namespace: "flux-giantswarm", Name: "wc1-default-agent"}, got[0].Release)

	none, err := ParsePlatformAgents("  ")
	require.NoError(t, err)
	assert.Nil(t, none)

	for name, raw := range map[string]string{
		"not JSON":           `{`,
		"unknown field":      `[{"namespace":"kagent","name":"a","release":{"namespace":"f","name":"a"},"settings":{}}]`,
		"no release":         `[{"namespace":"kagent","name":"a"}]`,
		"listed twice":       `[{"namespace":"kagent","name":"a","release":{"namespace":"f","name":"a"}},{"namespace":"kagent","name":"a","release":{"namespace":"f","name":"b"}}]`,
		"other values key":   `[{"namespace":"kagent","name":"a","release":{"namespace":"f","name":"a"},"values":{"modelConfig":{}}}]`,
		"bad setting":        `[{"namespace":"kagent","name":"a","release":{"namespace":"f","name":"a"},"values":{"tools":[{"server:github":"ask"}]}}]`,
		"bad default":        `[{"namespace":"kagent","name":"a","release":{"namespace":"f","name":"a"},"values":{"default":"Never"}}]`,
		"tools not a list":   `[{"namespace":"kagent","name":"a","release":{"namespace":"f","name":"a"},"values":{"tools":"server:github"}}]`,
		"muster other keys":  `[{"namespace":"kagent","name":"a","release":{"namespace":"f","name":"a"},"values":{"muster":{"url":"x"}}}]`,
		"duplicate selector": `[{"namespace":"kagent","name":"a","release":{"namespace":"f","name":"a"},"values":{"tools":["server:github",{"server:github":"Ask"}]}}]`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParsePlatformAgents(raw)
			assert.Error(t, err)
		})
	}
}
