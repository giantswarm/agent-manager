package skills

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSourcesAcceptsURLsAndObjects(t *testing.T) {
	for name, tc := range map[string]struct {
		in   string
		want []Source
	}{
		"empty":           {in: "", want: nil},
		"comma-separated": {in: "https://github.com/a/b, https://github.com/c/d", want: []Source{{URL: "https://github.com/a/b"}, {URL: "https://github.com/c/d"}}},
		"JSON strings":    {in: `["https://github.com/a/b"]`, want: []Source{{URL: "https://github.com/a/b"}}},
		"JSON mixed": {
			in: ` [{"url":"https://github.com/giantswarm/agent-platform","ref":"v1.4.0","path":"/skills/","readOnly":true},"https://github.com/c/d"]`,
			want: []Source{
				{URL: "https://github.com/giantswarm/agent-platform", Ref: "v1.4.0", Path: "skills", ReadOnly: true},
				{URL: "https://github.com/c/d"},
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := ParseSources(tc.in)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestParseSourcesRefusesMalformedEntries(t *testing.T) {
	for name, in := range map[string]string{
		"not JSON":       `[{"url":`,
		"no url":         `[{"ref":"main"}]`,
		"climbing path":  `[{"url":"https://github.com/a/b","path":"../x"}]`,
		"ref whitespace": `[{"url":"https://github.com/a/b","ref":"a b"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseSources(in)
			assert.Error(t, err)
		})
	}
}
