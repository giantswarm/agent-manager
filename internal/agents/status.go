package agents

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// errorsIs is errors.Is, named so the service file reads without the import.
func errorsIs(err, target error) bool { return errors.Is(err, target) }

// Readiness on kagent main is the template's: the controller compiles every
// AgentTemplate for each Harness whose admission selector matches it and
// reports one status entry per Harness (Accepted, ResolvedRefs, Compatible,
// then Ready once the golden snapshot exists; a failing stage sets its
// condition False with the reason). Harness.status stays empty; instances are
// not Kubernetes objects. The verdict folds the template's entries and the
// toolset carrier's acceptance into one line.

// The AgentTemplate condition types kagent reports per Harness.
const (
	conditionReady        = "Ready"
	conditionAccepted     = "Accepted"
	conditionResolvedRefs = "ResolvedRefs"
	conditionCompatible   = "Compatible"
)

// Status gathers the template's per-Harness status and the toolset carrier's
// acceptance and folds them into one verdict.
func (s *Service) Status(ctx context.Context, ns, name string) (*Status, error) {
	ns, err := s.Namespace(ns)
	if err != nil {
		return nil, err
	}
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	dyn, err := s.dyn(ctx)
	if err != nil {
		return nil, err
	}
	tpl, err := s.getTemplate(ctx, dyn, ns, name)
	if err != nil {
		return nil, err
	}
	if tpl == nil {
		return nil, notFoundf("agent %s/%s: no AgentTemplate of that name", ns, name)
	}
	carrier, err := s.getCarrier(ctx, dyn, ns, name)
	if err != nil {
		return nil, err
	}
	return statusOf(tpl, carrier, s.cfg.Compose), nil
}

// statusOf is the pure part of Status.
func statusOf(tpl, carrier *unstructured.Unstructured, cfg ComposeConfig) *Status {
	st := &Status{Name: tpl.GetName(), Namespace: tpl.GetNamespace(), Template: templateStatusOf(tpl)}
	d, _ := declarationOf(tpl, carrier, cfg)
	if d.Toolset != nil || (carrier == nil && !d.bindsPlatformServer && bindsCarrier(tpl, cfg)) {
		st.ToolsetCarrier = carrierView(tpl.GetName(), carrier, cfg)
	}
	st.Verdict, st.Summary = verdict(st)
	return st
}

// bindsCarrier reports whether the template binds its toolset carrier by name.
func bindsCarrier(tpl *unstructured.Unstructured, cfg ComposeConfig) bool {
	tools, _, _ := unstructured.NestedSlice(tpl.Object, "spec", "tools")
	want := CarrierName(cfg.MusterServer, tpl.GetName())
	for _, t := range tools {
		m, _ := t.(map[string]any)
		mcp, _ := m["mcp"].(map[string]any)
		server, _ := mcp["server"].(map[string]any)
		if name, _ := server["name"].(string); name == want {
			return true
		}
	}
	return false
}

// templateStatusOf reads status.harnesses[] and the generations.
func templateStatusOf(tpl *unstructured.Unstructured) *TemplateStatus {
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
		if warnings, _ := m["warnings"].([]any); len(warnings) > 0 {
			for _, w := range warnings {
				if str, ok := w.(string); ok {
					hs.Warnings = append(hs.Warnings, str)
				}
			}
		}
		ts.Harnesses = append(ts.Harnesses, hs)
	}
	sort.Slice(ts.Harnesses, func(i, j int) bool { return ts.Harnesses[i].Harness < ts.Harnesses[j].Harness })
	return ts
}

// carrierView reads the toolset carrier into the view; Exists false when nil.
func carrierView(agent string, carrier *unstructured.Unstructured, cfg ComposeConfig) *ToolsetCarrier {
	v := &ToolsetCarrier{Name: CarrierName(cfg.MusterServer, agent)}
	if carrier == nil {
		return v
	}
	v.Exists = true
	v.Header = carrierHeader(carrier)
	raw, _, _ := unstructured.NestedSlice(carrier.Object, "status", "conditions")
	conds := conditionsOf(raw)
	v.Accepted = conditionStatus(conds, conditionAccepted)
	if c := findCondition(conds, conditionAccepted); c != nil {
		v.Message = conditionText(c)
	}
	return v
}

// anyReady reports whether any Harness reports Ready=True for the current
// generation, and the names of those that do.
func anyReady(ts *TemplateStatus) []string {
	var ready []string
	for _, h := range ts.Harnesses {
		if h.Ready != nil && *h.Ready && currentGeneration(ts, h, conditionReady) {
			ready = append(ready, h.Harness)
		}
	}
	return ready
}

// currentGeneration reports whether the condition was set for the template's
// current generation (a stale Ready=True describes the previous revision).
func currentGeneration(ts *TemplateStatus, h HarnessStatus, typ string) bool {
	c := findCondition(h.Conditions, typ)
	return c != nil && (c.ObservedGeneration == 0 || c.ObservedGeneration >= ts.Generation)
}

// verdict folds the gathered facts into ready / progressing / failed / unknown
// and one actionable sentence.
func verdict(st *Status) (string, string) {
	if st.ToolsetCarrier != nil && !st.ToolsetCarrier.Exists {
		return VerdictFailed, fmt.Sprintf("the template binds its toolset carrier RemoteMCPServer %s, which does not exist: re-apply the toolset with update_agent", st.ToolsetCarrier.Name)
	}
	if st.ToolsetCarrier != nil && st.ToolsetCarrier.Accepted != nil && !*st.ToolsetCarrier.Accepted {
		return VerdictFailed, fmt.Sprintf("kagent rejected the toolset carrier RemoteMCPServer %s: %s", st.ToolsetCarrier.Name, st.ToolsetCarrier.Message)
	}
	ts := st.Template
	if ts == nil || !ts.Exists {
		return VerdictUnknown, "no AgentTemplate reported anything yet"
	}
	// A failing stage on any admitting Harness is the actionable fact.
	for _, h := range ts.Harnesses {
		for _, typ := range []string{conditionAccepted, conditionResolvedRefs, conditionCompatible, conditionReady} {
			if c := findCondition(h.Conditions, typ); c != nil && c.Status == "False" && currentGeneration(ts, h, typ) {
				return VerdictFailed, fmt.Sprintf("Harness %s: %s is False (%s)", h.Harness, typ, conditionText(c))
			}
		}
	}
	if ready := anyReady(ts); len(ready) > 0 {
		warnings := 0
		for _, h := range ts.Harnesses {
			warnings += len(h.Warnings)
		}
		summary := fmt.Sprintf("AgentTemplate is Ready on Harness %s", strings.Join(ready, ", "))
		if warnings > 0 {
			summary += fmt.Sprintf(" with %d compile warning(s)", warnings)
		}
		return VerdictReady, summary
	}
	if ts.ObservedGeneration == 0 {
		return VerdictProgressing, "kagent has not reported on the AgentTemplate yet"
	}
	if ts.ObservedGeneration < ts.Generation {
		return VerdictProgressing, fmt.Sprintf("kagent has not observed generation %d yet (observed %d)", ts.Generation, ts.ObservedGeneration)
	}
	if len(ts.Harnesses) == 0 {
		return VerdictFailed, "no Harness admits the AgentTemplate: no Harness of the namespace has an allowedAgentTemplates selector matching its labels (check the kagent.dev/harness label against `kubectl get harnesses`)"
	}
	names := make([]string, 0, len(ts.Harnesses))
	for _, h := range ts.Harnesses {
		names = append(names, h.Harness)
	}
	return VerdictProgressing, fmt.Sprintf("kagent is compiling the revision for Harness %s; Ready not reported yet", strings.Join(names, ", "))
}

// conditionsOf flattens a conditions list (status.conditions or
// status.harnesses[].conditions).
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
