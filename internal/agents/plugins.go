package agents

import (
	"fmt"
	"regexp"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agent-manager/internal/skills"
)

// The Harness runtimes the chart's Harness spec discriminates on
// (spec.kagent, spec.claude, spec.codex, spec.byo).
const (
	RuntimeKagent = "kagent"
	RuntimeClaude = "claude"
	RuntimeCodex  = "codex"
	RuntimeBYO    = "byo"
)

// The chart schema's bounds on agent.limits.
const (
	MaxTurnsCeiling = 10000
	MaxPlugins      = 20
	MaxPluginSkills = 50
)

var budgetUSD = regexp.MustCompile(`^(0|[1-9][0-9]{0,5})(\.[0-9]{1,4})?$`)

// ValidateLimits checks the bounds the way the chart schema and the CRD do:
// a decimal budget with up to four decimals, 1 to 10000 turns, at least one
// of the two. nil is no limits.
func ValidateLimits(l *Limits) error {
	if l == nil {
		return nil
	}
	if l.IsZero() {
		return invalidf("limits needs at least one of budgetUsd and maxTurns")
	}
	if l.BudgetUSD != "" && !budgetUSD.MatchString(l.BudgetUSD) {
		return invalidf("limits.budgetUsd %q must be a decimal amount in US dollars with up to four decimals, such as \"2.50\"", l.BudgetUSD)
	}
	if l.MaxTurns < 0 || l.MaxTurns > MaxTurnsCeiling {
		return invalidf("limits.maxTurns %d must be between 1 and %d", l.MaxTurns, MaxTurnsCeiling)
	}
	return nil
}

// RequireLimitsRuntime refuses limits on a Harness whose runtime is not Claude
// Code, the only one that enforces them; runtime is empty when the Harness
// could not be read.
func RequireLimitsRuntime(l *Limits, harness, runtime string) error {
	if l == nil || l.IsZero() {
		return nil
	}
	switch runtime {
	case RuntimeClaude:
		return nil
	case "":
		return invalidf("limits need a Harness whose runtime is Claude Code, and Harness %q could not be read to tell", harness)
	}
	return invalidf("limits are enforced by a Claude Code Harness only; Harness %q runs %s and rejects a template with limits", harness, runtime)
}

// ValidatePlugins checks what the chart schema and the CRD refuse: exactly
// one source per entry, a full commit id or a digest (a plugin is never
// resolved, it comes pinned), a relative path, at least one skill name,
// unique per entry.
func ValidatePlugins(list Plugins) error {
	if len(list) > MaxPlugins {
		return invalidf("plugins has %d entries; the agent chart accepts at most %d", len(list), MaxPlugins)
	}
	for i, p := range list {
		at := fmt.Sprintf("plugins[%d]", i)
		switch {
		case p.Git != nil && p.OCI != "":
			return invalidf("%s has both git and oci; a plugin has exactly one source", at)
		case p.Git == nil && p.OCI == "":
			return invalidf("%s has no source: give git: {url, commit} or oci: <reference>@sha256:<digest>", at)
		case p.Git != nil:
			if !httpURL.MatchString(p.Git.URL) {
				return invalidf("%s.git.url %q must be an http(s) URL", at, p.Git.URL)
			}
			if p.Git.Ref != "" {
				return invalidf("%s.git.ref is not accepted: a plugin is written pinned, give git.commit (list_skills reports head commits)", at)
			}
			if !skills.CommitPattern.MatchString(p.Git.Commit) {
				return invalidf("%s.git.commit %q is not a full commit id (40 or 64 hex characters); a plugin source is pinned", at, p.Git.Commit)
			}
		default:
			if !skills.DigestRefPattern.MatchString(p.OCI) {
				return invalidf("%s.oci %q is not digest-pinned; give <registry>/<repository>@sha256:<digest>", at, p.OCI)
			}
		}
		if err := validateSkillPath(p.Path); err != nil {
			return invalidf("%s.path %q %v", at, p.Path, err)
		}
		if len(p.Skills) == 0 {
			return invalidf("%s selects no skill; name at least one of the bundle's skills", at)
		}
		if len(p.Skills) > MaxPluginSkills {
			return invalidf("%s selects %d skills; at most %d", at, len(p.Skills), MaxPluginSkills)
		}
		seen := map[string]bool{}
		for _, name := range p.Skills {
			if name == "" {
				return invalidf("%s.skills contains an empty name", at)
			}
			if seen[name] {
				return invalidf("%s.skills names %q twice", at, name)
			}
			seen[name] = true
		}
	}
	return nil
}

// limitsValues renders the limits as the chart's agent.limits block; nil
// when unset.
func limitsValues(l *Limits) map[string]any {
	if l == nil || l.IsZero() {
		return nil
	}
	out := map[string]any{}
	if l.BudgetUSD != "" {
		out["budgetUSD"] = l.BudgetUSD
	}
	if l.MaxTurns > 0 {
		out["maxTurns"] = int64(l.MaxTurns)
	}
	return out
}

// limitsFrom reads a {budgetUSD, maxTurns} block (the chart value or the
// object's spec.template.limits); nil when absent or empty.
func limitsFrom(raw any) *Limits {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	l := &Limits{}
	l.BudgetUSD, _ = m["budgetUSD"].(string)
	switch v := m["maxTurns"].(type) {
	case int64:
		l.MaxTurns = int(v)
	case float64:
		l.MaxTurns = int(v)
	}
	if l.IsZero() {
		return nil
	}
	return l
}

// pluginsValues renders the plugins list as chart values; nil when empty.
func pluginsValues(list Plugins) []any {
	if len(list) == 0 {
		return nil
	}
	out := make([]any, 0, len(list))
	for _, p := range list {
		entry := map[string]any{"skills": toAnySlice(p.Skills)}
		if p.Git != nil {
			entry["git"] = map[string]any{"url": p.Git.URL, "commit": p.Git.Commit}
		} else {
			entry["oci"] = p.OCI
		}
		if p.Path != "" {
			entry["path"] = p.Path
		}
		out = append(out, entry)
	}
	return out
}

// pluginsFromValues reads the chart values' plugins list.
func pluginsFromValues(values map[string]any) Plugins {
	raw, _ := values["plugins"].([]any)
	var out Plugins
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, pluginFrom(m, m))
	}
	return out
}

// pluginsFromObject reads the Agent object's spec.template.plugins[]
// ({source: {git: {url, commit}, oci, path}, skills}).
func pluginsFromObject(obj *unstructured.Unstructured) Plugins {
	raw, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "plugins")
	var out Plugins
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		source, _ := m["source"].(map[string]any)
		out = append(out, pluginFrom(m, source))
	}
	return out
}

func pluginFrom(entry, source map[string]any) Plugin {
	p := Plugin{Skills: stringSlice(entry["skills"])}
	p.Path, _ = source["path"].(string)
	if git, ok := source["git"].(map[string]any); ok {
		g := &GitSkill{}
		g.URL, _ = git["url"].(string)
		g.Commit, _ = git["commit"].(string)
		p.Git = g
	}
	p.OCI, _ = source["oci"].(string)
	return p
}
