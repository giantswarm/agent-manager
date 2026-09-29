package agents

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agent-manager/internal/kube"
)

// errorsIs is errors.Is, named so the service file reads without the import.
func errorsIs(err, target error) bool { return errors.Is(err, target) }

// Readiness is the Agent object's: the controller compiles every Agent for
// the Harness it references and reports Accepted, ResolvedRefs, Compatible,
// then Ready once the golden snapshot exists (a failing stage sets its
// condition False with the reason; desiredRevision moves ahead of
// latestSuccessfulRevision while a new revision compiles). A Harness that
// does not exist is ResolvedRefs False. Harness.status is never written;
// there is no per-agent Deployment or pod.

// The Agent condition types kagent reports, and the Ready reason the
// controller writes while it waits for the golden snapshot.
const (
	conditionReady        = "Ready"
	conditionAccepted     = "Accepted"
	conditionResolvedRefs = "ResolvedRefs"
	conditionCompatible   = "Compatible"
	readyReasonPending    = "ActorTemplatePending"
)

const (
	statusTrue  = string(metav1.ConditionTrue)
	statusFalse = string(metav1.ConditionFalse)
)

// The helm-controller condition and Ready reasons verdict tells apart. A
// release waiting for its chart source or a dependency has Ready=False with
// one of the waiting reasons and is retried by helm-controller; every other
// Ready=False reason (InstallFailed, UpgradeFailed, ArtifactFailed, ...) and
// any Stalled=True is a failure it will not get past without a change.
const (
	conditionStalled         = "Stalled"
	reasonSourceNotReady     = "SourceNotReady"
	reasonDependencyNotReady = "DependencyNotReady"
	reasonProgressing        = "Progressing"
)

func isWaitingReason(reason string) bool {
	switch reason {
	case reasonSourceNotReady, reasonDependencyNotReady, reasonProgressing:
		return true
	}
	return false
}

// Status gathers the Agent object's status, the owning HelmRelease
// (conditions, history) and the namespace's recent Warning events for the
// agent, and folds them into one verdict.
// For an agent on a workload cluster the Agent and its events are read
// there, the HelmRelease and its events on the installation.
func (s *Service) Status(ctx context.Context, loc Location, name string) (*Status, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	st, err := s.site(ctx, loc)
	if err != nil {
		return nil, err
	}
	return s.status(ctx, st, name)
}

func (s *Service) status(ctx context.Context, site *site, name string) (*Status, error) {
	ns := site.ns()
	obj, hr, err := s.agentObjects(ctx, site, name)
	if err != nil {
		return nil, err
	}
	st := &Status{Name: name, Namespace: ns, Target: site.loc.Target, Agent: objectStatusOf(obj), HelmRelease: helmReleaseStatus(hr)}
	st.Events = s.warningEvents(ctx, site.agentClient, ns, name)
	if site.loc.IsSet() {
		st.Events = append(st.Events, s.warningEvents(ctx, site.fluxClient, site.fluxNS, site.releaseName(name))...)
	}
	st.sourceFailure = s.sourceFailure(ctx, site, hr)
	st.Verdict, st.Summary = verdict(st)
	return st, nil
}

// sourceFailure is the chart source's Ready condition when it is False, so a
// release waiting on a source that cannot fetch the chart (authentication,
// no matching tag) reads as failed with the source's own reason. nil when the
// release is not waiting on its source, the source is absent or still
// reconciling, or it cannot be read.
func (s *Service) sourceFailure(ctx context.Context, site *site, hr *unstructured.Unstructured) *Condition {
	if hr == nil {
		return nil
	}
	ready := findCondition(conditionsOfObject(hr), conditionReady)
	if ready == nil || ready.Status != statusFalse || ready.Reason != reasonSourceNotReady {
		return nil
	}
	kind, _, _ := unstructured.NestedString(hr.Object, "spec", "chartRef", "kind")
	name, _, _ := unstructured.NestedString(hr.Object, "spec", "chartRef", "name")
	if kind != kindOCIRepository || name == "" {
		return nil
	}
	ns, _, _ := unstructured.NestedString(hr.Object, "spec", "chartRef", "namespace")
	src, err := s.getObject(ctx, site.flux, s.ociRepositoryGVR(), orDefault(ns, hr.GetNamespace()), name, "ocirepository")
	if err != nil || src == nil {
		return nil
	}
	if c := findCondition(conditionsOfObject(src), conditionReady); c != nil && c.Status == statusFalse {
		return c
	}
	return nil
}

// objectStatusOf reads the Agent object's status.conditions, revisions,
// warnings and generations; Exists is false when the object is absent.
func objectStatusOf(obj *unstructured.Unstructured) *ObjectStatus {
	if obj == nil {
		return &ObjectStatus{}
	}
	os := &ObjectStatus{Exists: true, Generation: obj.GetGeneration()}
	os.Harness, _, _ = unstructured.NestedString(obj.Object, "spec", "harnessRef", "name")
	os.ObservedGeneration, _, _ = unstructured.NestedInt64(obj.Object, "status", "observedGeneration")
	os.DesiredRevision, _, _ = unstructured.NestedString(obj.Object, "status", "desiredRevision")
	os.LatestSuccessfulRevision, _, _ = unstructured.NestedString(obj.Object, "status", "latestSuccessfulRevision")
	os.Conditions = conditionsOfObject(obj)
	os.Ready = conditionStatus(os.Conditions, conditionReady)
	os.Accepted = conditionStatus(os.Conditions, conditionAccepted)
	os.ResolvedRefs = conditionStatus(os.Conditions, conditionResolvedRefs)
	os.Compatible = conditionStatus(os.Conditions, conditionCompatible)
	os.Warnings, _, _ = unstructured.NestedStringSlice(obj.Object, "status", "warnings")
	return os
}

// objectReady is the Agent object's verdict: Ready and the desired revision
// is the latest successful one. nil while unreported.
func objectReady(os *ObjectStatus) *bool {
	if os == nil || !os.Exists || os.Ready == nil {
		return nil
	}
	return boolPtr(*os.Ready && os.DesiredRevision == os.LatestSuccessfulRevision)
}

func helmReleaseStatus(hr *unstructured.Unstructured) *HelmReleaseStatus {
	if hr == nil {
		return &HelmReleaseStatus{Exists: false}
	}
	conds := conditionsOfObject(hr)
	out := &HelmReleaseStatus{Exists: true, Conditions: conds, Ready: conditionStatus(conds, conditionReady), GitOpsOwned: gitOpsOwnedHR(hr), Deleting: hr.GetDeletionTimestamp() != nil}
	out.Suspended, _, _ = unstructured.NestedBool(hr.Object, "spec", "suspend")
	out.LastAttemptedRevision, _, _ = unstructured.NestedString(hr.Object, "status", "lastAttemptedRevision")
	if history, found, _ := unstructured.NestedSlice(hr.Object, "status", "history"); found {
		for i, item := range history {
			if i >= 5 {
				break
			}
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			h := HelmReleaseHistory{}
			switch v := m["version"].(type) {
			case int64:
				h.Version = v
			case float64:
				h.Version = int64(v)
			}
			h.ChartVersion, _ = m["chartVersion"].(string)
			h.Status, _ = m["status"].(string)
			h.LastDeployed, _ = m["lastDeployed"].(string)
			out.History = append(out.History, h)
		}
	}
	return out
}

// warningEvents lists recent Warning events on the agent's objects (the
// Agent, the RemoteMCPServer and the HelmRelease share its name),
// newest first, at most ten. Events are diagnostics: a list failure yields
// none.
func (s *Service) warningEvents(ctx context.Context, client kube.Client, ns, name string) []Event {
	list, err := client.Typed().CoreV1().Events(ns).List(ctx, metav1.ListOptions{FieldSelector: "type=Warning,involvedObject.name=" + name})
	if err != nil {
		s.log.Debug("listing events failed", "namespace", ns, "error", err)
		return nil
	}
	var out []Event
	for i := range list.Items {
		ev := &list.Items[i]
		if ev.Type != corev1.EventTypeWarning || ev.InvolvedObject.Name != name {
			continue
		}
		last := ev.LastTimestamp.Time
		if last.IsZero() {
			last = ev.EventTime.Time
		}
		if last.IsZero() {
			last = ev.CreationTimestamp.Time
		}
		e := Event{Type: ev.Type, Reason: ev.Reason, Message: ev.Message, Object: ev.InvolvedObject.Kind + "/" + ev.InvolvedObject.Name, Count: ev.Count}
		if !last.IsZero() {
			e.Last = last.UTC().Format("2006-01-02T15:04:05Z")
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Last > out[j].Last })
	if len(out) > 10 {
		out = out[:10]
	}
	return out
}

// verdict folds the gathered facts into ready / progressing / failed / unknown
// and one actionable sentence.
func verdict(st *Status) (string, string) {
	hr, os := st.HelmRelease, st.Agent
	if hr != nil && hr.Deleting {
		return VerdictProgressing, "HelmRelease is being uninstalled by helm-controller; the Agent disappears with it"
	}
	if hr != nil && hr.Exists {
		if c := findCondition(hr.Conditions, conditionStalled); c != nil && c.Status == statusTrue {
			return VerdictFailed, "HelmRelease is stalled: " + conditionText(c)
		}
	}
	if hr != nil && hr.Exists && hr.Ready != nil && !*hr.Ready {
		c := findCondition(hr.Conditions, conditionReady)
		if c != nil && isWaitingReason(c.Reason) {
			if c.Reason == reasonSourceNotReady && st.sourceFailure != nil {
				return VerdictFailed, "the chart source is failing: " + conditionText(st.sourceFailure)
			}
			return VerdictProgressing, "HelmRelease is waiting: " + conditionText(c)
		}
		return VerdictFailed, "HelmRelease is not ready: " + conditionText(c)
	}
	if os != nil && os.Exists {
		return objectVerdict(os)
	}
	if hr != nil && hr.Exists {
		if hr.Ready == nil {
			return VerdictProgressing, "HelmRelease created; Flux has not reconciled it yet"
		}
		return VerdictProgressing, "HelmRelease is ready but the Agent has not been rendered yet"
	}
	return VerdictUnknown, "no Agent and no HelmRelease reported anything yet"
}

// objectVerdict reads the Agent object's conditions: a False Accepted,
// ResolvedRefs or Compatible is a failure with the condition's message; Ready
// on the desired revision is ready; everything else is a revision still
// compiling.
func objectVerdict(os *ObjectStatus) (string, string) {
	for _, typ := range []string{conditionAccepted, conditionResolvedRefs, conditionCompatible} {
		if c := findCondition(os.Conditions, typ); c != nil && c.Status == statusFalse {
			return VerdictFailed, fmt.Sprintf("Harness %s: %s is False (%s)", os.Harness, typ, conditionText(c))
		}
	}
	if os.Ready != nil && *os.Ready && os.DesiredRevision == os.LatestSuccessfulRevision {
		summary := fmt.Sprintf("Agent is Ready on Harness %s (revision %s)", os.Harness, os.LatestSuccessfulRevision)
		if n := len(os.Warnings); n > 0 {
			summary += fmt.Sprintf(" with %d warning(s): %s", n, strings.Join(os.Warnings, "; "))
		}
		return VerdictReady, summary
	}
	if os.LatestSuccessfulRevision != "" && os.DesiredRevision != os.LatestSuccessfulRevision {
		return VerdictProgressing, fmt.Sprintf("Harness %s is compiling revision %s; %s is the latest successful one", os.Harness, os.DesiredRevision, os.LatestSuccessfulRevision)
	}
	// Ready not yet True without a failed stage: the first revision is still
	// compiling while the controller waits for the golden snapshot
	// (readyReasonPending); any other False reason is a failure it will not
	// get past on its own.
	if c := findCondition(os.Conditions, conditionReady); c != nil && c.Status != statusTrue {
		if c.Status == statusFalse && c.Reason != readyReasonPending {
			return VerdictFailed, fmt.Sprintf("Harness %s: Ready is False (%s)", os.Harness, conditionText(c))
		}
		return VerdictProgressing, fmt.Sprintf("Harness %s is compiling revision %s; Ready is %s (%s)", os.Harness, os.DesiredRevision, c.Status, conditionText(c))
	}
	switch {
	case os.ObservedGeneration == 0:
		return VerdictProgressing, "kagent has not reported on the Agent yet"
	case os.ObservedGeneration < os.Generation:
		return VerdictProgressing, fmt.Sprintf("kagent has not observed generation %d of the Agent yet (observed %d)", os.Generation, os.ObservedGeneration)
	}
	return VerdictProgressing, fmt.Sprintf("Harness %s is compiling revision %s; Ready not reported yet", os.Harness, os.DesiredRevision)
}

// conditionsOfObject flattens an object's status.conditions.
func conditionsOfObject(obj *unstructured.Unstructured) []Condition {
	raw, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	return conditionsOf(raw)
}

// conditionsOf flattens a conditions list.
func conditionsOf(raw any) []Condition {
	items, _ := raw.([]any)
	if len(items) == 0 {
		return nil
	}
	out := make([]Condition, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		c := Condition{}
		c.Type, _ = m["type"].(string)
		c.Status, _ = m["status"].(string)
		c.Reason, _ = m["reason"].(string)
		c.Message, _ = m["message"].(string)
		c.LastTransitionTime, _ = m["lastTransitionTime"].(string)
		switch g := m["observedGeneration"].(type) {
		case int64:
			c.ObservedGeneration = g
		case float64:
			c.ObservedGeneration = int64(g)
		}
		out = append(out, c)
	}
	return out
}

func findCondition(conds []Condition, typ string) *Condition {
	for i := range conds {
		if conds[i].Type == typ {
			return &conds[i]
		}
	}
	return nil
}

// conditionStatus maps a condition to true/false, nil when absent or Unknown.
func conditionStatus(conds []Condition, typ string) *bool {
	c := findCondition(conds, typ)
	if c == nil {
		return nil
	}
	switch c.Status {
	case statusTrue:
		return boolPtr(true)
	case statusFalse:
		return boolPtr(false)
	}
	return nil
}

func boolPtr(b bool) *bool { return &b }

func conditionText(c *Condition) string {
	if c == nil {
		return "no condition reported"
	}
	if c.Message != "" {
		return c.Reason + ": " + c.Message
	}
	return c.Reason
}
