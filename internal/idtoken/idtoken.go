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

	"github.com/go-jose/go-jose/v4"
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

// signatureAlgorithms are the JWS algorithms whose tokens Claims reads. The
// signature is not checked: mcp-oauth verified it when the request came in.
var signatureAlgorithms = []jose.SignatureAlgorithm{
	jose.RS256, jose.RS384, jose.RS512,
	jose.PS256, jose.PS384, jose.PS512,
	jose.ES256, jose.ES384, jose.ES512,
	jose.EdDSA,
}

// Claims are the registered claims (RFC 7519 4.1) of an ID token, and
// whether it names an actor (RFC 8693 4.1 act).
type Claims struct {
	Issuer string
	Expiry time.Time
	Actor  bool
}

// ClaimsOf reads the claims of token without verifying it. The zero Claims
// when the token is not a JWS.
func ClaimsOf(token string) Claims {
	parsed, err := jwt.ParseSigned(token, signatureAlgorithms)
	if err != nil {
		return Claims{}
	}
	var registered jwt.Claims
	var private struct {
		Act any `json:"act"`
	}
	if err := parsed.UnsafeClaimsWithoutVerification(&registered, &private); err != nil {
		return Claims{}
	}
	return Claims{Issuer: registered.Issuer, Expiry: registered.Expiry.Time(), Actor: private.Act != nil}
}
