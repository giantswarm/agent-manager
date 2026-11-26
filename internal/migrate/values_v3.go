package migrate

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/giantswarm/agent-manager/internal/agents"
)

// The value-level half of the 2.x -> 3.x expand phase. Both majors render
// the same api.kagent.dev Agent; chart 3.0 changes two values:
//
//   - muster.requireApproval is removed (giantswarm/giantswarm#38902, slice
//     8): kagent's flag paused the agent before every tool call through the
//     muster binding; `default: Ask` makes muster ask a person before every
//     such call instead. `true` becomes `default: Ask` where the binding
//     rendered (muster on, a tool list other than exactly ["preset:none"]),
//     `false` is dropped. `true` beside tool approval settings never
//     rendered (2.x refused the combination) and is refused here too.
//   - modelConfig.name has no default any more (2.x rendered
//     default-model-config): an unset name is written as that, so the
//     Agent renders the model it rendered on 2.x.

// chart2DefaultModelConfig is the 2.x chart's default of modelConfig.name.
const chart2DefaultModelConfig = "default-model-config"

// rewriteValuesV3 turns 2.x values into 3.x values and says what changed.
// The returned map is a copy; values is never modified.
func rewriteValuesV3(_ context.Context, values map[string]any, _ string, _ agents.SkillPinner) (map[string]any, *ValueChanges, error) {
	out := runtime.DeepCopyJSON(values)
	if out == nil {
		out = map[string]any{}
	}
	ch := &ValueChanges{}
	if _, set := getPath(out, "modelConfig.name"); !set {
		setPath(out, "modelConfig.name", chart2DefaultModelConfig)
		ch.Set = append(ch.Set, "modelConfig.name="+chart2DefaultModelConfig)
	}
	v, present := getPath(out, "muster.requireApproval")
	if !present {
		return out, ch, nil
	}
	on, ok := v.(bool)
	if !ok {
		return nil, nil, fmt.Errorf("%w: muster.requireApproval is %v, not a boolean; the 2.x chart never rendered it", agents.ErrInvalid, v)
	}
	if on {
		if settings := approvalSettings(out); len(settings) > 0 {
			return nil, nil, fmt.Errorf("%w: muster.requireApproval: true together with tool approval settings (%s), which no chart renders (2.x refused the combination); remove muster.requireApproval and choose the settings, e.g. default: Ask", agents.ErrInvalid, strings.Join(settings, ", "))
		}
	}
	deletePath(out, "muster.requireApproval")
	ch.Removed = append(ch.Removed, "muster.requireApproval")
	dropEmptyBlock(out, "muster")
	if on && musterBindingRenders(out) {
		out[agents.DefaultValuesKey] = agents.SettingAsk
		ch.Set = append(ch.Set, agents.DefaultValuesKey+"="+agents.SettingAsk)
	}
	return out, ch, nil
}

// approvalSettings names the tool approval settings in values: `default`
// and every `tools` entry with a setting.
func approvalSettings(values map[string]any) []string {
	var out []string
	if d, ok := values[agents.DefaultValuesKey]; ok && d != nil {
		out = append(out, agents.DefaultValuesKey)
	}
	items, _ := values[agents.ToolsValuesKey].([]any)
	var entries []string
	for _, item := range items {
		if m, ok := item.(map[string]any); ok {
			for sel := range m {
				entries = append(entries, "tools entry "+sel)
			}
		}
	}
	sort.Strings(entries)
	return append(out, entries...)
}

// musterBindingRenders is the chart's agent.musterBinding: muster on and a
// tool list (`tools`, else `toolset`) other than exactly ["preset:none"].
func musterBindingRenders(values map[string]any) bool {
	if enabled, set := getPath(values, "muster.enabled"); set {
		if on, ok := enabled.(bool); ok && !on {
			return false
		}
	}
	list, ok := values[agents.ToolsValuesKey]
	if !ok || list == nil {
		list = values[agents.ToolsetValuesKey]
	}
	return !reflect.DeepEqual(list, []any{"preset:none"})
}
