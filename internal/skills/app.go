package skills

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
// App's own rate limit. No personal token is involved.
type App struct {
	apiURL         string
	appID          string
	installationID string
	key            *rsa.PrivateKey
	http           *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

// appTokenRenewal is how long before its expiry an installation token is
// replaced.
const appTokenRenewal = 5 * time.Minute

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
	return &App{apiURL: strings.TrimRight(apiURL, "/"), appID: strings.TrimSpace(appID), installationID: strings.TrimSpace(installationID), key: key, http: client}, nil
}

// Token returns a valid installation token, minting a new one when the
// current one is about to expire.
func (a *App) Token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	if a.token != "" && now.Add(appTokenRenewal).Before(a.expires) {
		return a.token, nil
	}
	// The App's own JWT: issued a minute in the past against clock drift,
	// valid for less than GitHub's ten-minute cap.
	signed, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
		Issuer:    a.appID,
		IssuedAt:  jwt.NewNumericDate(now.Add(-time.Minute)),
		ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute)),
	}).SignedString(a.key)
	if err != nil {
		return "", fmt.Errorf("github app: sign the App JWT: %w", err)
	}
	target := a.apiURL + "/app/installations/" + a.installationID + "/access_tokens"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+signed)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := a.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("github app: POST %s: %w", target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("github app: POST %s returned %d: %s", target, resp.StatusCode, firstLine(strings.TrimSpace(string(body))))
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Token == "" {
		return "", fmt.Errorf("github app: no token in the answer of POST %s", target)
	}
	a.token, a.expires = out.Token, out.ExpiresAt
	return a.token, nil
}
