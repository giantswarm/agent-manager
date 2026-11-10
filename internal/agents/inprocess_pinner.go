package agents

import (
	"context"
	"fmt"

	"github.com/giantswarm/agent-manager/internal/skillmanager"
	"github.com/giantswarm/agent-manager/internal/skills"
)

// SourceResolver resolves one source at a time in process, through the
// GitHub API and the registry (skills.Resolver). Deprecated: skill-manager
// pins skills.
type SourceResolver interface {
	// GitHead resolves ref of repoURL (empty: the default branch) to a commit.
	GitHead(ctx context.Context, repoURL, ref string) (string, error)
	// OCIDigest pins an OCI reference to <repository>@sha256:<digest>.
	OCIDigest(ctx context.Context, ref string) (string, error)
	// RequireReadable refuses a repository login (the caller's GitHub login,
	// empty when unknown) cannot read: skills.ErrNotReadable.
	RequireReadable(ctx context.Context, repoURL, login string) error
}

// InProcessPinner is the SkillPinner of r, item by item; a ReadChecker, and
// PluginFormats when r is. With a nil r, a git item needs its commit, an OCI
// tag stays as given and no repository is readable. Deprecated: skill-manager
// pins skills.
func InProcessPinner(r SourceResolver) SkillPinner { return inProcessPinner{r: r} }

type inProcessPinner struct{ r SourceResolver }

// itemError is the refusal of one item of a list.
type itemError struct {
	list string
	item int
	err  error
}

func (e *itemError) Error() string { return fmt.Sprintf("%s[%d]: %v", e.list, e.item, e.err) }

func (e *itemError) Unwrap() error { return e.err }

func (p inProcessPinner) Resolve(ctx context.Context, skillItems, pluginItems []skillmanager.Item) ([]skillmanager.Item, []skillmanager.Item, error) {
	gotSkills, err := p.pinList(ctx, skillmanager.ListSkills, skillItems)
	if err != nil {
		return nil, nil, err
	}
	gotPlugins, err := p.pinList(ctx, skillmanager.ListPlugins, pluginItems)
	if err != nil {
		return nil, nil, err
	}
	return gotSkills, gotPlugins, nil
}

func (p inProcessPinner) pinList(ctx context.Context, list string, items []skillmanager.Item) ([]skillmanager.Item, error) {
	out := make([]skillmanager.Item, len(items))
	for i, it := range items {
		pinned, err := p.pin(ctx, it)
		if err != nil {
			return nil, &itemError{list: list, item: i, err: err}
		}
		out[i] = pinned
	}
	return out, nil
}

func (p inProcessPinner) pin(ctx context.Context, it skillmanager.Item) (skillmanager.Item, error) {
	switch {
	case it.OCI != nil:
		if skills.DigestRefPattern.MatchString(it.OCI.Ref) || p.r == nil {
			return it, nil
		}
		digest, err := p.r.OCIDigest(ctx, it.OCI.Ref)
		if err != nil {
			return it, err
		}
		it.OCI = &skillmanager.OCI{Ref: digest}
	case it.Git != nil && it.Git.Commit == "":
		if p.r == nil {
			return it, fmt.Errorf("no resolver to look up %s — give git.commit", describeRef(it.Git.Ref))
		}
		commit, err := p.r.GitHead(ctx, it.Git.URL, it.Git.Ref)
		if err != nil {
			return it, err
		}
		it.Git = &skillmanager.Git{URL: it.Git.URL, Commit: commit}
	}
	return it, nil
}

func (p inProcessPinner) RequireReadable(ctx context.Context, repoURL, login string) error {
	if p.r == nil {
		return fmt.Errorf("%w: no resolver to check that %s can be read", skills.ErrNotReadable, repoURL)
	}
	return p.r.RequireReadable(ctx, repoURL, login)
}

// ClaudeFormatPlugin answers through r; a resolver that cannot tell
// reports none Claude-format.
func (p inProcessPinner) ClaudeFormatPlugin(ctx context.Context, repoURL, commit, at string) (bool, error) {
	formats, ok := p.r.(PluginFormats)
	if !ok {
		return false, nil
	}
	return formats.ClaudeFormatPlugin(ctx, repoURL, commit, at)
}
