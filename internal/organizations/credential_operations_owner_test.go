package organizations

import (
	"testing"

	"github.com/giantswarm/mcp-oauth/providers"

	"github.com/stretchr/testify/require"
)

// The digest folds ASCII case only: a Kelvin sign is not the letter k.
func TestVerifiedEmailDigestFoldsASCIICaseOnly(t *testing.T) {
	digest := func(email string, verified bool) string {
		return verifiedEmailDigest(&providers.UserInfo{Email: email, EmailVerified: verified})
	}
	require.NotEmpty(t, digest("kim@example.com", true))
	require.Equal(t, digest("kim@example.com", true), digest("KIM@Example.COM", true))
	require.NotEqual(t, digest("kim@example.com", true), digest("Kim@example.com", true))
	require.Empty(t, digest("kim@example.com", false))
	require.Empty(t, digest("", true))
	require.Empty(t, verifiedEmailDigest(nil))
}
