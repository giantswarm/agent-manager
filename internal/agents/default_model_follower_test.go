package agents

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// defaultAgent is the platform's default agent as the agent-platform chart
// renders it, modelConfig set to current ("" for none).
func defaultAgent(current string) *unstructured.Unstructured {
	obj := agentObject("kagent", "default-agent", "agent-platform-default-agent", "kagent", false, false)
	if current == "" {
		unstructured.RemoveNestedField(obj.Object, "spec", "template", "modelConfig")
	} else {
		_ = unstructured.SetNestedField(obj.Object, current, "spec", "template", "modelConfig", "name")
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

func TestTheFollowerFillsAnEmptyModelConfigOnce(t *testing.T) {
	tests := []struct {
		name    string
		objs    []runtime.Object
		want    string
		patched bool
	}{
		{
			name:    "empty and one default: filled",
			objs:    []runtime.Object{defaultAgent(""), markedModel("kagent", "claude"), modelConfig("kagent", "qwen", "OpenAI", "qwen3")},
			want:    "claude",
			patched: true,
		},
		{
			name: "set, another the default: untouched",
			objs: []runtime.Object{defaultAgent("qwen"), markedModel("kagent", "claude")},
			want: "qwen",
		},
		{
			name: "empty, several defaults: untouched",
			objs: []runtime.Object{defaultAgent(""), markedModel("kagent", "claude"), markedModel("kagent", "gpt")},
		},
		{
			name: "empty, no default: untouched",
			objs: []runtime.Object{defaultAgent(""), modelConfig("kagent", "qwen", "OpenAI", "qwen3")},
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

func TestTheFollowerKeepsAFilledModelWhenTheDefaultMoves(t *testing.T) {
	ctx := context.Background()
	dyn, f := followerFixture(defaultAgent(""), markedModel("kagent", "claude"), modelConfig("kagent", "qwen", "OpenAI", "qwen3"))
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
	assert.Equal(t, "claude", modelOf(t, dyn))
	assert.Equal(t, 1, patches(dyn))
}

func TestTheFollowerWarnsOnSeveralDefaults(t *testing.T) {
	var buf bytes.Buffer
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, defaultAgent(""), markedModel("kagent", "claude"), markedModel("kagent", "gpt"))
	f := NewDefaultModelFollower(dyn, "v1alpha3", "kagent", "default-agent", slog.New(slog.NewTextHandler(&buf, nil)))
	require.NoError(t, f.Follow(context.Background()))
	assert.Contains(t, buf.String(), "level=WARN")
	assert.Contains(t, buf.String(), "marked=\"[claude gpt]\"")
	assert.Zero(t, patches(dyn))
}
