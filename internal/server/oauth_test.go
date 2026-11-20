package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	oauth "github.com/giantswarm/mcp-oauth"
	"github.com/giantswarm/mcp-oauth/handler"
	"github.com/giantswarm/mcp-oauth/providers"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/api"
	"github.com/giantswarm/agent-manager/internal/identity"
	"github.com/giantswarm/agent-manager/internal/kube"
	"github.com/giantswarm/agent-manager/internal/kube/kubetest"
)

// fakeIdP is a minimal OIDC issuer on https://localhost: a discovery
// document, enough for mcp-oauth's Dex provider to boot, and a key to mint
// id_tokens the way the platform's Dex does.
type fakeIdP struct {
	issuer string
	key    *rsa.PrivateKey
	caFile string
	srv    *httptest.Server
}

const testKID = "test-key"

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	idp := &fakeIdP{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/dex/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                idp.issuer,
			"authorization_endpoint":                idp.issuer + "/auth",
			"token_endpoint":                        idp.issuer + "/token",
			"userinfo_endpoint":                     idp.issuer + "/userinfo",
			"jwks_uri":                              idp.issuer + "/keys",
			"response_types_supported":              []string{"code"},
			"scopes_supported":                      []string{"openid", "email", "profile", "groups", "offline_access"},
			"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
			"code_challenge_methods_supported":      []string{"S256"},
			"token_endpoint_auth_methods_supported": []string{"client_secret_basic"},
		})
	})

	// mcp-oauth rejects IP-literal issuers; a hostname that resolves to loopback
	// (localhost, like the agentlab Dex) boots the provider. httptest's
	// certificate has no localhost SAN,
	// so mint one.
	srv := httptest.NewUnstartedServer(mux)
	cert := selfSignedLocalhost(t)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	idp.srv = srv
	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	require.NoError(t, err)
	idp.issuer = "https://localhost:" + port + "/dex"

	caPath := filepath.Join(t.TempDir(), "ca.crt")
	require.NoError(t, os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0o600))
	idp.caFile = caPath
	return idp
}

func selfSignedLocalhost(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// idToken mints an id_token the fake IdP signed.
func (f *fakeIdP) idToken(t *testing.T, aud []string, exp time.Time) string {
	t.Helper()
	return f.idTokenWithout(t, aud, exp, "")
}

// idTokenWithout is idToken without the named claim.
func (f *fakeIdP) idTokenWithout(t *testing.T, aud []string, exp time.Time, claim string) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: f.key},
		(&jose.SignerOptions{}).WithHeader("kid", testKID).WithType("JWT"))
	require.NoError(t, err)
	claims := jwt.Claims{Issuer: f.issuer, Subject: "sub-admin", Audience: jwt.Audience(aud),
		Expiry: jwt.NewNumericDate(exp), IssuedAt: jwt.NewNumericDate(time.Now().Add(-time.Minute))}
	extra := map[string]any{"email": "admin@lab.local", "email_verified": true, "name": "Lab Admin", "groups": []string{"platform-admins"}}
	delete(extra, claim)
	tok, err := jwt.Signed(signer).Claims(claims).Claims(extra).Serialize()
	require.NoError(t, err)
	return tok
}

func (f *fakeIdP) config(downstream bool) OAuthConfig {
	return OAuthConfig{
		BaseURL:          "http://localhost:8080",
		Provider:         ProviderDex,
		DexIssuerURL:     f.issuer,
		DexClientID:      "agent-platform",
		DexClientSecret:  "lab-only",
		DexCAFile:        f.caFile,
		TrustedAudiences: []string{"agent-platform"},
		DownstreamOAuth:  downstream,
	}
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestOAuthConfigValidation(t *testing.T) {
	dexOK := OAuthConfig{BaseURL: "https://am.example.com", DexIssuerURL: "x", DexClientID: "y", DexClientSecret: "z"}
	require.Error(t, OAuthConfig{}.Validate())
	require.Error(t, OAuthConfig{BaseURL: "http://am.example.com", DexIssuerURL: "x", DexClientID: "y", DexClientSecret: "z"}.Validate(), "plain http only on loopback")
	require.NoError(t, OAuthConfig{BaseURL: "http://localhost:8080", DexIssuerURL: "x", DexClientID: "y", DexClientSecret: "z"}.Validate())
	require.NoError(t, dexOK.Validate())
	require.Error(t, OAuthConfig{BaseURL: "https://am.example.com", Provider: ProviderDex}.Validate(), "dex needs issuer + client")
	require.Error(t, OAuthConfig{BaseURL: "https://am.example.com", Provider: ProviderGoogle, GoogleClientID: "id"}.Validate(), "google needs the secret")
	require.NoError(t, OAuthConfig{BaseURL: "https://am.example.com", Provider: ProviderGoogle, GoogleClientID: "id", GoogleClientSecret: "s", TrustedAudiences: []string{"id"}}.Validate())
	require.Error(t, OAuthConfig{BaseURL: "https://am.example.com", Provider: "okta"}.Validate(), "unknown provider")
	bad := dexOK
	bad.TrustedAudiences = []string{"agent-platform", " "}
	require.Error(t, bad.Validate(), "empty audience")
}

// verifiedUser is the user mcp-oauth hands on once it has validated a
// forwarded IdP id_token.
func verifiedUser(emailVerified bool) *providers.UserInfo {
	return &providers.UserInfo{ID: "sub-admin", Email: "admin@lab.local", EmailVerified: emailVerified, Name: "Lab Admin", Groups: []string{"platform-admins"}, TokenSource: providers.TokenSourceSSO}
}

// afterValidation runs next as if mcp-oauth had validated the bearer and
// admitted info.
func afterValidation(info *providers.UserInfo, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(handler.ContextWithUserInfo(r.Context(), info)))
	})
}

// TestForwardedIDTokenBecomesTheCaller is the platform path: muster (or the
// portal through the gateway) forwards the IdP id_token for the platform
// client; once mcp-oauth has validated it, the request carries the caller and,
// with downstream OAuth, the caller's token.
func TestForwardedIDTokenBecomesTheCaller(t *testing.T) {
	idp := newFakeIdP(t)
	o, err := newOAuth(idp.config(true), "/mcp", quiet())
	require.NoError(t, err)
	require.True(t, o.server.Config.EnableClientIDMetadataDocuments)
	t.Cleanup(func() { o.shutdown(context.Background()) })

	var seen struct {
		id    *identity.Identity
		token string
		ok    bool
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.id, seen.ok = identity.FromContext(r.Context())
		seen.token, _ = identity.TokenFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})
	call := func(info *providers.UserInfo, bearer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
		req.Header.Set("Authorization", "Bearer "+bearer)
		rec := httptest.NewRecorder()
		afterValidation(info, o.attachIdentity(next)).ServeHTTP(rec, req)
		return rec
	}

	forwarded := idp.idToken(t, []string{"agent-platform"}, time.Now().Add(30*time.Minute))
	rec := call(verifiedUser(true), forwarded)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	require.True(t, seen.ok, "the caller reaches the handler")
	assert.Equal(t, "admin@lab.local", seen.id.Email)
	assert.True(t, seen.id.EmailVerified, "the IdP's email_verified claim")
	assert.Equal(t, "sub-admin", seen.id.Subject)
	assert.Equal(t, []string{"platform-admins"}, seen.id.Groups)
	assert.Equal(t, identity.SourceSSO, seen.id.Source)
	assert.Equal(t, idp.issuer, seen.id.Issuer)
	assert.JSONEq(t, `["agent-platform"]`, seen.id.Client, "the token's audiences")
	assert.Equal(t, forwarded, seen.token, "downstream OAuth: the forwarded id_token is what the Kubernetes API will see")

	rec = call(verifiedUser(false), forwarded)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.False(t, seen.id.EmailVerified, "an address the IdP did not verify stays unverified")

	// A bearer this server issued carries the IdP id_token stored next to it;
	// without one, downstream OAuth has nothing to act with.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer issued-by-this-server")
	rec = httptest.NewRecorder()
	issued := verifiedUser(true)
	issued.TokenSource = providers.TokenSourceOAuth
	afterValidation(issued, o.attachIdentity(next)).ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Header().Get("WWW-Authenticate"), "no identity token")

	// Nothing admitted, nothing attached.
	rec = httptest.NewRecorder()
	o.attachIdentity(next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// TestDownstreamOffStillCarriesTheCallerToken: without downstream OAuth the
// Kubernetes calls run as the ServiceAccount (the provider never reads the
// token), but the caller's token still travels with the request for the
// session operations, which act as the caller towards agentgateway.
func TestDownstreamOffStillCarriesTheCallerToken(t *testing.T) {
	idp := newFakeIdP(t)
	o, err := newOAuth(idp.config(false), "/mcp", quiet())
	require.NoError(t, err)
	t.Cleanup(func() { o.shutdown(context.Background()) })

	var caller, token string
	h := afterValidation(verifiedUser(true), o.attachIdentity(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller = identity.Caller(r.Context())
		token, _ = identity.TokenFromContext(r.Context())
	})))
	forwarded := idp.idToken(t, []string{"agent-platform"}, time.Now().Add(time.Minute))
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+forwarded)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "admin@lab.local", caller, "the caller is known and attributed")
	assert.Equal(t, forwarded, token, "the forwarded id_token is what agentgateway will see")
}

func TestServerConfig(t *testing.T) {
	pool := x509.NewCertPool()
	cfg := OAuthConfig{BaseURL: "https://am.example.com", TrustedAudiences: []string{"agent-platform", "kubernetes"}, AllowPublicClientRegistration: true}
	assert.Equal(t, &oauth.ServerConfig{
		EnableClientIDMetadataDocuments: true,
		Issuer:                          "https://am.example.com",
		AllowRefreshTokenRotation:       true,
		AllowPublicClientRegistration:   true,
		MaxClientsPerIP:                 10,
		TrustedAudiences:                []string{"agent-platform", "kubernetes"},
		JWKSRootCAs:                     pool,
	}, serverConfig(cfg, pool), "no private-address allowance: the issuer host is covered by the library")
}

func TestUnverifiedAudience(t *testing.T) {
	payload := func(claims string) string {
		return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".sig"
	}
	for name, tc := range map[string]struct {
		token string
		want  []string
		ok    bool
	}{
		"single audience":    {payload(`{"aud":"portal-client","sub":"x"}`), []string{"portal-client"}, true},
		"audience list":      {payload(`{"aud":["portal-client","kubernetes"]}`), []string{"portal-client", "kubernetes"}, true},
		"no audience":        {payload(`{"sub":"x"}`), nil, false},
		"empty audience":     {payload(`{"aud":""}`), nil, false},
		"not json":           {payload(`{"aud":`), nil, false},
		"opaque":             {"not-a-jwt", nil, false},
		"four segments":      {"a.b.c.d", nil, false},
		"payload not base64": {"a.!!!.c", nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := unverifiedAudience(tc.token)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestServerGuardsRESTAndMCPButNotProbes wires OAuth through the assembled
// server: probes and the OAuth metadata stay open, the API and the MCP
// endpoint demand a token.
func TestServerGuardsRESTAndMCPButNotProbes(t *testing.T) {
	idp := newFakeIdP(t)
	dyn := kubetest.NewDynamicClient()
	typed := kubefake.NewClientset()
	svc := agents.New(kube.NewServiceAccountProvider(kube.FromInterfaces(dyn, typed, typed.Discovery())), embeddedChart{}, nil, nil, agents.Config{Version: "test"}, nil)
	cfg := idp.config(true)
	srv, err := New(Config{Addr: "127.0.0.1:0", MCPEnabled: true, OAuth: &cfg}, svc, api.NewMCPServer(svc, "test"), quiet())
	require.NoError(t, err)
	t.Cleanup(func() { srv.oauth.shutdown(context.Background()) })
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	do := func(method, path, bearer string) (int, string) {
		req, err := http.NewRequest(method, ts.URL+path, nil)
		require.NoError(t, err)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if method == http.MethodPost {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode, string(body)
	}
	status := func(method, path, bearer string) int {
		code, _ := do(method, path, bearer)
		return code
	}
	assert.Equal(t, http.StatusOK, status(http.MethodGet, "/healthz", ""))
	assert.Equal(t, http.StatusOK, status(http.MethodGet, "/readyz", ""))
	assert.Equal(t, http.StatusUnauthorized, status(http.MethodGet, "/api/v1/info", ""), "REST needs a token")
	assert.Equal(t, http.StatusUnauthorized, status(http.MethodGet, "/api/v1/openapi.yaml", ""))
	assert.Equal(t, http.StatusUnauthorized, status(http.MethodPost, "/mcp", ""), "MCP needs a token")
	assert.Equal(t, http.StatusOK, status(http.MethodGet, "/.well-known/oauth-authorization-server", ""), "metadata stays public")

}
