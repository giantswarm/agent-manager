package agents

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agent-manager/internal/identity"
	"github.com/giantswarm/agent-manager/internal/oci"
	"github.com/giantswarm/agent-manager/internal/skillmanager"
	"github.com/giantswarm/agent-manager/internal/skills"
)

// A skill or a plugin is an immutable source: a git repository at a full
// commit id, or an OCI image by digest. Callers may still say "this branch",
// "this tag" or "this image tag" — agent-manager resolves the reference at
// write time and writes the pin. refreshSkills is the way to move every git
// skill and git plugin of an agent to the head of its repository's default
// branch again.

// SkillPinner pins skills and plugins in one call, in skill-manager's shape
// (POST /v1/resolve): a git item to a commit (its ref's; no ref: the
// pinner's default; a commit is kept), an OCI item to its digest. The answer
// holds the items in the order asked. A pinner that is not a ReadChecker
// refuses, as it pins, a repository the caller cannot read.
type SkillPinner interface {
	Resolve(ctx context.Context, skills, plugins []skillmanager.Item) ([]skillmanager.Item, []skillmanager.Item, error)
}

// ReadChecker is a SkillPinner that does not check readability as it pins:
// every newly given repository is checked through it first.
type ReadChecker interface {
	// RequireReadable refuses a repository login (the caller's GitHub login,
	// empty when unknown) cannot read: skills.ErrNotReadable.
	RequireReadable(ctx context.Context, repoURL, login string) error
}

// batchItem is a skill's or plugin's source as skill-manager takes it: a
// git source with its ref to resolve (with refresh, or without a commit),
// else its commit to keep.
func batchItem(at string, git *GitSkill, ociRef string, refresh bool) (skillmanager.Item, error) {
	if git == nil {
		return skillmanager.Item{OCI: &skillmanager.OCI{Ref: ociRef}}, nil
	}
	if !refresh && git.Commit != "" && git.Ref != "" {
		return skillmanager.Item{}, invalidf("%s gives both ref %q and commit %q; give the commit to pin, or the ref to have it resolved", at, git.Ref, git.Commit)
	}
	g := &skillmanager.Git{URL: git.URL, Ref: git.Ref}
	if !refresh && git.Commit != "" {
		g = &skillmanager.Git{URL: git.URL, Commit: git.Commit}
	}
	return skillmanager.Item{Git: g}, nil
}

// batchPinned is the pin answered for git or ociRef: a digest reference
// already given stays as written.
func batchPinned(git *GitSkill, ociRef string, got skillmanager.Item) (*GitSkill, string, error) {
	switch {
	case git != nil && got.Git != nil:
		return &GitSkill{URL: git.URL, Commit: got.Git.Commit}, "", nil
	case git == nil && got.OCI != nil:
		if skills.DigestRefPattern.MatchString(ociRef) {
			return nil, ociRef, nil
		}
		return nil, got.OCI.Ref, nil
	}
	return nil, "", fmt.Errorf("%w: the pinner answered another kind of source", skillmanager.ErrUnavailable)
}

// pinError names the item the pinner refused, else the lists sent.
func pinError(err error, skillAt, pluginAt func(int) string, nSkills, nPlugins int) error {
	list, item, cause := "", -1, err
	var refused *itemError
	var remote *skillmanager.ResolveError
	switch {
	case errors.As(err, &refused):
		list, item, cause = refused.list, refused.item, refused.err
	case errors.As(err, &remote):
		list, item = remote.List, remote.Item
	}
	switch {
	case list == skillmanager.ListSkills && item >= 0 && item < nSkills:
		return sourceError(skillAt(item), cause)
	case list == skillmanager.ListPlugins && item >= 0 && item < nPlugins:
		return sourceError(pluginAt(item), cause)
	case nPlugins == 0:
		return sourceError(skillmanager.ListSkills, err)
	case nSkills == 0:
		return sourceError(skillmanager.ListPlugins, err)
	}
	return sourceError(skillmanager.ListSkills+" and "+skillmanager.ListPlugins, err)
}

// sourceError marks a failed lookup of a skill or plugin source, named at:
// a repository the caller cannot read is forbidden, one that needs the
// person's grant of skill-manager's App a grant_required naming it,
// skill-manager refusing the bearer unauthenticated, skill-manager or GitHub
// behind it failing a backend error; anything else is an invalid reference.
func sourceError(at string, err error) error {
	if grant, ok := errors.AsType[*skillmanager.GrantRequiredError](err); ok {
		return &GrantRequiredError{App: grant.App, Reason: at + ": " + grant.Error()}
	}
	switch {
	case errors.Is(err, skills.ErrNotReadable):
		return fmt.Errorf("%w: %s: %w", ErrForbidden, at, err)
	case errors.Is(err, skillmanager.ErrUnauthenticated):
		return fmt.Errorf("%w: %s: %w", ErrUnauthenticated, at, err)
	case errors.Is(err, skillmanager.ErrUnavailable):
		return fmt.Errorf("%s: %w", at, err)
	}
	return fmt.Errorf("%w: %s: %w", ErrInvalid, at, err)
}

// skillManagerError maps a failed catalogue read of skill-manager.
func skillManagerError(err error) error {
	if grant, ok := errors.AsType[*skillmanager.GrantRequiredError](err); ok {
		return &GrantRequiredError{App: grant.App, Reason: grant.Error()}
	}
	switch {
	case errors.Is(err, skillmanager.ErrInvalid):
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	case errors.Is(err, skills.ErrNotReadable):
		return fmt.Errorf("%w: %w", ErrForbidden, err)
	case errors.Is(err, skillmanager.ErrUnauthenticated):
		return fmt.Errorf("%w: %w", ErrUnauthenticated, err)
	}
	return err
}

// gitHubLogin is the caller's verified GitHub login (the App-pinned
// registration), empty without one.
func gitHubLogin(ctx context.Context) string {
	if gh, ok := identity.GitHubFromContext(ctx); ok {
		return gh.Login
	}
	return ""
}

// requireReadableSkills refuses skills from a repository the caller cannot
// read: an agent never mounts a skill its creator could not read themselves,
// though the platform's credential could.
func requireReadableSkills(ctx context.Context, pinner SkillPinner, list Skills) error {
	seen := map[string]bool{}
	for _, sk := range list {
		if sk.Git == nil {
			continue
		}
		if err := requireReadableRepo(ctx, pinner, seen, sk.Git.URL, fmt.Sprintf("skill %q", sk.Name)); err != nil {
			return err
		}
	}
	return nil
}

// requireReadablePlugins is requireReadableSkills for git plugins.
func requireReadablePlugins(ctx context.Context, pinner SkillPinner, list Plugins) error {
	seen := map[string]bool{}
	for i, pl := range list {
		if pl.Git == nil {
			continue
		}
		if err := requireReadableRepo(ctx, pinner, seen, pl.Git.URL, fmt.Sprintf("plugins[%d]", i)); err != nil {
			return err
		}
	}
	return nil
}

// requireReadableRepo checks repoURL once per seen set; what names the
// entry. A pinner that is no ReadChecker checks as it pins.
func requireReadableRepo(ctx context.Context, pinner SkillPinner, seen map[string]bool, repoURL, what string) error {
	rc, ok := pinner.(ReadChecker)
	if !ok || seen[repoURL] {
		return nil
	}
	seen[repoURL] = true
	if err := rc.RequireReadable(ctx, repoURL, gitHubLogin(ctx)); err != nil {
		return sourceError(what, err)
	}
	return nil
}

// What the removed arguments are told.
const (
	toolNamesRemoved   = `toolNames never narrowed anything against muster (kagent filters muster's meta-tools only); declare a toolset instead, e.g. toolset: ["preset:read-only"]`
	runtimeRemoved     = `runtime is gone: the Harness an agent references is its runtime (the platform Harness, the Go ADK, unless harness names another of the namespace, such as claude for Claude Code); drop the argument`
	gitAuthRefRemoved  = `skills.gitAuthSecretName moved: the skills credential is the top-level gitAuthSecretName now (a Secret in the agent's namespace with the key token; omitted, the installation's), and skills is a list of {name, git: {url, ref | commit}, path} or {name, oci: <reference>}`
	skillsShapeChanged = `skills is a list now — [{name, git: {url, ref | commit}, path} | {name, oci: <registry>/<repository>:<tag>|@sha256:<digest>}] — not the 0.x object {refs, gitRefs}; every entry is pinned to a commit or a digest before it is written (list_skills reports the commits)`
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

// pinSources pins skills and plugins in one call of p: a git source to a
// full commit (its Commit when given, else the head of Ref, else of the
// default branch — with refresh, always the head of Ref or the default
// branch), an OCI source to its digest. Skill names are filled in. A
// reference that cannot be resolved is an error naming it; nothing is
// written by the caller then. Empty lists come back nil.
func pinSources(ctx context.Context, p SkillPinner, sk Skills, pl Plugins, refresh bool) (Skills, Plugins, error) {
	if len(sk) == 0 && len(pl) == 0 {
		return nil, nil, nil
	}
	if p == nil {
		p = InProcessPinner(nil)
	}
	skillAt := func(i int) string { return fmt.Sprintf("skills[%d] (%s)", i, sk[i].mountName()) }
	pluginAt := func(i int) string { return fmt.Sprintf("plugins[%d]", i) }
	skillItems := make([]skillmanager.Item, len(sk))
	for i, s := range sk {
		it, err := batchItem(skillAt(i), s.Git, s.OCI, refresh)
		if err != nil {
			return nil, nil, err
		}
		it.Name, it.Path = s.mountName(), s.Path
		skillItems[i] = it
	}
	pluginItems := make([]skillmanager.Item, len(pl))
	for i, plugin := range pl {
		it, err := batchItem(pluginAt(i), plugin.Git, plugin.OCI, refresh)
		if err != nil {
			return nil, nil, err
		}
		it.Path, it.Skills = plugin.Path, plugin.Skills
		pluginItems[i] = it
	}
	gotSkills, gotPlugins, err := p.Resolve(ctx, skillItems, pluginItems)
	if err != nil {
		return nil, nil, pinError(err, skillAt, pluginAt, len(sk), len(pl))
	}
	if len(gotSkills) != len(sk) || len(gotPlugins) != len(pl) {
		return nil, nil, fmt.Errorf("%w: the pinner answered other items than it was asked", skillmanager.ErrUnavailable)
	}
	var outSkills Skills
	for i, s := range sk {
		pinned := Skill{Name: s.mountName(), Path: s.Path}
		if pinned.Git, pinned.OCI, err = batchPinned(s.Git, s.OCI, gotSkills[i]); err != nil {
			return nil, nil, sourceError(skillAt(i), err)
		}
		outSkills = append(outSkills, pinned)
	}
	var outPlugins Plugins
	for i, plugin := range pl {
		pinned := Plugin{Path: plugin.Path, Skills: plugin.Skills}
		if pinned.Git, pinned.OCI, err = batchPinned(plugin.Git, plugin.OCI, gotPlugins[i]); err != nil {
			return nil, nil, sourceError(pluginAt(i), err)
		}
		outPlugins = append(outPlugins, pinned)
	}
	return outSkills, outPlugins, nil
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

// skillsFromObject reads the Agent object's spec.template.skills[] ({name,
// source: {git: {url, commit}, path} | {oci}}).
func skillsFromObject(obj *unstructured.Unstructured) Skills {
	raw, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "skills")
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

// PinSkills is the composer's pinning for other callers (the migrate command
// rewriting 0.x releases): every entry pinned to a commit or a digest and
// named the way create_agent names it, or an ErrInvalid naming the reference
// that could not be resolved.
func PinSkills(ctx context.Context, p SkillPinner, list Skills) (Skills, error) {
	pinned, _, err := pinSources(ctx, p, list, nil, false)
	return pinned, err
}

// SkillsValues renders a pinned skills list as the chart's `skills` value;
// nil when the list is empty.
func SkillsValues(list Skills) []any { return skillsValues(list) }
