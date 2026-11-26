package agents

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agent-manager/internal/oci"
	"github.com/giantswarm/agent-manager/internal/skills"
)

// The chart schema's bounds on plugins.
const (
	MaxPlugins      = 20
	MaxPluginSkills = 50
)

// ValidatePlugins checks what can be checked before any lookup: exactly one
// source per entry, an http(s) repository URL, ref or commit but not both, a
// well-formed commit, a parseable OCI reference, a relative path without
// "..", and non-empty selected skill names. The selection is optional: a
// Claude Code format plugin loads whole and needs none, an Agent Plugins
// package without one enables none of its skills.
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
			return invalidf("%s has no source: give git: {url, ref | commit} or oci: <reference>", at)
		case p.Git != nil:
			if err := checkGitURL(at, p.Git.URL); err != nil {
				return err
			}
			if p.Git.Commit != "" && !skills.CommitPattern.MatchString(p.Git.Commit) {
				return invalidf("%s.git.commit %q is not a full commit id (40 or 64 hex characters); give a branch or tag as ref to have it resolved", at, p.Git.Commit)
			}
			if strings.ContainsAny(p.Git.Ref, " \t\n") {
				return invalidf("%s.git.ref %q contains whitespace", at, p.Git.Ref)
			}
		default:
			if _, err := oci.ParseImageReference(p.OCI); err != nil {
				return invalidf("%s.oci: %v", at, err)
			}
		}
		if err := validateSkillPath(p.Path); err != nil {
			return invalidf("%s.path %q %v", at, p.Path, err)
		}
		if len(p.Skills) > MaxPluginSkills {
			return invalidf("%s selects %d skills; at most %d", at, len(p.Skills), MaxPluginSkills)
		}
		for _, name := range p.Skills {
			if name == "" {
				return invalidf("%s.skills contains an empty name", at)
			}
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
		entry := map[string]any{}
		if len(p.Skills) > 0 {
			entry["skills"] = toAnySlice(p.Skills)
		}
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
