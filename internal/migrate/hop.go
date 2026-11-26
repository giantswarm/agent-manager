package migrate

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"

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
	// renders; Helm removes it when the release upgrades. Empty when both
	// lines render the same object.
	leftover     schema.GroupVersionResource
	leftoverKind string
	// templates, when set, is where a sub-agent binding's templateRef
	// resolves on the target line.
	templates *schema.GroupVersionResource
	// crds are the CRDs the contract phase deletes once every namespace
	// passed. With emptyCRDsOnly a CRD that still holds an object is kept
	// and named; without it the objects of that kind go with the CRD.
	crds          []string
	emptyCRDsOnly bool
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
			// An installation that jumps from the 0.x line straight to 2.x
			// never ran the 0.x -> 1.x contract: its 0.x CRDs go here, but
			// only the empty ones, as other components may still use them.
			crds: LegacyCRDs, emptyCRDsOnly: true,
		}, nil
	case 3:
		// 2.x and 3.x render the same Agent: no object of the old line is
		// left behind and no CRD goes; only the values move (values_v3.go).
		return &hop{
			from: 2, to: 3, rewrite: rewriteValuesV3,
			rendered:     schema.GroupVersionResource{Group: agents.KagentAPIGroup, Version: kagentAPIVersion, Resource: "agents"},
			renderedKind: "api.kagent.dev Agent",
		}, nil
	}
	return nil, fmt.Errorf("no migration path to Generic chart %d.x: migrate moves releases from 0.x to 1.x, from 1.x to 2.x and from 2.x to 3.x", to)
}

// hasLeftover is true when the source line renders an object the target
// line does not.
func (h *hop) hasLeftover() bool { return h.leftover.Resource != "" }

// String names the hop, as the report and the logs carry it.
func (h *hop) String() string { return fmt.Sprintf("%d.x -> %d.x", h.from, h.to) }

// TargetMajor is the one chart major the range admits; the migration path
// follows from it (1: 0.x to 1.x, 2: 1.x to 2.x, 3: 2.x to 3.x). A range that admits no
// version or versions of two majors is refused. majorsOf reads every
// alternative of a `||` range, as the agent-platform chart's guard reads it
// (agent-platform.agentChart.majors).
func TargetMajor(constraint string) (uint64, error) {
	c, err := semver.NewConstraint(constraint)
	if err != nil {
		return 0, fmt.Errorf("target chart range %q: %w", constraint, err)
	}
	majors := majorsOf(c, constraint)
	switch len(majors) {
	case 0:
		return 0, fmt.Errorf("target chart range %q admits no release version", constraint)
	case 1:
		// A bound like <=3.0.0-rc.1 admits pre-releases of the next major,
		// which Flux would serve under a range read as this one.
		next := majors[0] + 1
		for _, pre := range []string{"0", "alpha.0", "rc.1", "rc.999"} {
			if c.Check(semver.New(next, 0, 0, pre, "")) {
				return 0, fmt.Errorf("target chart range %q admits pre-releases of %d.0.0 (%d.0.0-%s): Flux would serve chart %d.x under it; bound it below %d.0.0-0", constraint, next, next, pre, next, next)
			}
		}
		return majors[0], nil
	}
	return 0, fmt.Errorf("target chart range %q spans majors %v: migrate moves releases to exactly one major", constraint, majors)
}

// versionLiteral is a version a range names (2, 2.6, 2.6.1, v3.0.0-rc.1);
// its pre-release, if any, is not read.
var versionLiteral = regexp.MustCompile(`v?([0-9]+)(?:\.([0-9]+))?(?:\.([0-9]+))?`)

// majorsOf are the release majors c (parsed from constraint) admits, sorted.
// Two kinds of probe, either one counting: per major 0-99 the first and a
// very late version of every minor below 200 and a very late minor, which
// covers the forms a chart source carries (x-ranges, carets, tildes,
// bounds); and every version the range names itself, with the next patch
// and the next minor, which covers exact pins (2.6.1, = 2.6.1), narrow
// bounds (>=2.6.1 <2.6.9, 2.6.1 - 2.6.5), late minors (~2.250) and each
// alternative of a `||` range (3.0.1 || 2.6.0 admits 2 and 3). A probe only
// counts when c admits it, so a probe more never adds a major the range
// does not admit.
func majorsOf(c *semver.Constraints, constraint string) []uint64 {
	var majors []uint64
	add := func(m uint64) {
		if !slices.Contains(majors, m) {
			majors = append(majors, m)
		}
	}
	for _, lit := range versionLiteral.FindAllStringSubmatch(constraint, -1) {
		maj, err1 := strconv.ParseUint(lit[1], 10, 64)
		minor, err2 := parseOrZero(lit[2])
		patch, err3 := parseOrZero(lit[3])
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		for _, v := range []*semver.Version{semver.New(maj, minor, patch, "", ""), semver.New(maj, minor, patch+1, "", ""), semver.New(maj, minor+1, 0, "", "")} {
			if c.Check(v) {
				add(maj)
			}
		}
	}
	for m := range uint64(100) {
		if admitsMajor(c, m) {
			add(m)
		}
	}
	slices.Sort(majors)
	return majors
}

func parseOrZero(s string) (uint64, error) {
	if s == "" {
		return 0, nil
	}
	return strconv.ParseUint(s, 10, 64)
}

// admitsMajor probes the range with release versions of major m: every
// minor's first and a very late patch, and a very late minor.
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
