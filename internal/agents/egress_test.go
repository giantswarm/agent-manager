package agents

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestValidateEgress(t *testing.T) {
	require.NoError(t, ValidateEgress(nil))
	require.NoError(t, ValidateEgress([]string{
		"https://github.com:443",
		"https://*.githubusercontent.com",
		"http://proxy.internal:3128",
		"https://proxy.golang.org",
	}))
	long := make([]string, MaxEgress+1)
	for i := range long {
		long[i] = "https://host" + strings.Repeat("a", i/26+1) + string(rune('a'+i%26)) + ".example.com"
	}
	for name, tc := range map[string]struct {
		list []string
		want string
	}{
		"too many":          {long, "at most 64"},
		"too long":          {[]string{"https://" + strings.Repeat("a", 263) + ".com"}, "at most 270"},
		"no scheme":         {[]string{"github.com:443"}, "not an HTTP(S) origin"},
		"path":              {[]string{"https://github.com/giantswarm"}, "not an HTTP(S) origin"},
		"upper case":        {[]string{"https://GitHub.com"}, "not an HTTP(S) origin"},
		"wildcard tld":      {[]string{"https://*.com"}, "not an HTTP(S) origin"},
		"wildcard mid":      {[]string{"https://raw.*.com"}, "not an HTTP(S) origin"},
		"port zero":         {[]string{"https://github.com:0"}, "not an HTTP(S) origin"},
		"port out of range": {[]string{"https://github.com:65536"}, "not an HTTP(S) origin"},
		"duplicate":         {[]string{"https://github.com", "https://github.com"}, "listed twice"},
	} {
		t.Run(name, func(t *testing.T) {
			err := ValidateEgress(tc.list)
			require.Error(t, err)
			require.ErrorIs(t, err, ErrInvalid)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestEgressReadBack(t *testing.T) {
	values := map[string]any{"agent": map[string]any{"egress": []any{"https://github.com:443", 7}}}
	require.Equal(t, []string{"https://github.com:443"}, egressFromValues(values))
	require.Nil(t, egressFromValues(map[string]any{"agent": map[string]any{}}))
	obj := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"egress": []any{"https://*.githubusercontent.com"}}}}
	require.Equal(t, []string{"https://*.githubusercontent.com"}, egressFromObject(obj))
	require.Nil(t, egressFromObject(&unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{}}}))
}
