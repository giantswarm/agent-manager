// Package skills discovers agent skills in git repositories the way the
// portal backend's /agent-skills endpoint does: every SKILL.md in a GitHub
// repository is one skill, its frontmatter name and description describe it,
// and its directory is the skill an agent mounts (url + path). kagent API v2
// pins skills to immutable sources, so the ref that was read is resolved to
// its head commit and reported on the repository: the commit a skills entry
// of create_agent pins. Results are cached per repository for a short time
// so a meta agent listing skills repeatedly does not exhaust GitHub's rate
// limit. The Resolver (resolve.go) is the same GitHub path for the composer:
// a branch or tag to its head commit, a repository to its default branch.
package skills

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/giantswarm/agent-manager/internal/tags"
)

// Skill is one discovered skill. Its repository carries the URL, the ref and
// the commit it was read at.
type Skill struct {
	// Name is the frontmatter name, else the directory (or repository) name.
	Name string `json:"name"`
	// Description is the frontmatter description ("" when absent).
	Description string `json:"description"`
	// Path is the skill directory; "" at the repository root.
	Path string `json:"path"`
	// Tags are the frontmatter's metadata.tags, comma-joined there; a tag
	// the agent chart's tag rule refuses is dropped.
	Tags []string `json:"tags,omitempty"`
}

// Repository is the discovery result of one configured repository.
type Repository struct {
	RepoURL string `json:"repoUrl"`
	Ref     string `json:"ref,omitempty"`
	// Path is the directory the skills were discovered below; "" for the
	// whole repository. Skill paths stay repository-relative.
	Path string `json:"path,omitempty"`
	// ReadOnly is the source's readOnly, reported for consumers that write
	// skills; agent-manager has no write path into skill repositories.
	ReadOnly bool `json:"readOnly,omitempty"`
	// Commit is what Ref resolved to when the repository was read.
	Commit string  `json:"commit,omitempty"`
	Skills []Skill `json:"skills"`
	// Private is true for a repository only its collaborators can read:
	// list_skills shows it only to a caller who can read it.
	Private bool `json:"private,omitempty"`
	// Truncated is true when GitHub capped the tree or a SKILL.md read failed:
	// some skills may be missing.
	Truncated bool `json:"truncated"`
	// Error is set when the repository could not be read at all.
	Error     string     `json:"error,omitempty"`
	FetchedAt *time.Time `json:"fetchedAt,omitempty"`
}

// Result is what list_skills returns: every skill once, under its repository.
type Result struct {
	Repositories []Repository `json:"repositories"`
}

// Config tunes the discoverer and the resolver.
type Config struct {
	// Repositories are the configured skill repositories (github.com URLs,
	// each with an optional ref, path and readOnly).
	Repositories []Source
	// APIURL is the GitHub API base (https://api.github.com; tests override).
	APIURL string
	// Tokens authenticates GitHub requests (private repositories, higher
	// rate limit): a GitHub App's installation tokens or a StaticToken; nil
	// is anonymous.
	Tokens TokenSource
	// CacheTTL keeps a repository's result before it is re-read.
	CacheTTL time.Duration
	// HTTPClient overrides the client (tests).
	HTTPClient *http.Client
	// Tags is the agent chart's tag rule, which a skill's tags follow as an
	// agent's do; nil: skills carry no tags.
	Tags *tags.Rule
}

// Discoverer reads skills from GitHub with a per-repository cache.
type Discoverer struct {
	cfg Config
	gh  *github
	log *slog.Logger

	mu    sync.Mutex
	cache map[string]Repository
}

// New builds a discoverer.
func New(cfg Config, log *slog.Logger) *Discoverer {
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = 5 * time.Minute
	}
	if log == nil {
		log = slog.Default()
	}
	return &Discoverer{cfg: cfg, gh: newGitHub(cfg.APIURL, cfg.Tokens, cfg.HTTPClient), log: log, cache: map[string]Repository{}}
}

// Repositories returns the configured repositories' URLs, each once; a
// consumer that needs the ref, path or readOnly reads Sources.
func (d *Discoverer) Repositories() []string {
	out := make([]string, 0, len(d.cfg.Repositories))
	seen := map[string]bool{}
	for _, s := range d.cfg.Repositories {
		if !seen[s.URL] {
			seen[s.URL] = true
			out = append(out, s.URL)
		}
	}
	return out
}

// Sources returns the configured repositories.
func (d *Discoverer) Sources() []Source { return append([]Source(nil), d.cfg.Repositories...) }

// configured returns the configured sources of repoURL, compared by their
// canonical URL.
func (d *Discoverer) configured(repoURL string) []Source {
	owner, name, err := parseRepoURL(repoURL)
	if err != nil {
		return nil
	}
	want := strings.ToLower(canonicalRepoURL(owner, name))
	var out []Source
	for _, s := range d.cfg.Repositories {
		if o, n, err := parseRepoURL(s.URL); err == nil && strings.ToLower(canonicalRepoURL(o, n)) == want {
			out = append(out, s)
		}
	}
	return out
}

// SourceRef is the ref of the configured source a skill at path of repoURL
// comes from (the deepest source path holding it); "" when no configured
// source holds it or that source follows the default branch.
func (d *Discoverer) SourceRef(repoURL, path string) string {
	path = strings.Trim(path, "/")
	best, ref := -1, ""
	for _, s := range d.configured(repoURL) {
		if (path == s.Path || within(path, s.Path)) && len(s.Path) > best {
			best, ref = len(s.Path), s.Ref
		}
	}
	return ref
}

// List discovers skills. repository narrows to one repository (configured or
// not — any github.com URL is accepted; a configured one keeps its ref and
// path); ref overrides the source's ref (default: the default branch);
// refresh bypasses the cache.
func (d *Discoverer) List(ctx context.Context, repository, ref string, refresh bool) (*Result, error) {
	sources := d.cfg.Repositories
	if repository != "" {
		if _, _, err := parseRepoURL(repository); err != nil {
			return nil, err
		}
		sources = d.configured(repository)
		if len(sources) == 0 {
			sources = []Source{{URL: repository}}
		}
	}
	found := make([]Repository, len(sources))
	var wg sync.WaitGroup
	for i, src := range sources {
		if ref != "" {
			src.Ref = ref
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			found[i] = d.discover(ctx, src, refresh)
		}()
	}
	wg.Wait()
	return &Result{Repositories: found}, nil
}

func (d *Discoverer) discover(ctx context.Context, src Source, refresh bool) Repository {
	repoURL, ref := src.URL, src.Ref
	key := repoURL + "@" + ref + "#" + src.Path
	d.mu.Lock()
	cached, ok := d.cache[key]
	d.mu.Unlock()
	if ok && !refresh && cached.FetchedAt != nil && time.Since(*cached.FetchedAt) < d.cfg.CacheTTL {
		return cached
	}

	// The read outlives the caller: a client that gives up (muster answers
	// after 30 s) must not leave a half-read listing in the cache, and the
	// next call finds the finished one.
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), readTimeout)
	defer cancel()
	// An expired entry whose ref still points at the same commit is renewed
	// with two requests instead of re-reading every SKILL.md.
	if ok && !refresh && cached.Error == "" && !cached.Truncated && cached.Commit != "" {
		if head, err := d.headOf(readCtx, repoURL, ref); err == nil && head == cached.Commit {
			now := time.Now()
			cached.FetchedAt = &now
			d.mu.Lock()
			d.cache[key] = cached
			d.mu.Unlock()
			return cached
		}
	}

	repo, err := d.read(readCtx, src)
	now := time.Now()
	repo.FetchedAt = &now
	if err != nil {
		repo.RepoURL = repoURL
		repo.Ref = ref
		repo.Path = src.Path
		repo.ReadOnly = src.ReadOnly
		repo.Skills = []Skill{}
		repo.Error = err.Error()
		d.log.Warn("skill discovery failed", "repository", repoURL, "error", err)
	}
	d.mu.Lock()
	d.cache[key] = repo
	d.mu.Unlock()
	return repo
}

const skillFile = "SKILL.md"

// readTimeout bounds one repository read; readParallelism bounds the SKILL.md
// requests in flight per repository.
const (
	readTimeout     = 2 * time.Minute
	readParallelism = 8
)

// headOf resolves ref (the default branch when empty) to its head commit.
func (d *Discoverer) headOf(ctx context.Context, repoURL, ref string) (string, error) {
	owner, name, err := parseRepoURL(repoURL)
	if err != nil {
		return "", err
	}
	branch := ref
	if branch == "" {
		meta, err := d.gh.repository(ctx, owner, name)
		if err != nil {
			return "", err
		}
		branch = meta.DefaultBranch
	}
	return d.gh.headCommit(ctx, owner, name, branch)
}

func isSkillFile(path string) bool {
	return path == skillFile || strings.HasSuffix(path, "/"+skillFile)
}

// within reports whether path lies below dir ("" is the whole repository).
func within(path, dir string) bool {
	return dir == "" || strings.HasPrefix(path, dir+"/")
}

func skillDir(path string) string {
	if path == skillFile {
		return ""
	}
	return strings.TrimSuffix(path, "/"+skillFile)
}

func (d *Discoverer) read(ctx context.Context, src Source) (Repository, error) {
	owner, name, err := parseRepoURL(src.URL)
	if err != nil {
		return Repository{}, err
	}
	canonical := canonicalRepoURL(owner, name)
	meta, err := d.gh.repository(ctx, owner, name)
	if err != nil {
		return Repository{}, err
	}
	branch := src.Ref
	if branch == "" {
		branch = meta.DefaultBranch
	}
	// The commit the ref points at right now: what an agent pins. The tree
	// and the files are read at that commit, not the branch, so the listing
	// stays consistent even when the branch moves between the calls.
	head, err := d.gh.headCommit(ctx, owner, name, branch)
	if err != nil {
		return Repository{}, err
	}
	var tree struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"tree"`
		Truncated bool `json:"truncated"`
	}
	if err := d.gh.getJSON(ctx, d.gh.apiURL+"/repos/"+owner+"/"+name+"/git/trees/"+head+"?recursive=1", &tree); err != nil {
		return Repository{}, err
	}
	repo := Repository{RepoURL: canonical, Ref: branch, Path: src.Path, ReadOnly: src.ReadOnly, Commit: head, Private: meta.Private, Skills: []Skill{}, Truncated: tree.Truncated}
	var paths []string
	for _, entry := range tree.Tree {
		if entry.Type == "blob" && isSkillFile(entry.Path) && within(entry.Path, src.Path) {
			paths = append(paths, entry.Path)
		}
	}
	contents := make([]string, len(paths))
	errs := make([]error, len(paths))
	sem := make(chan struct{}, readParallelism)
	var wg sync.WaitGroup
	for i, path := range paths {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			contents[i], errs[i] = d.gh.raw(ctx, owner, name, path, head)
		}()
	}
	wg.Wait()
	for i, path := range paths {
		if errs[i] != nil {
			d.log.Warn("skipping unreadable SKILL.md", "repository", canonical, "path", path, "error", errs[i])
			repo.Truncated = true
			continue
		}
		content := contents[i]
		dir := skillDir(path)
		doc := frontmatter(content)
		fm := scalars(doc)
		skillName := strings.TrimSpace(fm["name"])
		if skillName == "" {
			if dir != "" {
				parts := strings.Split(dir, "/")
				skillName = parts[len(parts)-1]
			} else {
				skillName = name
			}
		}
		repo.Skills = append(repo.Skills, Skill{
			Name:        skillName,
			Description: strings.TrimSpace(fm["description"]),
			Path:        dir,
			Tags:        metadataTags(doc, d.cfg.Tags),
		})
	}
	sort.Slice(repo.Skills, func(i, j int) bool {
		if repo.Skills[i].Path != repo.Skills[j].Path {
			return repo.Skills[i].Path < repo.Skills[j].Path
		}
		return repo.Skills[i].Name < repo.Skills[j].Name
	})
	return repo, nil
}

// frontmatter parses the YAML block between the leading `---` lines; nil
// when there is none or it does not parse.
func frontmatter(content string) map[string]any {
	content = strings.TrimPrefix(content, "\uFEFF")
	lines := strings.Split(content, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return nil
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return nil
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(strings.Join(lines[1:end], "\n")), &doc); err != nil {
		return nil
	}
	return doc
}

// scalars keeps the frontmatter's string fields.
func scalars(doc map[string]any) map[string]string {
	out := map[string]string{}
	for k, v := range doc {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// metadataTags reads metadata.tags, a comma-joined string (the Agent Skills
// spec allows only string values in metadata), with the chart's tag rule: a
// malformed tag is dropped, never the skill.
func metadataTags(doc map[string]any, rule *tags.Rule) []string {
	metadata, _ := doc["metadata"].(map[string]any)
	raw, _ := metadata["tags"].(string)
	return rule.Parse(raw)
}
