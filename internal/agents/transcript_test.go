package agents

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
)

func TestUnwrapResponse(t *testing.T) {
	for name, tc := range map[string]struct {
		in         any
		want       any
		wantFailed bool
	}{
		"plain value":          {in: map[string]any{"pods": 3.0}, want: map[string]any{"pods": 3.0}},
		"runtime result":       {in: map[string]any{"result": "ok"}, want: "ok"},
		"runtime error":        {in: map[string]any{"result": "forbidden", "isError": true}, want: "forbidden", wantFailed: true},
		"mcp text":             {in: envelope("hello", false), want: "hello"},
		"mcp error":            {in: envelope("boom", true), want: "boom", wantFailed: true},
		"json in a string":     {in: `{"a":[1]}`, want: map[string]any{"a": []any{1.0}}},
		"nested envelopes":     {in: envelope(encoded(envelope(`{"a":1}`, false)), false), want: map[string]any{"a": 1.0}},
		"inner error":          {in: envelope(encoded(envelope("denied", true)), false), want: "denied", wantFailed: true},
		"error key":            {in: map[string]any{"error": "no route"}, want: map[string]any{"error": "no route"}, wantFailed: true},
		"text that is no json": {in: "{not json", want: "{not json"},
		"two contents stay": {
			in:   map[string]any{"content": []any{map[string]any{"type": "text", "text": "a"}, map[string]any{"type": "text", "text": "b"}}},
			want: map[string]any{"content": []any{map[string]any{"type": "text", "text": "a"}, map[string]any{"type": "text", "text": "b"}}},
		},
	} {
		got, failed := unwrapResponse(tc.in)
		assert.Equal(t, tc.want, got, name)
		assert.Equal(t, tc.wantFailed, failed, name)
	}
}

func TestClipTextKeepsRunesWhole(t *testing.T) {
	assert.Equal(t, "short", clipText("short", 100))
	got := clipText(strings.Repeat("é", 100), 51)
	assert.True(t, utf8.ValidString(got))
	assert.True(t, strings.HasSuffix(got, "bytes clipped]"))
	assert.LessOrEqual(t, len(got), 51)
}

func TestPackNewest(t *testing.T) {
	items := []string{"aaaa", "bbbb", "cccc"}
	kept, omitted := packNewest(items, 14, nil)
	assert.Equal(t, []string{"bbbb", "cccc"}, kept, "oldest first, the newest that fit")
	assert.Equal(t, 1, omitted)

	kept, omitted = packNewest(items, 3, nil)
	assert.Empty(t, kept)
	assert.Equal(t, 3, omitted)

	kept, omitted = packNewest(items, 3, func(s string, _ int) (string, bool) { return "c", true })
	assert.Equal(t, []string{"c"}, kept, "fit cuts the newest down")
	assert.Equal(t, 2, omitted)
}
