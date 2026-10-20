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
		"tls://db.example.com:5432",
		"tls://*.kafka.example.com:9093",
		"http://db.example.com",
		"tls://github.com:22",
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
		"no scheme":         {[]string{"github.com:443"}, "not an egress origin"},
		"other scheme":      {[]string{"tcp://db.example.com:5432"}, "not an egress origin"},
		"path":              {[]string{"https://github.com/giantswarm"}, "not an egress origin"},
		"upper case":        {[]string{"https://GitHub.com"}, "not an egress origin"},
		"wildcard tld":      {[]string{"https://*.com"}, "not an egress origin"},
		"wildcard mid":      {[]string{"https://raw.*.com"}, "not an egress origin"},
		"port zero":         {[]string{"https://github.com:0"}, "not an egress origin"},
		"port out of range": {[]string{"https://github.com:65536"}, "not an egress origin"},
		"duplicate":         {[]string{"https://github.com", "https://github.com"}, "listed twice"},
		"tls without port":  {[]string{"tls://db.example.com"}, "a tls:// origin requires one"},
		"tls path":          {[]string{"tls://db.example.com:5432/db"}, "not an egress origin"},
		"tls ip":            {[]string{"tls://192.0.2.1:5432"}, "not an egress origin"},
		"tls wildcard tld":  {[]string{"tls://*.com:443"}, "not an egress origin"},
		"tls every host":    {[]string{"tls://*:443"}, "not an egress origin"},
		"tls port zero":     {[]string{"tls://db.example.com:0"}, "not an egress origin"},
		"tls no slashes":    {[]string{"tls:db.example.com:5432"}, "not an egress origin"},
		"tls and https":     {[]string{"https://api.example.com", "tls://api.example.com:443"}, "reaches api.example.com:443 over tls, which another entry reaches over https"},
		"tls and http":      {[]string{"tls://db.example.com:5432", "http://db.example.com:5432"}, "reaches db.example.com:5432 over http, which another entry reaches over tls"},
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

// A tls:// destination passes create, validate and update into agent.egress
// unchanged; one without a port is refused with the reason before anything is
// written.
func TestTLSEgressOnCreateValidateAndUpdate(t *testing.T) {
	f := seeded(t)
	ctx := t.Context()
	tls := []string{"tls://db.example.com:5432", "https://github.com"}
	spec := Spec{Name: "dba", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"}, Egress: tls}

	dry, err := f.svc.ValidateCreate(ctx, spec)
	require.NoError(t, err)
	require.True(t, dry.Valid, dry.Errors)
	_, err = f.svc.Create(ctx, spec)
	require.NoError(t, err)
	require.Equal(t, []any{"tls://db.example.com:5432", "https://github.com"}, mustValues(mustGet(t, f, "dba"))["agent"].(map[string]any)["egress"])

	res, err := f.svc.Update(ctx, Update{Name: "verifier", Egress: &[]string{"tls://*.kafka.example.com:9093"}})
	require.NoError(t, err)
	require.Equal(t, []any{"tls://*.kafka.example.com:9093"}, res.After["agent"].(map[string]any)["egress"])

	const want = `egress[0] "tls://db.example.com" has no port: a tls:// origin requires one`
	spec.Name, spec.Egress = "dba-2", []string{"tls://db.example.com"}
	_, err = f.svc.Create(ctx, spec)
	require.ErrorIs(t, err, ErrInvalid)
	require.ErrorContains(t, err, want)
	dry, err = f.svc.ValidateCreate(ctx, spec)
	require.NoError(t, err)
	require.False(t, dry.Valid)
	require.True(t, containsLine(dry.Errors, want), dry.Errors)
	_, err = f.svc.Update(ctx, Update{Name: "verifier", Egress: &[]string{"tls://db.example.com"}})
	require.ErrorIs(t, err, ErrInvalid)
	require.ErrorContains(t, err, want)
}
