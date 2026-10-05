package agents

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agent-manager/internal/skills"
)

// The chart schema's bounds on plugins.
const (
	MaxPlugins      = 20
	MaxPluginSkills = 50
)

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
