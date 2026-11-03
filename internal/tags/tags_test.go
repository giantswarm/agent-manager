package tags

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// chartRule is the agent chart's rule from tagstest's copy of tag.schema.json
// (tagstest imports this package, so it is read from the file here).
func chartRule(t *testing.T, maxItems int) *Rule {
	t.Helper()
	schema, err := os.ReadFile("tagstest/tag.schema.json")
	require.NoError(t, err)
	r, err := NewRule(schema, maxItems)
	require.NoError(t, err)
	return r
}

func TestNormalize(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{name: "nil", in: nil, want: nil},
		{name: "trimmed and lowercased", in: []string{" Util ", "MARKETING"}, want: []string{"util", "marketing"}},
		{name: "duplicates dropped, first order kept", in: []string{"b", "a", "B", " a"}, want: []string{"b", "a"}},
		{name: "empty entries dropped", in: []string{"", "  ", "x"}, want: []string{"x"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Normalize(tc.in))
		})
	}
}

func TestParse(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{name: "empty", in: "", want: nil},
		{name: "comma-joined, normalized, deduplicated", in: " Triage, kubernetes,triage,, ", want: []string{"triage", "kubernetes"}},
		{name: "malformed dropped", in: "ok, bad_tag, -lead, trail-, a--b, two words", want: []string{"ok"}},
		{name: "63 characters kept", in: strings.Repeat("a", 63), want: []string{strings.Repeat("a", 63)}},
		{name: "64 characters dropped", in: strings.Repeat("a", 64), want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, chartRule(t, 50).Parse(tc.in))
		})
	}
}

func TestParseList(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{name: "nil", in: nil, want: nil},
		{name: "normalized", in: []string{" Ops", "ops", "TRIAGE"}, want: []string{"ops", "triage"}},
		{name: "malformed dropped", in: []string{"ok", "bad_tag", strings.Repeat("a", 64), "a,b"}, want: []string{"ok"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, chartRule(t, 50).ParseList(tc.in))
		})
	}
}

func TestRuleLimits(t *testing.T) {
	var none *Rule
	assert.Nil(t, none.ParseList([]string{"ok"}), "a chart without a tag schema keeps no tag")
	assert.Equal(t, []string{"a", "b"}, chartRule(t, 2).ParseList([]string{"a", "bad_tag", "b", "c"}), "at most maxItems valid tags")

	_, err := NewRule([]byte(`{"pattern": "(?<=a)"}`), 0)
	assert.Error(t, err, "a pattern Go cannot compile")
	_, err = NewRule([]byte(`[`), 0)
	assert.Error(t, err)
}

func TestFilter(t *testing.T) {
	tests := []struct {
		name   string
		values []string
		have   []string
		match  bool
	}{
		{name: "nil matches everything", values: nil, have: nil, match: true},
		{name: "only blanks match everything", values: []string{"", " , "}, have: []string{"a"}, match: true},
		{name: "repeated", values: []string{"x", "B"}, have: []string{"a", "b"}, match: true},
		{name: "comma-joined, case and spaces ignored", values: []string{"x, A "}, have: []string{"a"}, match: true},
		{name: "none shared", values: []string{"x"}, have: []string{"a"}},
		{name: "no tags", values: []string{"x"}, have: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.match, NewFilter(tc.values).Matches(tc.have))
		})
	}
}
