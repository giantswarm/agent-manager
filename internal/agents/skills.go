package agents

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agent-manager/internal/oci"
	"github.com/giantswarm/agent-manager/internal/skills"
)

// A skill on kagent API v2 is an immutable source: a git repository at a full
// commit id, or an OCI image by digest. Callers may still say "this branch",
// "this tag" or "this image tag" — agent-manager resolves the reference at
// write time and writes the pin. refreshSkills is the way to move every git
// skill of an agent to the head of its repository's default branch again.

// SkillPinner resolves mutable skill references to pins (skills.Resolver).
type SkillPinner interface {
	// GitHead resolves ref of repoURL (empty: the default branch) to a commit.
	GitHead(ctx context.Context, repoURL, ref string) (string, error)
	// OCIDigest pins an OCI reference to <repository>@sha256:<digest>.
	OCIDigest(ctx context.Context, ref string) (string, error)
}

// What the removed arguments are told.
const (
	toolNamesRemoved     = `toolNames never narrowed anything against muster (kagent filters muster's meta-tools only); declare a toolset instead, e.g. toolset: ["preset:read-only"]`
	runtimeRemoved       = `runtime is gone: on kagent API v2 the platform Harness (the Go ADK) is the runtime of every agent — there is no per-agent runtime and no Python runtime; drop the argument`
	gitAuthRefRemoved = `skills.gitAuthSecretName is gone: kagent API v2 and Generic chart 1.x carry no per-source skill credential — a skill is an immutable reference (a git commit or an OCI digest) the Harness reads without one; agent-manager resolves a branch or tag through the GitHub API with its own token (GITHUB_TOKEN) where a private repository needs it. Drop the field; skills is a list of {name, git: {url, ref | commit}, path} or {name, oci: <reference>}`
	skillsShapeChanged   = `skills is a list now — [{name, git: {url, ref | commit}, path} | {name, oci: <registry>/<repository>:<tag>|@sha256:<digest>}] — not the 0.x object {refs, gitRefs}; every entry is pinned to a commit or a digest before it is written (list_skills reports the commits)`
)

var httpURL = regexp.MustCompile(`^https?://\S+$`)

// ValidateSkills checks what can be checked before any lookup: exactly one
// source per entry, an http(s) repository URL, ref or commit but not both, a
// well-formed commit, a parseable OCI reference, a relative path without
// "..", unique names.
func ValidateSkills(list Skills) error {
	names := map[string]int{}
	for i, s := range list {
		at := fmt.Sprintf("skills[%d]", i)
		switch {
		case s.Git != nil && s.OCI != "":
			return invalidf("%s has both git and oci; a skill has exactly one source", at)
		case s.Git == nil && s.OCI == "":
			return invalidf("%s has no source: give git: {url, ref | commit} or oci: <reference>", at)
		case s.Git != nil:
			if !httpURL.MatchString(s.Git.URL) {
				return invalidf("%s.git.url %q must be an http(s) URL", at, s.Git.URL)
			}
			if s.Git.Commit != "" && !skills.CommitPattern.MatchString(s.Git.Commit) {
				return invalidf("%s.git.commit %q is not a full commit id (40 or 64 hex characters); give a branch or tag as ref to have it resolved", at, s.Git.Commit)
			}
			if strings.ContainsAny(s.Git.Ref, " \t\n") {
				return invalidf("%s.git.ref %q contains whitespace", at, s.Git.Ref)
			}
		default:
			if _, err := oci.ParseImageReference(s.OCI); err != nil {
				return invalidf("%s.oci: %v", at, err)
			}
			if s.Path != "" {
				return invalidf("%s.path is for git skills; an OCI skill is the whole image", at)
			}
		}
		if err := validateSkillPath(s.Path); err != nil {
			return invalidf("%s.path %q %v", at, s.Path, err)
		}
		name := s.mountName()
		if name == "" || len(name) > 63 {
			return invalidf("%s needs a name of at most 63 characters", at)
		}
		if prev, dup := names[name]; dup {
			return invalidf("%s and skills[%d] both mount as %q; give one of them another name", at, prev, name)
		}
		names[name] = i
	}
	return nil
}

func validateSkillPath(p string) error {
	if p == "" {
		return nil
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("must be relative to the repository root")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("must not climb out of the repository (\"..\")")
		}
	}
	return nil
}

// mountName is the directory a skill mounts under: the explicit name, else
// the last path segment, else the repository or image name.
func (s Skill) mountName() string {
	if s.Name != "" {
		return s.Name
	}
	if p := strings.Trim(s.Path, "/"); p != "" {
		parts := strings.Split(p, "/")
		return parts[len(parts)-1]
	}
	source := s.OCI
	if s.Git != nil {
		source = strings.TrimSuffix(strings.Trim(s.Git.URL, "/"), ".git")
	}
	if at := strings.Index(source, "@"); at >= 0 {
		source = source[:at]
	}
	if colon := strings.LastIndex(source, ":"); colon > strings.LastIndex(source, "/") {
		source = source[:colon]
	}
	parts := strings.Split(strings.Trim(source, "/"), "/")
	return parts[len(parts)-1]
}

// pinSkills returns the list with every entry pinned: a git skill to a full
// commit (its Commit when given, else the head of Ref, else of the default
// branch — with refresh, always the head of Ref or the default branch), an
// OCI skill to its digest. Names are filled in. A reference that cannot be
// resolved is an ErrInvalid naming it; nothing is written by the caller then.
func pinSkills(ctx context.Context, p SkillPinner, list Skills, refresh bool) (Skills, error) {
	if len(list) == 0 {
		return nil, nil
	}
	out := make(Skills, 0, len(list))
	for i, s := range list {
		pinned := Skill{Name: s.mountName(), Path: s.Path}
		switch {
		case s.Git != nil:
			g := GitSkill{URL: s.Git.URL, Commit: s.Git.Commit}
			switch {
			case !refresh && g.Commit != "" && s.Git.Ref != "":
				return nil, invalidf("skills[%d] (%s) gives both ref %q and commit %q; give the commit to pin, or the ref to have it resolved", i, pinned.Name, s.Git.Ref, g.Commit)
			case refresh || g.Commit == "":
				if p == nil {
					return nil, invalidf("skills[%d] (%s): no resolver to look up %s — give git.commit", i, pinned.Name, describeRef(s.Git.Ref))
				}
				commit, err := p.GitHead(ctx, g.URL, s.Git.Ref)
				if err != nil {
					return nil, invalidf("skills[%d] (%s): %v", i, pinned.Name, err)
				}
				g.Commit = commit
			}
			pinned.Git = &g
		default:
			if skills.DigestRefPattern.MatchString(s.OCI) || p == nil {
				pinned.OCI = s.OCI
				break
			}
			digest, err := p.OCIDigest(ctx, s.OCI)
			if err != nil {
				return nil, invalidf("skills[%d] (%s): %v", i, pinned.Name, err)
			}
			pinned.OCI = digest
		}
		out = append(out, pinned)
	}
	return out, nil
}

func describeRef(ref string) string {
	if ref == "" {
		return "the default branch"
	}
	return "ref " + ref
}

// skillsValues renders the skills list as chart values; nil when empty.
func skillsValues(list Skills) []any {
	if len(list) == 0 {
		return nil
	}
	out := make([]any, 0, len(list))
	for _, s := range list {
		entry := map[string]any{"name": s.mountName()}
		if s.Git != nil {
			entry["git"] = map[string]any{"url": s.Git.URL, "commit": s.Git.Commit}
			if s.Path != "" {
				entry["path"] = s.Path
			}
		} else {
			entry["oci"] = s.OCI
		}
		out = append(out, entry)
	}
	return out
}

// skillsFromValues reads the chart values' skills list.
func skillsFromValues(values map[string]any) Skills {
	raw, _ := values["skills"].([]any)
	var out Skills
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		s := Skill{}
		s.Name, _ = m["name"].(string)
		s.Path, _ = m["path"].(string)
		if git, ok := m["git"].(map[string]any); ok {
			g := &GitSkill{}
			g.URL, _ = git["url"].(string)
			g.Commit, _ = git["commit"].(string)
			s.Git = g
		}
		s.OCI, _ = m["oci"].(string)
		out = append(out, s)
	}
	return out
}

// skillsFromTemplate reads the AgentTemplate's spec.skills[] ({name, source:
// {git: {url, commit}, path} | {oci}}).
func skillsFromTemplate(tpl *unstructured.Unstructured) Skills {
	raw, _, _ := unstructured.NestedSlice(tpl.Object, "spec", "skills")
	var out Skills
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		s := Skill{}
		s.Name, _ = m["name"].(string)
		source, _ := m["source"].(map[string]any)
		s.Path, _ = source["path"].(string)
		if git, ok := source["git"].(map[string]any); ok {
			g := &GitSkill{}
			g.URL, _ = git["url"].(string)
			g.Commit, _ = git["commit"].(string)
			s.Git = g
		}
		s.OCI, _ = source["oci"].(string)
		out = append(out, s)
	}
	return out
}
