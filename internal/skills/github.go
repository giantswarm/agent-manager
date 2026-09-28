package skills

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// github is the GitHub REST client the discoverer and the resolver share: one
// base URL (tests override it), one token source, one HTTP client.
type github struct {
	apiURL string
	tokens TokenSource
	http   *http.Client
}

func newGitHub(apiURL string, tokens TokenSource, client *http.Client) *github {
	if apiURL == "" {
		apiURL = "https://api.github.com"
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	if tokens == nil {
		tokens = StaticToken("")
	}
	return &github{apiURL: strings.TrimRight(apiURL, "/"), tokens: tokens, http: client}
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

// canonicalRepoURL is https://github.com/<owner>/<repo> without .git or a
// trailing slash.
func canonicalRepoURL(owner, repo string) string {
	return "https://github.com/" + owner + "/" + repo
}

// repoMeta is what the repository endpoint says about a repository.
type repoMeta struct {
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
}

// repository reads the repository's default branch and visibility.
func (g *github) repository(ctx context.Context, owner, repo string) (repoMeta, error) {
	var meta repoMeta
	if err := g.getJSON(ctx, fmt.Sprintf("%s/repos/%s/%s", g.apiURL, owner, repo), &meta); err != nil {
		return repoMeta{}, err
	}
	if meta.DefaultBranch == "" {
		meta.DefaultBranch = "main"
	}
	return meta, nil
}

// defaultBranch reads the repository's default branch.
func (g *github) defaultBranch(ctx context.Context, owner, repo string) (string, error) {
	meta, err := g.repository(ctx, owner, repo)
	return meta.DefaultBranch, err
}

// permission is login's permission on the repository (admin, write, read or
// none); a login GitHub does not know as a collaborator is none.
func (g *github) permission(ctx context.Context, owner, repo, login string) (string, error) {
	var out struct {
		Permission string `json:"permission"`
	}
	err := g.getJSON(ctx, fmt.Sprintf("%s/repos/%s/%s/collaborators/%s/permission", g.apiURL, owner, repo, url.PathEscape(login)), &out)
	var api *apiError
	if errors.As(err, &api) && api.status == http.StatusNotFound {
		return "none", nil
	}
	if err != nil {
		return "", err
	}
	return out.Permission, nil
}

// headCommit resolves a ref (branch, tag or commit) to the commit it points
// at right now.
func (g *github) headCommit(ctx context.Context, owner, repo, ref string) (string, error) {
	var head struct {
		SHA string `json:"sha"`
	}
	if err := g.getJSON(ctx, fmt.Sprintf("%s/repos/%s/%s/commits/%s", g.apiURL, owner, repo, url.PathEscape(ref)), &head); err != nil {
		return "", err
	}
	if head.SHA == "" {
		return "", fmt.Errorf("GitHub reported no commit for %s/%s@%s", owner, repo, ref)
	}
	return head.SHA, nil
}

func (g *github) getJSON(ctx context.Context, target string, out any) error {
	body, err := g.get(ctx, target, "application/vnd.github+json")
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode %s: %w", target, err)
	}
	return nil
}

func (g *github) raw(ctx context.Context, owner, repo, path, ref string) (string, error) {
	segments := strings.Split(path, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	target := fmt.Sprintf("%s/repos/%s/%s/contents/%s?ref=%s", g.apiURL, owner, repo, strings.Join(segments, "/"), url.QueryEscape(ref))
	body, err := g.get(ctx, target, "application/vnd.github.raw+json")
	return string(body), err
}

// apiError is a non-200 answer of the GitHub API, kept so callers can tell a
// missing or private repository (404) from other failures.
type apiError struct {
	status int
	target string
	body   string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("GitHub API %s returned %d: %s", e.target, e.status, e.body)
}

func (g *github) get(ctx context.Context, target, accept string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	token, err := g.tokens.Token(ctx)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", target, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &apiError{status: resp.StatusCode, target: target, body: strings.TrimSpace(firstLine(string(body)))}
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
