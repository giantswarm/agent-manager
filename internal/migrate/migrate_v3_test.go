package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"

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

// fakeChart3 is the agent chart 3.x as the registry reports it: the values
// schema of the 3.0 chart itself (testdata/agent-3.0-values.schema.json,
// copied from giantswarm/agent's helm/agent at the commit that removed
// muster.requireApproval).
type fakeChart3 struct{ latest string }

var chart3Schema = sync.OnceValue(func() map[string]any {
	raw, err := os.ReadFile(filepath.Join("testdata", "agent-3.0-values.schema.json"))
	if err != nil {
		panic(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		panic(err)
	}
	return doc
})

func (fakeChart3) Schema(context.Context) chart.Schema {
	return chart.Schema{Document: chart3Schema(), Version: v3Latest, Source: chart.SourceRegistry}
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
			errContains: "muster.requireApproval: true together with tool approval settings (default), which no released chart renders",
		},
		"requireApproval true with an entry setting never rendered": {
			in:          map[string]any{"modelConfig": map[string]any{"name": "m"}, "muster": map[string]any{"requireApproval": true}, "tools": []any{map[string]any{"server:github": "Allow"}}},
			errContains: "muster.requireApproval: true together with tool approval settings (tools entry server:github), which no released chart renders",
		},
		"requireApproval null is Helm's unset, as 2.x rendered it: dropped": {
			in:      map[string]any{"modelConfig": map[string]any{"name": "m"}, "muster": map[string]any{"requireApproval": nil}},
			want:    map[string]any{"modelConfig": map[string]any{"name": "m"}},
			removed: []string{"muster.requireApproval"},
		},
		"requireApproval true with muster.enabled null rendered no binding: dropped": {
			in:      map[string]any{"modelConfig": map[string]any{"name": "m"}, "muster": map[string]any{"requireApproval": true, "enabled": nil}},
			want:    map[string]any{"modelConfig": map[string]any{"name": "m"}, "muster": map[string]any{"enabled": nil}},
			removed: []string{"muster.requireApproval"},
		},
		"a modelConfig that is not an object is left to the schema, not reported as set": {
			in: map[string]any{"modelConfig": "m"}, want: map[string]any{"modelConfig": "m"}, unchanged: true,
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
		helmRelease(kagentNS, "triage", approvalValues(), "2.6.1"), agentObj(kagentNS, "triage", "triage", kagentNS, true),
		helmRelease(kagentNS, "plain", narrowRewritten(), "2.6.1"), agentObj(kagentNS, "narrow", "plain", kagentNS, true),
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
	c := newCluster(ociRepository(kagentNS, v2Range), helmRelease(kagentNS, "triage", approvalValues(), "2.6.1"))

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
	hr := gitOpsLabeled(helmRelease("flux-system", "triage", approvalValues(), "2.6.1"))
	require.NoError(t, unstructured.SetNestedField(hr.Object, kagentNS, "spec", "targetNamespace"))
	c := newCluster(gitOpsLabeled(ociRepository("flux-system", v2Range)), hr, agentObj(kagentNS, "triage", "triage", "flux-system", true))

	res, err := c.runnerV3(t, Options{Namespaces: []string{kagentNS}}, v3Latest).Run(t.Context())
	require.NoError(t, err)
	rep := res.Reports[0]
	rel := byRelease(rep)["flux-system/triage"]
	require.Equal(t, ActionDiff, rel.Action)
	require.Contains(t, rel.Diff, "-      requireApproval: true\n")
	require.Contains(t, rel.Diff, "+    default: Ask\n")
	src := bySource(rep)["flux-system/agent"]
	require.Equal(t, ActionDiff, src.Action)
	require.Contains(t, src.Diff, "+    semver: '"+v3Target+"'\n")
	require.Equal(t, approvalValues(), c.values(t, "flux-system", "triage"), "a GitOps-owned release is never written")
	require.Empty(t, c.writes("flux-system"))
	require.Equal(t, PhaseExpand, rep.Phase)
	require.Contains(t, strings.Join(rep.Pending, "\n"), "release flux-system/triage: its rewrite (the diff in this report) is not applied in the owning repository yet")
}

func TestV3ApprovalWithSettingsFailsAndHoldsItsSiblings(t *testing.T) {
	broken := approvalValues()
	broken["default"] = "Ask"
	c := newCluster(
		ociRepository(kagentNS, v2Range),
		helmRelease(kagentNS, "broken", broken, "2.6.1"),
		helmRelease(kagentNS, "triage", approvalValues(), "2.6.1"),
	)

	res, err := c.runnerV3(t, Options{Namespaces: []string{kagentNS}}, v3Latest).Run(t.Context())
	require.NoError(t, err)
	rep := res.Reports[0]
	require.Equal(t, ActionFailed, byRelease(rep)["kagent/broken"].Action)
	require.Contains(t, byRelease(rep)["kagent/broken"].Reason, "which no released chart renders")
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

// valuesFromRelease is a release whose values also come from a ConfigMap.
func valuesFromRelease(ns, name string, values map[string]any) *unstructured.Unstructured {
	hr := helmRelease(ns, name, values, "2.6.1")
	_ = unstructured.SetNestedSlice(hr.Object, []any{map[string]any{"kind": "ConfigMap", "name": name + "-values"}}, "spec", "valuesFrom")
	return hr
}

func TestV3ReleaseWithValuesFromIsReportedNeverWritten(t *testing.T) {
	for name, tc := range map[string]struct {
		values   map[string]any
		reason   string
		diff     []string
		noDiffOf string
	}{
		"model only in valuesFrom: never spelled out over it": {
			values:   map[string]any{"agent": map[string]any{"name": "a"}},
			reason:   "takes values from spec.valuesFrom",
			noDiffOf: "default-model-config",
		},
		"requireApproval in valuesFrom: named in the reason": {
			values: map[string]any{"agent": map[string]any{"name": "a"}, "modelConfig": map[string]any{"name": "m"}},
			reason: "replace muster.requireApproval: true there with default: Ask",
		},
		"requireApproval inline: the diff, not a write agent-manager would refuse to bind": {
			values: approvalValues(),
			reason: "agent-manager binds tool approval settings only from spec.values",
			diff:   []string{"-      requireApproval: true\n", "+    default: Ask\n"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := newCluster(ociRepository(kagentNS, v2Range), valuesFromRelease(kagentNS, "vf", tc.values), helmRelease(kagentNS, "sibling", approvalValues(), "2.6.1"))
			res, err := c.runnerV3(t, Options{Namespaces: []string{kagentNS}}, v3Latest).Run(t.Context())
			require.NoError(t, err)
			rep := res.Reports[0]
			rel := byRelease(rep)["kagent/vf"]
			require.Equal(t, ActionFailed, rel.Action)
			require.Contains(t, rel.Reason, tc.reason)
			for _, d := range tc.diff {
				require.Contains(t, rel.Diff, d)
			}
			if tc.noDiffOf != "" {
				require.NotContains(t, rel.Diff, tc.noDiffOf)
			}
			require.Equal(t, tc.values, c.values(t, kagentNS, "vf"), "never written")
			require.Equal(t, ActionPending, byRelease(rep)["kagent/sibling"].Action, "its siblings are held")
			require.Equal(t, approvalValues(), c.values(t, kagentNS, "sibling"))
			require.Equal(t, v2Range, c.semver(t, kagentNS, "agent"))
		})
	}
}

func TestV3ReleaseOfASourceMigrateCannotMoveIsHeld(t *testing.T) {
	for name, src := range map[string]*unstructured.Unstructured{
		"pinned by tag": func() *unstructured.Unstructured {
			o := ociRepository(kagentNS, "")
			_ = unstructured.SetNestedField(o.Object, map[string]any{"tag": "2.6.1"}, "spec", "ref")
			return o
		}(),
		"GitOps-owned": gitOpsLabeled(ociRepository(kagentNS, v2Range)),
	} {
		t.Run(name, func(t *testing.T) {
			c := newCluster(src, helmRelease(kagentNS, "triage", approvalValues(), "2.6.1"))
			res, err := c.runnerV3(t, Options{Namespaces: []string{kagentNS}}, v3Latest).Run(t.Context())
			require.NoError(t, err)
			rel := byRelease(res.Reports[0])["kagent/triage"]
			require.Equal(t, ActionPending, rel.Action)
			require.Contains(t, rel.Reason, "this run cannot move its chart source")
			require.Contains(t, rel.Diff, "+    default: Ask\n")
			require.Equal(t, approvalValues(), c.values(t, kagentNS, "triage"), "3.x values are never written under a source left on 2.x")
		})
	}
}

func orgNamespace(name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"agent-platform.giantswarm.io/organization": "acme"}}}
}

// reportReleases is the OrganizationReadyReleasesAnnotation on the report in
// ns; ok is false when the report does not carry it.
func (c *cluster) reportReleases(t *testing.T, ns string) (string, bool) {
	t.Helper()
	cm, err := c.typed.CoreV1().ConfigMaps(ns).Get(t.Context(), DefaultReportConfigMap, metav1.GetOptions{})
	require.NoError(t, err)
	v, ok := cm.Annotations[OrganizationReadyReleasesAnnotation]
	return v, ok
}

// reportAnnotation is one annotation of the report in ns.
func (c *cluster) reportAnnotation(t *testing.T, ns, key string) (string, bool) {
	t.Helper()
	cm, err := c.typed.CoreV1().ConfigMaps(ns).Get(t.Context(), DefaultReportConfigMap, metav1.GetOptions{})
	require.NoError(t, err)
	v, ok := cm.Annotations[key]
	return v, ok
}

// reportReady is the OrganizationReadyAnnotation on the report in ns; ok is
// false when no report exists.
func (c *cluster) reportReady(t *testing.T, ns string) (string, bool) {
	t.Helper()
	cm, err := c.typed.CoreV1().ConfigMaps(ns).Get(t.Context(), DefaultReportConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", false
	}
	require.NoError(t, err)
	return cm.Annotations[OrganizationReadyAnnotation], true
}

// An organization's releases are migrated like a managed namespace's: the
// ones agent-manager owns are rewritten, the operator's source stays where it
// is, and the namespace's report marks it ready for the operator.
func TestV3OrganizationReleasesAreRewrittenAndMarkTheNamespaceReady(t *testing.T) {
	ctx := t.Context()
	c := newCluster(
		ociRepository(kagentNS, v2Range), helmRelease(kagentNS, "plain", narrowRewritten(), "2.6.1"),
		ociRepository("org-acme", v2Range),
		helmRelease("org-acme", "hand", approvalValues(), "2.6.1"),
		helmRelease("org-acme", "composed", narrowRewritten(), "2.6.1"),
	)
	res, err := c.runnerV3(t, Options{Namespaces: []string{kagentNS}, OrganizationNamespaces: []string{"org-acme"}}, v3Latest).Run(ctx)
	require.NoError(t, err)
	require.Len(t, res.Reports, 2)
	kagent, org := res.Reports[0], res.Reports[1]
	require.Equal(t, "org-acme", org.Namespace)
	hand := byRelease(org)["org-acme/hand"]
	require.Equal(t, ActionRewritten, hand.Action, hand.Reason)
	require.Equal(t, OwnershipOrganization, hand.Ownership)
	require.Equal(t, []string{"default=Ask"}, hand.Changes.Set)
	require.Equal(t, approvalRewritten(), c.values(t, "org-acme", "hand"), "requireApproval: true becomes default: Ask, never just dropped")
	require.Equal(t, ActionUnchanged, byRelease(org)["org-acme/composed"].Action)
	require.Equal(t, v2Range, c.semver(t, "org-acme", "agent"), "the operator's source is never moved here")
	require.Equal(t, []string{"update helmreleases"}, c.writes("org-acme"))
	src := bySource(org)["org-acme/agent"]
	require.Equal(t, ActionUnchanged, src.Action)
	require.Contains(t, src.Reason, OrganizationReadyAnnotation)

	ready, ok := c.reportReady(t, "org-acme")
	require.True(t, ok, "the organization's report is written")
	require.Equal(t, "3", ready, "every release there is on 3.x values: the operator may move the source")
	names, _ := c.reportReleases(t, "org-acme")
	require.Equal(t, "composed,hand", names, "the mark names the releases it covers, sorted")
	require.NotNil(t, c.reportConfigMap(t, "org-acme"))
	require.NotContains(t, strings.Join(kagent.Pending, "\n"), "organization namespace", "nothing of the organization is pending")
	require.Equal(t, SourceMoved, bySource(kagent)["kagent/agent"].Action)
	_, ok = c.reportReady(t, kagentNS)
	require.True(t, ok)
	ready, _ = c.reportReady(t, kagentNS)
	require.Empty(t, ready, "a managed namespace's report carries no mark: its source is the run's own")
}

// versioned sets a release's UID and generation as the API server would.
func versioned(hr *unstructured.Unstructured, uid string, generation int64) *unstructured.Unstructured {
	hr.SetUID(types.UID(uid))
	hr.SetGeneration(generation)
	return hr
}

// The ready mark records each release's UID and generation as the run left
// it: a release the run rewrote at the generation its write produced, so the
// operator can tell a release recreated or edited after the run.
func TestV3OrganizationMarkRecordsEachReleaseVersion(t *testing.T) {
	c := newCluster(
		ociRepository(kagentNS, v3Target),
		ociRepository("org-acme", v2Range),
		versioned(helmRelease("org-acme", "hand", approvalValues(), "2.6.1"), "uid-hand", 4),
		versioned(helmRelease("org-acme", "composed", narrowRewritten(), "2.6.1"), "uid-composed", 7),
	)
	// The API server bumps metadata.generation on a spec write.
	c.dyn.PrependReactor("update", "helmreleases", func(a k8stesting.Action) (bool, runtime.Object, error) {
		obj := a.(k8stesting.UpdateAction).GetObject().(*unstructured.Unstructured)
		obj.SetGeneration(obj.GetGeneration() + 1)
		return false, nil, nil
	})
	_, err := c.runnerV3(t, Options{Namespaces: []string{kagentNS}, OrganizationNamespaces: []string{"org-acme"}}, v3Latest).Run(t.Context())
	require.NoError(t, err)
	ready, _ := c.reportReady(t, "org-acme")
	require.Equal(t, "3", ready)
	raw, ok := c.reportAnnotation(t, "org-acme", OrganizationReadyVersionsAnnotation)
	require.True(t, ok, "the mark carries the release versions")
	var got map[string]ReleaseVersion
	require.NoError(t, json.Unmarshal([]byte(raw), &got))
	require.Equal(t, map[string]ReleaseVersion{
		"hand":     {UID: "uid-hand", Generation: 5},
		"composed": {UID: "uid-composed", Generation: 7},
	}, got, "the rewritten release at its written generation, the unchanged one as read")
}

// A GitOps-owned release in an organization gets the diff and holds its
// writable siblings; the namespace's report is not marked (a stale mark of an
// earlier run is removed), so the operator keeps the source on 2.x.
func TestV3OrganizationGitOpsReleaseHoldsTheMark(t *testing.T) {
	ctx := t.Context()
	c := newCluster(
		ociRepository(kagentNS, v3Target),
		ociRepository("org-acme", v2Range),
		gitOpsLabeled(helmRelease("org-acme", "git", approvalValues(), "2.6.1")),
		helmRelease("org-acme", "hand", approvalValues(), "2.6.1"),
	)
	_, err := c.typed.CoreV1().ConfigMaps("org-acme").Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: DefaultReportConfigMap, Namespace: "org-acme", Annotations: map[string]string{OrganizationReadyAnnotation: "3", OrganizationReadyReleasesAnnotation: "hand", OrganizationReadyVersionsAnnotation: `{"hand":{"uid":"u","generation":1}}`},
	}}, metav1.CreateOptions{})
	require.NoError(t, err)
	res, err := c.runnerV3(t, Options{Namespaces: []string{kagentNS}, OrganizationNamespaces: []string{"org-acme"}}, v3Latest).Run(ctx)
	require.NoError(t, err)
	kagent, org := res.Reports[0], res.Reports[1]
	git := byRelease(org)["org-acme/git"]
	require.Equal(t, ActionDiff, git.Action)
	require.Equal(t, OwnershipOrganization, git.Ownership)
	require.Contains(t, git.Reason, "apply the diff in the organization's repository")
	require.Contains(t, git.Diff, "+    default: Ask\n")
	hand := byRelease(org)["org-acme/hand"]
	require.Equal(t, ActionPending, hand.Action)
	require.Contains(t, hand.Reason, "also serves org-acme/git (diff)")
	require.Contains(t, hand.Diff, "+    default: Ask\n")
	require.Equal(t, approvalValues(), c.values(t, "org-acme", "hand"), "no 3.x values while a sibling keeps the source on 2.x")
	require.Empty(t, c.writes("org-acme"))
	ready, ok := c.reportReady(t, "org-acme")
	require.True(t, ok)
	require.Empty(t, ready, "the stale mark is removed")
	_, ok = c.reportReleases(t, "org-acme")
	require.False(t, ok, "with its release list")
	_, ok = c.reportAnnotation(t, "org-acme", OrganizationReadyVersionsAnnotation)
	require.False(t, ok, "and its release versions")
	require.Equal(t, PhaseExpand, org.Phase)
	require.Equal(t, PhaseExpand, kagent.Phase, "a pending organization release holds the run's state")
	require.Contains(t, strings.Join(kagent.Pending, "\n"), "organization namespace org-acme: release org-acme/git")
}

// Without a published 3.x chart nothing of an organization is written and
// its report carries no mark; a dry run writes no report at all.
func TestV3OrganizationIsNotMarkedBeforeTheChartIsPublishedNorInADryRun(t *testing.T) {
	objs := func() *cluster {
		return newCluster(ociRepository(kagentNS, v3Target), ociRepository("org-acme", v2Range), helmRelease("org-acme", "hand", approvalValues(), "2.6.1"))
	}
	c := objs()
	res, err := c.runnerV3(t, Options{Namespaces: []string{kagentNS}, OrganizationNamespaces: []string{"org-acme"}}, "").Run(t.Context())
	require.NoError(t, err)
	require.Equal(t, ActionPending, byRelease(res.Reports[1])["org-acme/hand"].Action)
	require.Equal(t, approvalValues(), c.values(t, "org-acme", "hand"))
	ready, ok := c.reportReady(t, "org-acme")
	require.True(t, ok)
	require.Empty(t, ready)

	c = objs()
	res, err = c.runnerV3(t, Options{Namespaces: []string{kagentNS}, OrganizationNamespaces: []string{"org-acme"}, DryRun: true}, v3Latest).Run(t.Context())
	require.NoError(t, err)
	require.Equal(t, ActionRewritten, byRelease(res.Reports[1])["org-acme/hand"].Action)
	require.Equal(t, approvalValues(), c.values(t, "org-acme", "hand"))
	_, ok = c.reportReady(t, "org-acme")
	require.False(t, ok, "a dry run writes no report, so it marks nothing")
}

// An organization without agents is ready at once.
func TestV3OrganizationWithoutReleasesIsMarkedReady(t *testing.T) {
	c := newCluster(ociRepository(kagentNS, v3Target), ociRepository("org-acme", v2Range))
	// An earlier run's report, unmarked: the mark is added to it.
	_, err := c.typed.CoreV1().ConfigMaps("org-acme").Create(t.Context(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: DefaultReportConfigMap, Namespace: "org-acme"}}, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = c.runnerV3(t, Options{Namespaces: []string{kagentNS}, OrganizationNamespaces: []string{"org-acme"}}, v3Latest).Run(t.Context())
	require.NoError(t, err)
	ready, _ := c.reportReady(t, "org-acme")
	require.Equal(t, "3", ready)
	names, ok := c.reportReleases(t, "org-acme")
	require.True(t, ok)
	require.Empty(t, names)
}

func TestV3OrganizationNamespacesAreFoundByLabel(t *testing.T) {
	c := newCluster()
	_, err := c.typed.CoreV1().Namespaces().Create(t.Context(), orgNamespace("org-acme"), metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = c.typed.CoreV1().Namespaces().Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "other"}}, metav1.CreateOptions{})
	require.NoError(t, err)
	names, err := OrganizationNamespaces(t.Context(), c.typed, []string{kagentNS})
	require.NoError(t, err)
	require.Equal(t, []string{"org-acme"}, names)
}

// an organization namespace on target, with Ready Agents, leaves
// the run complete: Flux and the Agents there are not gated on (agent-manager
// reads status in managed namespaces only).
func TestV3OrganizationOnTargetDoesNotHoldTheRun(t *testing.T) {
	c := newCluster(
		ociRepository(kagentNS, v3Target), helmRelease(kagentNS, "plain", narrowRewritten(), v3Latest), agentObj(kagentNS, "narrow", "plain", kagentNS, true),
		ociRepository("org-acme", v3Target), helmRelease("org-acme", "composed", narrowRewritten(), v3Latest), agentObj("org-acme", "narrow", "composed", "org-acme", true),
		ociRepository("org-beta", v2Range), helmRelease("org-beta", "waiting", narrowRewritten(), "2.6.1"), agentObj("org-beta", "narrow", "waiting", "org-beta", false),
	)
	res, err := c.runnerV3(t, Options{Namespaces: []string{kagentNS}, OrganizationNamespaces: []string{"org-acme", "org-beta"}}, v3Latest).Run(t.Context())
	require.NoError(t, err)
	kagent := res.Reports[0]
	require.Equal(t, PhaseComplete, kagent.Phase, strings.Join(kagent.Pending, "\n"))
	require.NotContains(t, strings.Join(append(kagent.Pending, kagent.Warnings...), "\n"), "organization namespace")
	for _, org := range res.Reports[1:] {
		require.Equal(t, PhaseComplete, org.Phase, org.Summary)
	}
	require.Equal(t, ActionUnchanged, bySource(res.Reports[2])["org-beta/agent"].Action, "the operator's source, never moved here")
	require.Equal(t, v2Range, c.semver(t, "org-beta", "agent"))
	require.Empty(t, c.writes("org-beta"))
}

// a release with valuesFrom already deployed from a 3.x chart is
// done: the 3.x chart refuses muster.requireApproval in its merged values.
func TestV3ValuesFromReleaseOnTargetIsUnchanged(t *testing.T) {
	hr := valuesFromRelease(kagentNS, "vf", map[string]any{"agent": map[string]any{"name": "a"}})
	hr.Object["status"].(map[string]any)["history"] = []any{map[string]any{"chartVersion": v3Latest, "status": "deployed"}}
	c := newCluster(ociRepository(kagentNS, v3Target), hr, helmRelease(kagentNS, "sibling", approvalRewritten(), v3Latest))
	res, err := c.runnerV3(t, Options{Namespaces: []string{kagentNS}}, v3Latest).Run(t.Context())
	require.NoError(t, err)
	rel := byRelease(res.Reports[0])["kagent/vf"]
	require.Equal(t, ActionUnchanged, rel.Action, rel.Reason)
	require.Contains(t, strings.Join(rel.Warnings, "\n"), "spec.valuesFrom")
	require.Equal(t, ActionUnchanged, byRelease(res.Reports[0])["kagent/sibling"].Action)
}

// an organization namespace that cannot be read fails the run
// before anything is written.
func TestV3UnreadableOrganizationFailsBeforeAnyWrite(t *testing.T) {
	c := newCluster(ociRepository(kagentNS, v2Range), helmRelease(kagentNS, "triage", approvalValues(), "2.6.1"))
	c.dyn.PrependReactor("list", "helmreleases", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetNamespace() == "org-acme" {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "helm.toolkit.fluxcd.io", Resource: "helmreleases"}, "", errors.New("no"))
		}
		return false, nil, nil
	})
	_, err := c.runnerV3(t, Options{Namespaces: []string{kagentNS}, OrganizationNamespaces: []string{"org-acme"}}, v3Latest).Run(t.Context())
	require.ErrorContains(t, err, "organization namespace org-acme")
	require.Equal(t, approvalValues(), c.values(t, kagentNS, "triage"))
	require.Equal(t, v2Range, c.semver(t, kagentNS, "agent"))
	require.Empty(t, c.writes(kagentNS))
}

// The operator's source is never moved, also when every release on it is on
// target values (no blocker).
func TestV3OrganizationSourceIsNeverMoved(t *testing.T) {
	c := newCluster(ociRepository(kagentNS, v3Target), ociRepository("org-acme", v2Range), helmRelease("org-acme", "composed", narrowRewritten(), "2.6.1"))
	res, err := c.runnerV3(t, Options{Namespaces: []string{kagentNS}, OrganizationNamespaces: []string{"org-acme"}}, v3Latest).Run(t.Context())
	require.NoError(t, err)
	src := bySource(res.Reports[1])["org-acme/agent"]
	require.Equal(t, ActionUnchanged, src.Action)
	require.Equal(t, OwnershipOrganization, src.Ownership)
	require.Empty(t, src.Diff)
	require.Equal(t, v2Range, c.semver(t, "org-acme", "agent"))
	require.Empty(t, c.writes("org-acme"))
}

// a range whose bound admits a pre-release of the next major is
// refused: Flux would serve that chart under it.
func TestTargetMajorRefusesThePreReleasesOfTheNextMajor(t *testing.T) {
	for _, r := range []string{">=2.6.0 <=3.0.0-rc.1", ">=2.6.0 <3.0.0-rc.1", ">=2.6.0 <3.0.0-alpha.2"} {
		_, err := TargetMajor(r)
		require.ErrorContains(t, err, "admits pre-releases of 3.0.0", r)
	}
	for _, r := range []string{v2Range, ">=2.6.0-0 <3.0.0-0", v3Target} {
		_, err := TargetMajor(r)
		require.NoError(t, err, r)
	}
}

// a GitOps-owned release waiting for its diff keeps the namespace's
// source on 2.x, so its writable siblings are held, not given 3.x values
// that every 2.x upgrade would refuse.
func TestV3DiffOfAGitOpsSiblingHoldsTheSource(t *testing.T) {
	git := gitOpsLabeled(helmRelease(kagentNS, "m3-git", approvalValues(), "2.6.1"))
	c := newCluster(ociRepository(kagentNS, v2Range), git, helmRelease(kagentNS, "m3-ra", approvalValues(), "2.6.1"))
	res, err := c.runnerV3(t, Options{Namespaces: []string{kagentNS}}, v3Latest).Run(t.Context())
	require.NoError(t, err)
	rep := res.Reports[0]
	require.Equal(t, ActionDiff, byRelease(rep)["kagent/m3-git"].Action)
	ra := byRelease(rep)["kagent/m3-ra"]
	require.Equal(t, ActionPending, ra.Action)
	require.Contains(t, ra.Reason, "also serves kagent/m3-git (diff)")
	require.Contains(t, ra.Diff, "+    default: Ask\n", "the held change is shown")
	require.Equal(t, approvalValues(), c.values(t, kagentNS, "m3-ra"), "no 3.x values under a source left on 2.x")
	require.Equal(t, v2Range, c.semver(t, kagentNS, "agent"))
	for _, w := range c.writes(kagentNS) {
		require.NotEqual(t, "update helmreleases", w)
	}
}

func ociRepositoryAt(ns, url, semver string) *unstructured.Unstructured {
	o := ociRepository(ns, semver)
	_ = unstructured.SetNestedField(o.Object, url, "spec", "url")
	return o
}

// an organization's chart source the operator created with another
// registry URL is still the agent chart's: its releases are migrated; a
// source of another chart is named in the warnings.
func TestV3OrganizationSourceOnAnotherRegistryIsStillReported(t *testing.T) {
	other := helmRelease("org-acme", "other-app", map[string]any{}, "1.0.0")
	_ = unstructured.SetNestedField(other.Object, "dashboards", "spec", "chartRef", "name")
	dash := ociRepositoryAt("org-acme", "oci://mirror.example.com/charts/dashboards", "1.x")
	dash.SetName("dashboards")
	c := newCluster(
		ociRepository(kagentNS, v3Target),
		ociRepositoryAt("org-acme", "oci://registry.example.com/charts/agent", v2Range),
		helmRelease("org-acme", "m3-org", approvalValues(), "2.6.1"),
		dash, other,
	)
	res, err := c.runnerV3(t, Options{Namespaces: []string{kagentNS}, OrganizationNamespaces: []string{"org-acme"}}, v3Latest).Run(t.Context())
	require.NoError(t, err)
	org := res.Reports[1]
	rel := byRelease(org)["org-acme/m3-org"]
	require.Equal(t, ActionRewritten, rel.Action, rel.Reason)
	require.Equal(t, OwnershipOrganization, rel.Ownership)
	require.Equal(t, approvalRewritten(), c.values(t, "org-acme", "m3-org"))
	require.NotContains(t, byRelease(org), "org-acme/other-app")
	require.Contains(t, strings.Join(org.Warnings, "\n"), "release org-acme/other-app renders from OCIRepository dashboards")
	require.Equal(t, []string{"update helmreleases"}, c.writes("org-acme"), "the other chart's release and both sources are left alone")
}

// A warning about an unrecognised release survives a complete phase.
func TestV3OrganizationWarningSurvivesACompletePhase(t *testing.T) {
	other := helmRelease("org-acme", "other-app", map[string]any{}, "1.0.0")
	_ = unstructured.SetNestedField(other.Object, "dashboards", "spec", "chartRef", "name")
	dash := ociRepositoryAt("org-acme", "oci://mirror.example.com/charts/dashboards", "1.x")
	dash.SetName("dashboards")
	c := newCluster(ociRepository(kagentNS, v3Target), dash, other)
	res, err := c.runnerV3(t, Options{Namespaces: []string{kagentNS}, OrganizationNamespaces: []string{"org-acme"}}, v3Latest).Run(t.Context())
	require.NoError(t, err)
	require.Equal(t, PhaseComplete, res.Reports[1].Phase)
	require.Contains(t, strings.Join(res.Reports[1].Warnings, "\n"), "other-app")
}

// An exact pin is read as its major, as the agent-platform chart reads it.
func TestTargetMajorReadsAnExactPin(t *testing.T) {
	for pin, want := range map[string]uint64{"2.6.1": 2, "=3.0.0": 3, "v3.0.0": 3} {
		got, err := TargetMajor(pin)
		require.NoError(t, err, pin)
		require.Equal(t, want, got, pin)
	}
	_, err := TargetMajor("3.0.0-rc.6")
	require.ErrorContains(t, err, "admits no release version")
}

// Every alternative of a `||` range and every version a range names counts,
// as the agent-platform chart's guard reads it (D-GO-08).
func TestTargetMajorReadsEveryAlternativeAndNamedVersion(t *testing.T) {
	for r, want := range map[string]uint64{
		">=2.6.1 <2.6.9":  2,
		"2.6.1 - 2.6.5":   2,
		"= 2.6.1":         2,
		"~2.250":          2,
		">2.6.1 <2.6.3":   2,
		"3.0.1 || 3.2.0":  3,
		"^3.0.0 || 3.0.1": 3,
	} {
		got, err := TargetMajor(r)
		require.NoError(t, err, r)
		require.Equal(t, want, got, r)
	}
	for _, r := range []string{"3.0.1 || 2.6.0", "2.6.0 || >=3.0.1 <3.0.5", "~2.250 || 3.0.7"} {
		_, err := TargetMajor(r)
		require.ErrorContains(t, err, "spans majors [2 3]", r)
	}
	_, err := TargetMajor(">2.6.1 <2.6.2")
	require.ErrorContains(t, err, "admits no release version")
}
