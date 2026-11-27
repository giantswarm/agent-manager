package organizations

import (
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/giantswarm/mcp-oauth/handler"
	"github.com/giantswarm/mcp-oauth/providers"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/agent-manager/internal/idtoken"
)

func TestCredentialOwnerRequiresTheValidatedProviderAccount(t *testing.T) {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))}, nil)
	require.NoError(t, err)
	for _, scenario := range []struct {
		name, issuer, subject string
		source                providers.TokenSource
		actor                 any
		want                  bool
	}{
		{"opaque access token", "https://dex.example.test", "alice", providers.TokenSourceOAuth, nil, true},
		{"same issuer", "https://manager.example.test", "alice", providers.TokenSourceTrustedIssuer, nil, true},
		{"different subject", "https://dex.example.test", "bob", providers.TokenSourceOAuth, nil, false},
		{"missing issuer", "", "alice", providers.TokenSourceOAuth, nil, false},
		{"missing subject", "https://dex.example.test", "", providers.TokenSourceOAuth, nil, false},
		{"other forwarded issuer", "https://dex.example.test", "alice", providers.TokenSourceTrustedIssuer, nil, false},
		{"provider actor", "https://dex.example.test", "alice", providers.TokenSourceOAuth, map[string]any{"sub": "agent"}, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			person := &providers.UserInfo{Issuer: "https://manager.example.test", ID: "alice", TokenSource: scenario.source}
			token, err := jwt.Signed(signer).Claims(jwt.Claims{Issuer: scenario.issuer, Subject: scenario.subject, Expiry: jwt.NewNumericDate(time.Now().Add(time.Hour))}).Claims(map[string]any{"act": scenario.actor}).Serialize()
			require.NoError(t, err)
			ctx := idtoken.ContextWith(handler.ContextWithUserInfo(t.Context(), person), token)
			owner := credentialOwner(ctx)
			if scenario.want {
				require.NotNil(t, owner)
				require.Equal(t, scenario.issuer, owner.Issuer)
				require.Equal(t, "alice", owner.ID)
			} else {
				require.Nil(t, owner)
			}
			require.Equal(t, "https://manager.example.test", person.Issuer)
		})
	}
}

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
