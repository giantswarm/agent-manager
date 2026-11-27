// Package idtoken carries the user's OpenID Connect ID token on a request
// context: the bearer the session operations present to agentgateway and,
// with --downstream-oauth, every Kubernetes call. The OAuth layer sets it
// whenever mcp-oauth resolves one for the validated user. Request
// continuations retain the token, including after expiry; work without a
// request carries none.
package idtoken

import (
	"context"
	"time"

	"github.com/giantswarm/mcp-oauth/providers/oidc"
	"github.com/go-jose/go-jose/v4/jwt"
)

type contextKey struct{}

// ContextWith returns ctx carrying token. An empty token adds nothing.
func ContextWith(ctx context.Context, token string) context.Context {
	if token == "" {
		return ctx
	}
	return context.WithValue(ctx, contextKey{}, token)
}

// FromContext returns the user's ID token, if the request carries one.
func FromContext(ctx context.Context) (string, bool) {
	token, ok := ctx.Value(contextKey{}).(string)
	return token, ok && token != ""
}

// Claims are the identity, expiry and actor presence of a validated ID token.
type Claims struct {
	Issuer  string
	Subject string
	Expiry  time.Time
	Actor   bool
}

// ClaimsOf decodes a previously validated token without verifying it again.
// Invalid claim encodings produce zero Claims.
func ClaimsOf(token string) Claims {
	var claims struct {
		jwt.Claims
		Act any `json:"act"`
	}
	if err := oidc.UnmarshalUnverifiedClaims(token, &claims); err != nil {
		return Claims{}
	}
	return Claims{Issuer: claims.Issuer, Subject: claims.Subject, Expiry: claims.Expiry.Time(), Actor: claims.Act != nil}
}
