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
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"

	"github.com/giantswarm/agent-manager/internal/kube"
)

// errorsIs is errors.Is, named so the service file reads without the import.
func errorsIs(err, target error) bool { return errors.Is(err, target) }

// Readiness on kagent API v2 is the template's: the controller compiles every
// AgentTemplate for each Harness whose admission selector matches it and
// reports one status entry per Harness (Accepted, ResolvedRefs, Compatible,
// then Ready once the golden snapshot exists; a failing stage sets its
// condition False with the reason; desiredRevision moves ahead of
// latestSuccessfulRevision while a new revision compiles). The platform runs
// one Harness — the configured one is the verdict's source. Harness.status is
// never written; there is no per-agent Deployment or pod.

// The AgentTemplate condition types kagent reports per Harness, and the Ready
// reason the controller writes while it waits for the golden snapshot.
const (
	conditionReady        = "Ready"
	conditionAccepted     = "Accepted"
	conditionResolvedRefs = "ResolvedRefs"
	conditionCompatible   = "Compatible"
	readyReasonPending    = "ActorTemplatePending"
)

// Status gathers the AgentTemplate's per-Harness status, the owning
// HelmRelease (conditions, history) and the namespace's recent Warning events
// for the agent, and folds them into one verdict.
func (s *Service) Status(ctx context.Context, ns, name string) (*Status, error) {
	ns, err := s.Namespace(ns)
	if err != nil {
		return nil, err
	}
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	dyn, client, err := s.dyn(ctx)
	if err != nil {
		return nil, err
	}
	tpl, err := s.getTemplate(ctx, dyn, ns, name)
	if err != nil {
		return nil, err
	}
	hrName, hrNs := name, ns
	if tpl != nil {
		if n, nsFromLabel := ownerOf(tpl); n != "" {
			hrName, hrNs = n, orDefault(nsFromLabel, ns)
		}
	}
	hr, err := s.getHelmRelease(ctx, dyn, hrNs, hrName)
	if err != nil {
		return nil, err
	}
	if tpl == nil && hr == nil {
		return nil, notFoundf("agent %s/%s: no AgentTemplate and no HelmRelease of that name", ns, name)
	}
	st := &Status{Name: name, Namespace: ns, Template: templateStatusOf(tpl), HelmRelease: helmReleaseStatus(hr)}
	st.Events = s.warningEvents(ctx, client, ns, name)
	st.Verdict, st.Summary = verdict(st, s.cfg.Compose.HarnessName)
	if st.Verdict == VerdictFailed && tpl != nil && len(st.Template.Harnesses) == 0 {
		// Nobody admits the template: say which Harnesses exist and what
		// they admit, so the label mismatch is visible from the answer.
		if described := s.describeHarnesses(ctx, dyn, ns, tpl.GetLabels()); described != "" {
			st.Summary += "; " + described
		}
	}
	return st, nil
}

// templateStatusOf reads status.harnesses[] and the generations; Exists is
// false when the template is absent.
func templateStatusOf(tpl *unstructured.Unstructured) *TemplateStatus {
	if tpl == nil {
		return &TemplateStatus{Harnesses: []HarnessStatus{}}
	}
	ts := &TemplateStatus{Exists: true, Generation: tpl.GetGeneration(), Harnesses: []HarnessStatus{}}
	ts.ObservedGeneration, _, _ = unstructured.NestedInt64(tpl.Object, "status", "observedGeneration")
	entries, _, _ := unstructured.NestedSlice(tpl.Object, "status", "harnesses")
	for _, e := range entries {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		hs := HarnessStatus{}
		hs.Harness, _ = m["harness"].(string)
		hs.DesiredRevision, _ = m["desiredRevision"].(string)
		hs.LatestSuccessfulRevision, _ = m["latestSuccessfulRevision"].(string)
		hs.Conditions = conditionsOf(m["conditions"])
		hs.Ready = conditionStatus(hs.Conditions, conditionReady)
		hs.Accepted = conditionStatus(hs.Conditions, conditionAccepted)
		hs.ResolvedRefs = conditionStatus(hs.Conditions, conditionResolvedRefs)
		hs.Compatible = conditionStatus(hs.Conditions, conditionCompatible)
		warnings, _ := m["warnings"].([]any)
		for _, w := range warnings {
			if str, ok := w.(string); ok {
				hs.Warnings = append(hs.Warnings, str)
			}
		}
		ts.Harnesses = append(ts.Harnesses, hs)
	}
	sort.Slice(ts.Harnesses, func(i, j int) bool { return ts.Harnesses[i].Harness < ts.Harnesses[j].Harness })
	return ts
}

// harnessEntry is the status entry of the named Harness, nil when it has not
// reported.
func harnessEntry(harnesses []HarnessStatus, name string) *HarnessStatus {
	for i := range harnesses {
		if harnesses[i].Harness == name {
			return &harnesses[i]
		}
	}
	return nil
}

// harnessReady is the platform Harness's verdict on a template: Ready and the
// desired revision is the latest successful one. nil while unreported.
func harnessReady(harnesses []HarnessStatus, name string) *bool {
	h := harnessEntry(harnesses, name)
	if h == nil || h.Ready == nil {
		return nil
	}
	return boolPtr(*h.Ready && h.DesiredRevision == h.LatestSuccessfulRevision)
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
// AgentTemplate, the RemoteMCPServer and the HelmRelease share its name),
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
// and one actionable sentence. harness is the platform Harness whose status
// entry decides.
func verdict(st *Status, harness string) (string, string) {
	hr, ts := st.HelmRelease, st.Template
	if hr != nil && hr.Deleting {
		return VerdictProgressing, "HelmRelease is being uninstalled by helm-controller; the AgentTemplate disappears with it"
	}
	if hr != nil && hr.Exists && hr.Ready != nil && !*hr.Ready {
		c := findCondition(hr.Conditions, conditionReady)
		return VerdictFailed, "HelmRelease is not ready: " + conditionText(c)
	}
	if ts != nil && ts.Exists {
		if h := harnessEntry(ts.Harnesses, harness); h != nil {
			return harnessVerdict(h)
		}
		switch {
		case ts.ObservedGeneration == 0:
			return VerdictProgressing, "kagent has not reported on the AgentTemplate yet"
		case ts.ObservedGeneration < ts.Generation:
			return VerdictProgressing, fmt.Sprintf("kagent has not observed generation %d of the AgentTemplate yet (observed %d)", ts.Generation, ts.ObservedGeneration)
		case len(ts.Harnesses) == 0:
			return VerdictFailed, "no Harness admits the AgentTemplate: no Harness of the namespace has an allowedAgentTemplates selector matching its labels"
		}
		names := make([]string, 0, len(ts.Harnesses))
		for _, h := range ts.Harnesses {
			names = append(names, h.Harness)
		}
		return VerdictFailed, fmt.Sprintf("the platform Harness %q does not admit the AgentTemplate; it is admitted by %s only", harness, strings.Join(names, ", "))
	}
	if hr != nil && hr.Exists {
		if hr.Ready == nil {
			return VerdictProgressing, "HelmRelease created; Flux has not reconciled it yet"
		}
		return VerdictProgressing, "HelmRelease is ready but the AgentTemplate has not been rendered yet"
	}
	return VerdictUnknown, "no AgentTemplate and no HelmRelease reported anything yet"
}

// harnessVerdict reads one Harness's entry: a False Accepted, ResolvedRefs or
// Compatible is a failure with the condition's message; Ready on the desired
// revision is ready; everything else is a revision still compiling.
func harnessVerdict(h *HarnessStatus) (string, string) {
	for _, typ := range []string{conditionAccepted, conditionResolvedRefs, conditionCompatible} {
		if c := findCondition(h.Conditions, typ); c != nil && c.Status == "False" {
			return VerdictFailed, fmt.Sprintf("Harness %s: %s is False (%s)", h.Harness, typ, conditionText(c))
		}
	}
	if h.Ready != nil && *h.Ready && h.DesiredRevision == h.LatestSuccessfulRevision {
		summary := fmt.Sprintf("AgentTemplate is Ready on Harness %s (revision %s)", h.Harness, h.LatestSuccessfulRevision)
		if n := len(h.Warnings); n > 0 {
			summary += fmt.Sprintf(" with %d warning(s): %s", n, strings.Join(h.Warnings, "; "))
		}
		return VerdictReady, summary
	}
	if h.LatestSuccessfulRevision != "" && h.DesiredRevision != h.LatestSuccessfulRevision {
		return VerdictProgressing, fmt.Sprintf("Harness %s is compiling revision %s; %s is the latest successful one", h.Harness, h.DesiredRevision, h.LatestSuccessfulRevision)
	}
	// Ready not yet True without a failed stage: the first revision is still
	// compiling while the controller waits for the golden snapshot
	// (readyReasonPending); any other False reason is a failure it will not
	// get past on its own.
	if c := findCondition(h.Conditions, conditionReady); c != nil && c.Status != "True" {
		if c.Status == "False" && c.Reason != readyReasonPending {
			return VerdictFailed, fmt.Sprintf("Harness %s: Ready is False (%s)", h.Harness, conditionText(c))
		}
		return VerdictProgressing, fmt.Sprintf("Harness %s is compiling revision %s; Ready is %s (%s)", h.Harness, h.DesiredRevision, c.Status, conditionText(c))
	}
	return VerdictProgressing, fmt.Sprintf("Harness %s is compiling revision %s; Ready not reported yet", h.Harness, h.DesiredRevision)
}

// describeHarnesses lists the Harnesses of ns with what they admit, for the
// "no Harness admits the template" answer. Best effort: "" on any failure.
func (s *Service) describeHarnesses(ctx context.Context, dyn dynamic.Interface, ns string, tplLabels map[string]string) string {
	list, err := dyn.Resource(s.harnessGVR()).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		s.log.Debug("listing harnesses failed", "namespace", ns, "error", err)
		return ""
	}
	if len(list.Items) == 0 {
		return fmt.Sprintf("no Harness exists in namespace %s", ns)
	}
	described := make([]string, 0, len(list.Items))
	for i := range list.Items {
		described = append(described, describeHarness(&list.Items[i]))
	}
	sort.Strings(described)
	return fmt.Sprintf("the template carries %s; Harnesses: %s", labels.Set(tplLabels).String(), strings.Join(described, "; "))
}

func describeHarness(h *unstructured.Unstructured) string {
	raw, found, _ := unstructured.NestedMap(h.Object, "spec", "allowedAgentTemplates", "selector")
	if !found {
		return h.GetName() + " (admits nothing: no allowedAgentTemplates selector)"
	}
	sel := &metav1.LabelSelector{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, sel); err != nil {
		return fmt.Sprintf("%s (unreadable selector: %v)", h.GetName(), err)
	}
	parsed, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		return fmt.Sprintf("%s (invalid selector: %v)", h.GetName(), err)
	}
	return fmt.Sprintf("%s (admits %s)", h.GetName(), parsed.String())
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
	case "True":
		return boolPtr(true)
	case "False":
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
