package agents

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/giantswarm/mcp-oauth/handler"
	"github.com/giantswarm/mcp-oauth/providers"
	"github.com/giantswarm/mcp-oauth/providers/oidc"
	"github.com/stretchr/testify/require"
)

func TestMutationLogNamesTheUserWithoutProfileData(t *testing.T) {
	user := &providers.UserInfo{Issuer: "https://dex.example.test", ID: "subject", ActorIssuer: "https://sts.example.test", ActorSubject: "agent", Email: "private@example.test", Name: "Private Name", Groups: []string{"private-group"}}
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))

	logger.Info("agent deleted", UserAttr(handler.ContextWithUserInfo(t.Context(), user)))
	var record map[string]any
	require.NoError(t, json.Unmarshal(output.Bytes(), &record))
	require.Equal(t, map[string]any{
		"issuer":  "https://dex.example.test",
		"subject": "subject",
		"actor":   map[string]any{"issuer": "https://sts.example.test", "subject": "agent"},
	}, record["user"])
	for _, private := range []string{user.Email, user.Name, user.Groups[0]} {
		require.NotContains(t, output.String(), private)
	}
	require.NotContains(t, record, "caller")

	output.Reset()
	logger.Info("skills boot Secret minted", UserAttr(t.Context()))
	record = nil
	require.NoError(t, json.Unmarshal(output.Bytes(), &record))
	require.NotContains(t, record, "user", "work without a request names no user")
}

func TestRequestedBy(t *testing.T) {
	require.Empty(t, RequestedBy(t.Context()))
	require.Empty(t, UserName(t.Context()))
	ctx := handler.ContextWithUserInfo(t.Context(), &providers.UserInfo{Issuer: "https://dex.example.test", ID: "subject", Email: "alice@example.com", Name: "Alice"})
	require.Equal(t, "alice@example.com", RequestedBy(ctx))
	require.Equal(t, "Alice", UserName(ctx))
	ctx = handler.ContextWithUserInfo(t.Context(), &providers.UserInfo{Issuer: "https://dex.example.test", ID: "subject"})
	require.Equal(t, "subject", RequestedBy(ctx), "without an email, the subject")
}

func TestDelegated(t *testing.T) {
	require.False(t, Delegated(nil))
	require.False(t, Delegated(&providers.UserInfo{ID: "alice"}))
	require.True(t, Delegated(&providers.UserInfo{ID: "alice", ActorSubject: "agent", ActorChain: []oidc.ActorClaim{{Subject: "agent"}}}))
	require.True(t, Delegated(&providers.UserInfo{ID: "alice", ActorChain: []oidc.ActorClaim{{}}}), "an act claim that names nobody still delegates")
}
