package api

import (
	"bytes"
	"encoding/json"
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
	"github.com/giantswarm/agent-manager/internal/kagent/kagenttest"
	"github.com/giantswarm/agent-manager/internal/kube"
)

const callerToken = "caller-id-token"

func newSessionService(t *testing.T) (*agents.Service, *kagenttest.Controller) {
	t.Helper()
	ctrl, conn := kagenttest.New(t)
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	typed := kubefake.NewClientset()
	svc := agents.New(kube.NewServiceAccountProvider(kube.FromInterfaces(dyn, typed, typed.Discovery())), embeddedChart{}, nil, pinner{}, agents.Config{
		DefaultNamespace: "kagent", Version: "test", Sessions: kagent.NewClient(conn),
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
	tasks := body["tasks"].([]any)
	require.Len(t, tasks, 1)
	assert.Equal(t, "TASK_STATE_COMPLETED", tasks[0].(map[string]any)["status"].(map[string]any)["state"])

	code, body = do(t, h, http.MethodGet, Prefix+"/agents/kagent/sre/sessions?allCreators=true", nil)
	require.Equal(t, http.StatusOK, code, body)
	assert.Len(t, body["sessions"], 1)
	assert.Equal(t, map[string]any{"namespace": "kagent", "name": "sre"}, body["agent"])

	// Unknown fields are refused, as on every write.
	code, _ = do(t, h, http.MethodPost, Prefix+"/agents/kagent/sre/sessions", map[string]any{"message": "hi", "agentInstance": "x"})
	assert.Equal(t, http.StatusBadRequest, code)

	ctrl.NotReady["kagent/fresh"] = true
	code, body = do(t, h, http.MethodPost, Prefix+"/agents/kagent/fresh/sessions", map[string]any{"message": "hi"})
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "conflict", body["error"].(map[string]any)["code"])

	// Without the caller's token nothing is sent.
	calls := len(ctrl.Calls())
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
	assert.Contains(t, text, "echo: hello")

	text, isErr = callToolCtx(t, ctx, srv, ToolListSessions, map[string]any{"name": "sre"})
	require.False(t, isErr, text)
	assert.Contains(t, text, started.Session.ID)

	text, isErr = callToolCtx(t, ctx, srv, ToolStartSession, map[string]any{"name": "sre"})
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
