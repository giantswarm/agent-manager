package muster

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recorded struct {
	method, path, auth, sender string
	body                       map[string]any
}

func recorder(t *testing.T, got *[]recorded) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		*got = append(*got, recorded{r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get(SenderHeader), body})
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestABindCarriesTheApprovalAndTheSenderTokenReadPerCall(t *testing.T) {
	var got []recorded
	srv := recorder(t, &got)
	token := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(token, []byte("sa-token-1\n"), 0o600))
	c, err := New(srv.URL+"/session-bindings", nil)
	require.NoError(t, err)
	c = c.WithSenderTokenFile(token)
	assert.True(t, c.SendsApproval())

	b := Binding{Actor: actor, Session: "kagent/0199", Providers: []string{"muster"}, Toolset: []string{"server:github"}, Agent: "kagent/alert-review",
		Approval: &Approval{Entries: []ApprovalEntry{{Entry: "server:github", Setting: "Ask"}}, Default: "Block", Grants: []Grant{{Entry: "default", Server: "github", UID: "u"}}}}
	require.NoError(t, c.Bind(t.Context(), "id-token", b))
	require.NoError(t, os.WriteFile(token, []byte("sa-token-2"), 0o600))
	require.NoError(t, c.DropGrants(t.Context(), "id-token", "kagent/alert-review", nil))
	require.NoError(t, c.Unbind(t.Context(), "id-token", actor))

	require.Len(t, got, 3)
	assert.Equal(t, "sa-token-1", got[0].sender)
	assert.Equal(t, "kagent/alert-review", got[0].body["agent"])
	assert.Equal(t, map[string]any{"entries": []any{map[string]any{"entry": "server:github", "setting": "Ask"}}, "default": "Block",
		"grants": []any{map[string]any{"entry": "default", "server": "github", "uid": "u"}}}, got[0].body["approval"])

	assert.Equal(t, http.MethodPatch, got[1].method)
	assert.Equal(t, "/session-bindings/grants", got[1].path)
	assert.Equal(t, "sa-token-2", got[1].sender, "the rotated token")
	assert.Equal(t, map[string]any{"agent": "kagent/alert-review", "keep": []any{}}, got[1].body)

	assert.Empty(t, got[2].sender, "an unbind needs no sender")
}

func TestDropGrantsNeedsTheSenderToken(t *testing.T) {
	var got []recorded
	srv := recorder(t, &got)
	c, err := New(srv.URL+"/session-bindings", nil)
	require.NoError(t, err)
	assert.False(t, c.SendsApproval())
	err = c.DropGrants(t.Context(), "id-token", "kagent/a", nil)
	require.True(t, errors.Is(err, ErrInvalid))
	assert.Empty(t, got)
}
