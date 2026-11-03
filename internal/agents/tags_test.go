package agents

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/giantswarm/agent-manager/internal/chart"
	"github.com/giantswarm/agent-manager/internal/skills"
	"github.com/giantswarm/agent-manager/internal/tags"
	"github.com/giantswarm/agent-manager/internal/tags/tagstest"
)

func TestBuildValuesComposesTags(t *testing.T) {
	tests := []struct {
		name    string
		tags    []string
		want    []any
		present bool
	}{
		{name: "omitted when nil", tags: nil},
		{name: "omitted when empty", tags: []string{}},
		{name: "omitted when only blanks", tags: []string{" "}},
		{name: "normalized", tags: []string{" Util", "marketing", "UTIL"}, want: []any{"util", "marketing"}, present: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			values := BuildValues(Spec{Name: "sre", ModelConfig: "mc", Tags: tc.tags}, ComposeConfig{})
			got, ok := values["agent"].(map[string]any)["tags"]
			assert.Equal(t, tc.present, ok)
			if tc.present {
				assert.Equal(t, tc.want, got)
			}
		})
	}
}

func TestMergedValuesTags(t *testing.T) {
	tags := func(v ...string) *[]string { return &v }
	tests := []struct {
		name    string
		current []any
		upd     *[]string
		want    []any
	}{
		{name: "nil leaves them", current: []any{"util"}, upd: nil, want: []any{"util"}},
		{name: "replaced, normalized", current: []any{"util"}, upd: tags("Marketing ", "ops", "OPS"), want: []any{"marketing", "ops"}},
		{name: "empty clears", current: []any{"util"}, upd: tags(), want: nil},
		{name: "only blanks clear", current: []any{"util"}, upd: tags(" "), want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := seeded(t)
			ctx := t.Context()
			hr, err := f.dyn.Resource(hrGVR).Namespace("kagent").Get(ctx, "verifier", metav1.GetOptions{})
			require.NoError(t, err)
			values := mustValues(hr)
			values["agent"].(map[string]any)["tags"] = tc.current
			hr.Object["spec"].(map[string]any)["values"] = values
			st, err := f.svc.site(ctx, Location{})
			require.NoError(t, err)

			after, before, err := f.svc.mergedValues(ctx, st, Update{Name: "verifier", Tags: tc.upd}, hr)
			require.NoError(t, err)
			assert.Equal(t, tc.current, before["agent"].(map[string]any)["tags"])
			got, ok := after["agent"].(map[string]any)["tags"]
			if tc.want == nil {
				assert.False(t, ok, "tags removed from the values")
				return
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestAgentReadsTags(t *testing.T) {
	rule := tagstest.Rule()
	obj := agentObject("kagent", "sre", "", "", true, true)
	annotations := obj.GetAnnotations()
	annotations[TagsAnnotation] = "util, marketing"
	obj.SetAnnotations(annotations)
	a := agentFromObject(obj, nil, rule)
	assert.Equal(t, []string{"util", "marketing"}, a.Tags, "from the annotation")
	assert.Equal(t, []string{"util", "marketing"}, a.Summary().Tags)
	assert.Nil(t, agentFromObject(obj, nil, nil).Tags, "a chart without a tag rule: no tags")

	// No Agent yet: the release's values.
	pending := Agent{Name: "pending", Namespace: "kagent", Managed: ManagedNone}
	applyHelmRelease(&pending, helmRelease("kagent", "pending", map[string]any{"agent": map[string]any{"name": "pending", "tags": []any{"ops"}}}, false, nil), rule)
	assert.Equal(t, []string{"ops"}, pending.Tags)
}

// TestAgentReadsTagsFromTheValuesFirst: the release's values are what
// agent-manager wrote; the Agent's annotation lags until Flux renders them.
func TestAgentReadsTagsFromTheValuesFirst(t *testing.T) {
	rule := tagstest.Rule()
	tests := []struct {
		name   string
		values map[string]any
		want   []string
	}{
		{name: "values' tags over the stale annotation", values: map[string]any{"agent": map[string]any{"name": "sre", "tags": []any{"ops"}}}, want: []string{"ops"}},
		{name: "values without tags: cleared", values: map[string]any{"agent": map[string]any{"name": "sre"}}, want: nil},
		{name: "no agent values: the annotation", values: map[string]any{"modelConfig": map[string]any{"name": "mc"}}, want: []string{"stale"}},
		{name: "no values: the annotation", values: nil, want: []string{"stale"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			obj := agentObject("kagent", "sre", "sre", "kagent", true, true)
			obj.SetAnnotations(map[string]string{TagsAnnotation: "stale"})
			a := agentFromObject(obj, nil, rule)
			applyHelmRelease(&a, helmRelease("kagent", "sre", tc.values, true, nil), rule)
			assert.Equal(t, tc.want, a.Tags)
		})
	}
}

func TestAgentReadsHandSetTagsNormalized(t *testing.T) {
	rule := tagstest.Rule()
	tests := []struct {
		name       string
		annotation string
		values     []any
		want       []string
	}{
		{name: "none", want: nil},
		{name: "case and duplicates", annotation: "Util,util, MARKETING ,,", values: []any{"Util", "util", " MARKETING ", ""}, want: []string{"util", "marketing"}},
		{name: "malformed dropped", annotation: "ok,bad_tag," + strings.Repeat("a", 64), values: []any{"ok", "bad_tag", strings.Repeat("a", 64)}, want: []string{"ok"}},
	}
	for _, tc := range tests {
		t.Run(tc.name+"/annotation", func(t *testing.T) {
			obj := agentObject("kagent", "bare", "", "", true, true)
			obj.SetAnnotations(map[string]string{TagsAnnotation: tc.annotation})
			assert.Equal(t, tc.want, agentFromObject(obj, nil, rule).Tags)
		})
		t.Run(tc.name+"/release values", func(t *testing.T) {
			var tags any
			if tc.values != nil {
				tags = tc.values
			}
			hr := helmRelease("kagent", "sre", map[string]any{"agent": map[string]any{"name": "sre", "tags": tags}}, true, nil)
			a := Agent{}
			applyHelmRelease(&a, hr, rule)
			assert.Equal(t, tc.want, a.Tags)
		})
	}
}

// TestListFiltersByTags: the service filters, for REST and MCP alike, and an
// update's tags count at once, before Flux renders the annotation.
func TestListFiltersByTags(t *testing.T) {
	f := newFixture(t, []runtime.Object{
		helmRelease("kagent", "a", map[string]any{"agent": map[string]any{"name": "a", "tags": []any{"ops", "triage"}}}, true, nil),
		helmRelease("kagent", "b", map[string]any{"agent": map[string]any{"name": "b", "tags": []any{"marketing"}}}, true, nil),
		helmRelease("kagent", "c", map[string]any{"agent": map[string]any{"name": "c"}}, true, nil),
	})
	tests := []struct {
		name string
		tags []string
		want []string
	}{
		{name: "nil keeps all", tags: nil, want: []string{"a", "b", "c"}},
		{name: "only blanks keep all", tags: []string{" ", ""}, want: []string{"a", "b", "c"}},
		{name: "one tag", tags: []string{"triage"}, want: []string{"a"}},
		{name: "any of them", tags: []string{"marketing", "ops"}, want: []string{"a", "b"}},
		{name: "case and spaces normalized", tags: []string{" MARKETING "}, want: []string{"b"}},
		{name: "none matches", tags: []string{"nope"}, want: []string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			list, err := f.svc.List(t.Context(), In("kagent"), tags.NewFilter(tc.tags))
			require.NoError(t, err)
			got := []string{}
			for _, a := range list {
				got = append(got, a.Name)
			}
			assert.Equal(t, tc.want, got)
		})
	}

	// The Agent object renders c with a stale annotation; c's values, which
	// an update wrote, decide.
	obj := agentObject("kagent", "c", "c", "kagent", true, true)
	obj.SetAnnotations(map[string]string{TagsAnnotation: "old"})
	mustCreate(t, f, agentGVR, obj)
	hr, err := f.dyn.Resource(hrGVR).Namespace("kagent").Get(t.Context(), "c", metav1.GetOptions{})
	require.NoError(t, err)
	require.NoError(t, unstructured.SetNestedSlice(hr.Object, []any{"new"}, "spec", "values", "agent", "tags"))
	_, err = f.dyn.Resource(hrGVR).Namespace("kagent").Update(t.Context(), hr, metav1.UpdateOptions{})
	require.NoError(t, err)
	for filter, want := range map[string]int{"new": 1, "old": 0} {
		list, err := f.svc.List(t.Context(), In("kagent"), tags.NewFilter([]string{filter}))
		require.NoError(t, err)
		assert.Len(t, list, want, filter)
	}
	got, err := f.svc.Get(t.Context(), In("kagent"), "c")
	require.NoError(t, err)
	assert.Equal(t, []string{"new"}, got.Tags)
}

func TestSkillsTagged(t *testing.T) {
	list := []skills.Skill{
		{Name: "triage", Tags: []string{"triage", "kubernetes"}},
		{Name: "notes", Tags: []string{"writing"}},
		{Name: "untagged"},
	}
	tests := []struct {
		name string
		tags []string
		want []string
	}{
		{name: "no filter", tags: nil, want: []string{"triage", "notes", "untagged"}},
		{name: "any of them", tags: []string{"writing", "kubernetes"}, want: []string{"triage", "notes"}},
		{name: "case and spaces normalized", tags: []string{" Kubernetes "}, want: []string{"triage"}},
		{name: "none matches", tags: []string{"nope"}, want: []string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			names := []string{}
			for _, sk := range skillsTagged(list, tags.NewFilter(tc.tags)) {
				names = append(names, sk.Name)
			}
			assert.Equal(t, tc.want, names)
		})
	}
	assert.Nil(t, skillsTagged(nil, tags.NewFilter([]string{"x"})), "an unreadable repository's nil stays nil")
	assert.Len(t, list, 3, "the input (the discoverer's cache) is not changed")
	assert.Equal(t, "notes", list[1].Name)
}

// tagsChart is the embedded agent chart with agent.tags declared or not.
type tagsChart struct {
	embeddedChart
	declares bool
}

func (c tagsChart) Schema(context.Context) chart.Schema {
	s := chart.EmbeddedSchema()
	doc := runtime.DeepCopyJSON(s.Document.(map[string]any))
	if c.declares {
		if err := unstructured.SetNestedField(doc, map[string]any{"type": "array"}, "properties", "agent", "properties", "tags"); err != nil {
			panic(err)
		}
	} else {
		unstructured.RemoveNestedField(doc, "properties", "agent", "properties", "tags")
	}
	s.Document = doc
	return s
}

func TestInfoTagsCapabilityFollowsTheChart(t *testing.T) {
	for _, declares := range []bool{true, false} {
		f := seeded(t)
		f.svc.chart = tagsChart{declares: declares}
		assert.Equal(t, declares, f.svc.Info(t.Context()).Capabilities["tags"])
	}
	f := seeded(t)
	f.svc.chart = tagsChart{declares: true}
	f.svc.cfg.Tags = nil
	assert.False(t, f.svc.Info(t.Context()).Capabilities["tags"], "no tag rule in the embedded chart: no tags")
}
