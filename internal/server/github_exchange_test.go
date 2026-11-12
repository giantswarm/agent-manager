package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"log/slog"

	"github.com/giantswarm/mcp-oauth/providers/oidc"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/identity"
)

func unsignedJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	require.NoError(t, err)
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`)) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestGitHubExchange(t *testing.T) {
	for _, scenario := range []string{"success", "wrong identity issuer", "delegated actor", "act claim", "opaque subject token", "missing identity token", "no grant", "broker error", "redirect", "invalid token type", "refresh token", "missing expiry", "GitHub refuses", "GitHub unavailable", "GitHub forbids"} {
		t.Run(scenario, func(t *testing.T) {
			subjectToken := unsignedJWT(t, map[string]any{"iss": "https://identity.example.com", "sub": "alice"})
			switch scenario {
			case "act claim":
				subjectToken = unsignedJWT(t, map[string]any{"iss": "https://identity.example.com", "sub": "alice", "act": map[string]any{"sub": "agent"}})
			case "opaque subject token":
				subjectToken = "opaque-subject-token" // #nosec G101 -- a synthetic non-JWT bearer
			}
			secretFile := filepath.Join(t.TempDir(), "client-secret")
			require.NoError(t, os.WriteFile(secretFile, []byte("fixture-secret"), 0o600))
			exchanges, verifications := 0, 0
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/token" {
					exchanges++
					require.Equal(t, http.MethodPost, r.Method)
					require.NoError(t, r.ParseForm())
					require.Equal(t, oidc.GrantTypeTokenExchange, r.Form.Get("grant_type"))
					require.Equal(t, subjectToken, r.Form.Get("subject_token"))
					require.Equal(t, oidc.TokenTypeIDToken, r.Form.Get("subject_token_type"))
					require.Equal(t, "manager-app", r.Form.Get("audience"))
					require.Empty(t, r.Form.Get("actor_token"))
					client, secret, ok := r.BasicAuth()
					require.True(t, ok)
					require.Equal(t, "manager-client", client)
					require.Equal(t, "fixture-secret", secret)
					switch scenario {
					case "no grant":
						w.WriteHeader(http.StatusBadRequest)
						_, _ = fmt.Fprint(w, `{"error":"invalid_target","error_description":"the requested audience cannot be served"}`)
						return
					case "broker error":
						w.WriteHeader(http.StatusInternalServerError)
						_, _ = fmt.Fprint(w, "do not expose verified-id-token fixture-secret")
						return
					case "redirect":
						w.Header().Set("Location", "/redirected")
						w.WriteHeader(http.StatusTemporaryRedirect)
						return
					}
					result := oidc.TokenExchangeResponse{AccessToken: "fixture-user-grant", TokenType: "Bearer", IssuedTokenType: oidc.TokenTypeAccessToken, ExpiresIn: 300} // #nosec G101 -- synthetic grants served by the test broker
					if scenario == "invalid token type" {
						result.IssuedTokenType = oidc.TokenTypeIDToken
					}
					if scenario == "refresh token" {
						result.RefreshToken = "fixture-refresh"
					}
					if scenario == "missing expiry" {
						result.ExpiresIn = 0
					}
					require.NoError(t, json.NewEncoder(w).Encode(result)) // #nosec G117 -- the fixture is a token endpoint
					return
				}
				require.Equal(t, "/user", r.URL.Path, "credentials must never follow a redirect")
				verifications++
				require.Empty(t, r.Header.Get("X-Muster-Id-Token"))
				require.Equal(t, "Bearer fixture-user-grant", r.Header.Get("Authorization"))
				require.Equal(t, "application/vnd.github+json", r.Header.Get("Accept"))
				switch scenario {
				case "GitHub refuses":
					w.WriteHeader(http.StatusUnauthorized)
					return
				case "GitHub forbids":
					w.WriteHeader(http.StatusForbidden)
					return
				case "GitHub unavailable":
					w.WriteHeader(http.StatusBadGateway)
					return
				}
				_, _ = fmt.Fprint(w, `{"login":"alice"}`)
			})
			upstream := httptest.NewTLSServer(handler)
			defer upstream.Close()
			client := upstream.Client()
			client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
			var logs bytes.Buffer
			configuration := GitHubExchangeConfig{TokenEndpoint: upstream.URL + "/token", ClientID: "manager-client", ClientSecretFile: secretFile, Audience: "manager-app", APIURL: upstream.URL, SubjectIssuer: "https://identity.example.com", Logger: slog.New(slog.NewTextHandler(&logs, nil))}
			ctx := identity.ContextWith(t.Context(), &identity.Identity{Issuer: configuration.SubjectIssuer, Subject: "alice"})
			if scenario == "wrong identity issuer" {
				ctx = identity.ContextWith(ctx, &identity.Identity{Issuer: "https://other.example.com", Subject: "alice"})
			}
			if scenario == "delegated actor" {
				ctx = identity.ContextWith(ctx, &identity.Identity{Issuer: configuration.SubjectIssuer, Subject: "alice", Delegated: true})
			}
			if scenario != "missing identity token" {
				ctx = identity.ContextWithToken(ctx, subjectToken)
			}
			resolve := newGitHubExchange(configuration, client)
			result, err := resolve(ctx)
			if scenario == "success" {
				require.NoError(t, err)
				require.Equal(t, &identity.GitHub{Login: "alice", Token: "fixture-user-grant", App: "manager-app"}, result) // #nosec G101 -- synthetic broker response
				_, err = resolve(ctx)
				require.NoError(t, err)
				require.Equal(t, 2, exchanges)
				require.Equal(t, 2, verifications, "user grants are not cached")
				return
			}
			require.Error(t, err)
			require.Nil(t, result)
			for _, value := range []string{subjectToken, "fixture-secret", "fixture-user-grant", "fixture-refresh"} {
				require.NotContains(t, err.Error(), value)
				require.NotContains(t, logs.String(), value)
			}
			_, grantRequired := errors.AsType[*agents.GrantRequiredError](err)
			switch scenario {
			case "no grant", "GitHub refuses":
				require.ErrorIs(t, err, agents.ErrAuthRequired)
				grant, ok := errors.AsType[*agents.GrantRequiredError](err)
				require.True(t, ok)
				require.Equal(t, "manager-app", grant.App)
				require.Contains(t, grant.Reason, "core_auth_login app=manager-app")
			case "delegated actor", "act claim", "opaque subject token":
				require.ErrorIs(t, err, agents.ErrForbidden)
				require.NotErrorIs(t, err, agents.ErrUnauthenticated)
				require.Zero(t, exchanges)
			case "wrong identity issuer", "missing identity token":
				require.ErrorIs(t, err, agents.ErrUnauthenticated)
				require.Zero(t, exchanges)
			case "GitHub unavailable", "GitHub forbids":
				require.False(t, grantRequired, "only a 401 asks for the grant again")
				require.NotErrorIs(t, err, agents.ErrAuthRequired)
				require.ErrorContains(t, err, "unavailable")
			default:
				require.False(t, grantRequired)
			}
			switch scenario {
			case "no grant":
				require.Contains(t, logs.String(), "status=400")
				require.Contains(t, logs.String(), "error=invalid_target")
				require.Contains(t, logs.String(), "the requested audience cannot be served")
			case "broker error":
				require.Contains(t, logs.String(), "status=500")
			}
		})
	}
}

func TestGitHubExchangeConfiguration(t *testing.T) {
	valid := GitHubExchangeConfig{TokenEndpoint: "https://broker.example.com/oauth/token", ClientID: "manager", ClientSecretFile: "/mounted/client-secret", Audience: "manager-app", SubjectIssuer: "https://identity.example.com"} // #nosec G101 -- configuration fixture with a path, no secret value
	require.NoError(t, valid.Validate())
	for _, endpoint := range []string{"http://broker.example.com/token", "https://user:password@broker.example.com/token", "https://broker.example.com/token?credential=value", "https://broker.example.com/token#fragment", "/token"} {
		configuration := valid
		configuration.TokenEndpoint = endpoint
		require.Error(t, configuration.Validate())
	}
	configuration := valid
	configuration.ClientSecretFile = ""
	require.Error(t, configuration.Validate())
	configuration = valid
	configuration.ClientID = ""
	require.Error(t, configuration.Validate())
	configuration.ClientIDFile = "/mounted/client-id"
	require.NoError(t, configuration.Validate())
}

func TestGitHubExchangeClientID(t *testing.T) {
	dir := t.TempDir()
	clientIDFile := filepath.Join(dir, "client-id")
	require.NoError(t, os.WriteFile(clientIDFile, []byte("file-client\n"), 0o600))
	emptyFile := filepath.Join(dir, "empty")
	require.NoError(t, os.WriteFile(emptyFile, []byte(" \n"), 0o600))
	absentFile := filepath.Join(dir, "absent")

	for _, scenario := range []struct {
		name         string
		clientID     string
		clientIDFile string
		want         string
	}{
		{name: "flag wins over the file", clientID: "flag-client", clientIDFile: clientIDFile, want: "flag-client"},
		{name: "file", clientIDFile: clientIDFile, want: "file-client"},
		{name: "flag with the file key absent", clientID: "flag-client", clientIDFile: absentFile, want: "flag-client"},
		{name: "neither"},
		{name: "file key absent", clientIDFile: absentFile},
		{name: "empty file", clientIDFile: emptyFile},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			configuration := GitHubExchangeConfig{TokenEndpoint: "https://broker.example.com/oauth/token", ClientID: scenario.clientID, ClientIDFile: scenario.clientIDFile, ClientSecretFile: filepath.Join(dir, "client-secret"), Audience: "manager-app", SubjectIssuer: "https://identity.example.com"} // #nosec G101 -- configuration fixture with a path, no secret value
			clientID, err := configuration.resolveClientID()
			if scenario.want != "" {
				require.NoError(t, err)
				require.Equal(t, scenario.want, clientID)
				return
			}
			require.Error(t, err)
			_, err = NewGitHubExchange(configuration)
			require.Error(t, err, "a missing client ID fails at startup")
		})
	}
}
