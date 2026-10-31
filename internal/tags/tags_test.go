package tags

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

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
			assert.Equal(t, tc.want, Parse(tc.in))
		})
	}
}

func TestHasAny(t *testing.T) {
	tests := []struct {
		name       string
		have, want []string
		match      bool
	}{
		{name: "one shared", have: []string{"a", "b"}, want: []string{"x", "b"}, match: true},
		{name: "none shared", have: []string{"a"}, want: []string{"x"}},
		{name: "no tags", have: nil, want: []string{"x"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.match, HasAny(tc.have, tc.want))
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
			assert.Equal(t, tc.want, ParseList(tc.in))
		})
	}
}

func TestSplit(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{name: "nil", in: nil, want: nil},
		{name: "one empty value", in: []string{""}, want: nil},
		{name: "repeated", in: []string{"a", "B"}, want: []string{"a", "b"}},
		{name: "comma-joined and repeated", in: []string{"a, B", " c ,a"}, want: []string{"a", "b", "c"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Split(tc.in))
		})
	}
}
