package agents

import (
	"context"
	"log/slog"

	"github.com/giantswarm/mcp-oauth/handler"
	"github.com/giantswarm/mcp-oauth/providers"
)

// User is the user mcp-oauth validated for the request, or nil without one
// (a server without OAuth, or work without a request).
func User(ctx context.Context) *providers.UserInfo {
	user, ok := handler.UserInfoFromContext(ctx)
	if !ok {
		return nil
	}
	return user
}

// UserAttr names the user a call acts for by issuer, subject and actor
// (providers.UserInfo's LogValue, never profile data). Work without a user
// logs an empty group, which handlers drop.
func UserAttr(ctx context.Context) slog.Attr {
	return slog.Any("user", User(ctx))
}

// RequestedBy is the attribution recorded on results and created agents: the
// user's email, else the subject, or "" without a user.
func RequestedBy(ctx context.Context) string {
	user := User(ctx)
	switch {
	case user == nil:
		return ""
	case user.Email != "":
		return user.Email
	default:
		return user.ID
	}
}

// UserName is the user's display name, or "" without one. For display only:
// RequestedBy is the identifier.
func UserName(ctx context.Context) string {
	if user := User(ctx); user != nil {
		return user.Name
	}
	return ""
}

// GitHubUserAccessToken is a GitHub App user access token for one commit,
// obtained through muster's broker, with the login GET /user answered for
// it. Commit mode opens the pull request with that token.
type GitHubUserAccessToken struct {
	Login string
	Token string
	// App names the provider App in muster the person granted, as
	// grant_required answers name it.
	App string
}
