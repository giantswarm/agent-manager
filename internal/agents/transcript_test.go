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
		"error true":           {in: map[string]any{"error": true}, want: map[string]any{"error": true}, wantFailed: true},
		"error object":         {in: map[string]any{"error": map[string]any{"code": 7.0}}, want: map[string]any{"error": map[string]any{"code": 7.0}}, wantFailed: true},
		"error false":          {in: map[string]any{"error": false, "items": []any{}}, want: map[string]any{"error": false, "items": []any{}}},
		"error empty":          {in: map[string]any{"error": map[string]any{}}, want: map[string]any{"error": map[string]any{}}},
		"error zero":           {in: map[string]any{"error": 0.0}, want: map[string]any{"error": 0.0}},
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

func TestClipTextStaysWithinItsBound(t *testing.T) {
	assert.Equal(t, "short", clipText("short", 100))
	got := clipText(strings.Repeat("é", 100), 51)
	assert.True(t, utf8.ValidString(got))
	assert.True(t, strings.HasSuffix(got, "bytes clipped]"))
	assert.LessOrEqual(t, len(got), 51)

	huge := strings.Repeat("x", 5_000_000)
	for _, n := range []int{0, 5, 23, 24, 30, 1000} {
		got := clipText(huge, n)
		assert.LessOrEqual(t, len(got), n, n)
	}
	assert.Equal(t, "xxxxx", clipText(huge, 5), "no room to say what was cut")
	assert.True(t, utf8.ValidString(clipText(strings.Repeat("é", 10), 5)))
}
