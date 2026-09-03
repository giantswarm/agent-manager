// Package skills discovers agent skills in git repositories the way the
// portal backend's /agent-skills endpoint does: every SKILL.md in a GitHub
// repository is one skill, its frontmatter name and description describe it,
// and its directory is the kagent spec.skills.gitRefs entry (url + path + ref)
// an agent mounts. Results are cached per repository for a short time so a
// meta agent listing skills repeatedly does not exhaust GitHub's rate limit.
package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
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
	// Ref is the git ref the skill was read from.
	Ref string `json:"ref"`
}

// GitRef is the skill as a chart skills.gitRefs entry.
func (s Skill) GitRef() map[string]string {
	name := s.Name
	if p := strings.Trim(s.Path, "/"); p != "" {
		parts := strings.Split(p, "/")
		name = parts[len(parts)-1]
	}
	out := map[string]string{"url": s.RepoURL, "name": name, "ref": s.Ref}
	if s.Path != "" {
		out["path"] = s.Path
	}
	return out
}

// Repository is the discovery result of one configured repository.
type Repository struct {
	RepoURL string  `json:"repoUrl"`
	Ref     string  `json:"ref,omitempty"`
	Skills  []Skill `json:"skills"`
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

// Config tunes the discoverer.
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
	cfg  Config
	http *http.Client
	log  *slog.Logger

	mu    sync.Mutex
	cache map[string]Repository
}

// New builds a discoverer.
func New(cfg Config, log *slog.Logger) *Discoverer {
	if cfg.APIURL == "" {
		cfg.APIURL = "https://api.github.com"
	}
	cfg.APIURL = strings.TrimRight(cfg.APIURL, "/")
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = 5 * time.Minute
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	if log == nil {
		log = slog.Default()
	}
	return &Discoverer{cfg: cfg, http: client, log: log, cache: map[string]Repository{}}
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

var repoURLRe = regexp.MustCompile(`^https://github\.com/([^/]+)/([^/]+?)(?:\.git)?/?$`)

// parseRepoURL splits https://github.com/<owner>/<repo>.
func parseRepoURL(repoURL string) (owner, repo string, err error) {
	m := repoURLRe.FindStringSubmatch(strings.TrimSpace(repoURL))
	if m == nil {
		return "", "", fmt.Errorf("not a github.com repository URL: %s (expected https://github.com/<owner>/<repo>)", repoURL)
	}
	return m[1], m[2], nil
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
	canonical := "https://github.com/" + owner + "/" + name
	branch := ref
	if branch == "" {
		var meta struct {
			DefaultBranch string `json:"default_branch"`
		}
		if err := d.getJSON(ctx, fmt.Sprintf("%s/repos/%s/%s", d.cfg.APIURL, owner, name), &meta); err != nil {
			return Repository{}, err
		}
		branch = meta.DefaultBranch
		if branch == "" {
			branch = "main"
		}
	}
	var tree struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"tree"`
		Truncated bool `json:"truncated"`
	}
	if err := d.getJSON(ctx, fmt.Sprintf("%s/repos/%s/%s/git/trees/%s?recursive=1", d.cfg.APIURL, owner, name, url.PathEscape(branch)), &tree); err != nil {
		return Repository{}, err
	}
	repo := Repository{RepoURL: canonical, Ref: branch, Skills: []Skill{}, Truncated: tree.Truncated}
	for _, entry := range tree.Tree {
		if entry.Type != "blob" || !isSkillFile(entry.Path) {
			continue
		}
		content, err := d.raw(ctx, owner, name, entry.Path, branch)
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

func (d *Discoverer) getJSON(ctx context.Context, target string, out any) error {
	body, err := d.get(ctx, target, "application/vnd.github+json")
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode %s: %w", target, err)
	}
	return nil
}

func (d *Discoverer) raw(ctx context.Context, owner, repo, path, ref string) (string, error) {
	segments := strings.Split(path, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	target := fmt.Sprintf("%s/repos/%s/%s/contents/%s?ref=%s", d.cfg.APIURL, owner, repo, strings.Join(segments, "/"), url.QueryEscape(ref))
	body, err := d.get(ctx, target, "application/vnd.github.raw+json")
	return string(body), err
}

func (d *Discoverer) get(ctx context.Context, target, accept string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if d.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+d.cfg.Token)
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", target, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API %s returned %d: %s", target, resp.StatusCode, strings.TrimSpace(firstLine(string(body))))
	}
	return body, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if len(s) > 200 {
		return s[:200]
	}
	return s
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
