package migrate

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/agent-manager/internal/agents"
)

// managedSource is a chart source agent-manager created on semver.
func managedSource(ns, semver string, labels map[string]string) *unstructured.Unstructured {
	src := ociRepository(ns, semver)
	all := map[string]string{agents.ManagedByLabel: agents.ManagedByValue}
	for k, v := range labels {
		all[k] = v
	}
	src.SetLabels(all)
	return src
}

func converge(t *testing.T, c *cluster, semver, filter string, namespaces ...string) map[string]SourceReport {
	t.Helper()
	cfg := agents.ComposeConfig{ChartName: "agent", ChartOCIURL: agents.DefaultChartOCIURL, ChartSemver: semver, ChartSemverFilter: filter, OCIRepositoryAPIVersion: "source.toolkit.fluxcd.io/v1"}
	out := map[string]SourceReport{}
	for _, rep := range ConvergeSources(context.Background(), c.dyn, namespaces, cfg, slog.New(slog.DiscardHandler)) {
		out[rep.Namespace] = rep
	}
	return out
}

// A changed --agent-chart-semver moves the shared source on start, within its
// major, and its tag filter with it.
func TestConvergeSourcesMovesWithinTheMajor(t *testing.T) {
	c := newCluster(managedSource(kagentNS, ">=2.0.0 <3.0.0", nil))

	reps := converge(t, c, "2.3.0", releaseTagFilter, kagentNS, "absent")

	require.Len(t, reps, 1, "a namespace without a source is skipped")
	assert.Equal(t, SourceMoved, reps[kagentNS].Action)
	assert.Equal(t, ">=2.0.0 <3.0.0", reps[kagentNS].From)
	assert.Equal(t, "2.3.0", c.semver(t, kagentNS, "agent"))
	filter, _ := c.semverFilter(t, kagentNS, "agent")
	assert.Equal(t, releaseTagFilter, filter)

	// Back to the range, the filter removed.
	reps = converge(t, c, "2.x", "", kagentNS)
	assert.Equal(t, SourceMoved, reps[kagentNS].Action)
	assert.Equal(t, "2.x", c.semver(t, kagentNS, "agent"))
	_, found := c.semverFilter(t, kagentNS, "agent")
	assert.False(t, found)
}

// An unchanged value writes nothing.
func TestConvergeSourcesLeavesAnUnchangedSource(t *testing.T) {
	c := newCluster(managedSource(kagentNS, "2.x", nil))
	updates := 0
	c.dyn.PrependReactor("update", "ocirepositories", func(k8stesting.Action) (bool, runtime.Object, error) {
		updates++
		return false, nil, nil
	})

	reps := converge(t, c, "2.x", "", kagentNS)

	assert.Equal(t, ActionUnchanged, reps[kagentNS].Action)
	assert.Zero(t, updates)
}

// A source on another major, one in GitOps or one agent-manager did not
// create is never written.
func TestConvergeSourcesRefusesWhatItDoesNotOwn(t *testing.T) {
	external := ociRepository("other", "2.x")
	pinned := managedSource("pinned", "", nil)
	c := newCluster(
		managedSource(kagentNS, "1.x", nil),
		managedSource("gitops", "2.0.0", map[string]string{agents.KustomizationNameLabel: "flux"}),
		external, pinned,
	)

	reps := converge(t, c, "2.3.0", "", kagentNS, "gitops", "other", "pinned")

	for ns, want := range map[string]string{kagentNS: "1.x", "gitops": "2.0.0", "other": "2.x", "pinned": ""} {
		assert.Equal(t, SourceNotMoved, reps[ns].Action, ns)
		assert.NotEmpty(t, reps[ns].Reason, ns)
		assert.Equal(t, want, c.semver(t, ns, "agent"), ns)
	}
	assert.Contains(t, reps[kagentNS].Reason, "migrate command")
}
