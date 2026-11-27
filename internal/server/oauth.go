package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/giantswarm/mcp-oauth/handler"
	"github.com/giantswarm/mcp-oauth/oauthconfig"
	"github.com/giantswarm/mcp-oauth/providers/oidc"
	"github.com/giantswarm/mcp-oauth/security"
	oauthserver "github.com/giantswarm/mcp-oauth/server"

	"github.com/giantswarm/agent-manager/internal/api"
	"github.com/giantswarm/agent-manager/internal/identity"
)

// OAuthConfig makes agent-manager an OAuth 2.1 resource server (mcp-oauth)
// in front of the MCP endpoint and the REST API. The platform's identity provider is the
// authority: on the Agent Platform, muster forwards the session's IdP
// id_token byte-identical (MCPServer auth.forwardToken) and the portal sends
// the signed-in user's id_token through the gateway; both are validated
// against the IdP's JWKS when their audience is one of the trusted audiences
// (OAUTH_TRUSTED_AUDIENCES, the platform's OAuth client). The caller's
// identity then travels with the request (identity package) and, with
// DownstreamOAuth, so does the token: the Kubernetes API is called as the
// caller.
type OAuthConfig struct {
	// Settings is mcp-oauth's configuration, read from the OAUTH_* variables
	// by oauthconfig.SettingsFromEnv.
	Settings oauthconfig.Settings
	// DownstreamOAuth puts the caller's IdP token on the request so
	// Kubernetes API calls are made as the caller instead of the
	// ServiceAccount (the apiserver must trust the IdP and the token's
	// audience). The ServiceAccount holds no permissions then, so a request
	// that yields no IdP token to present is refused (401).
	DownstreamOAuth bool
}

type oauthRuntime struct {
	server          *oauthserver.Server
	handler         *handler.Handler
	cleanup         func(context.Context) error
	downstreamOAuth bool
	issuer          string
	mcpPath         string
	log             *slog.Logger
}

func newOAuth(cfg OAuthConfig, mcpPath string, log *slog.Logger) (*oauthRuntime, error) {
	settings := cfg.Settings
	if settings.Server == nil {
		return nil, errors.New("oauth: no server settings")
	}
	serverConfig := *settings.Server
	serverConfig.ResourceMetadataByPath = protectedResources(&serverConfig, mcpPath)
	settings.Server = &serverConfig
	settings.Logger = log
	if settings.Dex != nil {
		dexConfig := *settings.Dex
		dexConfig.Logger = log
		settings.Dex = &dexConfig
	}
	runtime, cleanup, err := oauthconfig.Setup(settings, oauthserver.WithAuditor(security.NewAuditor(log, true)))
	if err != nil {
		return nil, fmt.Errorf("oauth: %w", err)
	}
	log.Info("OAuth resource server enabled", "provider", providerName(settings), "issuer", serverConfig.Issuer,
		"trustedAudiences", serverConfig.TrustedAudiences, "trustedIssuers", len(settings.TrustedIssuers),
		"downstreamOAuth", cfg.DownstreamOAuth)
	return &oauthRuntime{server: runtime.Server, handler: runtime.Handler, cleanup: cleanup,
		downstreamOAuth: cfg.DownstreamOAuth, issuer: providerIssuer(settings), mcpPath: mcpPath, log: log}, nil
}

// protectedResources adds the REST API and the MCP endpoint to the protected
// resources the configuration names. A path the configuration already
// describes keeps its entry; an added one advertises the main resource's
// authorization servers, as OAUTH_ADDITIONAL_RESOURCES entries do.
func protectedResources(configuration *oauthserver.Config, mcpPath string) map[string]oauthserver.ProtectedResourceConfig {
	resources := maps.Clone(configuration.ResourceMetadataByPath)
	if resources == nil {
		resources = map[string]oauthserver.ProtectedResourceConfig{}
	}
	// Metadata lookup ignores a trailing slash, so "/mcp/" describes "/mcp".
	described := func(path string) (oauthserver.ProtectedResourceConfig, bool) {
		for key, resource := range resources {
			if strings.TrimRight(key, "/") == strings.TrimRight(path, "/") {
				return resource, true
			}
		}
		return oauthserver.ProtectedResourceConfig{}, false
	}
	var advertised []string
	if main, err := url.Parse(configuration.GetResourceIdentifier()); err == nil {
		resource, _ := described(main.EscapedPath())
		advertised = resource.AuthorizationServers
	}
	for _, path := range []string{api.Prefix, mcpPath} {
		if path == "" || path == "/" {
			continue
		}
		if _, ok := described(path); !ok {
			resources[path] = oauthserver.ProtectedResourceConfig{AuthorizationServers: advertised}
		}
	}
	return resources
}

func providerName(settings oauthconfig.Settings) string {
	switch {
	case settings.Dex != nil:
		return "dex"
	case settings.Google != nil:
		return "google"
	default:
		return "none"
	}
}

// providerIssuer is the issuer of the provider people sign in with through
// this server's authorization server.
func providerIssuer(settings oauthconfig.Settings) string {
	switch {
	case settings.Dex != nil:
		return settings.Dex.IssuerURL
	case settings.Google != nil:
		return "https://accounts.google.com"
	default:
		return ""
	}
}

func (o *oauthRuntime) register(mux *http.ServeMux) {
	o.handler.RegisterOAuthRoutes(mux, handler.OAuthRoutesOptions{MCPPath: o.mcpPath, IncludeMetadata: true})
}

// protect requires a valid bearer token (mcp-oauth ValidateToken: this
// server's own access tokens, or a forwarded IdP id_token whose audience is
// trusted) and then attaches the caller and their IdP token to the request.
// With DownstreamOAuth a request without an IdP token is refused
// (RequireProviderIDToken): the ServiceAccount holds no permissions, so there
// is nothing else to run it as.
func (o *oauthRuntime) protect(next http.Handler) http.Handler {
	protected := o.attachIdentity(next)
	if o.downstreamOAuth {
		protected = o.handler.RequireProviderIDToken(protected)
	}
	return o.handler.ValidateToken(protected)
}

// attachIdentity translates the validated mcp-oauth user into the request's
// identity and puts the caller's IdP token on it when there is one: a
// forwarded id_token is the bearer itself; for a token this server issued,
// the provider's id_token stored with it. The token is what the session
// operations present to agentgateway and, with DownstreamOAuth, what every
// Kubernetes call presents.
func (o *oauthRuntime) attachIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		info, ok := handler.UserInfoFromContext(ctx)
		if !ok || info == nil {
			// ValidateToken admits nothing without user info; defensive.
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		id := &identity.Identity{Issuer: o.issuer, Subject: info.ID, Email: info.Email, EmailVerified: info.EmailVerified, Name: info.Name, Groups: info.Groups, Source: identity.SourceOAuth}
		if info.IsSSO() {
			id.Source = identity.SourceSSO
		}
		ctx = identity.ContextWith(ctx, id)
		tok, _ := handler.ProviderIDTokenFromContext(ctx)
		ctx = identity.ContextWithToken(ctx, tok)
		if claims, err := oidc.ParseUnverifiedClaims(tok); err == nil {
			if actor, exists := claims["act"]; exists && actor != nil {
				id.Delegated = true
			}
		}
		if audiences, ok := unverifiedAudience(tok); ok {
			slices.Sort(audiences)
			encoded, _ := json.Marshal(audiences)
			id.Client = string(encoded)
		}
		o.log.Debug("authenticated request", "caller", id.String(), "source", id.Source, "path", r.URL.Path)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// unverifiedAudience reads the aud claim (a string or a list) of a compact
// JWS without verifying anything about it.
func unverifiedAudience(token string) ([]string, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, false
	}
	var claims struct {
		Aud json.RawMessage `json:"aud"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || len(claims.Aud) == 0 {
		return nil, false
	}
	var one string
	if json.Unmarshal(claims.Aud, &one) == nil && one != "" {
		return []string{one}, true
	}
	var many []string
	if json.Unmarshal(claims.Aud, &many) == nil && len(many) > 0 {
		return many, true
	}
	return nil, false
}

func (o *oauthRuntime) shutdown(ctx context.Context) {
	if err := o.cleanup(ctx); err != nil {
		o.log.Warn("oauth server shutdown", "error", err)
	}
}
