package skills

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// TokenSource hands out the token GitHub requests go out with.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// StaticToken is a fixed token (empty: anonymous).
type StaticToken string

// Token returns the token.
func (t StaticToken) Token(context.Context) (string, error) { return string(t), nil }

// App mints installation tokens of a GitHub App: short-lived (an hour),
// limited to the repositories and permissions of the installation, at the
// App's own rate limit. No personal token is involved. Scoped tokens narrow
// one further to named repositories with read access to their contents.
type App struct {
	apiURL         string
	appID          string
	installationID string
	key            *rsa.PrivateKey
	http           *http.Client

	mu      sync.Mutex
	tokens  map[string]appToken
	account string
}

type appToken struct {
	token   string
	expires time.Time
}

// appTokenRenewal is how long before its expiry an installation token is
// replaced.
const appTokenRenewal = 5 * time.Minute

// MaxScopedRepositories is GitHub's cap on the repositories one installation
// token can be narrowed to.
const MaxScopedRepositories = 500

// NewApp builds the token source of the App appID's installation
// installationID from the App's PEM private key.
func NewApp(apiURL, appID, installationID string, privateKeyPEM []byte, client *http.Client) (*App, error) {
	if strings.TrimSpace(appID) == "" || strings.TrimSpace(installationID) == "" {
		return nil, fmt.Errorf("github app: the App ID and the installation ID are required")
	}
	key, err := jwt.ParseRSAPrivateKeyFromPEM(privateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("github app: parse the private key: %w", err)
	}
	if apiURL == "" {
		apiURL = "https://api.github.com"
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &App{apiURL: strings.TrimRight(apiURL, "/"), appID: strings.TrimSpace(appID), installationID: strings.TrimSpace(installationID), key: key, http: client, tokens: map[string]appToken{}}, nil
}

// Token returns a valid installation token, minting a new one when the
// current one is about to expire.
func (a *App) Token(ctx context.Context) (string, error) {
	token, _, err := a.TokenValidFor(ctx, appTokenRenewal)
	return token, err
}

// TokenValidFor returns an installation token for the whole installation that
// stays valid for at least d, and its expiry, minting a new one when the
// current one expires sooner. It never leaves agent-manager: agents get
// ScopedTokenValidFor's.
func (a *App) TokenValidFor(ctx context.Context, d time.Duration) (string, time.Time, error) {
	return a.tokenValidFor(ctx, "", nil, d)
}

// ScopedTokenValidFor returns an installation token that reads the contents
// of repositories (names in the installation's account, without the owner)
// and nothing else, valid for at least d. Tokens are shared per repository
// set. An empty set is refused: GitHub reads an absent list as the whole
// installation.
func (a *App) ScopedTokenValidFor(ctx context.Context, repositories []string, d time.Duration) (string, time.Time, error) {
	repos := slices.Clone(repositories)
	slices.Sort(repos)
	repos = slices.Compact(repos)
	if len(repos) == 0 || repos[0] == "" {
		return "", time.Time{}, fmt.Errorf("github app: a scoped token needs at least one repository")
	}
	if len(repos) > MaxScopedRepositories {
		return "", time.Time{}, fmt.Errorf("github app: %d repositories exceed the %d one token can be scoped to", len(repos), MaxScopedRepositories)
	}
	body, err := json.Marshal(map[string]any{"repositories": repos, "permissions": map[string]string{"contents": "read"}})
	if err != nil {
		return "", time.Time{}, err
	}
	return a.tokenValidFor(ctx, strings.Join(repos, ","), body, d)
}

func (a *App) tokenValidFor(ctx context.Context, key string, body []byte, d time.Duration) (string, time.Time, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	if cached, ok := a.tokens[key]; ok && now.Add(d).Before(cached.expires) {
		return cached.token, cached.expires, nil
	}
	var reqBody io.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	target := a.apiURL + "/app/installations/" + a.installationID + "/access_tokens"
	if err := a.call(ctx, http.MethodPost, target, reqBody, http.StatusCreated, &out); err != nil {
		return "", time.Time{}, err
	}
	if out.Token == "" {
		return "", time.Time{}, fmt.Errorf("github app: no token in the answer of POST %s", target)
	}
	for k, t := range a.tokens {
		if !now.Before(t.expires) {
			delete(a.tokens, k)
		}
	}
	a.tokens[key] = appToken{token: out.Token, expires: out.ExpiresAt}
	return out.Token, out.ExpiresAt, nil
}

// Account is the login of the account (organization or user) the App is
// installed on: the owner of every repository a scoped token can name.
func (a *App) Account(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.account != "" {
		return a.account, nil
	}
	var out struct {
		Account struct {
			Login string `json:"login"`
		} `json:"account"`
	}
	target := a.apiURL + "/app/installations/" + a.installationID
	if err := a.call(ctx, http.MethodGet, target, nil, http.StatusOK, &out); err != nil {
		return "", err
	}
	if out.Account.Login == "" {
		return "", fmt.Errorf("github app: no account in the answer of GET %s", target)
	}
	a.account = out.Account.Login
	return a.account, nil
}

// call sends an App-authenticated request and decodes the answer.
func (a *App) call(ctx context.Context, method, target string, body io.Reader, want int, out any) error {
	now := time.Now()
	// The App's own JWT: issued a minute in the past against clock drift,
	// valid for less than GitHub's ten-minute cap.
	signed, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
		Issuer:    a.appID,
		IssuedAt:  jwt.NewNumericDate(now.Add(-time.Minute)),
		ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute)),
	}).SignedString(a.key)
	if err != nil {
		return fmt.Errorf("github app: sign the App JWT: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+signed)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("github app: %s %s: %w", method, target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != want {
		return fmt.Errorf("github app: %s %s returned %d: %s", method, target, resp.StatusCode, firstLine(strings.TrimSpace(string(raw))))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("github app: decode the answer of %s %s: %w", method, target, err)
	}
	return nil
}
