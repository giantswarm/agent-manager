// Package identity carries the authenticated caller through a request: who
// the OAuth layer validated (subject, email, groups) and, when the server acts
// on the caller's behalf towards the Kubernetes API, the caller's own IdP
// token. agent-manager does nothing without a caller — every Kubernetes call
// belongs to a request — so a context without an identity only exists in
// tests and in a server that runs without OAuth (the ServiceAccount then).
package identity

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"strings"
	"time"
)

// Source says how the caller was authenticated.
type Source string

const (
	// SourceSSO is an IdP id_token forwarded by a trusted aggregator (muster)
	// or sent by the portal through the gateway, validated against the IdP's
	// JWKS.
	SourceSSO Source = "sso"
	// SourceOAuth is an access token issued by this server's own OAuth 2.1
	// flow (a client that authenticated directly, e.g. mcp-debug).
	SourceOAuth Source = "oauth"
)

// Identity is the authenticated caller.
type Identity struct {
	Issuer    string
	Client    string
	Delegated bool
	// Subject is the IdP's stable user id (the `sub` claim).
	Subject string
	// Email is the caller's email when the IdP provided one.
	Email string
	// EmailVerified is the IdP's `email_verified` claim. An absent claim is
	// false: the address is then not proven.
	EmailVerified bool
	// Name is the display name when the IdP provided one.
	Name string
	// Groups are the IdP group claims (Dex `groups`; empty for Google).
	Groups []string
	// Source is how the token was validated.
	Source Source
}

// String is the caller as logged and reported on results: the email, else
// the subject.
func (id *Identity) String() string {
	if id == nil {
		return ""
	}
	if id.Email != "" {
		return id.Email
	}
	return id.Subject
}

type ctxKey int

const (
	identityKey ctxKey = iota
	tokenKey
	agentCallerKey
)

// CallerKindHeader is the header muster adds to the calls an agent makes (an
// actor JWT or a delegated caller) to the servers its governance
// configuration names, and strips from what clients send. agent-manager
// cannot tell an agent from its person otherwise: muster forwards the
// person's identity for both.
const CallerKindHeader = "X-Muster-Caller-Kind"

// CallerKindAgent is CallerKindHeader's value for an agent caller.
const CallerKindAgent = "agent"

// ContextWithAgentCaller marks the request as an agent's (CallerKindHeader).
// The mark only ever takes rights away (an agent's save keeps no approval
// grant), so a client sending the header itself gains nothing.
func ContextWithAgentCaller(ctx context.Context) context.Context {
	return context.WithValue(ctx, agentCallerKey, true)
}

// IsAgentCaller reports whether the request carried CallerKindHeader: agent.
func IsAgentCaller(ctx context.Context) bool {
	agent, _ := ctx.Value(agentCallerKey).(bool)
	return agent
}

// CallerKind is "agent" for an agent caller, else "person", for the logs.
func CallerKind(ctx context.Context) string {
	if IsAgentCaller(ctx) {
		return CallerKindAgent
	}
	return "person"
}

// ContextWith returns ctx carrying id.
func ContextWith(ctx context.Context, id *Identity) context.Context {
	if id == nil {
		return ctx
	}
	return context.WithValue(ctx, identityKey, id)
}

// FromContext returns the caller, if any.
func FromContext(ctx context.Context) (*Identity, bool) {
	id, ok := ctx.Value(identityKey).(*Identity)
	return id, ok && id != nil
}

// Caller is the caller's String(), or "" without one.
func Caller(ctx context.Context) string {
	id, _ := FromContext(ctx)
	return id.String()
}

// VerifiedCaller is the caller's email where the IdP verified it, else its
// subject: what a record that someone relies on later names (an approval
// grant's grantedBy).
func VerifiedCaller(ctx context.Context) string {
	id, ok := FromContext(ctx)
	if !ok || id == nil {
		return ""
	}
	if id.Email != "" && id.EmailVerified {
		return id.Email
	}
	return id.Subject
}

// CallerName is the caller's display name, or "" without one. For display
// only: Caller is the identifier.
func CallerName(ctx context.Context) string {
	if id, ok := FromContext(ctx); ok {
		return id.Name
	}
	return ""
}

// LogAttr is the structured-log attribute every mutation carries: the caller,
// or the ServiceAccount marker when the server runs without OAuth.
func LogAttr(ctx context.Context) slog.Attr {
	if c := Caller(ctx); c != "" {
		return slog.String("caller", c)
	}
	return slog.String("caller", "service-account")
}

// ContextWithToken returns ctx carrying the caller's IdP token: what the
// session operations present to agentgateway and, with downstream OAuth, what
// every Kubernetes API call presents. Set whenever OAuth resolved one.
func ContextWithToken(ctx context.Context, token string) context.Context {
	if token == "" {
		return ctx
	}
	return context.WithValue(ctx, tokenKey, token)
}

// TokenFromContext returns the caller's IdP token, if any.
func TokenFromContext(ctx context.Context) (string, bool) {
	t, ok := ctx.Value(tokenKey).(string)
	return t, ok && t != ""
}

// TokenExpiry reads the `exp` claim of a JWT without verifying it — the token
// was validated when the request came in; this only bounds how long the
// per-caller Kubernetes clients built from it are worth keeping. Zero when the
// token is not a JWT or carries no exp.
func TokenExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp json.Number `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}
	}
	exp, err := claims.Exp.Int64()
	if err != nil || exp <= 0 {
		return time.Time{}
	}
	return time.Unix(exp, 0)
}

// GitHub is the caller's GitHub identity: the person's grant of a provider
// App, fetched from muster's broker, the login GET /user answered for it, and
// the token commit mode opens the pull request with.
type GitHub struct {
	Login string
	Token string
	// App names the provider App in muster whose grant Token is, as
	// grant_required answers name it.
	App string
}
