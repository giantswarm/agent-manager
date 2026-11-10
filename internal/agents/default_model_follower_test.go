package agents

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// followingAgent is the platform's default agent as the agent-platform chart
// renders it without a model: labelled to follow the default, modelConfig
// set to current ("" for none).
func followingAgent(current string, follows bool) *unstructured.Unstructured {
	obj := agentObject("kagent", "default-agent", "agent-platform-default-agent", "kagent", false, false)
	if current == "" {
		unstructured.RemoveNestedField(obj.Object, "spec", "template", "modelConfig")
	} else {
		_ = unstructured.SetNestedField(obj.Object, current, "spec", "template", "modelConfig", "name")
	}
	if follows {
		labels := obj.GetLabels()
		labels[FollowsDefaultModelLabel] = "true"
		obj.SetLabels(labels)
	}
	return obj
}

func followerFixture(objs ...runtime.Object) (*dynamicfake.FakeDynamicClient, *DefaultModelFollower) {
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, objs...)
	return dyn, NewDefaultModelFollower(dyn, "v1alpha3", "kagent", "default-agent", nil)
}

func patches(dyn *dynamicfake.FakeDynamicClient) int {
	n := 0
	for _, a := range dyn.Actions() {
		if a.GetVerb() == "patch" {
			n++
		}
	}
	return n
}

func modelOf(t *testing.T, dyn *dynamicfake.FakeDynamicClient) string {
	t.Helper()
	obj, err := dyn.Resource(agentGVR).Namespace("kagent").Get(context.Background(), "default-agent", metav1.GetOptions{})
	require.NoError(t, err)
	name, _, _ := unstructured.NestedString(obj.Object, "spec", "template", "modelConfig", "name")
	return name
}

func TestTheDefaultAgentFollowsTheDefaultModel(t *testing.T) {
	tests := []struct {
		name    string
		objs    []runtime.Object
		want    string
		patched bool
	}{
		{
			name:    "label and one default: patched",
			objs:    []runtime.Object{followingAgent("", true), markedModel("kagent", "claude"), modelConfig("kagent", "qwen", "OpenAI", "qwen3")},
			want:    "claude",
			patched: true,
		},
		{
			name: "no default: left",
			objs: []runtime.Object{followingAgent("", true), modelConfig("kagent", "qwen", "OpenAI", "qwen3")},
		},
		{
			name: "several defaults: left",
			objs: []runtime.Object{followingAgent("qwen", true), markedModel("kagent", "claude"), markedModel("kagent", "gpt")},
			want: "qwen",
		},
		{
			name: "no label: left",
			objs: []runtime.Object{followingAgent("qwen", false), markedModel("kagent", "claude")},
			want: "qwen",
		},
		{
			name: "already on the default: no patch",
			objs: []runtime.Object{followingAgent("claude", true), markedModel("kagent", "claude")},
			want: "claude",
		},
		{
			name: "no agent: nothing",
			objs: []runtime.Object{markedModel("kagent", "claude")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dyn, f := followerFixture(tt.objs...)
			require.NoError(t, f.Follow(context.Background()))
			assert.Equal(t, map[bool]int{true: 1, false: 0}[tt.patched], patches(dyn))
			if tt.name != "no agent: nothing" {
				assert.Equal(t, tt.want, modelOf(t, dyn))
			}
		})
	}
}

func TestTheDefaultAgentFollowsAMovedDefault(t *testing.T) {
	ctx := context.Background()
	dyn, f := followerFixture(followingAgent("", true), markedModel("kagent", "claude"), modelConfig("kagent", "qwen", "OpenAI", "qwen3"))
	require.NoError(t, f.Follow(ctx))
	assert.Equal(t, "claude", modelOf(t, dyn))

	// The mark moves to qwen.
	claude, err := dyn.Resource(mcGVR).Namespace("kagent").Get(ctx, "claude", metav1.GetOptions{})
	require.NoError(t, err)
	claude.SetAnnotations(nil)
	_, err = dyn.Resource(mcGVR).Namespace("kagent").Update(ctx, claude, metav1.UpdateOptions{})
	require.NoError(t, err)
	_, err = dyn.Resource(mcGVR).Namespace("kagent").Update(ctx, markedModel("kagent", "qwen"), metav1.UpdateOptions{})
	require.NoError(t, err)

	require.NoError(t, f.Follow(ctx))
	assert.Equal(t, "qwen", modelOf(t, dyn))
	assert.Equal(t, 2, patches(dyn))
}

func TestGetAndListReportFollowsDefaultModel(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, []runtime.Object{followingAgent("", true), agentObject("kagent", "sre", "sre", "kagent", true, false)})
	got, err := f.svc.Get(ctx, In(""), "default-agent")
	require.NoError(t, err)
	assert.True(t, got.FollowsDefaultModel)
	list, err := f.svc.List(ctx, In(""), nil)
	require.NoError(t, err)
	follows := map[string]bool{}
	for _, a := range list {
		follows[a.Name] = a.Summary().FollowsDefaultModel
	}
	assert.Equal(t, map[string]bool{"default-agent": true, "sre": false}, follows)
}
