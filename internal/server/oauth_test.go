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

	"github.com/giantswarm/mcp-oauth/oauthconfig"
	oauthserver "github.com/giantswarm/mcp-oauth/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/api"
	"github.com/giantswarm/agent-manager/internal/kube"
	"github.com/giantswarm/agent-manager/internal/kube/kubetest"
)

// fakeIdP is a minimal OIDC issuer on https://localhost: a discovery
// document, enough for mcp-oauth's Dex provider to boot.
type fakeIdP struct {
	issuer string
	caFile string
	srv    *httptest.Server
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	idp := &fakeIdP{}
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

func (f *fakeIdP) config(t *testing.T, downstream bool) OAuthConfig {
	t.Helper()
	return OAuthConfig{Settings: oauthSettings(t, dexEnvironment(f.issuer, f.caFile)), DownstreamOAuth: downstream}
}

// dexEnvironment is the OAUTH_* configuration of a Dex at issuer whose
// forwarded id_tokens for the platform client are accepted.
func dexEnvironment(issuer, caFile string) map[string]string {
	return map[string]string{
		"OAUTH_ISSUER":                              "http://localhost:8080",
		"OAUTH_PROVIDER":                            "dex",
		"OAUTH_DEX_ISSUER_URL":                      issuer,
		"OAUTH_DEX_CLIENT_ID":                       "agent-platform",
		"OAUTH_DEX_CLIENT_SECRET":                   "lab-only",
		"OAUTH_DEX_CA_FILE":                         caFile,
		"OAUTH_TRUSTED_AUDIENCES":                   "agent-platform",
		"OAUTH_ENABLE_CLIENT_ID_METADATA_DOCUMENTS": "true",
	}
}

// oauthSettings reads mcp-oauth's settings from environment the way the
// serve command does.
func oauthSettings(t *testing.T, environment map[string]string) oauthconfig.Settings {
	t.Helper()
	for name, value := range environment {
		t.Setenv(name, value)
	}
	settings, err := oauthconfig.SettingsFromEnv()
	require.NoError(t, err)
	return settings
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestProtectedResources(t *testing.T) {
	advertised := []string{"https://dex.example.com"}
	configuration := &oauthserver.Config{Issuer: "https://am.example.com", ResourceMetadataByPath: map[string]oauthserver.ProtectedResourceConfig{
		"/": {ResourceIdentifier: "https://am.example.com", AuthorizationServers: advertised},
	}}
	assert.Equal(t, map[string]oauthserver.ProtectedResourceConfig{
		"/":        {ResourceIdentifier: "https://am.example.com", AuthorizationServers: advertised},
		api.Prefix: {AuthorizationServers: advertised},
		"/mcp":     {AuthorizationServers: advertised},
	}, protectedResources(configuration, "/mcp"), "an added path advertises the main resource's servers")
	assert.Len(t, configuration.ResourceMetadataByPath, 1, "the configuration is not changed")

	assert.Equal(t, map[string]oauthserver.ProtectedResourceConfig{api.Prefix: {}, "/mcp": {}},
		protectedResources(&oauthserver.Config{Issuer: "https://am.example.com"}, "/mcp"), "nothing advertised: the server's own issuer")

	trailing := &oauthserver.Config{Issuer: "https://am.example.com", ResourceMetadataByPath: map[string]oauthserver.ProtectedResourceConfig{
		"/mcp/": {ResourceIdentifier: "https://am.example.com/mcp/", ScopesSupported: []string{"agents"}},
	}}
	assert.Equal(t, map[string]oauthserver.ProtectedResourceConfig{
		"/mcp/":    {ResourceIdentifier: "https://am.example.com/mcp/", ScopesSupported: []string{"agents"}},
		api.Prefix: {},
	}, protectedResources(trailing, "/mcp"), "a configured path with a trailing slash describes the MCP endpoint")
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
	cfg := idp.config(t, true)
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
