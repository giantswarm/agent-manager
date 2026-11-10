// Package skillmanager is agent-manager's client of skill-manager's REST API
// (giantswarm/skill-manager, docs/api.md): it pins skills and plugins through
// POST /v1/resolve and lists the skill catalogue through GET /v1/skills, as
// the caller.
package skillmanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"k8s.io/utils/lru"

	"github.com/giantswarm/agent-manager/internal/identity"
	"github.com/giantswarm/agent-manager/internal/skills"
)

// The errors of a call beyond skill-manager's 400, 403 and 404, which wrap
// skills.ErrUnresolvable (400 and 404 on a resolve), skills.ErrNotReadable
// (403) and ErrInvalid (400 and 404 on a list).
var (
	// ErrUnavailable: skill-manager, or GitHub or a registry behind it,
	// could not answer (5xx, unreachable, or an answer that is not the API's).
	ErrUnavailable = errors.New("skill-manager unavailable")
	// ErrUnauthenticated: skill-manager refused the bearer (401).
	ErrUnauthenticated = errors.New("skill-manager refused the bearer")
	// ErrInvalid: skill-manager refused a list request (400, 404), e.g. a
	// source it does not serve.
	ErrInvalid = errors.New("skill-manager refused the request")
)

// resolvePath is skill-manager's pinning call.
const resolvePath = "/v1/resolve"

// The lists of a resolve, as its errors name them.
const (
	ListSkills  = "skills"
	ListPlugins = "plugins"
)

// ResolveError is skill-manager refusing one item of a resolve: Item is its
// index in List (-1 when the answer names none). It wraps the sentinel of
// its status.
type ResolveError struct {
	List    string
	Item    int
	Status  int
	Message string
	kind    error
}

func (e *ResolveError) Error() string {
	return fmt.Sprintf("%v: skill-manager (%d): %s", e.kind, e.Status, e.Message)
}

func (e *ResolveError) Unwrap() error { return e.kind }

// Config configures the client.
type Config struct {
	// URL is skill-manager's base URL (http://skill-manager.agent-platform.svc:8080).
	URL string
	// Token is the bearer of a resolve with no caller on its context: the
	// service token of the migrate command. Empty sends none.
	Token string
	// HTTPClient overrides the client (tests); default 30s timeout.
	HTTPClient *http.Client
}

// Client calls skill-manager as the caller.
type Client struct {
	base  *url.URL
	token string
	http  *http.Client

	// formats are the git plugins' formats resolve reported, by
	// formatKey.
	formats *lru.Cache
}

// New builds the client of skill-manager at cfg.URL.
func New(cfg Config) (*Client, error) {
	u, err := url.Parse(strings.TrimSuffix(cfg.URL, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("skill-manager URL %q must be an http(s) URL", cfg.URL)
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{base: u, token: cfg.Token, http: hc, formats: lru.New(maxFormats)}, nil
}

// URL is skill-manager's base URL.
func (c *Client) URL() string { return c.base.String() }

// Git is a git source: a URL with a ref to resolve or a commit to keep.
type Git struct {
	URL    string `json:"url"`
	Ref    string `json:"ref,omitempty"`
	Commit string `json:"commit,omitempty"`
}

// OCI is an image source.
type OCI struct {
	Ref string `json:"ref"`
}

// Item is a skill or plugin of a resolve, in skill-manager's shape.
type Item struct {
	Name   string   `json:"name,omitempty"`
	Path   string   `json:"path,omitempty"`
	Skills []string `json:"skills,omitempty"`
	// ClaudeFormat answers, for a git plugin, whether it is Claude-format;
	// true in a request refuses one that is not.
	ClaudeFormat *bool `json:"claudeFormat,omitempty"`
	Git          *Git  `json:"git,omitempty"`
	OCI          *OCI  `json:"oci,omitempty"`
}

type resolveBody struct {
	Skills  []Item `json:"skills"`
	Plugins []Item `json:"plugins"`
}

// Resolve pins skills and plugins in one call, as the caller: a git item to
// a commit (its ref's; no ref: the configured source's holding its path,
// else the default branch; a commit is kept), an OCI item to its digest.
// skill-manager refuses a repository the caller cannot read and a path
// missing at the commit. Every git plugin's format is remembered for
// ClaudeFormatPlugin.
func (c *Client) Resolve(ctx context.Context, skillItems, pluginItems []Item) ([]Item, []Item, error) {
	if skillItems == nil {
		skillItems = []Item{}
	}
	if pluginItems == nil {
		pluginItems = []Item{}
	}
	raw, err := c.do(ctx, http.MethodPost, resolvePath, nil, resolveBody{Skills: skillItems, Plugins: pluginItems})
	if err != nil {
		return nil, nil, err
	}
	var out resolveBody
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Skills) != len(skillItems) || len(out.Plugins) != len(pluginItems) {
		return nil, nil, fmt.Errorf("%w: POST /v1/resolve answered other items than it was asked", ErrUnavailable)
	}
	for list, items := range map[string][]Item{ListSkills: out.Skills, ListPlugins: out.Plugins} {
		for i, it := range items {
			switch {
			case it.Git != nil && !skills.CommitPattern.MatchString(it.Git.Commit):
				return nil, nil, fmt.Errorf("%w: POST /v1/resolve answered no commit for %s[%d]", ErrUnavailable, list, i)
			case it.OCI != nil && !skills.DigestRefPattern.MatchString(it.OCI.Ref):
				return nil, nil, fmt.Errorf("%w: POST /v1/resolve answered no digest for %s[%d]", ErrUnavailable, list, i)
			case it.Git == nil && it.OCI == nil:
				return nil, nil, fmt.Errorf("%w: POST /v1/resolve answered no source for %s[%d]", ErrUnavailable, list, i)
			}
		}
	}
	for i, it := range out.Plugins {
		if it.Git == nil || it.ClaudeFormat == nil {
			continue
		}
		// Under the item as skill-manager answered it and as it was asked
		// (the URL and path an agent's values keep).
		c.formats.Add(formatKey(it.Git.URL, it.Git.Commit, it.Path), *it.ClaudeFormat)
		if asked := pluginItems[i]; asked.Git != nil {
			c.formats.Add(formatKey(asked.Git.URL, it.Git.Commit, asked.Path), *it.ClaudeFormat)
		}
	}
	return out.Skills, out.Plugins, nil
}

// GitHead resolves ref of repoURL (empty: the default branch) to a commit.
func (c *Client) GitHead(ctx context.Context, repoURL, ref string) (string, error) {
	got, _, err := c.Resolve(ctx, []Item{{Name: itemName(repoURL), Git: &Git{URL: repoURL, Ref: ref}}}, nil)
	if err != nil {
		return "", err
	}
	if got[0].Git == nil {
		return "", errOtherSource
	}
	return got[0].Git.Commit, nil
}

// OCIDigest pins an OCI reference to <repository>@sha256:<digest>.
func (c *Client) OCIDigest(ctx context.Context, ref string) (string, error) {
	got, _, err := c.Resolve(ctx, []Item{{Name: itemName(ref), OCI: &OCI{Ref: ref}}}, nil)
	if err != nil {
		return "", err
	}
	if got[0].OCI == nil {
		return "", errOtherSource
	}
	return got[0].OCI.Ref, nil
}

var errOtherSource = fmt.Errorf("%w: POST /v1/resolve answered another kind of source", ErrUnavailable)

// RequireReadable is a no-op: skill-manager checks readability as it
// resolves, which every write does for every git item.
func (c *Client) RequireReadable(context.Context, string, string) error { return nil }

// ClaudeFormatPlugin answers from the formats resolve reported, else
// resolves the plugin at commit to learn it.
func (c *Client) ClaudeFormatPlugin(ctx context.Context, repoURL, commit, at string) (bool, error) {
	if claude, ok := c.formats.Get(formatKey(repoURL, commit, at)); ok {
		return claude.(bool), nil
	}
	_, got, err := c.Resolve(ctx, nil, []Item{{Path: at, Git: &Git{URL: repoURL, Commit: commit}}})
	if err != nil {
		return false, err
	}
	if got[0].ClaudeFormat == nil {
		return false, fmt.Errorf("%w: POST /v1/resolve answered no claudeFormat for the plugin %s", ErrUnavailable, repoURL)
	}
	return *got[0].ClaudeFormat, nil
}

// maxFormats bounds the remembered plugin formats.
const maxFormats = 4096

// formatKey keys a plugin's format by its repository, commit and path, the
// path cleaned as skill-manager cleans it ("./p/" is "p", "." is "").
func formatKey(repoURL, commit, at string) string {
	at = path.Clean("/" + strings.TrimSpace(at))[1:]
	return strings.TrimSpace(repoURL) + "@" + commit + ":" + at
}

// Skills is GET /v1/skills: the catalogue of every configured source, or of
// source alone; refresh bypasses skill-manager's cache.
func (c *Client) Skills(ctx context.Context, source string, refresh bool) (map[string]any, error) {
	q := url.Values{}
	if source != "" {
		q.Set("source", source)
	}
	if refresh {
		q.Set("refresh", "true")
	}
	raw, err := c.do(ctx, http.MethodGet, "/v1/skills", q, nil)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil || out == nil {
		return nil, fmt.Errorf("%w: GET /v1/skills answered no JSON object", ErrUnavailable)
	}
	return out, nil
}

// maxAnswer bounds what is read of an answer.
const maxAnswer = 8 << 20

func (c *Client) do(ctx context.Context, method, p string, q url.Values, body any) ([]byte, error) {
	u := *c.base
	u.Path = path.Join(u.Path, p)
	u.RawQuery = q.Encode()
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if err := c.authorize(ctx, req, p); err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %s %s: %v", ErrUnavailable, method, p, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return nil, fmt.Errorf("%w: %s %s: %v", ErrUnavailable, method, p, err)
	}
	if resp.StatusCode/100 == 2 {
		return raw, nil
	}
	return nil, answerError(method, p, resp.StatusCode, raw)
}

// authorize sets the bearer as agent-manager received it: on the GitHub
// registration the person's GitHub token with the ID token in muster's
// forwarded header, else the ID token; with no caller, the service token on
// POST /v1/resolve, the one call skill-manager takes it on. skill-manager
// refuses a GitHub token alone, so such a call is refused here.
func (c *Client) authorize(ctx context.Context, req *http.Request, p string) error {
	idToken, _ := identity.TokenFromContext(ctx)
	if gh, ok := identity.GitHubFromContext(ctx); ok {
		if idToken == "" {
			return fmt.Errorf("%w: the call carries a GitHub token but no ID token, which skill-manager needs beside it (muster forwards it with auth.forwardIdentity: true)", ErrUnauthenticated)
		}
		req.Header.Set("Authorization", "Bearer "+gh.Token)
		req.Header.Set(identity.ForwardedIdentityHeader, idToken)
		return nil
	}
	bearer := idToken
	if bearer == "" && p == resolvePath {
		bearer = c.token
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return nil
}

// answerError maps skill-manager's refusal
// ({"error": "...", "item": n, "list": "skills" | "plugins"}).
func answerError(method, p string, status int, raw []byte) error {
	var body struct {
		Error string `json:"error"`
		Item  *int   `json:"item"`
		List  string `json:"list"`
	}
	msg := strings.TrimSpace(string(raw))
	if json.Unmarshal(raw, &body) == nil && body.Error != "" {
		msg = body.Error
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	resolve := p == resolvePath
	var kind error
	switch {
	case status == http.StatusForbidden:
		kind = skills.ErrNotReadable
	case status == http.StatusUnauthorized:
		kind = ErrUnauthenticated
	case resolve && (status == http.StatusBadRequest || status == http.StatusNotFound):
		kind = skills.ErrUnresolvable
	case status == http.StatusBadRequest || status == http.StatusNotFound:
		kind = ErrInvalid
	default:
		kind = ErrUnavailable
	}
	if !resolve {
		return fmt.Errorf("%w: skill-manager %s %s (%d): %s", kind, method, p, status, msg)
	}
	e := &ResolveError{List: body.List, Item: -1, Status: status, Message: msg, kind: kind}
	if body.Item != nil {
		e.Item = *body.Item
	}
	return e
}

// itemName names a single-source item: the last segment of its repository
// or image.
func itemName(source string) string {
	source = strings.TrimSuffix(strings.Trim(source, "/"), ".git")
	if i := strings.LastIndex(source, "/"); i >= 0 {
		source = source[i+1:]
	}
	if i := strings.IndexAny(source, ":@"); i > 0 {
		source = source[:i]
	}
	return source
}
