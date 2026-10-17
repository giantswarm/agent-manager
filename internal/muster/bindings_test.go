package muster

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const actor = "spiffe://substrate-actor.local/actor/kagent-sre/session-0199"

func TestActorID(t *testing.T) {
	assert.Equal(t, actor, ActorID("kagent-sre", "session-0199"))
}

func TestBindPutsTheBindingAsTheCaller(t *testing.T) {
	var got struct {
		method, path, auth, contentType string
		body                            Binding
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path, got.auth, got.contentType = r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(raw, &got.body))
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL+"/session-bindings", nil)
	require.NoError(t, err)

	b := Binding{Actor: actor, Session: "kagent/0199", Providers: []string{"github"}}
	require.NoError(t, c.Bind(t.Context(), "id-token", b))
	assert.Equal(t, http.MethodPut, got.method)
	assert.Equal(t, "/session-bindings", got.path)
	assert.Equal(t, "Bearer id-token", got.auth)
	assert.Equal(t, "application/json", got.contentType)
	assert.Equal(t, b, got.body)
}

func TestUnbindDeletesTheActorsBinding(t *testing.T) {
	var method, query, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, query, auth = r.Method, r.URL.Query().Get("actor"), r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL+"/session-bindings", nil)
	require.NoError(t, err)

	require.NoError(t, c.Unbind(t.Context(), "id-token", actor))
	assert.Equal(t, http.MethodDelete, method)
	assert.Equal(t, actor, query)
	assert.Equal(t, "Bearer id-token", auth)
}

func TestRefusalsAreSentinelsInMustersWords(t *testing.T) {
	for code, sentinel := range map[int]error{
		http.StatusForbidden:    ErrForbidden,
		http.StatusBadRequest:   ErrInvalid,
		http.StatusUnauthorized: ErrUnauthenticated,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"error":"refused","message":"the reason"}`))
		}))
		c, err := New(srv.URL+"/session-bindings", nil)
		require.NoError(t, err)
		err = c.Bind(t.Context(), "id-token", Binding{Actor: actor, Providers: []string{"github"}})
		require.ErrorIs(t, err, sentinel, code)
		assert.Contains(t, err.Error(), "muster")
		assert.Contains(t, err.Error(), "the reason")
		assert.NotContains(t, err.Error(), "id-token")
		srv.Close()
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL+"/session-bindings", nil)
	require.NoError(t, err)
	err = c.Unbind(t.Context(), "id-token", actor)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "muster session bindings answered 503")
}

func TestNoCallWithoutAToken(t *testing.T) {
	c, err := New("http://muster.example/session-bindings", nil)
	require.NoError(t, err)
	require.ErrorIs(t, c.Bind(t.Context(), "", Binding{}), ErrUnauthenticated)
}

func TestTheEndpointIsAnHTTPURL(t *testing.T) {
	for _, bad := range []string{"", "muster:8090", "grpc://muster:8090", "http://muster/session-bindings?x=1", "http:///session-bindings"} {
		_, err := New(bad, nil)
		require.Error(t, err, bad)
	}
}
