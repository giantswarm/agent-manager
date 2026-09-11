// Package migrate is the `agent-manager migrate` command: the expand–contract
// migration of an installation's agents from Generic chart 0.x releases
// (kagent.dev/v1alpha2 Agent) to Generic chart 1.x releases
// (kagent.dev/v1alpha3 AgentTemplate). It runs once per installation as a
// Job of the connectivity chart and by hand with a kubeconfig; every phase is
// idempotent and gated on the previous one, so re-running is always safe:
//
//   - expand: every Generic-chart release rendering into a managed namespace
//     gets 1.x values (removed keys dropped, muster.toolNames renamed, skills
//     pinned to commits and digests, validated against the 1.x schema before
//     anything is written); a GitOps-owned or external release is never
//     written — its rewrite is emitted as a diff for a pull request; the
//     namespace's agent-chart OCIRepository moves to the target range last,
//     only when every release it serves is on 1.x values and the registry has
//     a version in that range.
//   - wait: the contract runs only when every release is deployed from a
//     1.x chart, every AgentTemplate of the namespace is Ready on the
//     platform Harness and no v1alpha2 Agent is still rendered by a release
//     Flux has yet to upgrade; until then the report names what is pending.
//   - contract: the leftover v1alpha2 Agent objects of the managed namespaces
//     are deleted, then the five CRDs the v2 chart no longer ships.
//
// Every run records a report per managed namespace (ConfigMap
// agent-manager-migrate-report); --dry-run prints it and writes nothing.
package migrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/kube"
	"github.com/giantswarm/agent-manager/internal/skills"
)

// RemovedCRDs are the kagent.dev CRDs the API v2 chart no longer ships and
// Helm never deletes (helm.sh/resource-policy: keep); the contract phase does.
var RemovedCRDs = []string{"agents.kagent.dev", "sandboxagents.kagent.dev", "agentharnesses.kagent.dev", "memories.kagent.dev", "toolservers.kagent.dev"}

// The API of the objects the 0.x chart rendered.
var (
	legacyAgentGVR = schema.GroupVersionResource{Group: "kagent.dev", Version: "v1alpha2", Resource: "agents"}
	crdGVR         = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
)

// Options configures a run.
type Options struct {
	// Namespaces are the managed namespaces (the default one first): the
	// releases rendering into them are migrated, their reports written there.
	Namespaces []string
	// GitOpsNamespaces are searched for Generic-chart HelmReleases with a
	// targetNamespace in Namespaces, next to what the provenance labels name.
	GitOpsNamespaces []string
	// HarnessName is the platform Harness whose status entry decides a
	// template's readiness; composed as agent.harness when it is not the
	// chart default.
	HarnessName string
	// ChartOCIURL identifies the agent chart's sources; TargetSemver is the
	// range they move to (1.x).
	ChartOCIURL  string
	TargetSemver string
	// ReportConfigMap names the report per namespace.
	ReportConfigMap string
	// DryRun prints the report and writes nothing.
	DryRun bool
	// KagentAPIVersion serves agenttemplates (v1alpha3).
	KagentAPIVersion string
	// HelmReleaseAPIVersion / OCIRepositoryAPIVersion are the served Flux APIs.
	HelmReleaseAPIVersion   string
	OCIRepositoryAPIVersion string
	// Version is the agent-manager version recorded in the report.
	Version string
}

// StatusReader is the platform Harness's verdict on a template — the
// service's get_agent_status (agents.Service).
type StatusReader interface {
	Status(ctx context.Context, ns, name string) (*agents.Status, error)
}

// Runner runs the migration.
type Runner struct {
	client kube.Client
	chart  agents.ChartSource
	pinner agents.SkillPinner
	status StatusReader
	opts   Options
	log    *slog.Logger
	now    func() time.Time
}

// New builds a runner.
func New(client kube.Client, c agents.ChartSource, p agents.SkillPinner, st StatusReader, opts Options, log *slog.Logger) *Runner {
	if log == nil {
		log = slog.Default()
	}
	opts.HarnessName = orDefault(opts.HarnessName, agents.DefaultHarnessName)
	opts.ChartOCIURL = orDefault(opts.ChartOCIURL, c.OCIURL())
	opts.TargetSemver = orDefault(opts.TargetSemver, c.SemverRange())
	opts.ReportConfigMap = orDefault(opts.ReportConfigMap, DefaultReportConfigMap)
	opts.KagentAPIVersion = orDefault(opts.KagentAPIVersion, agents.DefaultKagentAPIVersion)
	opts.HelmReleaseAPIVersion = orDefault(opts.HelmReleaseAPIVersion, agents.DefaultHelmReleaseAPIVersion)
	opts.OCIRepositoryAPIVersion = orDefault(opts.OCIRepositoryAPIVersion, agents.DefaultOCIRepositoryAPIVersion)
	return &Runner{client: client, chart: c, pinner: p, status: st, opts: opts, log: log, now: time.Now}
}

// Result is one run: a report per managed namespace.
type Result struct {
	Reports []*Report
}

// Run performs the phases the cluster's state admits and writes the reports.
// The error is for what needs an operator (API failures, a write refused);
// everything the migration itself decides is in the reports.
func (r *Runner) Run(ctx context.Context) (*Result, error) {
	info := r.chart.Info(ctx)
	sch := r.chart.Schema(ctx)
	run := RunInfo{At: r.now().UTC().Format(time.RFC3339), DryRun: r.opts.DryRun, Version: r.opts.Version, Harness: r.opts.HarnessName,
		Chart: ChartInfo{OCIURL: r.opts.ChartOCIURL, TargetSemver: r.opts.TargetSemver, LatestVersion: info.LatestVersion, SchemaVersion: sch.Version, SchemaSource: sch.Source}}
	chartAvailable := info.LatestVersion != ""
	if !chartAvailable {
		r.log.Warn("the registry has no chart version in the target range; no source is moved and no writable release is rewritten this run", "chart", r.opts.ChartOCIURL, "range", r.opts.TargetSemver, "error", info.Error)
	}

	var (
		states []*nsState
		errs   []error
	)
	for _, ns := range r.opts.Namespaces {
		st, err := r.discover(ctx, ns)
		if err != nil {
			return nil, err
		}
		st.report.Run = run
		r.expand(ctx, st, chartAvailable)
		r.wait(ctx, st)
		states = append(states, st)
	}
	r.contract(ctx, states)

	res := &Result{}
	for _, st := range states {
		st.finish()
		res.Reports = append(res.Reports, st.report)
		errs = append(errs, st.errs...)
		if r.opts.DryRun {
			continue
		}
		if err := writeReport(ctx, r.client.Typed(), r.opts.ReportConfigMap, st.report); err != nil {
			errs = append(errs, err)
		}
	}
	return res, errors.Join(errs...)
}

// ---- state --------------------------------------------------------------------

type nsState struct {
	ns        string
	report    *Report
	sources   []*source
	releases  []*release
	agents    []*legacyAgent
	templates []*unstructured.Unstructured
	// expandDone: every release rendering into ns is on 1.x values and every
	// writable source on the target range.
	expandDone bool
	// gatePassed: expandDone and Flux and kagent have caught up.
	gatePassed bool
	// crdsPresent: any of RemovedCRDs existed when the run looked.
	crdsPresent bool
	errs        []error
}

type source struct {
	obj              *unstructured.Unstructured
	ns, name, semver string
	gitops, external bool
	report           *SourceReport
	releases         []*release
}

type release struct {
	obj              *unstructured.Unstructured
	ns, name         string
	source           *source
	gitops, external bool
	report           *ReleaseReport
	// onTarget: the values are 1.x after this run.
	onTarget bool
}

type legacyAgent struct {
	obj    *unstructured.Unstructured
	report *AgentReport
	// owner is the Generic-chart release rendering it, nil when none does.
	owner *release
}

func (st *nsState) fail(err error) { st.errs = append(st.errs, err) }

// templateName is the AgentTemplate a release renders: agent.name, else the
// Helm release name, else the object's name.
func (rel *release) templateName() string {
	values, _, _ := unstructured.NestedMap(rel.obj.Object, "spec", "values")
	if agent, _ := values["agent"].(map[string]any); agent != nil {
		if name, _ := agent["name"].(string); name != "" {
			return name
		}
	}
	if name, _, _ := unstructured.NestedString(rel.obj.Object, "spec", "releaseName"); name != "" {
		return name
	}
	return rel.name
}

func (rel *release) ownership() string {
	switch {
	case rel.external:
		return OwnershipExternal
	case rel.gitops:
		return OwnershipGitOps
	}
	return OwnershipHelmRelease
}

func (rel *release) id() string { return rel.ns + "/" + rel.name }

// ---- discovery ----------------------------------------------------------------

func (r *Runner) helmReleaseGVR() schema.GroupVersionResource {
	return gvrFor(r.opts.HelmReleaseAPIVersion, "helmreleases")
}

func (r *Runner) ociRepositoryGVR() schema.GroupVersionResource {
	return gvrFor(r.opts.OCIRepositoryAPIVersion, "ocirepositories")
}

func (r *Runner) templateGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "kagent.dev", Version: r.opts.KagentAPIVersion, Resource: "agenttemplates"}
}

func gvrFor(apiVersion, resource string) schema.GroupVersionResource {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		gv = schema.GroupVersion{Version: apiVersion}
	}
	return gv.WithResource(resource)
}

func (r *Runner) managed(ns string) bool {
	for _, m := range r.opts.Namespaces {
		if m == ns {
			return true
		}
	}
	return false
}

// discover reads everything the phases decide on: the agent-chart sources
// and the Generic-chart releases of the namespace, the v1alpha2 Agents and
// v1alpha3 AgentTemplates in it, and the releases outside it that render
// into it (reached through provenance labels and the GitOps namespaces).
func (r *Runner) discover(ctx context.Context, ns string) (*nsState, error) {
	dyn := r.client.Dynamic()
	st := &nsState{ns: ns, report: &Report{Namespace: ns, Releases: []ReleaseReport{}, Sources: []SourceReport{}, Agents: []AgentReport{}}}

	sources := map[string]*source{}
	ociList, err := dyn.Resource(r.ociRepositoryGVR()).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list ocirepositories in %s: %w", ns, err)
	}
	for i := range ociList.Items {
		if s := r.sourceOf(&ociList.Items[i], false); s != nil {
			sources[s.name] = s
			st.sources = append(st.sources, s)
		}
	}
	hrList, err := dyn.Resource(r.helmReleaseGVR()).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list helmreleases in %s: %w", ns, err)
	}
	for i := range hrList.Items {
		hr := &hrList.Items[i]
		ref := chartRefOf(hr)
		if ref == nil || ref.Kind != "OCIRepository" || (ref.Namespace != "" && ref.Namespace != ns) {
			continue
		}
		src, ok := sources[ref.Name]
		if !ok {
			continue
		}
		st.addRelease(hr, src, false)
	}

	agentList, err := dyn.Resource(legacyAgentGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	switch {
	case apierrors.IsNotFound(err):
		// The v1alpha2 CRD is gone: nothing left of the old world here.
	case err != nil:
		return nil, fmt.Errorf("list kagent.dev/v1alpha2 agents in %s: %w", ns, err)
	default:
		for i := range agentList.Items {
			st.agents = append(st.agents, &legacyAgent{obj: &agentList.Items[i]})
		}
	}
	tplList, err := dyn.Resource(r.templateGVR()).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list agenttemplates in %s: %w", ns, err)
	}
	for i := range tplList.Items {
		st.templates = append(st.templates, &tplList.Items[i])
	}

	// Releases outside the namespace that render into it.
	owners := map[string]bool{}
	for _, a := range st.agents {
		if name, hrNs := ownerOf(a.obj); name != "" {
			owners[orDefault(hrNs, ns)+"/"+name] = true
		}
	}
	for _, tpl := range st.templates {
		if name, hrNs := ownerOf(tpl); name != "" {
			owners[orDefault(hrNs, ns)+"/"+name] = true
		}
	}
	for _, gitopsNs := range r.opts.GitOpsNamespaces {
		list, err := dyn.Resource(r.helmReleaseGVR()).Namespace(gitopsNs).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list helmreleases in GitOps namespace %s: %w", gitopsNs, err)
		}
		for i := range list.Items {
			if target, _, _ := unstructured.NestedString(list.Items[i].Object, "spec", "targetNamespace"); target == ns {
				owners[gitopsNs+"/"+list.Items[i].GetName()] = true
			}
		}
	}
	external := map[string]*source{}
	for _, id := range sortedKeys(owners) {
		hrNs, hrName, _ := strings.Cut(id, "/")
		if hrNs == ns || r.managed(hrNs) {
			continue // its own namespace's iteration handles it
		}
		if err := r.discoverExternal(ctx, st, hrNs, hrName, external); err != nil {
			return nil, err
		}
	}
	for _, s := range external {
		st.sources = append(st.sources, s)
	}

	sort.Slice(st.releases, func(i, j int) bool { return st.releases[i].id() < st.releases[j].id() })
	sort.Slice(st.sources, func(i, j int) bool {
		return st.sources[i].ns+"/"+st.sources[i].name < st.sources[j].ns+"/"+st.sources[j].name
	})
	r.classifyAgents(st)
	return st, nil
}

// sourceOf reads an OCIRepository of the agent chart; nil for another chart.
func (r *Runner) sourceOf(obj *unstructured.Unstructured, external bool) *source {
	url, _, _ := unstructured.NestedString(obj.Object, "spec", "url")
	if url != r.opts.ChartOCIURL {
		return nil
	}
	s := &source{obj: obj, ns: obj.GetNamespace(), name: obj.GetName(), gitops: gitOpsOwned(obj), external: external}
	s.semver, _, _ = unstructured.NestedString(obj.Object, "spec", "ref", "semver")
	return s
}

func (st *nsState) addRelease(hr *unstructured.Unstructured, src *source, external bool) *release {
	rel := &release{obj: hr, ns: hr.GetNamespace(), name: hr.GetName(), source: src, gitops: gitOpsOwned(hr), external: external}
	src.releases = append(src.releases, rel)
	st.releases = append(st.releases, rel)
	return rel
}

// discoverExternal reads a release outside the managed namespaces and its
// source; a release of another chart (a bundled example agent's kagent
// release) or one that cannot be read is not a Generic-chart release and is
// left to the Agent classification.
func (r *Runner) discoverExternal(ctx context.Context, st *nsState, hrNs, hrName string, sources map[string]*source) error {
	dyn := r.client.Dynamic()
	hr, err := dyn.Resource(r.helmReleaseGVR()).Namespace(hrNs).Get(ctx, hrName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		r.log.Debug("owning HelmRelease not readable", "namespace", hrNs, "name", hrName, "error", err)
		return nil
	}
	if err != nil {
		return fmt.Errorf("get helmrelease %s/%s: %w", hrNs, hrName, err)
	}
	ref := chartRefOf(hr)
	if ref == nil || ref.Kind != "OCIRepository" {
		return nil
	}
	srcNs := orDefault(ref.Namespace, hrNs)
	src, seen := sources[srcNs+"/"+ref.Name]
	if !seen {
		obj, err := dyn.Resource(r.ociRepositoryGVR()).Namespace(srcNs).Get(ctx, ref.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
			r.log.Debug("chart source of the owning HelmRelease not readable", "namespace", srcNs, "name", ref.Name, "error", err)
			return nil
		}
		if err != nil {
			return fmt.Errorf("get ocirepository %s/%s: %w", srcNs, ref.Name, err)
		}
		if src = r.sourceOf(obj, true); src == nil {
			return nil // another chart
		}
		sources[srcNs+"/"+ref.Name] = src
	}
	st.addRelease(hr, src, true)
	return nil
}

// classifyAgents says, for every v1alpha2 Agent, whether a Generic-chart
// release renders it (Helm removes it when the release upgrades) or whether
// only the contract phase can remove it.
func (r *Runner) classifyAgents(st *nsState) {
	byID := map[string]*release{}
	for _, rel := range st.releases {
		byID[rel.id()] = rel
	}
	sort.Slice(st.agents, func(i, j int) bool { return st.agents[i].obj.GetName() < st.agents[j].obj.GetName() })
	for _, a := range st.agents {
		rep := &AgentReport{Name: a.obj.GetName(), Namespace: st.ns}
		name, hrNs := ownerOf(a.obj)
		if name == "" {
			rep.Action, rep.Reason = AgentNotMigratable, "no Flux provenance labels: not rendered by a HelmRelease; only the contract phase removes it"
			a.report = rep
			continue
		}
		rep.Owner = orDefault(hrNs, st.ns) + "/" + name
		if owner, ok := byID[rep.Owner]; ok {
			a.owner = owner
			rep.Action, rep.Reason = AgentAwaitingUpgrade, fmt.Sprintf("rendered by Generic-chart release %s: Helm replaces it with the AgentTemplate when the release upgrades to chart %s; the contract phase sweeps what is left", rep.Owner, r.opts.TargetSemver)
		} else {
			rep.Action, rep.Reason = AgentNotMigratable, fmt.Sprintf("rendered by HelmRelease %s, which is not a Generic-chart release (a bundled example agent of the kagent chart, or a chart this command does not know): nothing to rewrite; only the contract phase removes it", rep.Owner)
		}
		a.report = rep
	}
}

// ---- expand -------------------------------------------------------------------

// expand rewrites every release's values (writing the writable ones when the
// target chart exists), then moves the writable sources whose releases are
// all on 1.x values.
func (r *Runner) expand(ctx context.Context, st *nsState, chartAvailable bool) {
	dyn := r.client.Dynamic()
	for _, rel := range st.releases {
		rel.report = r.rewriteRelease(ctx, st, dyn, rel, chartAvailable)
	}
	for _, src := range st.sources {
		src.report = r.moveSource(ctx, st, dyn, src, chartAvailable)
	}
}

func (r *Runner) rewriteRelease(ctx context.Context, st *nsState, dyn dynamic.Interface, rel *release, chartAvailable bool) *ReleaseReport {
	rep := &ReleaseReport{Name: rel.name, Namespace: rel.ns, Ownership: rel.ownership(), ChartVersion: deployedChartVersion(rel.obj)}
	if target, _, _ := unstructured.NestedString(rel.obj.Object, "spec", "targetNamespace"); target != "" && target != rel.ns {
		rep.TargetNamespace = target
	}
	values, _, _ := unstructured.NestedMap(rel.obj.Object, "spec", "values")
	if values == nil {
		values = map[string]any{}
	}
	rewritten, changes, err := rewriteValues(ctx, values, r.opts.HarnessName, r.pinner)
	if err != nil {
		rep.Action, rep.Reason = ActionFailed, err.Error()
		if errors.Is(err, skills.ErrUnresolvable) {
			rep.Action, rep.Reason = ActionPending, "a skill reference could not be resolved, the release is left as it is: "+err.Error()
		}
		return rep
	}
	rewrittenHR := &unstructured.Unstructured{Object: runtime.DeepCopyJSON(rel.obj.Object)}
	_ = unstructured.SetNestedMap(rewrittenHR.Object, rewritten, "spec", "values")
	changes.DriftIgnoreRemoved = removeDriftIgnore(rewrittenHR)
	if changes.Empty() && reflect.DeepEqual(values, rewritten) {
		rep.Action, rep.Reason = ActionUnchanged, "already on the 1.x values"
		rel.onTarget = true
		return rep
	}
	rep.Changes = changes
	sch, violations := agents.ValidateValues(ctx, r.chart, rewritten)
	if len(violations) > 0 {
		rep.Action, rep.Reason = ActionFailed, fmt.Sprintf("the rewritten values do not satisfy the agent chart schema %s (%s): %s; the release is left as it is", sch.Version, sch.Source, strings.Join(violations, "; "))
		return rep
	}
	if rel.gitops || rel.external {
		rep.Action = ActionDiff
		rep.Diff = manifestDiff(rel.obj, rewrittenHR)
		rep.Reason = "never written by this command: its desired state lives in git — apply the diff in the owning repository"
		if rel.gitops && !rel.external {
			rep.Reason = fmt.Sprintf("applied by Flux Kustomization %q, never written by this command: apply the diff in the owning repository", rel.obj.GetLabels()[agents.KustomizationNameLabel])
		}
		return rep
	}
	if !chartAvailable {
		rep.Action, rep.Reason = ActionPending, fmt.Sprintf("the registry has no version of the chart in %s yet; the values are rewritten once it exists (1.x values under a 0.x chart would fail the render)", r.opts.TargetSemver)
		return rep
	}
	rep.Action = ActionRewritten
	rel.onTarget = true
	if r.opts.DryRun {
		rep.Reason = "dry run: would be written"
		return rep
	}
	if _, err := dyn.Resource(r.helmReleaseGVR()).Namespace(rel.ns).Update(ctx, rewrittenHR, metav1.UpdateOptions{FieldManager: agents.FieldManager}); err != nil {
		rel.onTarget = false
		rep.Action, rep.Reason = ActionFailed, fmt.Sprintf("update refused: %v", err)
		st.fail(fmt.Errorf("update HelmRelease %s: %w", rel.id(), err))
		return rep
	}
	st.report.Changed = true
	r.log.Info("release rewritten to 1.x values", "release", rel.id(), "removed", changes.Removed, "renamed", changes.Renamed, "skills", len(changes.Skills))
	return rep
}

// moveSource moves one agent-chart OCIRepository to the target range: last,
// only when every release it serves is on 1.x values and the registry has a
// version in the range; a GitOps-owned or external source gets the diff.
func (r *Runner) moveSource(ctx context.Context, st *nsState, dyn dynamic.Interface, src *source, chartAvailable bool) *SourceReport {
	rep := &SourceReport{Name: src.name, Namespace: src.ns, From: src.semver, To: r.opts.TargetSemver}
	switch {
	case src.external:
		rep.Ownership = OwnershipExternal
	case src.gitops:
		rep.Ownership = OwnershipGitOps
	default:
		rep.Ownership = OwnershipHelmRelease
	}
	if src.semver == r.opts.TargetSemver {
		rep.Action, rep.Reason = ActionUnchanged, "already on the target range"
		return rep
	}
	moved := &unstructured.Unstructured{Object: runtime.DeepCopyJSON(src.obj.Object)}
	_ = unstructured.SetNestedField(moved.Object, r.opts.TargetSemver, "spec", "ref", "semver")
	if src.gitops || src.external {
		rep.Action, rep.Diff = ActionDiff, manifestDiff(src.obj, moved)
		rep.Reason = "never written by this command: move the range in the owning repository together with its releases' values"
		return rep
	}
	if src.semver == "" {
		rep.Action, rep.Reason = SourceNotMoved, "spec.ref carries no semver range (a tag or digest pin); move it by hand"
		return rep
	}
	if !chartAvailable {
		rep.Action, rep.Reason = SourceNotMoved, fmt.Sprintf("the registry has no version of the chart in %s yet", r.opts.TargetSemver)
		return rep
	}
	var blockers []string
	for _, rel := range src.releases {
		if !rel.onTarget {
			blockers = append(blockers, fmt.Sprintf("%s (%s)", rel.id(), rel.report.Action))
		}
	}
	if len(blockers) > 0 {
		rep.Action, rep.Reason = SourceNotMoved, "releases not on 1.x values yet: "+join(blockers)
		return rep
	}
	rep.Action = SourceMoved
	if r.opts.DryRun {
		rep.Reason = "dry run: would be moved"
		return rep
	}
	if _, err := dyn.Resource(r.ociRepositoryGVR()).Namespace(src.ns).Update(ctx, moved, metav1.UpdateOptions{FieldManager: agents.FieldManager}); err != nil {
		rep.Action, rep.Reason = SourceNotMoved, fmt.Sprintf("update refused: %v", err)
		st.fail(fmt.Errorf("update OCIRepository %s/%s: %w", src.ns, src.name, err))
		return rep
	}
	src.semver = r.opts.TargetSemver
	st.report.Changed = true
	r.log.Info("chart source moved", "source", src.ns+"/"+src.name, "from", rep.From, "to", rep.To)
	return rep
}

// ---- wait ---------------------------------------------------------------------

// wait decides whether the namespace may contract and names what is pending.
func (r *Runner) wait(ctx context.Context, st *nsState) {
	var pending []string
	for _, rel := range st.releases {
		switch rel.report.Action {
		case ActionPending, ActionFailed:
			pending = append(pending, fmt.Sprintf("release %s: %s", rel.id(), rel.report.Reason))
		case ActionDiff:
			pending = append(pending, fmt.Sprintf("release %s: its rewrite (the diff in this report) is not applied in the owning repository yet", rel.id()))
		}
	}
	for _, src := range st.sources {
		if src.report.Action == SourceNotMoved || (src.report.Action == ActionDiff && !src.external) {
			pending = append(pending, fmt.Sprintf("chart source %s/%s: %s", src.ns, src.name, src.report.Reason))
		}
	}
	st.expandDone = len(pending) == 0
	if !st.expandDone {
		st.report.Pending = pending
		return
	}
	// Expand is done: is the platform caught up?
	for _, rel := range st.releases {
		if !r.releaseUpgraded(rel) {
			pending = append(pending, fmt.Sprintf("release %s is deployed from chart %q; waiting for Flux to upgrade it to %s%s", rel.id(), rel.report.ChartVersion, r.opts.TargetSemver, suspendedNote(rel.obj)))
			continue
		}
		tplName := rel.templateName()
		rel.report.Template = &TemplateReport{Name: tplName}
		if !hasTemplate(st.templates, tplName) {
			pending = append(pending, fmt.Sprintf("release %s is on chart %s but has not rendered AgentTemplate %s/%s yet", rel.id(), rel.report.ChartVersion, st.ns, tplName))
		}
	}
	for _, tpl := range st.templates {
		rep := r.templateVerdict(ctx, st.ns, tpl.GetName())
		for _, rel := range st.releases {
			if rel.report.Template != nil && rel.report.Template.Name == tpl.GetName() {
				rel.report.Template = rep
			}
		}
		if rep.Verdict != agents.VerdictReady {
			pending = append(pending, fmt.Sprintf("AgentTemplate %s/%s is %s: %s", st.ns, tpl.GetName(), rep.Verdict, rep.Summary))
		}
	}
	for _, a := range st.agents {
		if a.owner != nil {
			pending = append(pending, fmt.Sprintf("kagent.dev/v1alpha2 Agent %s/%s is still rendered by release %s; Helm removes it when the release upgrades to chart %s", st.ns, a.obj.GetName(), a.owner.id(), r.opts.TargetSemver))
		}
	}
	st.report.Pending = pending
	st.gatePassed = len(pending) == 0
}

func (r *Runner) templateVerdict(ctx context.Context, ns, name string) *TemplateReport {
	rep := &TemplateReport{Name: name, Exists: true}
	status, err := r.status.Status(ctx, ns, name)
	if err != nil {
		rep.Verdict, rep.Summary = agents.VerdictUnknown, "status could not be read: "+err.Error()
		return rep
	}
	rep.Verdict, rep.Summary = status.Verdict, status.Summary
	return rep
}

// releaseUpgraded is true when the release's deployed chart is in the target
// range.
func (r *Runner) releaseUpgraded(rel *release) bool {
	return versionInRange(rel.report.ChartVersion, r.opts.TargetSemver)
}

// versionInRange checks a deployed chart version (Flux may append the OCI
// digest as @sha256:… or build metadata) against the range.
func versionInRange(version, constraint string) bool {
	v, _, _ := strings.Cut(version, "@")
	v, _, _ = strings.Cut(v, "+")
	parsed, err := semver.StrictNewVersion(v)
	if err != nil {
		return false
	}
	c, err := semver.NewConstraint(constraint)
	if err != nil {
		return false
	}
	return c.Check(parsed)
}

// deployedChartVersion is the last deployed chart version (status.history,
// newest first), else the last attempted revision.
func deployedChartVersion(hr *unstructured.Unstructured) string {
	if history, found, _ := unstructured.NestedSlice(hr.Object, "status", "history"); found && len(history) > 0 {
		if entry, ok := history[0].(map[string]any); ok {
			if v, _ := entry["chartVersion"].(string); v != "" {
				return v
			}
		}
	}
	v, _, _ := unstructured.NestedString(hr.Object, "status", "lastAttemptedRevision")
	return v
}

func suspendedNote(hr *unstructured.Unstructured) string {
	if suspended, _, _ := unstructured.NestedBool(hr.Object, "spec", "suspend"); suspended {
		return " (the release is suspended: Flux will not act until it is resumed)"
	}
	return ""
}

func hasTemplate(templates []*unstructured.Unstructured, name string) bool {
	for _, tpl := range templates {
		if tpl.GetName() == name {
			return true
		}
	}
	return false
}

// ---- contract -----------------------------------------------------------------

// contract deletes the leftover v1alpha2 Agents of every namespace whose gate
// passed and, once every namespace passed, the removed CRDs. Without a
// passed gate it only records whether the CRDs still exist.
func (r *Runner) contract(ctx context.Context, states []*nsState) {
	dyn := r.client.Dynamic()
	allPassed := true
	for _, st := range states {
		if !st.gatePassed {
			allPassed = false
		}
	}
	crds := r.contractCRDs(ctx, dyn, states, allPassed)
	for _, st := range states {
		st.crdsPresent = crdsPresent(crds)
		if !st.gatePassed {
			continue
		}
		rep := &ContractReport{AgentsDeleted: []string{}, CRDs: crds}
		for _, a := range st.agents {
			name := a.obj.GetName()
			if !r.opts.DryRun {
				if err := dyn.Resource(legacyAgentGVR).Namespace(st.ns).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
					a.report.Reason = "delete refused: " + err.Error()
					st.fail(fmt.Errorf("delete kagent.dev/v1alpha2 Agent %s/%s: %w", st.ns, name, err))
					continue
				}
				st.report.Changed = true
			}
			a.report.Action = AgentDeleted
			a.report.Reason = "leftover of the 0.x chart, deleted by the contract phase"
			if r.opts.DryRun {
				a.report.Reason += " (dry run: would be deleted)"
			}
			rep.AgentsDeleted = append(rep.AgentsDeleted, name)
		}
		st.report.Contract = rep
	}
}

// contractCRDs deletes the removed CRDs when allPassed (and no v1alpha2 Agent
// is left in a namespace this run does not manage), else reports their state.
func (r *Runner) contractCRDs(ctx context.Context, dyn dynamic.Interface, states []*nsState, allPassed bool) []CRDReport {
	var (
		out    []CRDReport
		keep   string
		wrote  bool
		before = map[string]bool{}
	)
	if allPassed {
		keep = r.foreignAgents(ctx, dyn)
	}
	for _, name := range RemovedCRDs {
		rep := CRDReport{Name: name}
		_, err := dyn.Resource(crdGVR).Get(ctx, name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			rep.Action = CRDAbsent
		case err != nil:
			rep.Action, rep.Reason = CRDKept, "could not be read: "+err.Error()
			for _, st := range states {
				st.fail(fmt.Errorf("get CustomResourceDefinition %s: %w", name, err))
			}
		case !allPassed:
			rep.Action, rep.Reason = CRDKept, "a managed namespace has not passed the wait gate yet"
			before[name] = true
		case keep != "":
			rep.Action, rep.Reason = CRDKept, keep
			before[name] = true
		case r.opts.DryRun:
			rep.Action, rep.Reason = CRDDeleted, "dry run: would be deleted"
			before[name] = true
		default:
			if err := dyn.Resource(crdGVR).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				rep.Action, rep.Reason = CRDKept, "delete refused: "+err.Error()
				for _, st := range states {
					st.fail(fmt.Errorf("delete CustomResourceDefinition %s: %w", name, err))
				}
				before[name] = true
				break
			}
			rep.Action, rep.Reason = CRDDeleted, "removed from the cluster; the objects of that kind went with it"
			wrote = true
			r.log.Info("CRD deleted", "crd", name)
		}
		out = append(out, rep)
	}
	if wrote {
		for _, st := range states {
			st.report.Changed = true
		}
	}
	return out
}

// foreignAgents names the v1alpha2 Agents outside the managed namespaces —
// deleting the CRD would take them along — or "" when there are none or the
// cluster-wide read is not permitted (then the managed namespaces decide).
func (r *Runner) foreignAgents(ctx context.Context, dyn dynamic.Interface) string {
	list, err := dyn.Resource(legacyAgentGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			r.log.Warn("cluster-wide check for kagent.dev/v1alpha2 Agents not possible; the managed namespaces decide", "error", err)
		}
		return ""
	}
	var foreign []string
	for i := range list.Items {
		if !r.managed(list.Items[i].GetNamespace()) {
			foreign = append(foreign, list.Items[i].GetNamespace()+"/"+list.Items[i].GetName())
		}
	}
	if len(foreign) == 0 {
		return ""
	}
	sort.Strings(foreign)
	return fmt.Sprintf("kagent.dev/v1alpha2 Agent objects exist outside the managed namespaces and would vanish with the CRD: %s — manage those namespaces too, or remove the objects first", join(foreign))
}

// crdsPresent is true when a removed CRD still exists after this run.
func crdsPresent(crds []CRDReport) bool {
	for _, c := range crds {
		if c.Action == CRDKept || (c.Action == CRDDeleted && strings.HasPrefix(c.Reason, "dry run")) {
			return true
		}
	}
	return false
}

// ---- report -------------------------------------------------------------------

// finish folds the state into the report: rows, phase and summary.
func (st *nsState) finish() {
	for _, rel := range st.releases {
		st.report.Releases = append(st.report.Releases, *rel.report)
	}
	for _, src := range st.sources {
		st.report.Sources = append(st.report.Sources, *src.report)
	}
	for _, a := range st.agents {
		st.report.Agents = append(st.report.Agents, *a.report)
	}
	st.report.Phase, st.report.Summary = st.phase()
}

func (st *nsState) phase() (string, string) {
	counts := map[string]int{}
	for _, rel := range st.releases {
		counts[rel.report.Action]++
	}
	if !st.expandDone {
		return PhaseExpand, fmt.Sprintf("expand phase: %d release(s) rewritten, %d unchanged, %d with a diff for the owning repository, %d pending, %d failed; %d item(s) gate the next phase",
			counts[ActionRewritten], counts[ActionUnchanged], counts[ActionDiff], counts[ActionPending], counts[ActionFailed], len(st.report.Pending))
	}
	contracted := st.report.Contract != nil && len(st.report.Contract.AgentsDeleted) > 0
	if len(st.agents) == 0 && !contracted && !st.crdsPresent && !crdsDeletedThisRun(st.report.Contract) {
		// Nothing of the 0.x world is left: readiness gates nothing any more,
		// what the wait phase found is informational.
		summary := fmt.Sprintf("nothing left to migrate: %d Generic-chart release(s) on 1.x values, no kagent.dev/v1alpha2 Agent objects, none of the removed CRDs", len(st.releases))
		if len(st.report.Pending) > 0 {
			summary += fmt.Sprintf("; %d item(s) not Ready yet (see warnings)", len(st.report.Pending))
		}
		st.report.Warnings, st.report.Pending = st.report.Pending, nil
		return PhaseComplete, summary
	}
	if !st.gatePassed {
		return PhaseWait, fmt.Sprintf("expand done (%d release(s) on 1.x values); waiting for Flux and kagent: %d item(s) pending before the contract phase", len(st.releases), len(st.report.Pending))
	}
	deleted := 0
	if st.report.Contract != nil {
		for _, c := range st.report.Contract.CRDs {
			if c.Action == CRDDeleted {
				deleted++
			}
		}
	}
	return PhaseContract, fmt.Sprintf("contract phase: %d kagent.dev/v1alpha2 Agent object(s) deleted in the namespace, %d of %d removed CRDs deleted", len(st.report.Contract.AgentsDeleted), deleted, len(RemovedCRDs))
}

func crdsDeletedThisRun(c *ContractReport) bool {
	if c == nil {
		return false
	}
	for _, crd := range c.CRDs {
		if crd.Action == CRDDeleted {
			return true
		}
	}
	return false
}

// ---- helpers --------------------------------------------------------------------

func ownerOf(obj *unstructured.Unstructured) (name, namespace string) {
	labels := obj.GetLabels()
	return labels[agents.HelmReleaseNameLabel], labels[agents.HelmReleaseNamespaceLabel]
}

func gitOpsOwned(obj *unstructured.Unstructured) bool {
	_, ok := obj.GetLabels()[agents.KustomizationNameLabel]
	return ok
}

func chartRefOf(hr *unstructured.Unstructured) *agents.ChartRef {
	ref, found, _ := unstructured.NestedMap(hr.Object, "spec", "chartRef")
	if !found {
		return nil
	}
	out := &agents.ChartRef{}
	out.Kind, _ = ref["kind"].(string)
	out.Name, _ = ref["name"].(string)
	out.Namespace, _ = ref["namespace"].(string)
	return out
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
