package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/yaml"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/api"
	"github.com/giantswarm/agent-manager/internal/kube"
	"github.com/giantswarm/agent-manager/internal/organizations"
)

// This proof uses only the dedicated throwaway lab and its synthetic users.
func TestCredentialLab(t *testing.T) {
	directory := os.Getenv("AGENT_MANAGER_CREDENTIAL_LAB_DIR")
	if directory == "" {
		t.Skip("set AGENT_MANAGER_CREDENTIAL_LAB_DIR for the dedicated agentlab-credential-route proof")
	}
	var lab struct {
		ClusterName string `json:"clusterName"`
		DexPort     int    `json:"dexPort"`
	}
	configuration, err := os.ReadFile(filepath.Join(directory, "agentlab.yaml")) // #nosec G304 G703 -- operator-selected isolated lab directory
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal(configuration, &lab))
	require.Equal(t, "agentlab-credential-route", lab.ClusterName)
	loading := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(&clientcmd.ClientConfigLoadingRules{ExplicitPath: filepath.Join(directory, "state", "kubeconfig")}, &clientcmd.ConfigOverrides{CurrentContext: "kind-agentlab-credential-route"})
	administratorREST, err := loading.ClientConfig()
	require.NoError(t, err)
	address, err := url.Parse(administratorREST.Host)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1", address.Hostname())
	administrator, err := kubernetes.NewForConfig(administratorREST)
	require.NoError(t, err)
	namespace, err := administrator.CoreV1().Namespaces().Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "agents-slice14-", Labels: map[string]string{"agent-platform.giantswarm.io/organization": "credential-proof-" + uuid.NewString()[:8], "agent-platform.giantswarm.io/parent": "org-lab"}}}, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = administrator.CoreV1().Namespaces().Delete(cleanup, namespace.Name, metav1.DeleteOptions{})
	})
	serviceToken, err := administrator.CoreV1().ServiceAccounts("agent-platform").CreateToken(t.Context(), "slice14-proof", &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: new(int64(900))}}, metav1.CreateOptions{})
	require.NoError(t, err)
	managerConfiguration := clientcmdapi.NewConfig()
	managerConfiguration.Clusters["proof"] = &clientcmdapi.Cluster{Server: administratorREST.Host, CertificateAuthority: administratorREST.CAFile, CertificateAuthorityData: administratorREST.CAData}
	managerConfiguration.AuthInfos["proof"] = &clientcmdapi.AuthInfo{Token: serviceToken.Status.Token}
	managerConfiguration.Contexts["proof"] = &clientcmdapi.Context{Cluster: "proof", AuthInfo: "proof"}
	managerConfiguration.CurrentContext = "proof"
	encoded, err := clientcmd.Write(*managerConfiguration)
	require.NoError(t, err)
	managerKubeconfig := filepath.Join(t.TempDir(), "manager-kubeconfig")
	require.NoError(t, os.WriteFile(managerKubeconfig, encoded, 0o600))
	manager, err := kube.New(kube.Config{Kubeconfig: managerKubeconfig, Context: "proof"})
	require.NoError(t, err)
	_, err = manager.Typed().CoreV1().ConfigMaps("agent-platform").List(t.Context(), metav1.ListOptions{})
	require.True(t, apierrors.IsForbidden(err), "the manager must not read application ConfigMaps")
	_, err = manager.Typed().CoreV1().Secrets(namespace.Name).List(t.Context(), metav1.ListOptions{})
	require.True(t, apierrors.IsForbidden(err), "the manager must not read tenant Secrets as its ServiceAccount")
	caFile := filepath.Join(directory, "certs", "ca.crt")
	roots, err := x509.SystemCertPool()
	require.NoError(t, err)
	certificate, err := os.ReadFile(caFile) // #nosec G304 G703 -- public CA in the operator-selected isolated lab directory
	require.NoError(t, err)
	require.True(t, roots.AppendCertsFromPEM(certificate))
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}}, Timeout: 15 * time.Second}
	issuer := fmt.Sprintf("https://localhost:%d/dex", lab.DexPort)
	login := func(email string, platform bool) string {
		clientID, clientSecret, scope := "agent-platform", "agent-platform-lab-secret", "openid email groups profile audience:server:client_id:kubernetes" // #nosec G101 -- isolated agentlab client
		if !platform {
			clientID, clientSecret, scope = "kubernetes", "kubernetes-lab-secret", "openid email groups profile" // #nosec G101 -- isolated agentlab client
		}
		form := url.Values{"grant_type": {"password"}, "username": {email}, "password": {"password"}, "scope": {scope}}
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, issuer+"/token", strings.NewReader(form.Encode()))
		require.NoError(t, err)
		request.SetBasicAuth(clientID, clientSecret)
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response, err := client.Do(request)
		require.NoError(t, err)
		defer func() { _ = response.Body.Close() }()
		require.Equal(t, http.StatusOK, response.StatusCode)
		var token struct {
			IDToken string `json:"id_token"`
		}
		require.NoError(t, json.NewDecoder(io.LimitReader(response.Body, 64*1024)).Decode(&token))
		require.NotEmpty(t, token.IDToken)
		return token.IDToken
	}
	adminToken, developerToken, wrongAudience := login("admin@lab.local", true), login("dev@lab.local", true), login("admin@lab.local", false)
	provider := kube.NewCallerProvider(manager, nil)
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	operations := organizations.NewCredentialOperations(manager.Typed().CoreV1().ConfigMaps("slice14-credential-operations"))
	organizationNamespaces := organizations.NewNamespaces(manager.Typed(), nil, "kagent")
	organizationService := organizations.New(provider, organizations.Config{Namespaces: organizationNamespaces, CredentialUIURL: "https://credentials.example.com/?operation={operation}", CredentialOperations: operations}, logger) // #nosec G101 -- public fixture UI URL
	service := agents.New(provider, embeddedChart{}, nil, nil, agents.Config{}, logger)
	oauth := OAuthConfig{BaseURL: "http://localhost:8080", Provider: ProviderDex, DexIssuerURL: issuer, DexClientID: "agent-platform", DexClientSecret: "agent-platform-lab-secret", DexCAFile: caFile, DexAllowPrivateIP: true, TrustedAudiences: []string{"agent-platform"}, SSOAllowPrivateIPs: true, DownstreamOAuth: true} // #nosec G101 -- isolated agentlab client
	server, err := New(Config{Organizations: organizationService, OAuth: &oauth, MCPEnabled: true}, service, api.NewMCPServer(service, "slice14-lab", api.WithOrganizations(organizationService), api.WithLifecycle(t.Context())), logger)
	require.NoError(t, err)
	listener := httptest.NewTLSServer(server.Handler())
	defer listener.Close()
	managerHTTP := listener.Client()
	managerHTTP.Timeout = 15 * time.Second
	request := func(method, path, token string, body any, version string) (int, []byte, http.Header) {
		var encoded []byte
		if body != nil {
			encoded, err = json.Marshal(body)
			require.NoError(t, err)
		}
		request, err := http.NewRequestWithContext(t.Context(), method, listener.URL+path, bytes.NewReader(encoded))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		if path == "/mcp" {
			request.Header.Set("Mcp-Protocol-Version", "2026-07-28")
			request.Header.Set("Mcp-Method", "tools/call")
			request.Header.Set("Mcp-Name", body.(map[string]any)["params"].(map[string]any)["name"].(string))
		}
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		if version != "" {
			request.Header.Set("If-Match", `"`+version+`"`)
		}
		response, err := managerHTTP.Do(request)
		require.NoError(t, err)
		defer func() { _ = response.Body.Close() }()
		data, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024))
		require.NoError(t, err)
		return response.StatusCode, data, response.Header
	}
	prepare := func(tool string, arguments map[string]any) organizations.ResourceResult {
		call := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": arguments, "_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientInfo": map[string]any{"name": "slice14-proof", "version": "1"}, "io.modelcontextprotocol/clientCapabilities": map[string]any{}}}}
		status, body, _ := request(http.MethodPost, "/mcp", adminToken, call, "")
		require.Equal(t, http.StatusOK, status, string(body))
		var response struct {
			Error  any `json:"error"`
			Result struct {
				IsError bool `json:"isError"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		require.NoError(t, json.Unmarshal(body, &response))
		require.Nil(t, response.Error, string(body))
		require.False(t, response.Result.IsError, string(body))
		require.NotEmpty(t, response.Result.Content)
		var result organizations.ResourceResult
		require.NoError(t, json.Unmarshal([]byte(response.Result.Content[0].Text), &result))
		require.True(t, result.Pending, string(body))
		return result
	}
	read := func(operation string) (organizations.CredentialOperation, string) {
		status, body, headers := request(http.MethodGet, api.Prefix+"/credential-operations/"+operation, adminToken, nil, "")
		require.Equal(t, http.StatusOK, status, string(body))
		var operationMetadata organizations.CredentialOperation
		require.NoError(t, json.Unmarshal(body, &operationMetadata))
		return operationMetadata, strings.Trim(headers.Get("ETag"), `"`)
	}
	const credential = "slice14-synthetic-credential" // #nosec G101 -- isolated lab fixture
	created := prepare(api.ToolSetOrganizationModel, map[string]any{"namespace": namespace.Name, "name": "proof-model", "provider": "Anthropic", "model": "fixture-model", "requestId": "create"})
	operation, version := read(created.CredentialOperation)
	require.Equal(t, "pending", operation.State)
	path := api.Prefix + "/credential-operations/" + created.CredentialOperation
	for _, token := range []string{"", wrongAudience} {
		status, _, _ := request(http.MethodGet, path, token, nil, "")
		require.Equal(t, http.StatusUnauthorized, status)
	}
	status, _, _ := request(http.MethodGet, path, developerToken, nil, "")
	require.Equal(t, http.StatusNotFound, status)
	body := map[string]any{"values": []organizations.CredentialValue{{Secret: created.Credentials[0].Secret, Key: created.Credentials[0].Key, Value: credential}}}
	status, answer, _ := request(http.MethodPost, path, adminToken, body, "stale")
	require.Equal(t, http.StatusPreconditionFailed, status, string(answer))
	status, answer, _ = request(http.MethodPost, path, adminToken, body, version)
	require.Equal(t, http.StatusOK, status, string(answer))
	require.NotContains(t, string(answer), credential)
	status, answer, _ = request(http.MethodPost, path, adminToken, body, version)
	require.Equal(t, http.StatusOK, status, string(answer))
	rotated := prepare(api.ToolReplaceOrganizationCredential, map[string]any{"namespace": namespace.Name, "secret": created.Credentials[0].Secret, "key": created.Credentials[0].Key, "requestId": "rotation"})
	_, version = read(rotated.CredentialOperation)
	body = map[string]any{"values": []organizations.CredentialValue{{Secret: rotated.Credentials[0].Secret, Key: rotated.Credentials[0].Key, Value: credential + "-rotated"}}}
	status, answer, _ = request(http.MethodPost, api.Prefix+"/credential-operations/"+rotated.CredentialOperation, adminToken, body, version)
	require.Equal(t, http.StatusOK, status, string(answer))
	metadata, err := manager.Typed().CoreV1().ConfigMaps("slice14-credential-operations").List(t.Context(), metav1.ListOptions{})
	require.NoError(t, err)
	for _, item := range metadata.Items {
		require.NotContains(t, item.Data["operation"], credential)
		require.NotContains(t, item.Data["operation"], adminToken)
		require.NotContains(t, item.Data["operation"], serviceToken.Status.Token)
	}
	require.NotContains(t, logs.String(), credential)
	require.NotContains(t, logs.String(), adminToken)
	require.NotContains(t, logs.String(), serviceToken.Status.Token)
	t.Log("Verified real Dex -> manager REST/MCP -> caller-authorized Kubernetes create and replacement, replay, wrong audience, cross-user access, stale confirmation, metadata isolation and ServiceAccount restrictions")
}
