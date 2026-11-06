package server

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apov1alpha1 "github.com/giantswarm/agent-platform-operator/api/v1alpha1"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/api"
	"github.com/giantswarm/agent-manager/internal/identity"
	"github.com/giantswarm/agent-manager/internal/kube"
	"github.com/giantswarm/agent-manager/internal/kube/kubetest"
	"github.com/giantswarm/agent-manager/internal/organizations"
)

// The credential route's request body reaches no span and no log line,
// through the whole listener: written, refused and malformed.
func TestTheCredentialBodyIsInNoTraceOrLog(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter), sdktrace.WithSampler(sdktrace.AlwaysSample()))
	prevTP := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prevTP) })

	kagent := func(resource string) schema.GroupVersionResource {
		return schema.GroupVersionResource{Group: agents.KagentAPIGroup, Version: agents.DefaultKagentAPIVersion, Resource: resource}
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		kagent("modelconfigs"): "ModelConfigList", kagent("remotemcpservers"): "RemoteMCPServerList", kagent("egresspolicies"): "EgressPolicyList",
	})
	kubetest.AnswerDryRuns(dyn)
	typed := kubefake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "agents-payments-x7k2q",
		Labels: map[string]string{apov1alpha1.OrganizationLabel: "payments", apov1alpha1.ParentLabel: "org-acme"}}})
	typed.PrependReactor("create", "selfsubjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
		review.Status.Allowed = true
		return true, review, nil
	})
	typed.PrependReactor("create", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		action.(k8stesting.CreateAction).GetObject().(*corev1.ConfigMap).ResourceVersion = "1"
		return false, nil, nil
	})
	provider := kube.NewServiceAccountProvider(kube.FromInterfaces(dyn, typed, typed.Discovery()))
	logs := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	idp := newFakeIdP(t)
	orgs := organizations.New(provider, organizations.Config{Namespaces: organizations.NewNamespaces(typed, "kagent"), CredentialUIURL: "https://credentials.example.com/?operation={operation}", CredentialOperations: organizations.NewCredentialOperations(typed.CoreV1().ConfigMaps("credential-operations"))}, log) // #nosec G101 -- a fixture UI URL and fake ConfigMap store
	ctx := identity.ContextWith(t.Context(), &identity.Identity{Issuer: idp.issuer, Subject: "sub-admin", Client: "fixture"})
	ctx = organizations.ContextWithCredentialRequest(ctx, api.ToolSetOrganizationModel, "fixture-arguments", "fixture-client")
	prepared, err := orgs.SetModel(ctx, organizations.ModelRequest{Namespace: "agents-payments-x7k2q", Name: "claude", Provider: "Anthropic", Model: "claude-sonnet-4-6"})
	require.NoError(t, err)
	confirmation, err := orgs.ReadCredentialOperation(ctx, prepared.CredentialOperation)
	require.NoError(t, err)
	svc := agents.New(provider, embeddedChart{}, nil, nil, agents.Config{Version: "test"}, nil)
	oauth := idp.config(false)
	srv, err := New(Config{Addr: "127.0.0.1:0", Organizations: orgs, OAuth: &oauth}, svc, nil, log)
	require.NoError(t, err)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	const value = "sk-ant-traced-7777" // #nosec G101 -- a test value
	url := ts.URL + api.Prefix + "/credential-operations/" + prepared.CredentialOperation
	bearer := idp.idToken(t, []string{"agent-platform"}, time.Now().Add(time.Hour))
	for _, tc := range []struct {
		body string
		want int
	}{
		{fmt.Sprintf(`{"values":[{"secret":"tenant-credential-claude","key":"apiKey","value":%q}]}`, value), http.StatusOK},
		{fmt.Sprintf(`{"values":[{"secret":"tenant-credential-claude","key":"apiKey","value":%q}]}`, value+"-again"), http.StatusOK},
		{fmt.Sprintf(`{"values":[{"secret":"tenant-credential-claude","key":"apiKey","value":%q}],"x":1}`, value), http.StatusBadRequest},
		{fmt.Sprintf(`{"value":"%s\q"}`, value), http.StatusBadRequest},
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(tc.body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("If-Match", `"`+confirmation.Version+`"`)
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, tc.want, resp.StatusCode, tc.body)
	}

	spans := exporter.GetSpans()
	require.NotEmpty(t, spans)
	secrets := []string{value, base64.StdEncoding.EncodeToString([]byte(value))}
	for _, s := range spans {
		var seen []string
		for _, a := range s.Attributes {
			seen = append(seen, a.Value.String())
		}
		for _, e := range s.Events {
			seen = append(seen, e.Name)
			for _, a := range e.Attributes {
				seen = append(seen, a.Value.String())
			}
		}
		for _, v := range secrets {
			require.NotContains(t, strings.Join(seen, "\n"), v, "span %s", s.Name)
		}
	}
	for _, v := range secrets {
		require.NotContains(t, logs.String(), v)
	}
}
