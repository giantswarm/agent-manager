package agents

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestEgressReadBack(t *testing.T) {
	values := map[string]any{"agent": map[string]any{"egress": []any{"https://github.com:443", 7}}}
	require.Equal(t, []string{"https://github.com:443"}, egressFromValues(values))
	require.Nil(t, egressFromValues(map[string]any{"agent": map[string]any{}}))
	obj := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"egress": []any{"https://*.githubusercontent.com"}}}}
	require.Equal(t, []string{"https://*.githubusercontent.com"}, egressFromObject(obj))
	require.Nil(t, egressFromObject(&unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{}}}))
}

// A tls:// destination passes create, validate and update into agent.egress
// unchanged; one without a port is refused by the chart's schema before
// anything is written.
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

	const want = `/agent/egress/0: 'tls://db.example.com' does not match pattern`
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
