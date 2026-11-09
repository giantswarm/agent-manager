package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/organizations"
)

const credentialsPath = Prefix + "/organizations/org-acme/payments/credentials/"

// routeFixture serves the REST API over f's organization tools, logging at
// debug level into logs.
type routeFixture struct {
	*orgFixture
	mux  *http.ServeMux
	logs *bytes.Buffer
}

func newRoute(t *testing.T, f *orgFixture) *routeFixture {
	t.Helper()
	logs := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	svc, _ := newService(t)
	mux := http.NewServeMux()
	NewREST(svc, log, WithOrganizations(organizations.New(f.provider, f.cfg, log))).Register(mux)
	return &routeFixture{orgFixture: f, mux: mux, logs: logs}
}

// do sends a request as ctx's caller; a body that is not a string is JSON.
func (r *routeFixture) do(t *testing.T, ctx context.Context, method, path string, body any) (int, string) {
	t.Helper()
	var raw []byte
	switch b := body.(type) {
	case nil:
	case string:
		raw = []byte(b)
	default:
		var err error
		raw, err = json.Marshal(b)
		require.NoError(t, err)
	}
	req := httptest.NewRequestWithContext(ctx, method, path, bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func connector(name, secret, key string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "api.kagent.dev/v1alpha3", "kind": "RemoteMCPServer",
		"metadata": map[string]any{"name": name, "namespace": orgNS},
		"spec": map[string]any{"url": "https://mcp.example.com/mcp?api_key=k", "headersFrom": []any{
			map[string]any{"name": "Authorization", "valueFrom": map[string]any{"type": "Secret", "name": secret, "key": key}},
		}},
	}}
}

func egressPolicy(name string, destinations ...any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "api.kagent.dev/v1alpha3", "kind": "EgressPolicy",
		"metadata": map[string]any{"name": name, "namespace": orgNS},
		"spec":     map[string]any{"destinations": destinations},
	}}
}

// The read lists every object that names the Secret, with the key it reads,
// a connector's URL and the origins an egress policy sends the value to.
func TestTheCredentialRouteReadsWhoNamesASecret(t *testing.T) {
	r := newRoute(t, newOrgFixture(t,
		model("claude", true),
		connector("gh", "tenant-credential-gh", "token"),
		egressPolicy("github",
			map[string]any{"origin": "https://api.github.com", "authentication": map[string]any{"header": "Authorization", "secretKeyRef": map[string]any{"name": "tenant-credential-gh", "key": "token"}}},
			map[string]any{"origin": "https://uploads.github.com", "authentication": map[string]any{"header": "Authorization", "secretKeyRef": map[string]any{"name": "tenant-credential-gh", "key": "token"}}},
			map[string]any{"origin": "https://example.com"},
		),
	))
	status, text := r.do(t, r.ctx, http.MethodGet, credentialsPath+"tenant-credential-gh?kind=EgressPolicy&name=github", nil)
	require.Equal(t, http.StatusOK, status, text)
	res := decode(t, text)
	assert.Equal(t, orgNS, res["namespace"])
	assert.Equal(t, agents.ModeApply, res["mode"])
	assert.Equal(t, []any{
		map[string]any{"kind": "RemoteMCPServer", "name": "gh", "key": "token", "url": "https://mcp.example.com/mcp?api_key=REDACTED"},
		map[string]any{"kind": "EgressPolicy", "name": "github", "key": "token", "origins": []any{"https://api.github.com", "https://uploads.github.com"}},
	}, res["references"])

	status, text = r.do(t, r.ctx, http.MethodGet, credentialsPath+"tenant-credential-unused", nil)
	require.Equal(t, http.StatusOK, status, text)
	assert.Equal(t, []any{}, decode(t, text)["references"])
	assert.NotContains(t, decode(t, text), "mode")
}

// Without the organization tools the route answers unsupported.
func TestTheCredentialRouteNeedsTheOrganizationTools(t *testing.T) {
	svc, _ := newService(t)
	mux := http.NewServeMux()
	NewREST(svc, nil).Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, Prefix+"/credential-operations/"+strings.Repeat("a", 48), strings.NewReader(`{}`)))
	assert.Equal(t, http.StatusNotImplemented, rec.Code, rec.Body.String())

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, Prefix+"/openapi.yaml", nil))
	assert.Contains(t, rec.Body.String(), "  /api/v1/organizations/{parent}/{organization}/credentials/{secret}:")
	assert.Contains(t, rec.Body.String(), "operationId: saveCredentialOperation")
	assert.Contains(t, rec.Body.String(), "operationId: getCredentialReferences")
}
