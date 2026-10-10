package agents

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agent-manager/internal/identity"
	"github.com/giantswarm/agent-manager/internal/muster"
)

// fakeToolsets resolves toolsets from a table keyed by selector, reports a
// selector missing from it as unmatched, and refuses a call without a caller
// token as muster's client does. Like muster, it reports a server awaiting
// sign-in for a toolset that names it: by a preset, by server or by a tool
// of its (x_<server>_ prefix).
type fakeToolsets struct {
	tools         map[string][]muster.Tool
	requiringAuth []string
	err           error
	calls         int
	url, token    string
}

func (f *fakeToolsets) awaiting(selectors []string) []string {
	var out []string
	for _, server := range f.requiringAuth {
		for _, sel := range selectors {
			if isPreset(sel) || sel == "server:"+server || strings.HasPrefix(sel, "tool:x_"+server+"_") {
				out = append(out, server)
				break
			}
		}
	}
	return out
}

func readOnlyTool(name string) muster.Tool {
	yes := true
	return muster.Tool{Name: name, Annotations: &muster.Annotations{ReadOnlyHint: &yes}}
}

func newFakeToolsets() *fakeToolsets {
	return &fakeToolsets{tools: map[string][]muster.Tool{
		"preset:read-only":                    {readOnlyTool("x_kubernetes_get"), readOnlyTool("x_kubernetes_list"), readOnlyTool("core_workflow_get"), readOnlyTool("x_agent-manager_list_agents")},
		"preset:infrastructure":               {readOnlyTool("x_kubernetes_get")},
		"preset:full":                         {readOnlyTool("x_kubernetes_get"), {Name: "x_kubernetes_delete"}},
		"preset:agent-invocation":             {readOnlyTool("x_kagent_invoke_agent_instance")},
		"preset:agent-platform":               {readOnlyTool("core_workflow_get"), readOnlyTool("x_agent-manager_list_agents"), {Name: "x_agent-manager_create_agent"}, {Name: "x_kagent_invoke_agent_instance"}},
		"tool:x_kubernetes_get":               {readOnlyTool("x_kubernetes_get")},
		"tool:x_kubernetes_delete":            {{Name: "x_kubernetes_delete"}},
		"tool:x_agent-manager_create_agent":   {{Name: "x_agent-manager_create_agent"}},
		"tool:x_kagent_invoke_agent_instance": {readOnlyTool("x_kagent_invoke_agent_instance")},
		"tool:x_agent-manager_list_agents":    {readOnlyTool("x_agent-manager_list_agents")},
		"server:github":                       {},
	}}
}

func (f *fakeToolsets) ResolveToolsets(_ context.Context, url, token string, toolsets ...[]string) ([]muster.Resolution, error) {
	f.calls++
	f.url, f.token = url, token
	if f.err != nil {
		return nil, f.err
	}
	if token == "" {
		return nil, muster.ErrNoToken
	}
	out := make([]muster.Resolution, 0, len(toolsets))
	for _, ts := range toolsets {
		res := muster.Resolution{RequiringAuth: f.awaiting(ts)}
		for _, sel := range ts {
			tools, known := f.tools[sel]
			if !known {
				res.Unmatched = append(res.Unmatched, sel)
			}
			res.Tools = append(res.Tools, tools...)
		}
		out = append(out, res)
	}
	return out, nil
}

// claudeFixture is the seeded namespace plus a claude Harness and a coding
// agent's release on it.
func claudeFixture(t *testing.T, values map[string]any) (*fixture, context.Context) {
	t.Helper()
	f := seeded(t)
	mustCreate(t, f, f.svc.harnessGVR(), harness("kagent", "claude", "claude"))
	if values != nil {
		mustCreate(t, f, hrGVR, helmRelease("kagent", "coder", values, true, nil))
	}
	return f, identity.ContextWithToken(t.Context(), "caller-token")
}

func coderValues(toolset ...string) map[string]any {
	v := map[string]any{
		"agent":       map[string]any{"name": "coder", "harness": "claude", "systemMessage": "You keep the GitOps repositories tidy."},
		"modelConfig": map[string]any{"name": "default-model-config"},
	}
	if len(toolset) > 0 {
		v[ToolsetValuesKey] = toAnySlice(toolset)
	}
	return v
}

func coderSpec(toolset ...string) Spec {
	return Spec{Name: "coder", ModelConfig: "default-model-config", Harness: "claude", SystemMessage: "You keep the GitOps repositories tidy.", Toolset: toolset}
}

// refusedEverywhere asserts a refusal on validate_agent, create_agent and
// update_agent alike, and that nothing was written.
func refusedEverywhere(t *testing.T, f *fixture, ctx context.Context, spec Spec, upd Update, want string) {
	t.Helper()
	spec.Name = "fresh" // the update targets the existing release
	dry, err := f.svc.ValidateCreate(ctx, spec)
	require.NoError(t, err)
	assert.False(t, dry.Valid)
	assert.True(t, containsLine(dry.Errors, want), "validate create: %v", dry.Errors)

	_, err = f.svc.Create(ctx, spec)
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), want)
	_, err = f.dyn.Resource(hrGVR).Namespace("kagent").Get(ctx, spec.Name, metav1.GetOptions{})
	assert.Error(t, err, "nothing is created")

	before := mustValues(mustGet(t, f, upd.Name))
	dryUpd, err := f.svc.ValidateUpdate(ctx, upd)
	require.NoError(t, err)
	assert.False(t, dryUpd.Valid)
	assert.True(t, containsLine(dryUpd.Errors, want), "validate update: %v", dryUpd.Errors)

	_, err = f.svc.Update(ctx, upd)
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), want)
	assert.Equal(t, before, mustValues(mustGet(t, f, upd.Name)), "nothing is updated")
}

func containsLine(lines []string, want string) bool {
	for _, l := range lines {
		if strings.Contains(l, want) {
			return true
		}
	}
	return false
}

func mustGet(t *testing.T, f *fixture, name string) *unstructured.Unstructured {
	t.Helper()
	hr, err := f.dyn.Resource(hrGVR).Namespace("kagent").Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	return hr
}

func TestClaudeHarnessAcceptsAReadOnlyToolset(t *testing.T) {
	f, ctx := claudeFixture(t, nil)
	res, err := f.svc.Create(ctx, coderSpec("preset:read-only"))
	require.NoError(t, err)
	values := mustValues(mustGet(t, f, "coder"))
	agentBlock := values["agent"].(map[string]any)
	assert.Equal(t, []any{"https://github.com", "https://*.githubusercontent.com"}, agentBlock["egress"], "the Harness's list is written on create")
	assert.Equal(t, res.Manifests.Values, values)
	assert.Equal(t, "http://muster.agent-platform.svc.cluster.local:8090/mcp", f.toolsets.url)
	assert.Equal(t, "caller-token", f.toolsets.token, "the toolset is resolved as the caller")
}

func TestClaudeHarnessRefusesAnEmptyToolset(t *testing.T) {
	// create_agent refuses a missing toolset for every agent; an existing
	// release written without one is refused by the claude guard on update.
	f, ctx := claudeFixture(t, coderValues())
	refusedEverywhere(t, f, ctx, coderSpec(), Update{Name: "coder", Description: str("tidy")}, "toolset is required")

	dryUpd, err := f.svc.ValidateUpdate(ctx, Update{Name: "coder", Description: str("tidy")})
	require.NoError(t, err)
	assert.Contains(t, strings.Join(dryUpd.Errors, "\n"), "on a claude Harness agent")
}

func TestClaudeHarnessRefusesAWriteTool(t *testing.T) {
	f, ctx := claudeFixture(t, coderValues("preset:read-only"))
	refusedEverywhere(t, f, ctx,
		coderSpec("preset:read-only", "tool:x_kubernetes_delete"),
		Update{Name: "coder", Toolset: &[]string{"preset:read-only", "tool:x_kubernetes_delete"}},
		"not marked read-only (no readOnlyHint): x_kubernetes_delete")
}

func TestClaudeHarnessRefusesDeniedTools(t *testing.T) {
	for name, sel := range map[string]string{
		"agent invocation":      "tool:x_kagent_invoke_agent_instance",
		"agent-platform preset": "tool:x_agent-manager_create_agent",
	} {
		t.Run(name, func(t *testing.T) {
			f, ctx := claudeFixture(t, coderValues("preset:read-only"))
			denied := strings.TrimPrefix(sel, "tool:")
			refusedEverywhere(t, f, ctx,
				coderSpec("preset:read-only", sel),
				Update{Name: "coder", Toolset: &[]string{"preset:read-only", sel}},
				"may not use (invoke_agent_instance and the preset:agent-platform tools that are not read-only): "+denied)
		})
	}
}

// The preset the guard's own refusal recommends passes it, read-only
// agent-platform and core tools included, as do the other presets and a
// single read tool.
func TestClaudeHarnessAcceptsReadOnlySelectors(t *testing.T) {
	for _, toolset := range [][]string{
		{"preset:read-only"},
		{"preset:infrastructure"},
		{"preset:none"},
		{"tool:x_kubernetes_get"},
		{"tool:x_agent-manager_list_agents"},
	} {
		t.Run(strings.Join(toolset, ","), func(t *testing.T) {
			f, ctx := claudeFixture(t, coderValues("preset:read-only"))
			spec := coderSpec(toolset...)
			spec.Name = "fresh"
			dry, err := f.svc.ValidateCreate(ctx, spec)
			require.NoError(t, err)
			assert.True(t, dry.Valid, "validate create: %v", dry.Errors)
			_, err = f.svc.Create(ctx, spec)
			require.NoError(t, err)
			_, err = f.svc.Update(ctx, Update{Name: "coder", Toolset: &toolset})
			require.NoError(t, err)
		})
	}
}

func TestClaudeHarnessRefusesUnknownSelectors(t *testing.T) {
	for _, sel := range []string{"tool:x_kubernetes_drain", "server:nothing", "workflow:nothing"} {
		t.Run(sel, func(t *testing.T) {
			f, ctx := claudeFixture(t, coderValues("preset:read-only"))
			refusedEverywhere(t, f, ctx,
				coderSpec("preset:read-only", sel),
				Update{Name: "coder", Toolset: &[]string{"preset:read-only", sel}},
				"toolset selectors resolve to no tool for you: "+sel)
		})
	}
}

func TestClaudeHarnessSignInRefusalNamesServers(t *testing.T) {
	f, ctx := claudeFixture(t, nil)
	f.toolsets.requiringAuth = []string{""}
	dry, err := f.svc.ValidateCreate(ctx, coderSpec("preset:read-only"))
	require.NoError(t, err)
	assert.True(t, dry.Valid, "no server awaits sign-in: %v", dry.Errors)
	assert.NotContains(t, strings.Join(dry.Errors, "\n"), "sign in")
}

// A preset spans every server, also those awaiting the caller's sign-in:
// muster resolves it per request and admits the preset's tools only, so the
// servers it spans need no sign-in before the write.
func TestClaudeHarnessPresetSkipsServersAwaitingSignIn(t *testing.T) {
	f, ctx := claudeFixture(t, coderValues("preset:infrastructure"))
	f.toolsets.requiringAuth = []string{"github", "jira"}
	toolset := []string{"preset:read-only"}
	spec := coderSpec(toolset...)
	spec.Name = "fresh"

	dry, err := f.svc.ValidateCreate(ctx, spec)
	require.NoError(t, err)
	assert.True(t, dry.Valid, "validate create: %v", dry.Errors)
	_, err = f.svc.Create(ctx, spec)
	require.NoError(t, err)
	assert.Equal(t, []any{"preset:read-only"}, mustValues(mustGet(t, f, "fresh"))[ToolsetValuesKey])

	dryUpd, err := f.svc.ValidateUpdate(ctx, Update{Name: "coder", Toolset: &toolset})
	require.NoError(t, err)
	assert.True(t, dryUpd.Valid, "validate update: %v", dryUpd.Errors)
	_, err = f.svc.Update(ctx, Update{Name: "coder", Toolset: &toolset})
	require.NoError(t, err)
	assert.Equal(t, []any{"preset:read-only"}, mustValues(mustGet(t, f, "coder"))[ToolsetValuesKey])
}

// Selectors that name a server awaiting sign-in, by server or by a tool of
// its, stay refused beside a preset, naming only that server; a preset that
// reaches a write or a denied tool stays refused while servers await sign-in.
func TestClaudeHarnessSignInStillRefusesExplicitSelectors(t *testing.T) {
	for name, tc := range map[string]struct {
		toolset []string
		want    string
	}{
		"server":            {[]string{"preset:read-only", "server:github"}, "unknown until you sign in to them (github):"},
		"tool":              {[]string{"preset:read-only", "tool:x_github_get_issue"}, "unknown until you sign in to them (github):"},
		"write preset":      {[]string{"preset:full"}, "not marked read-only (no readOnlyHint): x_kubernetes_delete"},
		"invocation preset": {[]string{"preset:agent-invocation"}, "may not use (invoke_agent_instance and the preset:agent-platform tools that are not read-only): x_kagent_invoke_agent_instance"},
	} {
		t.Run(name, func(t *testing.T) {
			f, ctx := claudeFixture(t, coderValues("preset:read-only"))
			f.toolsets.requiringAuth = []string{"github", "jira"}
			refusedEverywhere(t, f, ctx, coderSpec(tc.toolset...), Update{Name: "coder", Toolset: &tc.toolset}, tc.want)
			dry, err := f.svc.ValidateCreate(ctx, Spec{Name: "other", ModelConfig: "default-model-config", Harness: "claude", Toolset: tc.toolset})
			require.NoError(t, err)
			assert.NotContains(t, strings.Join(dry.Errors, "\n"), "jira", "a server only the preset spans is not refused")
		})
	}
}

func TestClaudeHarnessRefusesRequireApproval(t *testing.T) {
	// create_agent writes no requireApproval, so on create the guard meets it
	// only in values it composes; update meets it on a release written
	// elsewhere.
	f, ctx := claudeFixture(t, nil)
	st, err := f.svc.site(ctx, Location{})
	require.NoError(t, err)
	values := BuildValues(coderSpec("preset:read-only"), st.compose)
	values["muster"].(map[string]any)["requireApproval"] = true
	errs := f.svc.guardClaude(ctx, st, "claude", values, nil)
	require.Len(t, errs, 1)
	assert.Contains(t, errs[0].Error(), "takes no requireApproval binding")

	withApproval := coderValues("preset:read-only")
	withApproval["muster"] = map[string]any{"requireApproval": true}
	mustCreate(t, f, hrGVR, helmRelease("kagent", "coder", withApproval, true, nil))
	dry, err := f.svc.ValidateUpdate(ctx, Update{Name: "coder", Description: str("tidy")})
	require.NoError(t, err)
	assert.True(t, containsLine(dry.Errors, "takes no requireApproval binding"), dry.Errors)
	_, err = f.svc.Update(ctx, Update{Name: "coder", Description: str("tidy")})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "takes no requireApproval binding")

	// Bindings beside muster's are not resolved through it.
	extra := coderValues("preset:read-only")
	extra["extraTools"] = []any{map[string]any{"mcp": map[string]any{"server": map[string]any{"kind": "RemoteMCPServer", "name": "github"}}}}
	assert.Len(t, refuseApproval(extra), 1)
}

func TestClaudeHarnessEgressIsTheHarnessList(t *testing.T) {
	f, ctx := claudeFixture(t, coderValues("preset:read-only"))
	spec := coderSpec("preset:read-only")
	spec.Name = "other"
	spec.Egress = []string{"https://github.com", "https://registry.npmjs.org"}
	dry, err := f.svc.ValidateCreate(ctx, spec)
	require.NoError(t, err)
	assert.False(t, dry.Valid)
	assert.True(t, containsLine(dry.Errors, `egress origin "https://registry.npmjs.org" is not one of Harness claude's origins (https://github.com, https://*.githubusercontent.com)`), dry.Errors)
	_, err = f.svc.Create(ctx, spec)
	require.ErrorIs(t, err, ErrInvalid)

	// An origin of the list is accepted, and the whole list is written.
	spec.Egress = []string{"https://github.com"}
	_, err = f.svc.Create(ctx, spec)
	require.NoError(t, err)
	assert.Equal(t, []any{"https://github.com", "https://*.githubusercontent.com"}, mustValues(mustGet(t, f, "other"))["agent"].(map[string]any)["egress"])

	_, err = f.svc.Update(ctx, Update{Name: "coder", Egress: &[]string{"https://example.com"}})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), `egress origin "https://example.com" is not one of Harness claude's origins`)

	// An update rewrites the list, also on a release written before the guard.
	res, err := f.svc.Update(ctx, Update{Name: "coder", Egress: &[]string{}})
	require.NoError(t, err)
	assert.Equal(t, []any{"https://github.com", "https://*.githubusercontent.com"}, res.After["agent"].(map[string]any)["egress"])

	// A claude Harness the platform lists no origins for reaches none.
	mustCreate(t, f, f.svc.harnessGVR(), harness("kagent", "claude-go", "claude"))
	spec = coderSpec("preset:read-only")
	spec.Name, spec.Harness, spec.Egress = "gopher", "claude-go", []string{"https://proxy.golang.org"}
	dry, err = f.svc.ValidateCreate(ctx, spec)
	require.NoError(t, err)
	assert.True(t, containsLine(dry.Errors, "is not one of Harness claude-go's origins (none)"), dry.Errors)
}

func TestClaudeHarnessFailsClosedWhenMusterCannotAnswer(t *testing.T) {
	cases := map[string]func(*fixture) context.Context{
		"muster unreachable": func(f *fixture) context.Context {
			f.toolsets.err = errors.New("dial tcp 10.0.0.1:8090: connect: connection refused")
			return identity.ContextWithToken(context.Background(), "caller-token")
		},
		"muster answers an error": func(f *fixture) context.Context {
			f.toolsets.err = errors.New("filter_tools: muster answered an error: unknown preset")
			return identity.ContextWithToken(context.Background(), "caller-token")
		},
		"no caller token": func(*fixture) context.Context { return context.Background() },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f, _ := claudeFixture(t, coderValues("preset:read-only"))
			ctx := setup(f)
			refusedEverywhere(t, f, ctx, coderSpec("preset:read-only"), Update{Name: "coder", Description: str("tidy")},
				"toolset could not be checked with muster's filter_tools")
		})
	}

	t.Run("no muster client", func(t *testing.T) {
		f, ctx := claudeFixture(t, coderValues("preset:read-only"))
		f.svc.cfg.Toolsets = nil
		refusedEverywhere(t, f, ctx, coderSpec("preset:read-only"), Update{Name: "coder", Description: str("tidy")}, "no muster client")
	})

	t.Run("servers awaiting the caller's sign-in", func(t *testing.T) {
		f, ctx := claudeFixture(t, coderValues("preset:read-only"))
		f.toolsets.requiringAuth = []string{"github"}
		refusedEverywhere(t, f, ctx, coderSpec("preset:read-only", "server:github"), Update{Name: "coder", Toolset: &[]string{"preset:read-only", "server:github"}},
			"unknown until you sign in to them (github)")
	})
}

func TestClaudeHarnessSystemPromptCarriesTheEgressNote(t *testing.T) {
	f, ctx := claudeFixture(t, nil)
	_, err := f.svc.Create(ctx, coderSpec("preset:read-only"))
	require.NoError(t, err)
	prompt := mustValues(mustGet(t, f, "coder"))["agent"].(map[string]any)["systemMessage"].(string)
	assert.True(t, strings.HasPrefix(prompt, "You keep the GitOps repositories tidy.\n\n"+egressNoteHeading), prompt)
	assert.Contains(t, prompt, "only these origins: https://github.com, https://*.githubusercontent.com")

	// A new prompt gets the note once, never a second copy.
	res, err := f.svc.Update(ctx, Update{Name: "coder", SystemMessage: str("You review pull requests.")})
	require.NoError(t, err)
	prompt = res.After["agent"].(map[string]any)["systemMessage"].(string)
	assert.True(t, strings.HasPrefix(prompt, "You review pull requests.\n\n"+egressNoteHeading), prompt)
	assert.Equal(t, 1, strings.Count(prompt, egressNoteHeading))
	res, err = f.svc.Update(ctx, Update{Name: "coder", Description: str("tidy")})
	require.NoError(t, err)
	assert.Equal(t, []string{"agent.description"}, res.Changed, "an unchanged list rewrites nothing")

	// Without a prompt of its own the note is the prompt.
	assert.Equal(t, egressNote(nil), withEgressNote("", nil))
}

func TestDeclarativeAgentsAreUnchanged(t *testing.T) {
	f, ctx := claudeFixture(t, nil)
	spec := Spec{Name: "helper", ModelConfig: "default-model-config", SystemMessage: "Help.", Toolset: []string{"preset:full"}, Egress: []string{"https://example.com"}}
	dry, err := f.svc.ValidateCreate(ctx, spec)
	require.NoError(t, err)
	assert.True(t, dry.Valid, dry.Errors)
	_, err = f.svc.Create(ctx, spec)
	require.NoError(t, err)
	agentBlock := mustValues(mustGet(t, f, "helper"))["agent"].(map[string]any)
	assert.Equal(t, "Help.", agentBlock["systemMessage"])
	assert.Equal(t, []any{"https://example.com"}, agentBlock["egress"])

	_, err = f.svc.Update(ctx, Update{Name: "verifier", Toolset: &[]string{"preset:full"}, Egress: &[]string{"https://example.com"}})
	require.NoError(t, err)
	assert.Zero(t, f.toolsets.calls, "a Declarative agent's toolset is not resolved")
}
