package agents

import (
	"encoding/json"
	"strings"
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

// egressPolicy is an EgressPolicy of ns; an origin given as "origin|header"
// carries a credential sent in that header.
func egressPolicy(ns, name string, origins ...string) *unstructured.Unstructured {
	dests := make([]any, 0, len(origins))
	for _, o := range origins {
		origin, header, _ := strings.Cut(o, "|")
		d := map[string]any{"origin": origin}
		if header != "" {
			d["authentication"] = map[string]any{"header": header, "prefix": "Bearer ", "secretKeyRef": map[string]any{"name": name + "-token", "key": "token"}}
		}
		dests = append(dests, d)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": kagentAPI, "kind": "EgressPolicy",
		"metadata": map[string]any{"name": name, "namespace": ns},
		"spec":     map[string]any{"destinations": dests},
	}}
}

func TestEgressPolicyRefsReadBack(t *testing.T) {
	values := map[string]any{"agent": map[string]any{"egressPolicyRefs": []any{"github", 7}}}
	require.Equal(t, []string{"github"}, egressPolicyRefsFromValues(values))
	require.Nil(t, egressPolicyRefsFromValues(map[string]any{}))
	obj := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"egressPolicyRefs": []any{map[string]any{"name": "github"}, map[string]any{"name": "go-modules"}}}}}
	require.Equal(t, []string{"github", "go-modules"}, egressPolicyRefsFromObject(obj))
	require.Nil(t, egressPolicyRefsFromObject(&unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{}}}))
}

// list_egress_policies reports each origin and its credential header's name,
// never the Secret behind it.
func TestListEgressPolicies(t *testing.T) {
	f := seeded(t)
	mustCreate(t, f, egressGVR, egressPolicy("kagent", "go-modules", "https://proxy.golang.org", "https://sum.golang.org"))
	mustCreate(t, f, egressGVR, egressPolicy("kagent", "github", "https://api.github.com|Authorization", "https://github.com"))
	mustCreate(t, f, egressGVR, egressPolicy("tenant", "elsewhere", "https://example.com"))

	list, err := f.svc.ListEgressPolicies(t.Context(), Location{})
	require.NoError(t, err)
	require.Equal(t, []EgressPolicy{
		{Name: "github", Namespace: "kagent", Destinations: []EgressDestination{{Origin: "https://api.github.com", CredentialHeader: "Authorization"}, {Origin: "https://github.com"}}},
		{Name: "go-modules", Namespace: "kagent", Destinations: []EgressDestination{{Origin: "https://proxy.golang.org"}, {Origin: "https://sum.golang.org"}}},
	}, list)
	b, err := json.Marshal(list)
	require.NoError(t, err)
	require.NotContains(t, string(b), "github-token")
	require.NotContains(t, string(b), "Bearer")
}

// egressPolicyRefs pass create, validate and update into
// agent.egressPolicyRefs, render as the Agent's spec.egressPolicyRefs and
// read back on get; a name with no EgressPolicy in the namespace is refused,
// naming the entry and the valid ones.
func TestEgressPolicyRefsOnCreateValidateUpdateAndGet(t *testing.T) {
	f := seeded(t)
	ctx := t.Context()
	mustCreate(t, f, egressGVR, egressPolicy("kagent", "github", "https://github.com"))
	mustCreate(t, f, egressGVR, egressPolicy("kagent", "go-modules", "https://proxy.golang.org"))
	spec := Spec{Name: "gopher", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"}, Egress: []string{"https://example.com"}, EgressPolicyRefs: []string{"github", "go-modules"}}

	dry, err := f.svc.ValidateCreate(ctx, spec)
	require.NoError(t, err)
	require.True(t, dry.Valid, dry.Errors)
	require.Contains(t, dry.Manifests.HelmRelease, "egressPolicyRefs")
	rendered := f.dryRuns.last("Agent")
	require.NotNil(t, rendered)
	require.Equal(t, []string{"github", "go-modules"}, egressPolicyRefsFromObject(rendered))

	_, err = f.svc.Create(ctx, spec)
	require.NoError(t, err)
	agentBlock := mustValues(mustGet(t, f, "gopher"))["agent"].(map[string]any)
	require.Equal(t, []any{"github", "go-modules"}, agentBlock["egressPolicyRefs"])
	require.Equal(t, []any{"https://example.com"}, agentBlock["egress"])
	got, err := f.svc.Get(ctx, Location{}, "gopher")
	require.NoError(t, err)
	require.Equal(t, []string{"github", "go-modules"}, got.EgressPolicyRefs)

	res, err := f.svc.Update(ctx, Update{Name: "gopher", EgressPolicyRefs: &[]string{"go-modules"}})
	require.NoError(t, err)
	require.Equal(t, []any{"go-modules"}, res.After["agent"].(map[string]any)["egressPolicyRefs"])
	require.Equal(t, []any{"https://example.com"}, res.After["agent"].(map[string]any)["egress"])
	res, err = f.svc.Update(ctx, Update{Name: "gopher", EgressPolicyRefs: &[]string{}})
	require.NoError(t, err)
	require.NotContains(t, res.After["agent"].(map[string]any), "egressPolicyRefs")

	const want = `egressPolicyRefs: EgressPolicy "npm" does not exist in namespace kagent; valid: github, go-modules`
	spec.Name, spec.EgressPolicyRefs = "gopher-2", []string{"github", "npm"}
	_, err = f.svc.Create(ctx, spec)
	require.ErrorIs(t, err, ErrInvalid)
	require.ErrorContains(t, err, want)
	dry, err = f.svc.ValidateCreate(ctx, spec)
	require.NoError(t, err)
	require.False(t, dry.Valid)
	require.True(t, containsLine(dry.Errors, want), dry.Errors)
	_, err = f.svc.Update(ctx, Update{Name: "gopher", EgressPolicyRefs: &[]string{"npm"}})
	require.ErrorIs(t, err, ErrInvalid)
	require.ErrorContains(t, err, `EgressPolicy "npm" does not exist`)
	upd, err := f.svc.ValidateUpdate(ctx, Update{Name: "gopher", EgressPolicyRefs: &[]string{"npm"}})
	require.NoError(t, err)
	require.False(t, upd.Valid)
	require.True(t, containsLine(upd.Errors, `EgressPolicy "npm" does not exist`), upd.Errors)
}

// A namespace without EgressPolicies says so instead of listing none.
func TestEgressPolicyRefsWithoutPolicies(t *testing.T) {
	f := seeded(t)
	_, err := f.svc.Create(t.Context(), Spec{Name: "gopher", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"}, EgressPolicyRefs: []string{"github"}})
	require.ErrorIs(t, err, ErrInvalid)
	require.ErrorContains(t, err, `EgressPolicy "github" does not exist in namespace kagent (no EgressPolicies there`)
}

// An agent on a coding Harness reaches its Harness's origins only: any
// egressPolicyRefs entry is refused, at create, validate and update, and on
// a move to a coding Harness of an agent that picks one.
func TestEgressPolicyRefsRefusedOnCodingHarness(t *testing.T) {
	const want = "egressPolicyRefs github: an agent on coding Harness claude reaches its Harness's origins only"
	f, ctx := claudeFixture(t, coderValues("preset:read-only"))
	mustCreate(t, f, egressGVR, egressPolicy("kagent", "github", "https://github.com"))

	spec := coderSpec("preset:read-only")
	spec.Name, spec.EgressPolicyRefs = "coder-2", []string{"github"}
	_, err := f.svc.Create(ctx, spec)
	require.ErrorIs(t, err, ErrInvalid)
	require.ErrorContains(t, err, want)
	dry, err := f.svc.ValidateCreate(ctx, spec)
	require.NoError(t, err)
	require.True(t, containsLine(dry.Errors, want), dry.Errors)

	_, err = f.svc.Update(ctx, Update{Name: "coder", EgressPolicyRefs: &[]string{"github"}})
	require.ErrorIs(t, err, ErrInvalid)
	require.ErrorContains(t, err, want)

	_, err = f.svc.Update(ctx, Update{Name: "verifier", EgressPolicyRefs: &[]string{"github"}})
	require.NoError(t, err)
	claude := "claude"
	_, err = f.svc.Update(ctx, Update{Name: "verifier", Harness: &claude})
	require.ErrorIs(t, err, ErrInvalid)
	require.ErrorContains(t, err, "egressPolicyRefs github: an agent on coding Harness claude")
}
