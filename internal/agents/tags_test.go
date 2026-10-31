package agents

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/agent-manager/internal/skills"
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
			ctx := context.Background()
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
	obj := agentObject("kagent", "sre", "", "", true, true)
	annotations := obj.GetAnnotations()
	annotations[TagsAnnotation] = "util, marketing"
	obj.SetAnnotations(annotations)
	a := agentFromObject(obj, nil)
	assert.Equal(t, []string{"util", "marketing"}, a.Tags, "from the annotation")
	assert.Equal(t, []string{"util", "marketing"}, a.Summary().Tags)

	// No Agent yet: the release's values.
	pending := Agent{Name: "pending", Namespace: "kagent", Managed: ManagedNone}
	applyHelmRelease(&pending, helmRelease("kagent", "pending", map[string]any{"agent": map[string]any{"name": "pending", "tags": []any{"ops"}}}, false, nil))
	assert.Equal(t, []string{"ops"}, pending.Tags)
}

func TestAgentReadsHandSetTagsNormalized(t *testing.T) {
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
			assert.Equal(t, tc.want, agentFromObject(obj, nil).Tags)
		})
		t.Run(tc.name+"/release values", func(t *testing.T) {
			var tags any
			if tc.values != nil {
				tags = tc.values
			}
			hr := helmRelease("kagent", "sre", map[string]any{"agent": map[string]any{"name": "sre", "tags": tags}}, true, nil)
			a := Agent{}
			applyHelmRelease(&a, hr)
			assert.Equal(t, tc.want, a.Tags)
		})
	}
}

func TestFilterByTags(t *testing.T) {
	list := []Agent{
		{Name: "a", Tags: []string{"ops", "triage"}},
		{Name: "b", Tags: []string{"marketing"}},
		{Name: "c"},
	}
	tests := []struct {
		name string
		tags []string
		want []string
	}{
		{name: "nil keeps all", tags: nil, want: []string{"a", "b", "c"}},
		{name: "empty keeps all", tags: []string{}, want: []string{"a", "b", "c"}},
		{name: "only blanks keep all", tags: []string{" ", ""}, want: []string{"a", "b", "c"}},
		{name: "one tag", tags: []string{"triage"}, want: []string{"a"}},
		{name: "any of them", tags: []string{"marketing", "ops"}, want: []string{"a", "b"}},
		{name: "case and spaces normalized", tags: []string{" MARKETING "}, want: []string{"b"}},
		{name: "none matches", tags: []string{"nope"}, want: []string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := []string{}
			for _, a := range FilterByTags(list, tc.tags) {
				got = append(got, a.Name)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestFilterSkillsByTags(t *testing.T) {
	res := &skills.Result{Repositories: []skills.Repository{
		{RepoURL: "https://github.com/o/one", Commit: "c1", Skills: []skills.Skill{
			{Name: "triage", Tags: []string{"triage", "kubernetes"}},
			{Name: "notes", Tags: []string{"writing"}},
			{Name: "untagged"},
		}},
		{RepoURL: "https://github.com/o/two", Error: "unreadable", Skills: []skills.Skill{}},
		{RepoURL: "https://github.com/o/three", Error: "unreadable"},
	}}
	tests := []struct {
		name string
		tags []string
		want [][]string
	}{
		{name: "no filter", tags: nil, want: [][]string{{"triage", "notes", "untagged"}, {}}},
		{name: "any of them", tags: []string{"writing", "kubernetes"}, want: [][]string{{"triage", "notes"}, {}}},
		{name: "case and spaces normalized", tags: []string{" Kubernetes "}, want: [][]string{{"triage"}, {}}},
		{name: "none matches, repositories stay", tags: []string{"nope"}, want: [][]string{{}, {}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := FilterSkillsByTags(res, tc.tags)
			require.Len(t, got.Repositories, 3)
			assert.Nil(t, got.Repositories[2].Skills, "nil stays nil, as unfiltered")
			assert.Equal(t, "c1", got.Repositories[0].Commit)
			assert.Equal(t, "unreadable", got.Repositories[1].Error)
			for i, repo := range got.Repositories[:2] {
				names := []string{}
				for _, sk := range repo.Skills {
					names = append(names, sk.Name)
				}
				assert.Equal(t, tc.want[i], names)
			}
		})
	}
	assert.Len(t, res.Repositories[0].Skills, 3, "the input (the discoverer's cache) is not changed")
}
