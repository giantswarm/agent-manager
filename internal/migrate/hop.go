package migrate

import (
	"context"
	"fmt"

	"github.com/Masterminds/semver/v3"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/giantswarm/agent-manager/internal/agents"
)

// hop is one step between two Generic chart majors: how a release's values
// move from one to the other, what the target chart renders (the object the
// wait phase gates on) and the agent objects the old line leaves behind (the
// ones the contract phase deletes). A cluster serves one kagent line at a
// time and a namespace's releases share one chart source, so the hop is the
// installation's, chosen by the target range; each release's deployed chart
// is checked against it.
type hop struct {
	from, to uint64
	// rewrite turns values of chart from.x into values of chart to.x.
	rewrite func(ctx context.Context, values map[string]any, harness string, pinner agents.SkillPinner) (map[string]any, *ValueChanges, error)
	// rendered is what a release on chart to.x renders, one per release.
	rendered     schema.GroupVersionResource
	renderedKind string
	// leftover is the old line's agent object a release on chart from.x
	// renders; Helm removes it when the release upgrades.
	leftover     schema.GroupVersionResource
	leftoverKind string
	// templates, when set, is where a sub-agent binding's templateRef
	// resolves on the target line.
	templates *schema.GroupVersionResource
	// crds are the CRDs the contract phase deletes once every namespace
	// passed; none when the platform owns the old CRDs' removal.
	crds []string
}

// legacyTemplateGVR is the AgentTemplate of the kagent.dev line, the object
// Generic chart 1.x renders.
var legacyTemplateGVR = schema.GroupVersionResource{Group: "kagent.dev", Version: "v1alpha3", Resource: "agenttemplates"}

// hopFor is the hop to chart to.x; kagentAPIVersion is the version of the
// kagent API the target chart renders into (kagent.dev for 1.x,
// api.kagent.dev for 2.x).
func hopFor(to uint64, kagentAPIVersion string) (*hop, error) {
	switch to {
	case 1:
		return &hop{
			from: 0, to: 1, rewrite: rewriteValues,
			rendered:     schema.GroupVersionResource{Group: "kagent.dev", Version: kagentAPIVersion, Resource: "agenttemplates"},
			renderedKind: "kagent.dev AgentTemplate",
			leftover:     legacyAgentGVR, leftoverKind: "kagent.dev/v1alpha2 Agent",
			crds: RemovedCRDs,
		}, nil
	case 2:
		templates := schema.GroupVersionResource{Group: agents.KagentAPIGroup, Version: kagentAPIVersion, Resource: "agenttemplates"}
		return &hop{
			from: 1, to: 2, rewrite: rewriteValuesV2,
			rendered:     schema.GroupVersionResource{Group: agents.KagentAPIGroup, Version: kagentAPIVersion, Resource: "agents"},
			renderedKind: "api.kagent.dev Agent",
			leftover:     legacyTemplateGVR, leftoverKind: "kagent.dev/v1alpha3 AgentTemplate",
			templates: &templates,
		}, nil
	}
	return nil, fmt.Errorf("no migration path to Generic chart %d.x: migrate moves releases from 0.x to 1.x and from 1.x to 2.x", to)
}

// String names the hop, as the report and the logs carry it.
func (h *hop) String() string { return fmt.Sprintf("%d.x -> %d.x", h.from, h.to) }

// TargetMajor is the one chart major the range admits; the migration path
// follows from it (1: 0.x to 1.x, 2: 1.x to 2.x). A range that admits no
// version or versions of two majors is refused.
func TargetMajor(constraint string) (uint64, error) {
	c, err := semver.NewConstraint(constraint)
	if err != nil {
		return 0, fmt.Errorf("target chart range %q: %w", constraint, err)
	}
	var majors []uint64
	for m := range uint64(100) {
		if admitsMajor(c, m) {
			majors = append(majors, m)
		}
	}
	switch len(majors) {
	case 0:
		return 0, fmt.Errorf("target chart range %q admits no release version", constraint)
	case 1:
		return majors[0], nil
	}
	return 0, fmt.Errorf("target chart range %q spans majors %v: migrate moves releases to exactly one major", constraint, majors)
}

// admitsMajor probes the range with release versions of major m: every
// minor's first and a very late patch, and a very late minor, which covers
// the range forms a chart source carries (x-ranges, carets, tildes, bounds).
func admitsMajor(c *semver.Constraints, m uint64) bool {
	const late = 999999
	if c.Check(semver.New(m, late, late, "", "")) {
		return true
	}
	for n := range uint64(200) {
		if c.Check(semver.New(m, n, 0, "", "")) || c.Check(semver.New(m, n, late, "", "")) {
			return true
		}
	}
	return false
}

// chartMajor is the major of a deployed chart version; ok is false when the
// release has none (never deployed) or it does not parse.
func chartMajor(version string) (uint64, bool) {
	v, err := semver.StrictNewVersion(chartTag(version))
	if err != nil {
		return 0, false
	}
	return v.Major(), true
}
