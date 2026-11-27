package idtoken

import (
	"crypto/rand"
	"crypto/rsa"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"
)

func TestContext(t *testing.T) {
	_, ok := FromContext(t.Context())
	require.False(t, ok)
	_, ok = FromContext(ContextWith(t.Context(), ""))
	require.False(t, ok, "an empty token adds nothing")
	token, ok := FromContext(ContextWith(t.Context(), "id-token"))
	require.True(t, ok)
	require.Equal(t, "id-token", token)
}

func TestClaimsOf(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, nil)
	require.NoError(t, err)
	expiry := time.Unix(1_900_000_000, 0)
	sign := func(extra map[string]any) string {
		token, err := jwt.Signed(signer).Claims(jwt.Claims{Issuer: "https://dex.example.test", Subject: "alice", Expiry: jwt.NewNumericDate(expiry)}).Claims(extra).Serialize()
		require.NoError(t, err)
		return token
	}

	require.Equal(t, Claims{Issuer: "https://dex.example.test", Subject: "alice", Expiry: expiry}, ClaimsOf(sign(nil)))
	require.True(t, ClaimsOf(sign(map[string]any{"act": map[string]any{"sub": "agent"}})).Actor)
	require.True(t, ClaimsOf(sign(map[string]any{"act": map[string]any{}})).Actor, "an act claim that names nobody is still an actor")
	require.Equal(t, Claims{}, ClaimsOf("opaque-token"))
}
