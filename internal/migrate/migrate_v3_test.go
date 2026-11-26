package migrate

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/chart"
	"github.com/giantswarm/agent-manager/internal/kube"
	"github.com/giantswarm/agent-manager/internal/skills"
)

// The 2.x -> 3.x hop: both majors render the same api.kagent.dev Agent;
// chart 3.0 removes muster.requireApproval (default: Ask replaces true) and
// drops modelConfig.name's default (2.x rendered default-model-config).

const (
	v2Range  = ">=2.6.0 <3.0.0"
	v3Target = ">=3.0.0 <4.0.0"
	v3Latest = "3.0.0"
)

// fakeChart3 is the agent chart 3.x as the registry reports it: the embedded
// 2.x schema without muster.requireApproval and with `default`, which is all
// of 3.0's values contract the hop touches.
type fakeChart3 struct{ latest string }

func (fakeChart3) Schema(context.Context) chart.Schema {
	s := chart.EmbeddedSchema()
	raw, err := json.Marshal(s.Document)
	if err != nil {
		panic(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		panic(err)
	}
	props := doc["properties"].(map[string]any)
	delete(props["muster"].(map[string]any)["properties"].(map[string]any), "requireApproval")
	props["default"] = map[string]any{"type": []any{"string", "null"}, "enum": []any{"Allow", "Ask", "Block", nil}}
	return chart.Schema{Document: doc, Version: v3Latest, Source: chart.SourceRegistry}
}

func (c fakeChart3) Info(context.Context) chart.Info {
	info := chart.Info{OCIURL: agents.DefaultChartOCIURL, Semver: v3Target, LatestVersion: c.latest, SchemaVersion: v3Latest, SchemaSource: chart.SourceRegistry}
	if c.latest == "" {
		info.Error = "no chart version satisfies \"" + v3Target + "\""
	}
	return info
}
func (fakeChart3) Name() string        { return "agent" }
func (fakeChart3) OCIURL() string      { return agents.DefaultChartOCIURL }
func (fakeChart3) SemverRange() string { return v3Target }

func (c *cluster) runnerV3(t *testing.T, opts Options, latest string) *Runner {
	t.Helper()
	ch := fakeChart3{latest: latest}
	p := agents.InProcessPinner(pinner{git: skills.NewResolver(fakeGitHub(t).URL, skills.StaticToken(""), nil, nil)})
	svc := agents.New(kube.NewServiceAccountProvider(c.client), ch, nil, p, agents.Config{
		DefaultNamespace: opts.Namespaces[0], ManagedNamespaces: opts.Namespaces, Compose: agents.ComposeConfig{HarnessName: kagentNS}, KagentAPIVersion: "v1alpha3",
	}, nil)
	opts.Version, opts.TargetSemver = "test", v3Target
	r, err := New(c.client, ch, p, svc, opts, nil)
	require.NoError(t, err)
	return r
}

// approvalValues are 2.x values of an agent that asks before every tool call
// through kagent's flag.
func approvalValues() map[string]any {
	return map[string]any{
		"agent":       map[string]any{"name": "triage"},
		"modelConfig": map[string]any{"name": "default-model-config"},
		"muster":      map[string]any{"requireApproval": true},
		"toolset":     []any{"server:github"},
	}
}

func approvalRewritten() map[string]any {
	return map[string]any{
		"agent":       map[string]any{"name": "triage"},
		"modelConfig": map[string]any{"name": "default-model-config"},
		"toolset":     []any{"server:github"},
		"default":     "Ask",
	}
}

func TestRewriteValuesV3(t *testing.T) {
	for name, tc := range map[string]struct {
		in, want     map[string]any
		removed, set []string
		unchanged    bool
		errContains  string
	}{
		"requireApproval true becomes default Ask": {
			in: approvalValues(), want: approvalRewritten(),
			removed: []string{"muster.requireApproval"}, set: []string{"default=Ask"},
		},
		"requireApproval true keeps the rest of the muster block": {
			in: map[string]any{
				"modelConfig": map[string]any{"name": "m"},
				"muster":      map[string]any{"requireApproval": true, "tools": []any{"call_tool"}},
				"tools":       []any{"server:github"},
			},
			want: map[string]any{
				"modelConfig": map[string]any{"name": "m"},
				"muster":      map[string]any{"tools": []any{"call_tool"}},
				"tools":       []any{"server:github"},
				"default":     "Ask",
			},
			removed: []string{"muster.requireApproval"}, set: []string{"default=Ask"},
		},
		"requireApproval true without a list asks for every tool": {
			in:      map[string]any{"modelConfig": map[string]any{"name": "m"}, "muster": map[string]any{"requireApproval": true}},
			want:    map[string]any{"modelConfig": map[string]any{"name": "m"}, "default": "Ask"},
			removed: []string{"muster.requireApproval"}, set: []string{"default=Ask"},
		},
		"requireApproval false is dropped": {
			in:      map[string]any{"modelConfig": map[string]any{"name": "m"}, "muster": map[string]any{"requireApproval": false, "url": "http://muster"}},
			want:    map[string]any{"modelConfig": map[string]any{"name": "m"}, "muster": map[string]any{"url": "http://muster"}},
			removed: []string{"muster.requireApproval"},
		},
		"requireApproval true on a chat-only agent rendered no binding: dropped": {
			in:      map[string]any{"modelConfig": map[string]any{"name": "m"}, "muster": map[string]any{"requireApproval": true}, "toolset": []any{"preset:none"}},
			want:    map[string]any{"modelConfig": map[string]any{"name": "m"}, "toolset": []any{"preset:none"}},
			removed: []string{"muster.requireApproval"},
		},
		"requireApproval true with muster off rendered no binding: dropped": {
			in:      map[string]any{"modelConfig": map[string]any{"name": "m"}, "muster": map[string]any{"requireApproval": true, "enabled": false}},
			want:    map[string]any{"modelConfig": map[string]any{"name": "m"}, "muster": map[string]any{"enabled": false}},
			removed: []string{"muster.requireApproval"},
		},
		"an unset modelConfig.name is written as the 2.x default": {
			in:   map[string]any{"agent": map[string]any{"name": "a"}},
			want: map[string]any{"agent": map[string]any{"name": "a"}, "modelConfig": map[string]any{"name": "default-model-config"}},
			set:  []string{"modelConfig.name=default-model-config"},
		},
		"an empty modelConfig.name rendered no model on 2.x either: kept": {
			in: map[string]any{"modelConfig": map[string]any{"name": ""}}, want: map[string]any{"modelConfig": map[string]any{"name": ""}},
			unchanged: true,
		},
		"3.x values are unchanged": {
			in: approvalRewritten(), want: approvalRewritten(), unchanged: true,
		},
		"requireApproval true with a default never rendered": {
			in:          map[string]any{"modelConfig": map[string]any{"name": "m"}, "muster": map[string]any{"requireApproval": true}, "default": "Allow"},
			errContains: "muster.requireApproval: true together with tool approval settings (default), which no chart renders",
		},
		"requireApproval true with an entry setting never rendered": {
			in:          map[string]any{"modelConfig": map[string]any{"name": "m"}, "muster": map[string]any{"requireApproval": true}, "tools": []any{map[string]any{"server:github": "Allow"}}},
			errContains: "muster.requireApproval: true together with tool approval settings (tools entry server:github), which no chart renders",
		},
		"a requireApproval that is not a boolean": {
			in:          map[string]any{"modelConfig": map[string]any{"name": "m"}, "muster": map[string]any{"requireApproval": "yes"}},
			errContains: "muster.requireApproval is yes, not a boolean",
		},
	} {
		t.Run(name, func(t *testing.T) {
			before := deepCopy(tc.in)
			got, ch, err := rewriteValuesV3(t.Context(), tc.in, "", nil)
			require.Equal(t, before, tc.in, "the input is never modified")
			if tc.errContains != "" {
				require.ErrorIs(t, err, agents.ErrInvalid)
				require.ErrorContains(t, err, tc.errContains)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
			require.Equal(t, tc.unchanged, ch.Empty())
			require.Equal(t, tc.removed, ch.Removed)
			require.Equal(t, tc.set, ch.Set)
		})
	}
}

func deepCopy(m map[string]any) map[string]any {
	raw, _ := json.Marshal(m)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func TestV3RewritesRequireApprovalBeforeMovingTheSource(t *testing.T) {
	ctx := t.Context()
	c := newCluster(
		ociRepository(kagentNS, v2Range),
		helmRelease(kagentNS, "triage", approvalValues(), "2.7.0"), agentObj(kagentNS, "triage", "triage", kagentNS, true),
		helmRelease(kagentNS, "plain", narrowRewritten(), "2.7.0"), agentObj(kagentNS, "narrow", "plain", kagentNS, true),
	)

	res, err := c.runnerV3(t, Options{Namespaces: []string{kagentNS}}, v3Latest).Run(ctx)
	require.NoError(t, err)
	rep := res.Reports[0]
	require.Equal(t, "2.x -> 3.x", rep.Run.Path)
	triage := byRelease(rep)["kagent/triage"]
	require.Equal(t, ActionRewritten, triage.Action, triage.Reason)
	require.Equal(t, []string{"muster.requireApproval"}, triage.Changes.Removed)
	require.Equal(t, []string{"default=Ask"}, triage.Changes.Set)
	require.Equal(t, approvalRewritten(), c.values(t, kagentNS, "triage"))
	plain := byRelease(rep)["kagent/plain"]
	require.Equal(t, ActionUnchanged, plain.Action, "2.x values without requireApproval and with a model are 3.x values")
	require.Equal(t, "already on the 3.x values", plain.Reason)
	require.Equal(t, SourceMoved, bySource(rep)["kagent/agent"].Action)
	require.Equal(t, v3Target, c.semver(t, kagentNS, "agent"))
	require.True(t, rep.Changed)
	require.Empty(t, rep.Agents, "2.x and 3.x render the same Agent: nothing of the old line is left behind")

	// Flux upgrades both releases; the second run changes nothing.
	c.upgrade(t, kagentNS, "triage", v3Latest)
	c.upgrade(t, kagentNS, "plain", v3Latest)
	res, err = c.runnerV3(t, Options{Namespaces: []string{kagentNS}}, v3Latest).Run(ctx)
	require.NoError(t, err)
	rep = res.Reports[0]
	require.Equal(t, PhaseComplete, rep.Phase, rep.Summary)
	require.Equal(t, "nothing left to migrate (2.x -> 3.x): 2 Generic-chart release(s) on 3.x values", rep.Summary)
	require.False(t, rep.Changed)
	require.Nil(t, rep.Contract.CRDs, "the hop removes no CRD")
}

func TestV3SourceWaitsWhileTheChartIsNotPublished(t *testing.T) {
	c := newCluster(ociRepository(kagentNS, v2Range), helmRelease(kagentNS, "triage", approvalValues(), "2.7.0"))

	res, err := c.runnerV3(t, Options{Namespaces: []string{kagentNS}}, "").Run(t.Context())
	require.NoError(t, err)
	rep := res.Reports[0]
	triage := byRelease(rep)["kagent/triage"]
	require.Equal(t, ActionPending, triage.Action)
	require.Contains(t, triage.Reason, "3.x values under a 2.x chart would fail the render")
	require.Equal(t, approvalValues(), c.values(t, kagentNS, "triage"), "values are not rewritten before the 3.x chart exists")
	require.Equal(t, v2Range, c.semver(t, kagentNS, "agent"))
}

func TestV3GitOpsOwnedReleaseGetsTheDiff(t *testing.T) {
	hr := gitOpsLabeled(helmRelease("flux-giantswarm", "triage", approvalValues(), "2.7.0"))
	require.NoError(t, unstructured.SetNestedField(hr.Object, kagentNS, "spec", "targetNamespace"))
	c := newCluster(gitOpsLabeled(ociRepository("flux-giantswarm", v2Range)), hr, agentObj(kagentNS, "triage", "triage", "flux-giantswarm", true))

	res, err := c.runnerV3(t, Options{Namespaces: []string{kagentNS}}, v3Latest).Run(t.Context())
	require.NoError(t, err)
	rep := res.Reports[0]
	rel := byRelease(rep)["flux-giantswarm/triage"]
	require.Equal(t, ActionDiff, rel.Action)
	require.Contains(t, rel.Diff, "-      requireApproval: true\n")
	require.Contains(t, rel.Diff, "+    default: Ask\n")
	src := bySource(rep)["flux-giantswarm/agent"]
	require.Equal(t, ActionDiff, src.Action)
	require.Contains(t, src.Diff, "+    semver: '"+v3Target+"'\n")
	require.Equal(t, approvalValues(), c.values(t, "flux-giantswarm", "triage"), "a GitOps-owned release is never written")
	require.Empty(t, c.writes("flux-giantswarm"))
	require.Equal(t, PhaseExpand, rep.Phase)
	require.Contains(t, strings.Join(rep.Pending, "\n"), "release flux-giantswarm/triage: its rewrite (the diff in this report) is not applied in the owning repository yet")
}

func TestV3ApprovalWithSettingsFailsAndHoldsItsSiblings(t *testing.T) {
	broken := approvalValues()
	broken["default"] = "Ask"
	c := newCluster(
		ociRepository(kagentNS, v2Range),
		helmRelease(kagentNS, "broken", broken, "2.7.0"),
		helmRelease(kagentNS, "triage", approvalValues(), "2.7.0"),
	)

	res, err := c.runnerV3(t, Options{Namespaces: []string{kagentNS}}, v3Latest).Run(t.Context())
	require.NoError(t, err)
	rep := res.Reports[0]
	require.Equal(t, ActionFailed, byRelease(rep)["kagent/broken"].Action)
	require.Contains(t, byRelease(rep)["kagent/broken"].Reason, "which no chart renders")
	require.Equal(t, ActionPending, byRelease(rep)["kagent/triage"].Action)
	require.Contains(t, byRelease(rep)["kagent/triage"].Reason, "held: chart source kagent/agent also serves kagent/broken (failed)")
	require.Equal(t, approvalValues(), c.values(t, kagentNS, "triage"))
	require.Equal(t, SourceNotMoved, bySource(rep)["kagent/agent"].Action)
	require.Equal(t, v2Range, c.semver(t, kagentNS, "agent"))
}

func TestV3ReleaseOn1xIsRefused(t *testing.T) {
	c := newCluster(ociRepository(kagentNS, v1Range), helmRelease(kagentNS, "old", narrowRewritten(), "1.5.3"))

	res, err := c.runnerV3(t, Options{Namespaces: []string{kagentNS}}, v3Latest).Run(t.Context())
	require.NoError(t, err)
	rel := byRelease(res.Reports[0])["kagent/old"]
	require.Equal(t, ActionFailed, rel.Action)
	require.Contains(t, rel.Reason, "older than the 2.x line this migration (2.x -> 3.x) starts from: migrate it to 2.x first")
}

func TestTargetMajorPicksTheV3Hop(t *testing.T) {
	major, err := TargetMajor(v3Target)
	require.NoError(t, err)
	require.Equal(t, uint64(3), major)
	h, err := hopFor(major, "v1alpha3")
	require.NoError(t, err)
	require.Equal(t, "2.x -> 3.x", h.String())
	_, err = hopFor(4, "v1alpha3")
	require.ErrorContains(t, err, "no migration path to Generic chart 4.x")
}
