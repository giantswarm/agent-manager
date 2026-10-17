package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/identity"
	"github.com/giantswarm/agent-manager/internal/kagent"
	apiv1alpha1 "github.com/giantswarm/agent-manager/internal/kagent/gen/kagent/api/v1alpha1"
	"github.com/giantswarm/agent-manager/internal/kagent/kagenttest"
	"github.com/giantswarm/agent-manager/internal/kube"
	"github.com/giantswarm/agent-manager/internal/muster"
)

const callerToken = "caller-id-token"

func newSessionService(t *testing.T) (*agents.Service, *kagenttest.Controller) {
	t.Helper()
	ctrl, conn := kagenttest.New(t)
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	typed := kubefake.NewClientset()
	// muster's session bindings, accepting every bind and unbind.
	bindings := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	t.Cleanup(bindings.Close)
	binder, err := muster.New(bindings.URL+"/session-bindings", nil)
	require.NoError(t, err)
	svc := agents.New(kube.NewServiceAccountProvider(kube.FromInterfaces(dyn, typed, typed.Discovery())), embeddedChart{}, nil, pinner{}, agents.Config{
		DefaultNamespace: "kagent", Version: "test", Sessions: kagent.NewClient(conn), Bindings: binder,
	}, nil)
	return svc, ctrl
}

// asCaller is the identity the OAuth layer attaches: the caller and the IdP
// token the session operations present.
func asCaller(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := identity.ContextWith(r.Context(), &identity.Identity{Email: "admin@lab.local"})
		next.ServeHTTP(w, r.WithContext(identity.ContextWithToken(ctx, callerToken)))
	})
}

func waitTurn(t *testing.T, ctrl *kagenttest.Controller) {
	t.Helper()
	select {
	case <-ctrl.TurnEnded():
	case <-time.After(5 * time.Second):
		t.Fatal("the turn's stream stayed open")
	}
}

func TestSessionsOverREST(t *testing.T) {
	svc, ctrl := newSessionService(t)
	mux := http.NewServeMux()
	NewREST(svc, nil).Register(mux)
	h := asCaller(mux)

	code, info := do(t, h, http.MethodGet, Prefix+"/info", nil)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, true, info["capabilities"].(map[string]any)["sessions"])

	code, body := do(t, h, http.MethodPost, Prefix+"/agents/kagent/sre/sessions", map[string]any{"message": "hello", "requestId": "req-1"})
	require.Equal(t, http.StatusCreated, code, body)
	session := body["session"].(map[string]any)
	assert.Equal(t, "RUNTIME_STATE_READY", session["state"])
	assert.Equal(t, "TASK_STATE_SUBMITTED", body["turn"].(map[string]any)["task"].(map[string]any)["status"].(map[string]any)["state"])
	waitTurn(t, ctrl)

	code, body = do(t, h, http.MethodGet, Prefix+"/sessions/"+session["id"].(string), nil)
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, "summary", body["detail"])
	turns := body["turns"].([]any)
	require.Len(t, turns, 1)
	assert.Equal(t, "echo: hello", turns[0].(map[string]any)["reply"])
	assert.NotContains(t, body, "tasks")

	code, body = do(t, h, http.MethodGet, Prefix+"/sessions/"+session["id"].(string)+"?detail=full&lastTurns=1&maxBytes=50000", nil)
	require.Equal(t, http.StatusOK, code, body)
	tasks := body["tasks"].([]any)
	require.Len(t, tasks, 1)
	assert.Equal(t, "TASK_STATE_COMPLETED", tasks[0].(map[string]any)["status"].(map[string]any)["state"])

	code, body = do(t, h, http.MethodGet, Prefix+"/sessions/"+session["id"].(string)+"/turns/last", nil)
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, tasks[0].(map[string]any)["id"], body["taskId"])
	assert.Equal(t, "transcript", body["detail"])
	assert.Len(t, body["messages"], 2, "the user's message and the reply")

	code, body = do(t, h, http.MethodGet, Prefix+"/sessions/"+session["id"].(string)+"/turns/0?fromMessage=1&detail=full", nil)
	require.Equal(t, http.StatusOK, code, body)
	assert.Len(t, body["history"], 1)

	// maxBytes bounds the body as sent: compact, not indented.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Prefix+"/sessions/"+session["id"].(string)+"?detail=transcript", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "\n  ")

	for _, query := range []string{"?maxBytes=lots", "?lastTurns=-1", "?detail=everything"} {
		code, body = do(t, h, http.MethodGet, Prefix+"/sessions/"+session["id"].(string)+query, nil)
		assert.Equal(t, http.StatusBadRequest, code, query)
		assert.Equal(t, "invalid_request", body["error"].(map[string]any)["code"], query)
	}
	code, _ = do(t, h, http.MethodGet, Prefix+"/sessions/"+session["id"].(string)+"/turns/0?fromMessage=x", nil)
	assert.Equal(t, http.StatusBadRequest, code)
	code, _ = do(t, h, http.MethodGet, Prefix+"/sessions/"+session["id"].(string)+"/turns/5", nil)
	assert.Equal(t, http.StatusNotFound, code)

	code, body = do(t, h, http.MethodGet, Prefix+"/agents/kagent/sre/sessions?allCreators=true", nil)
	require.Equal(t, http.StatusOK, code, body)
	assert.Len(t, body["sessions"], 1)
	assert.Equal(t, map[string]any{"namespace": "kagent", "name": "sre"}, body["agent"])

	// Without a message the session is only created: no turn, nothing sent.
	calls := len(ctrl.Calls())
	code, body = do(t, h, http.MethodPost, Prefix+"/agents/kagent/sre/sessions", map[string]any{"sessionName": "attachments"})
	require.Equal(t, http.StatusCreated, code, body)
	assert.Equal(t, "attachments", body["session"].(map[string]any)["name"])
	assert.NotContains(t, body, "turn")
	require.Len(t, ctrl.Calls(), calls+1)
	assert.Equal(t, "CreateSession", ctrl.Calls()[calls].Method)

	// Unknown fields are refused, as on every write.
	code, _ = do(t, h, http.MethodPost, Prefix+"/agents/kagent/sre/sessions", map[string]any{"message": "hi", "agentInstance": "x"})
	assert.Equal(t, http.StatusBadRequest, code)

	// A workspace grant starts the session in the workspace; one kagent would
	// refuse is refused here with the reason, and the grant is echoed nowhere.
	signer := kagenttest.NewSigner(t)
	grant := signer.Sign(t, "admin@lab.local", 5*time.Minute, kagenttest.WorkspaceGrant())
	code, body = do(t, h, http.MethodPost, Prefix+"/agents/kagent/sre/sessions", map[string]any{"message": "hello", "requestId": "req-2", "workspaceGrant": grant})
	require.Equal(t, http.StatusCreated, code, body)
	assert.Equal(t, "kagent/platform", body["workspace"])
	assert.Equal(t, "nfs.csi.k8s.io", body["session"].(map[string]any)["volumeSource"].(map[string]any)["volume"].(map[string]any)["csiDriver"])
	waitTurn(t, ctrl)
	var last *apiv1alpha1.CreateSessionRequest
	for _, call := range ctrl.Calls() {
		if call.Method == "CreateSession" {
			last = call.Request.(*apiv1alpha1.CreateSessionRequest)
		}
	}
	assert.Equal(t, grant, last.GetAdmissionToken())
	code, body = do(t, h, http.MethodPost, Prefix+"/agents/kagent/sre/sessions", map[string]any{"message": "hello", "workspaceGrant": signer.Sign(t, "dev@lab.local", 5*time.Minute, kagenttest.WorkspaceGrant())})
	assert.Equal(t, http.StatusForbidden, code)
	assert.Equal(t, "forbidden", body["error"].(map[string]any)["code"])
	assert.Contains(t, body["error"].(map[string]any)["message"], "names another person")
	assert.NotContains(t, fmt.Sprint(body), grant[:16])

	// DELETE ends the caller's session; it is gone from every read after.
	code, body = do(t, h, http.MethodDelete, Prefix+"/sessions/"+session["id"].(string), nil)
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, "RUNTIME_STATE_DELETED", body["session"].(map[string]any)["state"])
	code, _ = do(t, h, http.MethodGet, Prefix+"/sessions/"+session["id"].(string), nil)
	assert.Equal(t, http.StatusNotFound, code)
	theirs := ctrl.AddSessionOf("dev@lab.local", "kagent", "sre")
	code, body = do(t, h, http.MethodDelete, Prefix+"/sessions/"+theirs.GetId(), nil)
	assert.Equal(t, http.StatusNotFound, code)
	assert.Equal(t, "not_found", body["error"].(map[string]any)["code"])

	ctrl.NotReady["kagent/fresh"] = true
	code, body = do(t, h, http.MethodPost, Prefix+"/agents/kagent/fresh/sessions", map[string]any{"message": "hi"})
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "conflict", body["error"].(map[string]any)["code"])

	// Without the caller's token nothing is sent.
	calls = len(ctrl.Calls())
	code, body = do(t, mux, http.MethodGet, Prefix+"/agents/kagent/sre/sessions", nil)
	assert.Equal(t, http.StatusUnauthorized, code)
	assert.Equal(t, "unauthenticated", body["error"].(map[string]any)["code"])
	assert.Len(t, ctrl.Calls(), calls)
}

func TestSessionsOverMCP(t *testing.T) {
	svc, ctrl := newSessionService(t)
	srv := NewMCPServer(svc, "test")
	ctx := identity.ContextWithToken(t.Context(), callerToken)

	text, isErr := callToolCtx(t, ctx, srv, ToolStartSession, map[string]any{"name": "sre", "message": "hello", "sessionName": "triage"})
	require.False(t, isErr, text)
	var started struct {
		Session struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"session"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &started))
	assert.Equal(t, "triage", started.Session.Name)
	waitTurn(t, ctrl)

	text, isErr = callToolCtx(t, ctx, srv, ToolGetSession, map[string]any{"sessionId": started.Session.ID})
	require.False(t, isErr, text)
	assert.Contains(t, text, `"reply":"echo: hello"`)

	text, isErr = callToolCtx(t, ctx, srv, ToolGetSession, map[string]any{"sessionId": started.Session.ID, "detail": "full", "lastTurns": 1, "maxBytes": 50000})
	require.False(t, isErr, text)
	assert.Contains(t, text, `"tasks":[`)

	text, isErr = callToolCtx(t, ctx, srv, ToolGetSession, map[string]any{"sessionId": started.Session.ID, "maxBytes": 10})
	assert.True(t, isErr)
	assert.Contains(t, text, "invalid_request")

	// turn takes an index as a number as well as a string.
	for _, turn := range []any{0, "0", "last"} {
		text, isErr = callToolCtx(t, ctx, srv, ToolGetSessionTurn, map[string]any{"sessionId": started.Session.ID, "turn": turn})
		require.False(t, isErr, text)
		assert.Contains(t, text, `"text":"echo: hello"`)
	}
	text, isErr = callToolCtx(t, ctx, srv, ToolGetSessionTurn, map[string]any{"sessionId": started.Session.ID, "turn": 0, "toolCall": 0})
	assert.True(t, isErr)
	assert.Contains(t, text, "no tool call")

	text, isErr = callToolCtx(t, ctx, srv, ToolListSessions, map[string]any{"name": "sre"})
	require.False(t, isErr, text)
	assert.Contains(t, text, started.Session.ID)

	// Without a message the session is only created, with no turn.
	calls := len(ctrl.Calls())
	text, isErr = callToolCtx(t, ctx, srv, ToolStartSession, map[string]any{"name": "sre"})
	require.False(t, isErr, text)
	assert.Contains(t, text, `"session":`)
	assert.NotContains(t, text, `"turn"`)
	require.Len(t, ctrl.Calls(), calls+1)
	assert.Equal(t, "CreateSession", ctrl.Calls()[calls].Method)

	text, isErr = callToolCtx(t, ctx, srv, ToolStartSession, map[string]any{"name": "sre", "message": " "})
	assert.True(t, isErr)
	assert.Contains(t, text, "invalid_request")

	// With a workspace grant, as the person it names.
	person := identity.ContextWith(ctx, &identity.Identity{Subject: "dex-admin", Email: "admin@lab.local"})
	signer := kagenttest.NewSigner(t)
	grant := signer.Sign(t, "admin@lab.local", 5*time.Minute, kagenttest.WorkspaceGrant())
	text, isErr = callToolCtx(t, person, srv, ToolStartSession, map[string]any{"name": "sre", "message": "hello", "workspaceGrant": grant})
	require.False(t, isErr, text)
	assert.Contains(t, text, `"workspace":"kagent/platform"`)
	assert.Contains(t, text, `"csiDriver":"nfs.csi.k8s.io"`)
	assert.NotContains(t, text, grant[:16])
	waitTurn(t, ctrl)
	text, isErr = callToolCtx(t, person, srv, ToolStartSession, map[string]any{"name": "sre", "message": "hello", "workspaceGrant": signer.Sign(t, "admin@lab.local", -time.Minute, kagenttest.WorkspaceGrant())})
	assert.True(t, isErr)
	assert.Contains(t, text, "forbidden")
	assert.Contains(t, text, "expired at")

	// delete_session removes the caller's own session and refuses another's.
	text, isErr = callToolCtx(t, ctx, srv, ToolDeleteSession, map[string]any{"sessionId": started.Session.ID})
	require.False(t, isErr, text)
	assert.Contains(t, text, `"state":"RUNTIME_STATE_DELETED"`)
	text, isErr = callToolCtx(t, ctx, srv, ToolListSessions, map[string]any{"name": "sre"})
	require.False(t, isErr, text)
	assert.NotContains(t, text, started.Session.ID)
	theirs := ctrl.AddSessionOf("dev@lab.local", "kagent", "sre")
	text, isErr = callToolCtx(t, ctx, srv, ToolDeleteSession, map[string]any{"sessionId": theirs.GetId()})
	assert.True(t, isErr)
	assert.Contains(t, text, "not_found")
	text, isErr = callToolCtx(t, ctx, srv, ToolDeleteSession, map[string]any{})
	assert.True(t, isErr)
	assert.Contains(t, text, "invalid_request")

	text, isErr = callTool(t, srv, ToolListSessions, map[string]any{"name": "sre"})
	assert.True(t, isErr)
	assert.Contains(t, text, "unauthenticated")
}

func TestSessionsUnsupportedWithoutATarget(t *testing.T) {
	svc, _ := newService(t)
	mux := http.NewServeMux()
	NewREST(svc, nil).Register(mux)
	req := httptest.NewRequest(http.MethodPost, Prefix+"/agents/kagent/sre/sessions", bytes.NewBufferString(`{"message":"hi"}`))
	rec := httptest.NewRecorder()
	asCaller(mux).ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotImplemented, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "--kagent-target")
}
