// Package skillmanager is agent-manager's client of skill-manager's REST API
// (giantswarm/skill-manager): it pins skills and plugins through
// POST /v1/resolve and lists the skill catalogue through GET /v1/skills, as
// the caller. Its Client is an agents.SkillPinner.
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

	"github.com/giantswarm/agent-manager/internal/identity"
	"github.com/giantswarm/agent-manager/internal/skills"
)

// The errors of a call beyond skill-manager's 400, 403 and 404, which wrap
// skills.ErrUnresolvable (400 and 404 on a resolve), skills.ErrNotReadable
// (403) and ErrInvalid (400 on a list).
var (
	// ErrUnavailable: skill-manager, or GitHub behind it, could not answer
	// (5xx, unreachable, or an answer that is not the contract's).
	ErrUnavailable = errors.New("skill-manager unavailable")
	// ErrUnauthenticated: skill-manager refused the bearer (401).
	ErrUnauthenticated = errors.New("skill-manager refused the bearer")
	// ErrInvalid: skill-manager refused a list request (400), e.g. a source
	// it does not serve.
	ErrInvalid = errors.New("skill-manager refused the request")
)

// forwardedIdentityHeader is muster's forwarded ID token header
// (server.ForwardedIdentityHeader), sent next to a GitHub bearer.
const forwardedIdentityHeader = "X-Muster-Id-Token"

// PluginFormats tells a Claude-format plugin (agents.PluginFormats);
// skill-manager's API has no such call.
type PluginFormats interface {
	ClaudeFormatPlugin(ctx context.Context, repoURL, commit, path string) (bool, error)
}

// Config configures the client.
type Config struct {
	// URL is skill-manager's base URL (http://skill-manager.agent-platform.svc:8080).
	URL string
	// Token is the bearer of a call with no caller on its context (the
	// migrate command); empty sends none.
	Token string
	// Formats answers ClaudeFormatPlugin; nil takes no plugin for
	// Claude-format.
	Formats PluginFormats
	// HTTPClient overrides the client (tests); default 30s timeout.
	HTTPClient *http.Client
}

// Client calls skill-manager as the caller.
type Client struct {
	base    *url.URL
	token   string
	formats PluginFormats
	http    *http.Client
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
	return &Client{base: u, token: cfg.Token, formats: cfg.Formats, http: hc}, nil
}

// URL is skill-manager's base URL.
func (c *Client) URL() string { return c.base.String() }

// gitSource and item are POST /v1/resolve's item shape.
type gitSource struct {
	URL    string `json:"url"`
	Ref    string `json:"ref,omitempty"`
	Commit string `json:"commit,omitempty"`
}

type ociSource struct {
	Ref string `json:"ref"`
}

type item struct {
	Name string     `json:"name"`
	Path string     `json:"path,omitempty"`
	Git  *gitSource `json:"git,omitempty"`
	OCI  *ociSource `json:"oci,omitempty"`
}

type resolveBody struct {
	Skills  []item `json:"skills"`
	Plugins []item `json:"plugins"`
}

// GitHead resolves ref of repoURL (empty: the default branch) to a commit.
func (c *Client) GitHead(ctx context.Context, repoURL, ref string) (string, error) {
	return c.GitHeadAt(ctx, repoURL, ref, "")
}

// GitHeadAt resolves ref of repoURL for the source at path: with no ref,
// skill-manager takes the ref of the configured source path lies below,
// else the default branch.
func (c *Client) GitHeadAt(ctx context.Context, repoURL, ref, at string) (string, error) {
	got, err := c.resolve(ctx, item{Name: itemName(repoURL, at), Path: at, Git: &gitSource{URL: repoURL, Ref: ref}})
	if err != nil {
		return "", err
	}
	if got.Git == nil || !skills.CommitPattern.MatchString(got.Git.Commit) {
		return "", fmt.Errorf("%w: POST /v1/resolve answered no commit for %s", ErrUnavailable, repoURL)
	}
	return got.Git.Commit, nil
}

// OCIDigest pins an OCI reference to <reference>@sha256:<digest>.
func (c *Client) OCIDigest(ctx context.Context, ref string) (string, error) {
	got, err := c.resolve(ctx, item{Name: itemName(ref, ""), OCI: &ociSource{Ref: ref}})
	if err != nil {
		return "", err
	}
	if got.OCI == nil || !skills.DigestRefPattern.MatchString(got.OCI.Ref) {
		return "", fmt.Errorf("%w: POST /v1/resolve answered no digest for %s", ErrUnavailable, ref)
	}
	return got.OCI.Ref, nil
}

// RequireReadable has skill-manager resolve repoURL's default branch as the
// caller: it refuses a repository the caller cannot read (403,
// skills.ErrNotReadable). login is skill-manager's to take from the bearer.
func (c *Client) RequireReadable(ctx context.Context, repoURL, _ string) error {
	_, err := c.GitHead(ctx, repoURL, "")
	return err
}

// ClaudeFormatPlugin asks the configured checker.
func (c *Client) ClaudeFormatPlugin(ctx context.Context, repoURL, commit, at string) (bool, error) {
	if c.formats == nil {
		return false, nil
	}
	return c.formats.ClaudeFormatPlugin(ctx, repoURL, commit, at)
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

// resolve pins one item.
func (c *Client) resolve(ctx context.Context, it item) (item, error) {
	raw, err := c.do(ctx, http.MethodPost, "/v1/resolve", nil, resolveBody{Skills: []item{it}, Plugins: []item{}})
	if err != nil {
		return item{}, err
	}
	var out resolveBody
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Skills) != 1 {
		return item{}, fmt.Errorf("%w: POST /v1/resolve answered no pinned item", ErrUnavailable)
	}
	return out.Skills[0], nil
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
	c.authorize(ctx, req)
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
// forwarded header, else the ID token; with no caller, the configured token.
func (c *Client) authorize(ctx context.Context, req *http.Request) {
	idToken, _ := identity.TokenFromContext(ctx)
	if gh, ok := identity.GitHubFromContext(ctx); ok {
		req.Header.Set("Authorization", "Bearer "+gh.Token)
		if idToken != "" {
			req.Header.Set(forwardedIdentityHeader, idToken)
		}
		return
	}
	bearer := idToken
	if bearer == "" {
		bearer = c.token
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
}

// answerError maps skill-manager's refusal ({"error": "...", "item": n}).
func answerError(method, p string, status int, raw []byte) error {
	var body struct {
		Error string `json:"error"`
	}
	msg := strings.TrimSpace(string(raw))
	if json.Unmarshal(raw, &body) == nil && body.Error != "" {
		msg = body.Error
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	what := fmt.Sprintf("skill-manager %s %s (%d): %s", method, p, status, msg)
	resolve := p == "/v1/resolve"
	switch {
	case status == http.StatusForbidden:
		return fmt.Errorf("%w: %s", skills.ErrNotReadable, what)
	case status == http.StatusUnauthorized:
		return fmt.Errorf("%w: %s", ErrUnauthenticated, what)
	case resolve && (status == http.StatusBadRequest || status == http.StatusNotFound):
		return fmt.Errorf("%w: %s", skills.ErrUnresolvable, what)
	case status == http.StatusBadRequest || status == http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrInvalid, what)
	default:
		return fmt.Errorf("%w: %s", ErrUnavailable, what)
	}
}

// itemName names an item for skill-manager: the last segment of its path,
// else of its repository or image.
func itemName(source, at string) string {
	if at = strings.Trim(at, "/"); at != "" {
		return path.Base(at)
	}
	source = strings.TrimSuffix(strings.Trim(source, "/"), ".git")
	if i := strings.LastIndex(source, "/"); i >= 0 {
		source = source[i+1:]
	}
	if i := strings.IndexAny(source, ":@"); i > 0 {
		source = source[:i]
	}
	return source
}
