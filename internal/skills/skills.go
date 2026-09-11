// Package skills discovers agent skills in git repositories the way the
// portal backend's /agent-skills endpoint does: every SKILL.md in a GitHub
// repository is one skill, its frontmatter name and description describe it,
// and its directory is the skill an agent mounts (url + path). kagent API v2
// pins skills to immutable sources, so the ref that was read is resolved to
// its head commit and reported next to it: the skills entry a listing yields
// feeds create_agent as is. Results are cached per repository for a short time
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
)

// Skill is one discovered skill, shaped as the portal returns it.
type Skill struct {
	// Name is the frontmatter name, else the directory (or repository) name.
	Name string `json:"name"`
	// Description is the frontmatter description ("" when absent).
	Description string `json:"description"`
	// RepoURL is the canonical https://github.com/<owner>/<repo>.
	RepoURL string `json:"repoUrl"`
	// Path is the skill directory; "" at the repository root.
	Path string `json:"path"`
	// Ref is the git ref (branch) the skill was read from.
	Ref string `json:"ref"`
	// Commit is the commit Ref resolved to when it was read: the immutable
	// reference an agent pins the skill to.
	Commit string `json:"commit"`
}

// Entry is the skill as a create_agent/update_agent skills entry: pinned to
// the commit it was read at.
func (s Skill) Entry() map[string]any {
	out := map[string]any{"name": s.mountName(), "git": map[string]any{"url": s.RepoURL, "commit": s.Commit}}
	if s.Path != "" {
		out["path"] = s.Path
	}
	return out
}

// mountName is the directory the skill mounts under: the last path segment,
// else the frontmatter name.
func (s Skill) mountName() string {
	if p := strings.Trim(s.Path, "/"); p != "" {
		parts := strings.Split(p, "/")
		return parts[len(parts)-1]
	}
	return s.Name
}

// Repository is the discovery result of one configured repository.
type Repository struct {
	RepoURL string `json:"repoUrl"`
	Ref     string `json:"ref,omitempty"`
	// Commit is what Ref resolved to when the repository was read.
	Commit string  `json:"commit,omitempty"`
	Skills []Skill `json:"skills"`
	// Truncated is true when GitHub capped the tree or a SKILL.md read failed:
	// some skills may be missing.
	Truncated bool `json:"truncated"`
	// Error is set when the repository could not be read at all.
	Error     string     `json:"error,omitempty"`
	FetchedAt *time.Time `json:"fetchedAt,omitempty"`
}

// Result is what list_skills returns.
type Result struct {
	Repositories []Repository `json:"repositories"`
	// Skills flattens every repository's skills (stable order).
	Skills []Skill `json:"skills"`
}

// Config tunes the discoverer and the resolver.
type Config struct {
	// Repositories are the configured skill repositories (github.com URLs).
	Repositories []string
	// APIURL is the GitHub API base (https://api.github.com; tests override).
	APIURL string
	// Token authenticates GitHub requests (private repositories, higher rate
	// limit); empty is anonymous.
	Token string
	// CacheTTL keeps a repository's result before it is re-read.
	CacheTTL time.Duration
	// HTTPClient overrides the client (tests).
	HTTPClient *http.Client
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
	return &Discoverer{cfg: cfg, gh: newGitHub(cfg.APIURL, cfg.Token, cfg.HTTPClient), log: log, cache: map[string]Repository{}}
}

// Repositories returns the configured repositories.
func (d *Discoverer) Repositories() []string { return append([]string(nil), d.cfg.Repositories...) }

// List discovers skills. repository narrows to one repository (configured or
// not — any github.com URL is accepted); ref overrides the default branch;
// refresh bypasses the cache.
func (d *Discoverer) List(ctx context.Context, repository, ref string, refresh bool) (*Result, error) {
	repos := d.cfg.Repositories
	if repository != "" {
		if _, _, err := parseRepoURL(repository); err != nil {
			return nil, err
		}
		repos = []string{repository}
	}
	res := &Result{Repositories: []Repository{}, Skills: []Skill{}}
	for _, r := range repos {
		repo := d.discover(ctx, r, ref, refresh)
		res.Repositories = append(res.Repositories, repo)
		res.Skills = append(res.Skills, repo.Skills...)
	}
	return res, nil
}

func (d *Discoverer) discover(ctx context.Context, repoURL, ref string, refresh bool) Repository {
	key := repoURL + "@" + ref
	d.mu.Lock()
	if cached, ok := d.cache[key]; ok && !refresh && cached.FetchedAt != nil && time.Since(*cached.FetchedAt) < d.cfg.CacheTTL {
		d.mu.Unlock()
		return cached
	}
	d.mu.Unlock()

	repo, err := d.read(ctx, repoURL, ref)
	now := time.Now()
	repo.FetchedAt = &now
	if err != nil {
		repo.RepoURL = repoURL
		repo.Ref = ref
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

func isSkillFile(path string) bool {
	return path == skillFile || strings.HasSuffix(path, "/"+skillFile)
}

func skillDir(path string) string {
	if path == skillFile {
		return ""
	}
	return strings.TrimSuffix(path, "/"+skillFile)
}

func (d *Discoverer) read(ctx context.Context, repoURL, ref string) (Repository, error) {
	owner, name, err := parseRepoURL(repoURL)
	if err != nil {
		return Repository{}, err
	}
	canonical := canonicalRepoURL(owner, name)
	branch := ref
	if branch == "" {
		if branch, err = d.gh.defaultBranch(ctx, owner, name); err != nil {
			return Repository{}, err
		}
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
	repo := Repository{RepoURL: canonical, Ref: branch, Commit: head, Skills: []Skill{}, Truncated: tree.Truncated}
	for _, entry := range tree.Tree {
		if entry.Type != "blob" || !isSkillFile(entry.Path) {
			continue
		}
		content, err := d.gh.raw(ctx, owner, name, entry.Path, head)
		if err != nil {
			d.log.Warn("skipping unreadable SKILL.md", "repository", canonical, "path", entry.Path, "error", err)
			repo.Truncated = true
			continue
		}
		dir := skillDir(entry.Path)
		fm := parseFrontmatter(content)
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
			RepoURL:     canonical,
			Path:        dir,
			Ref:         branch,
			Commit:      head,
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

// parseFrontmatter reads the YAML block between the leading `---` lines and
// returns its scalar string fields; anything else yields an empty map.
func parseFrontmatter(content string) map[string]string {
	out := map[string]string{}
	content = strings.TrimPrefix(content, "\uFEFF")
	lines := strings.Split(content, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return out
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return out
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(strings.Join(lines[1:end], "\n")), &doc); err != nil {
		return out
	}
	for k, v := range doc {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}
