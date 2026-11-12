package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/giantswarm/mcp-oauth/providers/oidc"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/identity"
)

// GitHubExchangeConfig identifies a confidential RFC 8693 client and its
// operator-controlled target. No caller can select the endpoint or target.
type GitHubExchangeConfig struct {
	TokenEndpoint    string
	ClientID         string
	ClientSecretFile string
	Audience         string
	CAFile           string
	APIURL           string
	SubjectIssuer    string
	// GrantApp is the provider App muster keeps the person's grant under,
	// named in the grant_required answer.
	GrantApp string
}

func (c GitHubExchangeConfig) Validate() error {
	endpoint, err := url.Parse(c.TokenEndpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return errors.New("GitHub token exchange requires an HTTPS token endpoint without credentials, query or fragment")
	}
	if c.ClientID == "" || c.ClientSecretFile == "" || c.Audience == "" || c.SubjectIssuer == "" || c.GrantApp == "" {
		return errors.New("GitHub token exchange requires a client ID, client secret file, target audience, identity issuer and grant App")
	}
	if c.APIURL != "" {
		apiURL, err := url.Parse(c.APIURL)
		if err != nil || apiURL.Scheme != "https" || apiURL.Host == "" || apiURL.User != nil || apiURL.RawQuery != "" || apiURL.Fragment != "" {
			return errors.New("GitHub token exchange requires an HTTPS GitHub API URL")
		}
	}
	return nil
}

// NewGitHubExchange obtains a user grant on demand. Tokens are neither cached
// nor attached to a session; only the returned request-local identity holds one.
func NewGitHubExchange(configuration GitHubExchangeConfig) (func(context.Context) (*identity.GitHub, error), error) {
	if err := configuration.Validate(); err != nil {
		return nil, err
	}
	roots, err := loadRootCAs(configuration.CAFile)
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if roots != nil {
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		transport.TLSClientConfig.RootCAs = roots
	}
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	return newGitHubExchange(configuration, client), nil
}

func newGitHubExchange(configuration GitHubExchangeConfig, client *http.Client) func(context.Context) (*identity.GitHub, error) {
	return func(ctx context.Context) (*identity.GitHub, error) {
		person, ok := identity.FromContext(ctx)
		subjectToken, hasToken := identity.TokenFromContext(ctx)
		if !ok || !hasToken || person.Delegated || person.Issuer != configuration.SubjectIssuer || person.Subject == "" {
			return nil, fmt.Errorf("%w: a verified platform identity is required for GitHub authorization", agents.ErrUnauthenticated)
		}
		secret, err := os.ReadFile(configuration.ClientSecretFile)
		if err != nil || len(strings.TrimSpace(string(secret))) == 0 {
			return nil, errors.New("GitHub token exchange credentials are unavailable")
		}
		form := url.Values{"grant_type": {oidc.GrantTypeTokenExchange}, "subject_token": {subjectToken}, "subject_token_type": {oidc.TokenTypeIDToken}, "requested_token_type": {oidc.TokenTypeAccessToken}, "audience": {configuration.Audience}}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, configuration.TokenEndpoint, strings.NewReader(form.Encode()))
		if err != nil {
			return nil, errors.New("GitHub token exchange request is invalid")
		}
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.SetBasicAuth(url.QueryEscape(configuration.ClientID), url.QueryEscape(strings.TrimSpace(string(secret))))
		response, err := client.Do(request)
		if err != nil {
			return nil, errors.New("GitHub authorization service is unavailable")
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode != http.StatusOK {
			var refusal struct {
				Error string `json:"error"`
			}
			_ = json.NewDecoder(io.LimitReader(response.Body, 16*1024)).Decode(&refusal)
			if response.StatusCode == http.StatusBadRequest && refusal.Error == "invalid_target" {
				return nil, &agents.GrantRequiredError{App: configuration.GrantApp, Reason: fmt.Sprintf("this call opens a pull request as you with your grant of the GitHub App %s in muster, which you have not given: grant it (core_auth_login app=%s), then call again", configuration.GrantApp, configuration.GrantApp)}
			}
			return nil, errors.New("GitHub authorization service refused the exchange")
		}
		var result oidc.TokenExchangeResponse
		if err := json.NewDecoder(io.LimitReader(response.Body, 64*1024)).Decode(&result); err != nil || result.AccessToken == "" || !strings.EqualFold(result.TokenType, "Bearer") || result.IssuedTokenType != oidc.TokenTypeAccessToken || result.ExpiresIn <= 0 || result.RefreshToken != "" {
			return nil, errors.New("GitHub authorization service returned an invalid access token response")
		}
		apiURL := strings.TrimRight(configuration.APIURL, "/")
		if apiURL == "" {
			apiURL = DefaultGitHubAPIURL
		}
		request, err = http.NewRequestWithContext(ctx, http.MethodGet, apiURL+"/user", nil)
		if err != nil {
			return nil, errors.New("GitHub API URL is invalid")
		}
		request.Header.Set("Authorization", "Bearer "+result.AccessToken)
		response, err = client.Do(request)
		if err != nil {
			return nil, errors.New("GitHub user verification is unavailable")
		}
		defer func() { _ = response.Body.Close() }()
		var user struct {
			Login string `json:"login"`
		}
		if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 16*1024)).Decode(&user) != nil || user.Login == "" {
			return nil, &agents.GrantRequiredError{App: configuration.GrantApp, Reason: fmt.Sprintf("GitHub refused your grant of the GitHub App %s: grant it again in muster (core_auth_login app=%s), then call again", configuration.GrantApp, configuration.GrantApp)}
		}
		return &identity.GitHub{Login: user.Login, Token: result.AccessToken}, nil
	}
}
