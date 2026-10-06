package migrate

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/giantswarm/agent-manager/internal/agents"
)

// The value-level half of the 1.x -> 2.x expand phase. The 2.x values
// schema only adds keys (agent.egress, plugins); what changes is the kagent
// API under two open keys and one label:
//
//   - a tool binding's `agent: {name, description, templateRef, isolation}`
//     is `subAgent: {name, description, templateRef}` on api.kagent.dev,
//     where every sub-agent is Shared (compiled into the parent's runtime).
//     extraTools[] carries raw bindings and extraAgentSpec.tools[] replaces
//     the curated ones, so both are rewritten; a Dedicated binding has no
//     2.x form and is refused.
//   - labels[agent-platform.giantswarm.io/harness] chose the Harness on 1.x
//     (the Harness admitted templates by that label, and the extra labels
//     won over the chart's); on 2.x the Agent names its Harness in
//     spec.harnessRef from agent.harness and nothing reads the label.

// HarnessAdmissionLabel is the 1.x label a Harness admitted templates by.
const HarnessAdmissionLabel = "agent-platform.giantswarm.io/harness"

// chartDefaultHarness is agent.harness when the values leave it unset.
const chartDefaultHarness = "kagent"

// rewriteValuesV2 turns 1.x values into 2.x values and says what changed.
// The returned map is a copy; values is never modified. The pinner is unused:
// 1.x values carry pinned skills already.
func rewriteValuesV2(_ context.Context, values map[string]any, harness string, _ agents.SkillPinner) (map[string]any, *ValueChanges, error) {
	out := runtime.DeepCopyJSON(values)
	if out == nil {
		out = map[string]any{}
	}
	ch := &ValueChanges{}
	if tools, ok := out["extraTools"].([]any); ok {
		if err := rewriteToolBindings(tools, "extraTools", ch); err != nil {
			return nil, nil, err
		}
	}
	if spec, ok := out["extraAgentSpec"].(map[string]any); ok {
		if tools, ok := spec["tools"].([]any); ok {
			if err := rewriteToolBindings(tools, "extraAgentSpec.tools", ch); err != nil {
				return nil, nil, err
			}
		}
	}
	if labels, ok := out["labels"].(map[string]any); ok {
		if v, present := labels[HarnessAdmissionLabel]; present {
			delete(labels, HarnessAdmissionLabel)
			ch.Removed = append(ch.Removed, "labels."+HarnessAdmissionLabel)
			current, _ := getPath(out, "agent.harness")
			effective, _ := current.(string)
			if effective == "" {
				effective = chartDefaultHarness
			}
			if admitted, _ := v.(string); admitted != "" && admitted != effective {
				setPath(out, "agent.harness", admitted)
				ch.Set = append(ch.Set, "agent.harness="+admitted)
			}
		}
		dropEmptyBlock(out, "labels")
	}
	if harness != "" && harness != agents.DefaultHarnessName {
		if _, set := getPath(out, "agent.harness"); !set {
			setPath(out, "agent.harness", harness)
			ch.Set = append(ch.Set, "agent.harness="+harness)
		}
	}
	return out, ch, nil
}

// rewriteToolBindings renames every `agent` binding of tools to `subAgent`
// in place, dropping a Shared isolation; field names the list in the report.
func rewriteToolBindings(tools []any, field string, ch *ValueChanges) error {
	for i, item := range tools {
		binding, ok := item.(map[string]any)
		if !ok {
			continue
		}
		sub, ok := binding["agent"].(map[string]any)
		if !ok {
			continue
		}
		at := fmt.Sprintf("%s[%d]", field, i)
		if _, both := binding["subAgent"]; both {
			return fmt.Errorf("%w: %s carries both agent and subAgent", agents.ErrInvalid, at)
		}
		if iso, present := sub["isolation"]; present {
			switch iso {
			case "Shared":
				delete(sub, "isolation")
				ch.Removed = append(ch.Removed, at+".agent.isolation")
			case "Dedicated":
				return fmt.Errorf("%w: %s.agent.isolation is Dedicated, which api.kagent.dev has no form for (a sub-agent is compiled into its parent's runtime, Shared); bind it without isolation or remove the binding", agents.ErrInvalid, at)
			default:
				return fmt.Errorf("%w: %s.agent.isolation %v is neither Shared nor Dedicated", agents.ErrInvalid, at, iso)
			}
		}
		delete(binding, "agent")
		binding["subAgent"] = sub
		ch.Renamed = append(ch.Renamed, at+".agent -> "+at+".subAgent")
	}
	return nil
}

// subAgentTemplates are the templateRef names of the sub-agent bindings in
// 2.x values (extraTools[] and extraAgentSpec.tools[]).
func subAgentTemplates(values map[string]any) []string {
	var names []string
	collect := func(tools []any) {
		for _, item := range tools {
			binding, _ := item.(map[string]any)
			sub, _ := binding["subAgent"].(map[string]any)
			ref, _ := sub["templateRef"].(map[string]any)
			if name, _ := ref["name"].(string); name != "" {
				names = append(names, name)
			}
		}
	}
	if tools, ok := values["extraTools"].([]any); ok {
		collect(tools)
	}
	if spec, ok := values["extraAgentSpec"].(map[string]any); ok {
		if tools, ok := spec["tools"].([]any); ok {
			collect(tools)
		}
	}
	return names
}
