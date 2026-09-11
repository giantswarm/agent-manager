package migrate

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/skills"
)

// The value-level half of the expand phase: Generic chart 0.x values become
// the 1.x values contract. Which keys go and which move is the composer's
// list (agents.RemovedValuePaths, agents.RenamedValuePaths) — there is no
// second list here — and skills are pinned by the composer's own path
// (agents.PinSkills), so a migrated release looks exactly like one
// create_agent would write today.

// ValueChanges records what a rewrite did to one release's values.
type ValueChanges struct {
	// Removed are the 0.x keys dropped (dotted paths).
	Removed []string `json:"removed,omitempty"`
	// Renamed are the keys moved, as "old -> new".
	Renamed []string `json:"renamed,omitempty"`
	// Set are the keys the platform adds, as "key=value".
	Set []string `json:"set,omitempty"`
	// Skills are the skill references pinned, one per entry whose written
	// form changed.
	Skills []SkillPin `json:"skills,omitempty"`
	// DriftIgnoreRemoved are the v1alpha2 driftDetection.ignore paths dropped
	// from the HelmRelease.
	DriftIgnoreRemoved []string `json:"driftIgnoreRemoved,omitempty"`
}

// Empty is true when the rewrite changed nothing.
func (c *ValueChanges) Empty() bool {
	return c == nil || (len(c.Removed) == 0 && len(c.Renamed) == 0 && len(c.Set) == 0 && len(c.Skills) == 0 && len(c.DriftIgnoreRemoved) == 0)
}

// SkillPin is one skill reference and the pin it was written as.
type SkillPin struct {
	Name string `json:"name"`
	// Source is the reference as the 0.x values carried it (repository@ref
	// with its path, or the OCI reference).
	Source string `json:"source"`
	// Pinned is the commit id or the digest reference written.
	Pinned string `json:"pinned"`
}

// rewriteValues turns 0.x values into 1.x values and says what changed. The
// returned map is a copy; values is never modified. A skill that cannot be
// resolved is the error (agents.ErrInvalid wrapping skills.ErrUnresolvable);
// nothing is validated here — the caller runs the chart schema.
func rewriteValues(ctx context.Context, values map[string]any, harness string, pinner agents.SkillPinner) (map[string]any, *ValueChanges, error) {
	out := runtime.DeepCopyJSON(values)
	if out == nil {
		out = map[string]any{}
	}
	ch := &ValueChanges{}
	for _, p := range agents.RemovedValuePaths {
		if deletePath(out, p) {
			ch.Removed = append(ch.Removed, p)
		}
	}
	renamed := make([]string, 0, len(agents.RenamedValuePaths))
	for old := range agents.RenamedValuePaths {
		renamed = append(renamed, old)
	}
	sort.Strings(renamed)
	for _, old := range renamed {
		v, ok := getPath(out, old)
		if !ok {
			continue
		}
		deletePath(out, old)
		target := agents.RenamedValuePaths[old]
		if _, exists := getPath(out, target); !exists {
			setPath(out, target, v)
		}
		ch.Renamed = append(ch.Renamed, old+" -> "+target)
	}
	if err := rewriteSkills(ctx, out, pinner, ch); err != nil {
		return nil, nil, err
	}
	if harness != "" && harness != agents.DefaultHarnessName {
		if _, set := getPath(out, "agent.harness"); !set {
			setPath(out, "agent.harness", harness)
			ch.Set = append(ch.Set, "agent.harness="+harness)
		}
	}
	dropEmptyBlock(out, "muster")
	dropEmptyBlock(out, "skills")
	return out, ch, nil
}

// rewriteSkills reshapes the 0.x `skills` object ({refs[], gitRefs[{url, ref,
// path, name}]}) into the 1.x list and pins every entry; a list already in
// the 1.x shape goes through the same pinning (a commit or a digest is kept
// as it is, a ref is resolved).
func rewriteSkills(ctx context.Context, out map[string]any, pinner agents.SkillPinner, ch *ValueChanges) error {
	raw, present := out["skills"]
	if !present || raw == nil {
		return nil
	}
	var list agents.Skills
	switch v := raw.(type) {
	case map[string]any:
		list = legacySkills(v)
	case []any:
		list = listSkills(v)
	default:
		return fmt.Errorf("%w: skills is neither the 0.x object nor the 1.x list (%T)", agents.ErrInvalid, raw)
	}
	if len(list) == 0 {
		delete(out, "skills")
		return nil
	}
	if err := agents.ValidateSkills(list); err != nil {
		return err
	}
	pinned, err := agents.PinSkills(ctx, pinner, list)
	if err != nil {
		return err
	}
	rendered := agents.SkillsValues(pinned)
	before, _ := raw.([]any)
	for i, entry := range rendered {
		if i < len(before) && reflect.DeepEqual(before[i], entry) {
			continue
		}
		ch.Skills = append(ch.Skills, SkillPin{Name: pinned[i].Name, Source: describeSource(list[i]), Pinned: pinOf(pinned[i])})
	}
	out["skills"] = rendered
	return nil
}

// legacySkills reads the 0.x object: OCI refs as strings, git refs as
// {url, ref, path, name}. gitAuthSecretRef is gone already (a removed path).
func legacySkills(m map[string]any) agents.Skills {
	var out agents.Skills
	gitRefs, _ := m["gitRefs"].([]any)
	for _, item := range gitRefs {
		g, ok := item.(map[string]any)
		if !ok {
			continue
		}
		s := agents.Skill{Git: &agents.GitSkill{}}
		s.Name, _ = g["name"].(string)
		s.Path, _ = g["path"].(string)
		s.Git.URL, _ = g["url"].(string)
		s.Git.Ref, _ = g["ref"].(string)
		if skills.CommitPattern.MatchString(s.Git.Ref) {
			s.Git.Commit, s.Git.Ref = s.Git.Ref, ""
		}
		out = append(out, s)
	}
	for _, ref := range stringsOf(m["refs"]) {
		out = append(out, agents.Skill{OCI: ref})
	}
	return out
}

// listSkills reads the 1.x list ({name, path, git: {url, ref, commit}} |
// {name, oci}).
func listSkills(items []any) agents.Skills {
	var out agents.Skills
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		s := agents.Skill{}
		s.Name, _ = m["name"].(string)
		s.Path, _ = m["path"].(string)
		if git, ok := m["git"].(map[string]any); ok {
			s.Git = &agents.GitSkill{}
			s.Git.URL, _ = git["url"].(string)
			s.Git.Ref, _ = git["ref"].(string)
			s.Git.Commit, _ = git["commit"].(string)
		}
		s.OCI, _ = m["oci"].(string)
		out = append(out, s)
	}
	return out
}

func describeSource(s agents.Skill) string {
	if s.Git == nil {
		return s.OCI
	}
	ref := s.Git.Ref
	if ref == "" {
		ref = s.Git.Commit
	}
	if ref == "" {
		ref = "(default branch)"
	}
	src := s.Git.URL + "@" + ref
	if s.Path != "" {
		src += " path=" + s.Path
	}
	return src
}

func pinOf(s agents.Skill) string {
	if s.Git != nil {
		return s.Git.Commit
	}
	return s.OCI
}

// removeDriftIgnore drops the v1alpha2 paths (/spec/declarative/…) from the
// HelmRelease's driftDetection.ignore — entries left without a path go, an
// empty ignore list goes, drift detection itself stays as configured.
func removeDriftIgnore(hr *unstructured.Unstructured) []string {
	ignore, found, _ := unstructured.NestedSlice(hr.Object, "spec", "driftDetection", "ignore")
	if !found {
		return nil
	}
	var removed []string
	kept := make([]any, 0, len(ignore))
	for _, entry := range ignore {
		m, ok := entry.(map[string]any)
		if !ok {
			kept = append(kept, entry)
			continue
		}
		var paths []any
		for _, p := range stringsOf(m["paths"]) {
			if strings.HasPrefix(p, "/spec/declarative") {
				removed = append(removed, p)
				continue
			}
			paths = append(paths, p)
		}
		if len(paths) == 0 {
			continue
		}
		m["paths"] = paths
		kept = append(kept, m)
	}
	if len(removed) == 0 {
		return nil
	}
	if len(kept) == 0 {
		unstructured.RemoveNestedField(hr.Object, "spec", "driftDetection", "ignore")
	} else {
		_ = unstructured.SetNestedSlice(hr.Object, kept, "spec", "driftDetection", "ignore")
	}
	return removed
}

// ---- dotted-path helpers over map[string]any ---------------------------------

func splitPath(p string) []string { return strings.Split(p, ".") }

func getPath(m map[string]any, p string) (any, bool) {
	v, found, err := unstructured.NestedFieldNoCopy(m, splitPath(p)...)
	return v, found && err == nil
}

func setPath(m map[string]any, p string, v any) {
	_ = unstructured.SetNestedField(m, v, splitPath(p)...)
}

// deletePath removes p and reports whether it existed.
func deletePath(m map[string]any, p string) bool {
	if _, ok := getPath(m, p); !ok {
		return false
	}
	unstructured.RemoveNestedField(m, splitPath(p)...)
	return true
}

// dropEmptyBlock removes key when the rewrite left an empty object behind.
func dropEmptyBlock(m map[string]any, key string) {
	if block, ok := m[key].(map[string]any); ok && len(block) == 0 {
		delete(m, key)
	}
}

func stringsOf(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
