package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// An agent's tool list is the agent chart's `tools` value (and `default`),
// which replaces `toolset`: each entry a toolset selector, optionally with an
// approval setting (Allow, Ask, Block). The selectors bound what the agent
// sees, as a toolset does; the settings are bound into each Session's muster
// binding, where muster combines them with the connectors' own settings
// (strictest wins; an Allow over a connector's Ask holds only with a grant,
// approval_grants.go).

const (
	// ToolsValuesKey is the agent chart's tool list value.
	ToolsValuesKey = "tools"
	// DefaultValuesKey is the agent chart's default approval setting.
	DefaultValuesKey = "default"
	// MaxToolEntries caps an agent's tool list (the chart's maxItems).
	MaxToolEntries = 32

	// The approval settings, case-sensitive.
	SettingAllow = "Allow"
	SettingAsk   = "Ask"
	SettingBlock = "Block"

	// DefaultEntry names the list's default where a grant names an entry.
	DefaultEntry = "default"
)

// ToolEntry is one entry of an agent's tool list: a selector and its
// approval setting, "" for none.
type ToolEntry struct {
	Selector string
	Setting  string
}

// ToolList is an agent's tool list. On the wire and in the chart values an
// entry is the selector string, or a one-key object {selector: setting}.
type ToolList []ToolEntry

// MarshalJSON writes the chart's shape.
func (l ToolList) MarshalJSON() ([]byte, error) {
	if l == nil {
		return []byte("null"), nil
	}
	return json.Marshal(l.values())
}

// UnmarshalJSON reads the chart's shape.
func (l *ToolList) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		*l = nil
		return nil
	}
	var items []any
	if err := json.Unmarshal(data, &items); err != nil {
		return fmt.Errorf("tools must be a list of selectors or {selector: Allow|Ask|Block} objects: %w", err)
	}
	list, err := toolListFrom(items)
	if err != nil {
		return err
	}
	*l = list
	return nil
}

// toolListFrom reads decoded list items.
func toolListFrom(items []any) (ToolList, error) {
	list := make(ToolList, 0, len(items))
	for _, item := range items {
		switch v := item.(type) {
		case string:
			list = append(list, ToolEntry{Selector: v})
		case map[string]any:
			if len(v) != 1 {
				return nil, invalidf("tools entry %v must have exactly one selector with its setting, e.g. {\"server:github\": \"Ask\"}", v)
			}
			for sel, setting := range v {
				s, ok := setting.(string)
				if !ok {
					return nil, invalidf("tools entry %q: the setting must be Allow, Ask or Block", sel)
				}
				list = append(list, ToolEntry{Selector: sel, Setting: s})
			}
		default:
			return nil, invalidf("tools entry %v must be a selector or {selector: Allow|Ask|Block}", item)
		}
	}
	return list, nil
}

// values renders the list as chart values.
func (l ToolList) values() []any {
	out := make([]any, 0, len(l))
	for _, e := range l {
		if e.Setting == "" {
			out = append(out, e.Selector)
		} else {
			out = append(out, map[string]any{e.Selector: e.Setting})
		}
	}
	return out
}

// Selectors are the entries' selectors, settings stripped: the toolset.
func (l ToolList) Selectors() []string {
	if l == nil {
		return nil
	}
	out := make([]string, 0, len(l))
	for _, e := range l {
		out = append(out, e.Selector)
	}
	return out
}

// HasSettings reports whether any entry carries a setting.
func (l ToolList) HasSettings() bool {
	return slices.ContainsFunc(l, func(e ToolEntry) bool { return e.Setting != "" })
}

// Setting returns the entry's setting and whether the selector is listed.
func (l ToolList) Setting(selector string) (string, bool) {
	for _, e := range l {
		if e.Selector == selector {
			return e.Setting, true
		}
	}
	return "", false
}

// ToolListOf is a toolset as a tool list without settings.
func ToolListOf(selectors []string) ToolList {
	if selectors == nil {
		return nil
	}
	l := make(ToolList, 0, len(selectors))
	for _, s := range selectors {
		l = append(l, ToolEntry{Selector: s})
	}
	return l
}

// validSetting reports whether s is Allow, Ask or Block.
func validSetting(s string) bool {
	return s == SettingAllow || s == SettingAsk || s == SettingBlock
}

// ValidateToolList checks a tool list: the toolset grammar for every
// selector, each selector once, settings Allow, Ask or Block, and
// preset:none alone without a setting. The count is the chart's to judge
// (its schema: 32 for tools, the toolset's inline limits for a chart
// without tools), so a toolset carried over keeps working. A nil list is
// "absent": refused when required.
func ValidateToolList(l ToolList, required bool) error {
	if err := ValidateToolset(l.Selectors(), required); err != nil {
		return err
	}
	if l == nil {
		return nil
	}
	seen := map[string]bool{}
	for _, e := range l {
		if seen[e.Selector] {
			return invalidf("tools lists %q twice; each selector appears once, with or without a setting", e.Selector)
		}
		seen[e.Selector] = true
		if e.Setting != "" && !validSetting(e.Setting) {
			return invalidf("tools entry %q: setting %q is not Allow, Ask or Block (case-sensitive)", e.Selector, e.Setting)
		}
		if e.Selector == presetNone && (e.Setting != "" || len(l) > 1) {
			return invalidf("tools: preset:none is an agent without tools: list it alone, without a setting")
		}
	}
	return nil
}

// ValidateDefault checks the list's default: "" (none), Allow, Ask or Block.
func ValidateDefault(d string) error {
	if d != "" && !validSetting(d) {
		return invalidf("default %q is not Allow, Ask or Block (case-sensitive)", d)
	}
	return nil
}

// toolListFromValues reads an agent's tool list from its release values:
// `tools`, else `toolset` as entries without settings; and `default`.
func toolListFromValues(values map[string]any) (ToolList, string) {
	def, _ := values[DefaultValuesKey].(string)
	if items, ok := values[ToolsValuesKey].([]any); ok {
		if l, err := toolListFrom(items); err == nil {
			return l, def
		}
	}
	if _, ok := values[ToolsetValuesKey]; ok {
		return ToolListOf(stringSlice(values[ToolsetValuesKey])), def
	}
	return nil, def
}

// setToolListValues writes a tool list and default into values: as `tools`
// and `default` when the chart takes them (dropping a `toolset` the release
// had), else as `toolset`, refusing settings the chart cannot carry.
func setToolListValues(values map[string]any, l ToolList, def string, chartTakesTools bool, chartVersion string, binds bool) error {
	if !chartTakesTools {
		if l.HasSettings() || def != "" {
			return fmt.Errorf("%w: the agent chart %s has no tool approval settings (tools, default); use a chart that has them", ErrUnsupported, orDefault(chartVersion, "in use"))
		}
		if l != nil {
			values[ToolsetValuesKey] = toAnySlice(l.Selectors())
		}
		return nil
	}
	if (l.HasSettings() || def != "") && !binds {
		return errNoBinding
	}
	if l != nil {
		values[ToolsValuesKey] = l.values()
		delete(values, ToolsetValuesKey)
	}
	if def == "" {
		delete(values, DefaultValuesKey)
	} else {
		values[DefaultValuesKey] = def
	}
	return nil
}

// errNoBinding refuses settings an installation would not enforce
// (M2-S3-C-02, S-03).
var errNoBinding = fmt.Errorf("%w: this installation does not bind tool approval settings to sessions (agent-manager's muster.toolApproval is off, or it has no muster bindings endpoint or sender token), so an Allow, Ask, Block or default would not be enforced; save the tool list without settings, or turn tool approval on", ErrUnsupported)

// releaseTakesTools reports whether the chart the release runs takes `tools`
// (M2-S3-C-17): the registry's latest chart must, and the release must run
// it, or already carry `tools`. A release still on another chart version
// keeps the key it has until Flux upgrades it.
func (s *Service) releaseTakesTools(ctx context.Context, hr *unstructured.Unstructured, before map[string]any) (bool, string) {
	// The schema judged by is the one of SchemaVersion (the resolved chart).
	latest := s.chart.Info(ctx).SchemaVersion
	if latest == "" {
		latest = s.chartVersion(ctx)
	}
	if !s.chartTakesTools(ctx) {
		return false, latest
	}
	if _, has := before[ToolsValuesKey]; has {
		return true, latest
	}
	running := ""
	if hr != nil {
		if history, found, _ := unstructured.NestedSlice(hr.Object, "status", "history"); found && len(history) > 0 {
			if entry, ok := history[0].(map[string]any); ok {
				running, _ = entry["chartVersion"].(string)
			}
		}
	}
	if running != "" && latest != "" && running != latest {
		return false, running
	}
	return true, latest
}

// sourceTakesTools reports whether the chart a create in this site gets
// takes `tools` (M2I-1, decision #87): the chart the namespace's agent chart
// source (OCIRepository) serves, not agent-manager's own resolved one. A
// source that serves another chart version than the schema agent-manager
// judges by (an organization's copy the operator created once), or points
// at another chart, is taken not to: the release gets `toolset` and settings
// are refused instead of an install that fails on `tools`. A source not yet
// reconciled (no artifact) or one agent-manager creates follows the resolved
// chart. version names the chart judged by, for messages.
func (s *Service) sourceTakesTools(ctx context.Context, st *site) (takes bool, version string) {
	schemaVersion := s.chart.Info(ctx).SchemaVersion
	if schemaVersion == "" {
		schemaVersion = s.chartVersion(ctx)
	}
	if !s.chartTakesTools(ctx) {
		return false, schemaVersion
	}
	name := BuildOCIRepository(st.fluxNS, st.compose).GetName()
	src, err := st.flux.Resource(s.ociRepositoryGVR()).Namespace(st.fluxNS).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return true, schemaVersion
	}
	where := fmt.Sprintf("that OCIRepository %s/%s, the namespace's agent chart source, serves", st.fluxNS, name)
	if url, _, _ := unstructured.NestedString(src.Object, "spec", "url"); url != "" && url != s.cfg.Compose.ChartOCIURL {
		return false, url + " " + where
	}
	revision, _, _ := unstructured.NestedString(src.Object, "status", "artifact", "revision")
	served, _, _ := strings.Cut(revision, "@")
	served = strings.TrimPrefix(served, "v")
	if served == "" || schemaVersion == "" || served == strings.TrimPrefix(schemaVersion, "v") {
		return true, schemaVersion
	}
	return false, served + " " + where
}

// chartTakesTools reports whether the resolved agent chart declares `tools`.
func (s *Service) chartTakesTools(ctx context.Context) bool {
	doc, _ := s.chart.Schema(ctx).Document.(map[string]any)
	_, found, _ := unstructured.NestedFieldNoCopy(doc, "properties", ToolsValuesKey)
	return found
}

// chartVersion names the resolved chart for messages.
func (s *Service) chartVersion(ctx context.Context) string {
	return s.chart.Info(ctx).LatestVersion
}

// toolListChange is what a save does to the list: the entries that are new
// or carry a changed setting.
func changedEntries(before, after ToolList) []string {
	var out []string
	for _, e := range after {
		old, listed := before.Setting(e.Selector)
		if !listed || old != e.Setting {
			out = append(out, e.Selector)
		}
	}
	sort.Strings(out)
	return out
}

// toolListLog renders a list and default for the logs.
func toolListLog(l ToolList, def string) map[string]any {
	entries := make([]string, 0, len(l))
	for _, e := range l {
		if e.Setting == "" {
			entries = append(entries, e.Selector)
		} else {
			entries = append(entries, e.Selector+": "+e.Setting)
		}
	}
	return map[string]any{"tools": strings.Join(entries, ", "), "default": def}
}

// normalizeTools folds the deprecated toolset into tools: one of the two,
// never both. After it Toolset mirrors the list's selectors.
func (s *Spec) normalizeTools() error {
	if s.Toolset != nil && s.Tools != nil {
		return invalidf("pass tools or the deprecated toolset, not both")
	}
	if s.Tools == nil && s.Toolset != nil {
		s.Tools = ToolListOf(s.Toolset)
	}
	s.Toolset = s.Tools.Selectors()
	return nil
}

// normalizeTools folds the deprecated toolset into tools.
func (u *Update) normalizeTools() error {
	if u.Toolset != nil && u.Tools != nil {
		return invalidf("pass tools or the deprecated toolset, not both")
	}
	if u.Tools == nil && u.Toolset != nil {
		l := ToolListOf(*u.Toolset)
		if l == nil {
			l = ToolList{}
		}
		u.Tools = &l
		u.Toolset = nil
	}
	return nil
}
