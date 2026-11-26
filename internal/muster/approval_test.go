package muster

import (
	"encoding/json"
	"errors"
	"fmt"
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
		Approval: &Approval{Entries: []ApprovalEntry{{Entry: "server:github", Setting: "Ask"}}, Default: "Block", Grants: []Grant{{Entry: "default", Server: "github", UID: "u", Tools: []string{"merge"}}}}}
	_, err = c.Bind(t.Context(), "id-token", b)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(token, []byte("sa-token-2"), 0o600))
	require.NoError(t, c.DropGrants(t.Context(), "id-token", "kagent/alert-review", nil))
	require.NoError(t, c.Unbind(t.Context(), "id-token", actor))

	require.Len(t, got, 3)
	assert.Equal(t, "sa-token-1", got[0].sender)
	assert.Equal(t, "kagent/alert-review", got[0].body["agent"])
	assert.Equal(t, map[string]any{"entries": []any{map[string]any{"entry": "server:github", "setting": "Ask"}}, "default": "Block",
		"grants": []any{map[string]any{"entry": "default", "server": "github", "uid": "u", "tools": []any{"merge"}}}}, got[0].body["approval"])

	assert.Equal(t, http.MethodPatch, got[1].method)
	assert.Equal(t, "/session-bindings/grants", got[1].path)
	assert.Equal(t, "sa-token-2", got[1].sender, "the rotated token")
	assert.Equal(t, map[string]any{"agent": "kagent/alert-review", "keep": []any{}}, got[1].body)

	assert.Equal(t, "sa-token-2", got[2].sender, "an unbind carries the sender too: muster deletes a sender-written binding only with it (M2-S3-R-01)")
	assert.Equal(t, http.MethodDelete, got[2].method)
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

// muster's answer on the agent's settings is read from its response header;
// a muster that predates them refuses the unknown field, which the client
// tells apart; a refused replacement is a conflict (M2-S3-C-06, S-01).
func TestBindReadsWhatMusterDidWithTheSettings(t *testing.T) {
	answer := func(status int, header, body string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if header != "" {
				w.Header().Set(SettingsHeader, header)
			}
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	b := Binding{Actor: actor, Session: "kagent/0199", Providers: []string{"muster"}, Agent: "kagent/a"}

	c, err := New(answer(http.StatusNoContent, SettingsBound, "").URL+"/session-bindings", nil)
	require.NoError(t, err)
	res, err := c.Bind(t.Context(), "id-token", b)
	require.NoError(t, err)
	assert.Equal(t, SettingsBound, res.Settings)

	c, _ = New(answer(http.StatusNoContent, "", "").URL+"/session-bindings", nil)
	res, err = c.Bind(t.Context(), "id-token", b)
	require.NoError(t, err)
	assert.Empty(t, res.Settings, "a muster without the header")

	c, _ = New(answer(http.StatusBadRequest, "", `bad request: json: unknown field "agent"`).URL+"/session-bindings", nil)
	_, err = c.Bind(t.Context(), "id-token", b)
	require.ErrorIs(t, err, ErrUnknownField)
	require.ErrorIs(t, err, ErrInvalid)

	c, _ = New(answer(http.StatusConflict, "", "conflict").URL+"/session-bindings", nil)
	_, err = c.Bind(t.Context(), "id-token", b)
	require.ErrorIs(t, err, ErrConflict)
}

// The sender and the person's tokens never follow a redirect (S-13).
func TestTheClientFollowsNoRedirect(t *testing.T) {
	var reached bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { reached = true; w.WriteHeader(http.StatusNoContent) }))
	t.Cleanup(target.Close)
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/session-bindings", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirect.Close)
	c, err := New(redirect.URL+"/session-bindings", &http.Client{})
	require.NoError(t, err)
	_, err = c.Bind(t.Context(), "id-token", Binding{Actor: actor, Providers: []string{"github"}})
	require.Error(t, err)
	assert.False(t, reached, "the redirect was not followed")
}

// A binding at muster's 64 KiB limit goes through; past it the refusal names
// the approval part (M2-S3-C-10).
func TestBindLimitFitsTheApprovalSettings(t *testing.T) {
	var got []recorded
	srv := recorder(t, &got)
	c, err := New(srv.URL+"/session-bindings", nil)
	require.NoError(t, err)
	big := Binding{Actor: actor, Session: "kagent/0199", Providers: []string{"muster"}, Agent: "kagent/a", Approval: &Approval{}}
	for i := 0; i < 128; i++ {
		sel := fmt.Sprintf("tool:x_github_tool_with_a_long_name_%03d", i)
		big.Toolset = append(big.Toolset, sel)
		big.Approval.Entries = append(big.Approval.Entries, ApprovalEntry{Entry: sel, Setting: "Allow"})
	}
	for i := 0; i < 256; i++ {
		big.Approval.Grants = append(big.Approval.Grants, Grant{Entry: big.Toolset[i%128], Server: "connectors/github", UID: "0b8e8f8a-1c2d-4e5f-8a9b-0c1d2e3f4a5b", Tools: []string{"tool_with_a_long_name"}})
	}
	_, err = c.Bind(t.Context(), "id-token", big)
	require.NoError(t, err, "128 entries and 256 grants fit")
	require.Len(t, got, 1)
	for i := 0; i < 400; i++ {
		big.Approval.Grants = append(big.Approval.Grants, big.Approval.Grants[0])
	}
	_, err = c.Bind(t.Context(), "id-token", big)
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "tool approval settings and grants")
}

// A grant drop goes with the sender token alone where no person's token is
// at hand (the startup sweep); a bind never does.
func TestDropGrantsWithTheSenderTokenAlone(t *testing.T) {
	var got []recorded
	srv := recorder(t, &got)
	token := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(token, []byte("sa-token"), 0o600))
	c, err := New(srv.URL+"/session-bindings", nil)
	require.NoError(t, err)
	c = c.WithSenderTokenFile(token)
	require.NoError(t, c.DropGrants(t.Context(), "", "kagent/a", nil))
	require.Len(t, got, 1)
	assert.Empty(t, got[0].auth, "no person bearer")
	assert.Equal(t, "sa-token", got[0].sender)
	_, err = c.Bind(t.Context(), "", Binding{Actor: actor, Providers: []string{"github"}})
	require.ErrorIs(t, err, ErrUnauthenticated)
}
