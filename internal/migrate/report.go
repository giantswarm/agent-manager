package migrate

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"

	"github.com/giantswarm/agent-manager/internal/agents"
)

// The report is the migration's state of one managed namespace as one
// object: a ConfigMap (DefaultReportConfigMap) with the keys
//
//	phase        expand | wait | contract | complete
//	summary      one sentence
//	report.yaml  the Report below
//
// written by every run that is not a dry run; a dry run prints the same
// report and writes nothing. agentlab, the connectivity chart's Job and the
// fleet's cut-over pull requests read it.

// DefaultReportConfigMap is the report's ConfigMap name.
const DefaultReportConfigMap = "agent-manager-migrate-report"

// Report ConfigMap data keys.
const (
	ReportKeyPhase   = "phase"
	ReportKeySummary = "summary"
	ReportKeyReport  = "report.yaml"
)

// The phases a namespace can be in.
const (
	// PhaseExpand: a Generic-chart release still carries the old major's
	// values (pending, failed, or GitOps-owned with its diff not merged), or a
	// chart source is not on the target range yet.
	PhaseExpand = "expand"
	// PhaseWait: every release is on the target major's values and every
	// writable source on the target range; Flux and kagent have not finished
	// (a release still on the old chart, a rendered object not Ready, the
	// target line's resource not served yet).
	PhaseWait = "wait"
	// PhaseContract: the gate passed; the old line's leftover agent objects
	// of the namespace were deleted (0.x -> 1.x: the CRDs too when every
	// namespace passed).
	PhaseContract = "contract"
	// PhaseComplete: nothing of the old line is left that this command
	// handles.
	PhaseComplete = "complete"
)

// Actions on a release.
const (
	ActionRewritten = "rewritten"
	ActionUnchanged = "unchanged"
	ActionDiff      = "diff"
	ActionPending   = "pending"
	ActionFailed    = "failed"
)

// Actions on a chart source.
const (
	SourceMoved    = "moved"
	SourceNotMoved = "not-moved"
)

// Actions on an agent object of the old line.
const (
	AgentAwaitingUpgrade = "awaiting-upgrade"
	AgentNotMigratable   = "not-migratable"
	AgentDeleted         = "deleted"
)

// Ownership of a release or source.
const (
	OwnershipHelmRelease = agents.ManagedHelmRelease
	OwnershipGitOps      = agents.ManagedGitOps
	// OwnershipExternal: the object lives outside the managed namespaces
	// (reached through provenance labels); read-only for this command.
	OwnershipExternal = "external"
)

// Actions on a CRD in the contract phase.
const (
	CRDDeleted = "deleted"
	CRDAbsent  = "absent"
	CRDKept    = "kept"
)

// Report is one namespace's migration state.
type Report struct {
	Namespace string  `json:"namespace"`
	Phase     string  `json:"phase"`
	Summary   string  `json:"summary"`
	Run       RunInfo `json:"run"`
	// Changed is true when this run wrote something in the namespace.
	Changed bool `json:"changed"`
	// Releases are the Generic-chart releases rendering into the namespace.
	Releases []ReleaseReport `json:"releases"`
	// Sources are the OCIRepositories of the agent chart those releases use.
	Sources []SourceReport `json:"sources"`
	// Agents are the old line's agent objects found in the namespace:
	// kagent.dev/v1alpha2 Agents (0.x -> 1.x) or kagent.dev/v1alpha3
	// AgentTemplates (1.x -> 2.x).
	Agents []AgentReport `json:"agents"`
	// Pending names what gates the next phase.
	Pending []string `json:"pending,omitempty"`
	// Warnings are what the wait phase found when nothing gates on it any
	// more (an object not Ready in a namespace that has nothing left to
	// contract).
	Warnings []string `json:"warnings,omitempty"`
	// Contract is what the contract phase did, when it ran.
	Contract *ContractReport `json:"contract,omitempty"`
}

// RunInfo says which run wrote the report.
type RunInfo struct {
	At      string `json:"at"`
	DryRun  bool   `json:"dryRun"`
	Version string `json:"version"`
	Harness string `json:"harness"`
	// Path is the hop the target range picks: "0.x -> 1.x" or "1.x -> 2.x".
	Path  string    `json:"path"`
	Chart ChartInfo `json:"chart"`
}

// ChartInfo is the target chart as the run saw it.
type ChartInfo struct {
	OCIURL string `json:"ociUrl"`
	// TargetSemver is the range every source moves to.
	TargetSemver string `json:"targetSemver"`
	// TargetSemverFilter is the tag filter every source moves to; absent
	// when each source keeps its own.
	TargetSemverFilter *string `json:"targetSemverFilter,omitempty"`
	// LatestVersion is the newest published version in that range, "" when
	// none exists (no source is moved then).
	LatestVersion string `json:"latestVersion,omitempty"`
	// SchemaVersion / SchemaSource say which values.schema.json judged the
	// rewrites.
	SchemaVersion string `json:"schemaVersion"`
	SchemaSource  string `json:"schemaSource"`
}

// ReleaseReport is one Generic-chart release.
type ReleaseReport struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// TargetNamespace is where the release renders when it is not its own.
	TargetNamespace string `json:"targetNamespace,omitempty"`
	Ownership       string `json:"ownership"`
	Action          string `json:"action"`
	Reason          string `json:"reason,omitempty"`
	// ChartVersion is the deployed chart version (status.history), else the
	// last attempted revision.
	ChartVersion string        `json:"chartVersion,omitempty"`
	Changes      *ValueChanges `json:"changes,omitempty"`
	// Diff is the rewrite as a unified diff of the manifest (GitOps-owned and
	// external releases; never written).
	Diff string `json:"diff,omitempty"`
	// Warnings are what the rewrite cannot settle and the write does not
	// wait for (a sub-agent binding to a template that does not exist).
	Warnings []string `json:"warnings,omitempty"`
	// Template is the object the release renders on the target line (a
	// kagent.dev AgentTemplate on 1.x, an api.kagent.dev Agent on 2.x), as
	// the wait phase saw it.
	Template *TemplateReport `json:"template,omitempty"`
}

// TemplateReport is the verdict on the object a release renders.
type TemplateReport struct {
	Name    string `json:"name"`
	Exists  bool   `json:"exists"`
	Verdict string `json:"verdict,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// SourceReport is one OCIRepository of the agent chart.
type SourceReport struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Ownership string `json:"ownership"`
	Action    string `json:"action"`
	From      string `json:"from,omitempty"`
	To        string `json:"to,omitempty"`
	// FromSemverFilter and ToSemverFilter are the source's ref.semverFilter
	// before and after, beside the range.
	FromSemverFilter string `json:"fromSemverFilter,omitempty"`
	ToSemverFilter   string `json:"toSemverFilter,omitempty"`
	Reason           string `json:"reason,omitempty"`
	Diff             string `json:"diff,omitempty"`
}

// AgentReport is one agent object of the old line.
type AgentReport struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// Kind is the object's API and kind, such as kagent.dev/v1alpha2 Agent.
	Kind string `json:"kind"`
	// Owner is the HelmRelease the provenance labels name (namespace/name).
	Owner  string `json:"owner,omitempty"`
	Action string `json:"action"`
	Reason string `json:"reason,omitempty"`
}

// ContractReport is what the contract phase did.
type ContractReport struct {
	// AgentsDeleted are the old line's agent objects removed from the
	// namespace.
	AgentsDeleted []string `json:"agentsDeleted"`
	// CRDs is the fate of the five CRDs the 0.x -> 1.x hop removes
	// (cluster-scoped, so the same in every namespace's report); absent on
	// 1.x -> 2.x, where the platform removes the kagent.dev CRDs.
	CRDs []CRDReport `json:"crds,omitempty"`
}

// CRDReport is one removed CRD.
type CRDReport struct {
	Name   string `json:"name"`
	Action string `json:"action"`
	Reason string `json:"reason,omitempty"`
}

// YAML renders the report.
func (r *Report) YAML() string {
	out, err := yaml.Marshal(r)
	if err != nil {
		return fmt.Sprintf("# marshal error: %v\n", err)
	}
	return string(out)
}

// String is the report with a header line, for stdout.
func (r *Report) String() string {
	mode := ""
	if r.Run.DryRun {
		mode = " (dry run: nothing was written)"
	}
	return fmt.Sprintf("# namespace %s: phase %s%s\n# %s\n%s", r.Namespace, r.Phase, mode, r.Summary, r.YAML())
}

// writeReport creates or updates the report ConfigMap of the namespace.
func writeReport(ctx context.Context, typed kubernetes.Interface, name string, r *Report) error {
	data := map[string]string{ReportKeyPhase: r.Phase, ReportKeySummary: r.Summary, ReportKeyReport: r.YAML()}
	cms := typed.CoreV1().ConfigMaps(r.Namespace)
	existing, err := cms.Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.Namespace, Labels: map[string]string{agents.ManagedByLabel: agents.ManagedByValue}},
			Data:       data,
		}
		_, err = cms.Create(ctx, cm, metav1.CreateOptions{FieldManager: agents.FieldManager})
	case err == nil:
		existing.Data = data
		if existing.Labels == nil {
			existing.Labels = map[string]string{}
		}
		existing.Labels[agents.ManagedByLabel] = agents.ManagedByValue
		_, err = cms.Update(ctx, existing, metav1.UpdateOptions{FieldManager: agents.FieldManager})
	}
	if err != nil {
		return fmt.Errorf("write report ConfigMap %s/%s: %w", r.Namespace, name, err)
	}
	return nil
}

// join renders a list for a summary sentence.
func join(items []string) string { return strings.Join(items, ", ") }
