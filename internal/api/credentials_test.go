package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/giantswarm/agent-manager/internal/organizations"
)

// liveValue is the value of the credential Secrets the tests seed; no tool
// answer may carry it.
const liveValue = "sk-live-0123456789abcdef" // #nosec G101 -- a test value

var secretGVR = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}

// seedSecret puts a credential Secret holding liveValue in the fake API
// server, where the tools find it by a dry-run create and never read it.
func seedSecret(t *testing.T, f *orgFixture, name, key string) {
	t.Helper()
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata": map[string]any{"name": name, "namespace": orgNS},
		"data":     map[string]any{key: base64.StdEncoding.EncodeToString([]byte(liveValue))},
	}}
	_, err := f.dyn.Resource(secretGVR).Namespace(orgNS).Create(t.Context(), u, metav1.CreateOptions{})
	require.NoError(t, err)
}

// Every organization tool, called with arguments that reach its answer:
// none of them carries a credential Secret's value, whether the Secret is
// present or replaced.
func TestNoOrganizationToolAnswerCarriesASecretValue(t *testing.T) {
	f := newOrgFixture(t, organization("org-acme", "payments", nil), model("claude", true))
	seedSecret(t, f, "tenant-credential-claude", "apiKey")
	seedSecret(t, f, "tenant-credential-gh", "token")
	args := map[string]map[string]any{
		ToolListOrganizations:         {"namespace": "org-acme"},
		ToolGetOrganization:           {"namespace": "org-acme", "name": "payments"},
		ToolCreateOrganization:        {"namespace": "org-acme", "name": "billing", "adminGroups": []any{"billing-leads"}, "dryRun": true},
		ToolUpdateOrganization:        {"namespace": "org-acme", "name": "payments", "displayName": "Payments"},
		ToolDeleteOrganization:        {"namespace": "org-acme", "name": "payments"},
		ToolListOrganizationMembers:   {"namespace": orgNS},
		ToolAddOrganizationMember:     {"namespace": orgNS, "group": "payments-devs", "role": "editor"},
		ToolRemoveOrganizationMember:  {"namespace": orgNS, "group": "payments-devs"},
		ToolSetOrganizationModel:      {"namespace": orgNS, "name": "claude"},
		ToolListOrganizationConnector: {"namespace": orgNS},
		ToolSetOrganizationConnector:  {"namespace": orgNS, "name": "gh", "url": "https://mcp.github.example/mcp", "credentialHeader": "Authorization"},
		ToolSetOrganizationEgress: {"namespace": orgNS, "name": "github", "destinations": []any{map[string]any{
			"origin": "https://api.github.com", "credentialHeader": "Authorization", "credentialSecret": "tenant-credential-gh"}}}, // #nosec G101 -- a Secret's name
		ToolDeleteOrganizationObject:      {"namespace": orgNS, "kind": organizations.KindConnector, "name": "gh"},
		ToolReplaceOrganizationCredential: {"namespace": orgNS, "secret": "tenant-credential-claude", "key": "apiKey"}, // #nosec G101 -- a Secret's name
	}
	for _, tool := range OrganizationToolNames() {
		in, ok := args[tool]
		require.True(t, ok, "no call of %s: every organization tool is checked", tool)
		text, isErr := f.call(t, tool, in)
		require.False(t, isErr, "%s: %s", tool, text)
		assert.NotContains(t, text, liveValue, tool)
		assert.NotContains(t, text, "c2stbGl2ZS0wMTIzNDU2Nzg5YWJjZGVm", tool)
	}
}

// rawCall sends one JSON-RPC message and answers the whole response.
func rawCall(t *testing.T, ctx context.Context, srv *mcpserver.MCPServer, msg map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(msg)
	require.NoError(t, err)
	out, err := json.Marshal(srv.HandleMessage(ctx, raw))
	require.NoError(t, err)
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(out, &parsed), string(out))
	return parsed
}

func toolCall(name string, args map[string]any, extra map[string]any) map[string]any {
	params := map[string]any{"name": name, "arguments": args}
	for k, v := range extra {
		params[k] = v
	}
	return map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": params}
}

// modernMeta is the _meta of a 2026-07-28 request; withURL declares URL
// elicitation.
func modernMeta(withURL bool) map[string]any {
	capabilities := map[string]any{}
	if withURL {
		capabilities["elicitation"] = map[string]any{"url": map[string]any{}}
	}
	return map[string]any{"_meta": map[string]any{
		mcp.MetaKeyProtocolVersion:    "2026-07-28",
		mcp.MetaKeyClientInfo:         map[string]any{"name": "t", "version": "0"},
		mcp.MetaKeyClientCapabilities: capabilities,
	}}
}

// legacySession is a client session of a protocol before 2026-07-28 that keeps the
// capabilities it declared at initialize.
type legacySession struct {
	mu           sync.Mutex
	capabilities mcp.ClientCapabilities
	version      string
	ch           chan mcp.JSONRPCNotification
}

func (s *legacySession) Initialize()                                         {}
func (s *legacySession) Initialized() bool                                   { return true }
func (s *legacySession) NotificationChannel() chan<- mcp.JSONRPCNotification { return s.ch }
func (s *legacySession) SessionID() string                                   { return "legacy" }
func (s *legacySession) GetClientInfo() mcp.Implementation                   { return mcp.Implementation{} }
func (s *legacySession) SetClientInfo(mcp.Implementation)                    {}
func (s *legacySession) GetClientCapabilities() mcp.ClientCapabilities {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.capabilities
}
func (s *legacySession) SetClientCapabilities(c mcp.ClientCapabilities) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.capabilities = c
}
func (s *legacySession) ProtocolVersion() string { return s.version }
func (s *legacySession) SetProtocolVersion(v string) {
	s.version = v
}

// legacyContext initializes a session of a protocol before 2026-07-28 that
// declares, or not, URL elicitation.
func legacyContext(t *testing.T, f *orgFixture, version string, withURL bool) context.Context {
	t.Helper()
	session := &legacySession{ch: make(chan mcp.JSONRPCNotification, 16)}
	require.NoError(t, f.srv.RegisterSession(f.ctx, session))
	t.Cleanup(func() { f.srv.UnregisterSession(context.Background(), session.SessionID()) })
	ctx := f.srv.WithContext(f.ctx, session)
	capabilities := map[string]any{}
	if withURL {
		capabilities["elicitation"] = map[string]any{"url": map[string]any{}}
	}
	res := rawCall(t, ctx, f.srv, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{
		"protocolVersion": version, "capabilities": capabilities, "clientInfo": map[string]any{"name": "t", "version": "0"},
	}})
	require.Nil(t, res["error"], res)
	return ctx
}

// resultText is the text of a tools/call result.
func resultText(t *testing.T, res map[string]any) string {
	t.Helper()
	result, ok := res["result"].(map[string]any)
	require.True(t, ok, res)
	content, _ := result["content"].([]any)
	require.NotEmpty(t, content, res)
	return content[0].(map[string]any)["text"].(string)
}

// A missing credential is answered with a URL elicitation of its form: a
// -32042 error before protocol 2026-07-28, an input_required result from
// it; a client without URL elicitation gets the link as text. No shape
// carries a value.
func TestAMissingCredentialIsAURLElicitation(t *testing.T) {
	arguments := map[string]any{"namespace": orgNS, "name": "claude", "provider": "Anthropic", "model": "claude-sonnet-4-6"}
	for _, modern := range []bool{false, true} {
		t.Run(fmt.Sprintf("modern=%v", modern), func(t *testing.T) {
			fixture := newOrgFixture(t)
			ctx := fixture.ctx
			var extra map[string]any
			if modern {
				extra = modernMeta(true)
			} else {
				ctx = legacyContext(t, fixture, mcp.ProtocolVersion20251125, true)
			}
			response := rawCall(t, ctx, fixture.srv, toolCall(ToolSetOrganizationModel, arguments, extra))
			var elicitation map[string]any
			if modern {
				require.Nil(t, response["error"], response)
				result := response["result"].(map[string]any)
				require.Equal(t, "input_required", result["resultType"])
				requests := result["inputRequests"].(map[string]any)
				require.Len(t, requests, 1)
				for _, request := range requests {
					elicitation = request.(map[string]any)["params"].(map[string]any)
				}
			} else {
				refusal := response["error"].(map[string]any)
				require.EqualValues(t, mcp.URL_ELICITATION_REQUIRED, refusal["code"])
				requests := refusal["data"].(map[string]any)["elicitations"].([]any)
				require.Len(t, requests, 1)
				elicitation = requests[0].(map[string]any)
			}
			require.Equal(t, "url", elicitation["mode"])
			link, err := url.Parse(elicitation["url"].(string))
			require.NoError(t, err)
			id := link.Query().Get("operation")
			require.NotEmpty(t, id)
			require.NotEmpty(t, elicitation["elicitationId"])
			require.Nil(t, fixture.get(t, mcGVR, orgNS, "claude"))
			if modern {
				accepted := modernMeta(true)
				accepted["inputResponses"] = map[string]any{credentialInputPrefix + elicitation["elicitationId"].(string): map[string]any{"action": "accept"}}
				retry := rawCall(t, ctx, fixture.srv, toolCall(ToolSetOrganizationModel, arguments, accepted))
				require.Equal(t, "input_required", retry["result"].(map[string]any)["resultType"], "accepting navigation does not save a credential")
			}
			confirmation := confirmOperation(t, fixture, id)
			status, body, _ := operationHTTP(t, fixture, http.MethodPost, id, confirmation.Version, valuesBody(t, confirmation.Result, "synthetic-value"), nil)
			require.Equal(t, http.StatusOK, status, body)
			if !modern {
				session := mcpserver.ClientSessionFromContext(ctx).(*legacySession)
				select {
				case notification := <-session.ch:
					raw, err := json.Marshal(notification)
					require.NoError(t, err)
					require.Contains(t, string(raw), "notifications/elicitation/complete")
					require.Contains(t, string(raw), elicitation["elicitationId"])
				case <-time.After(3 * time.Second):
					t.Fatal("no elicitation completion notification")
				}
			}
			retry := rawCall(t, ctx, fixture.srv, toolCall(ToolSetOrganizationModel, arguments, extra))
			require.Nil(t, retry["error"], retry)
			require.Contains(t, resultText(t, retry), `"state":"present"`)
		})
	}
}

func TestCredentialInputCanBeCancelledWithoutWriting(t *testing.T) {
	for action, outcome := range map[string]string{"decline": "declined", "cancel": "cancelled"} {
		t.Run(action, func(t *testing.T) {
			fixture := newOrgFixture(t)
			arguments := map[string]any{"namespace": orgNS, "name": "claude", "provider": "Anthropic", "model": "claude-sonnet-4-6"}
			response := rawCall(t, fixture.ctx, fixture.srv, toolCall(ToolSetOrganizationModel, arguments, modernMeta(true)))
			requests := response["result"].(map[string]any)["inputRequests"].(map[string]any)
			key := ""
			for name := range requests {
				key = name
			}
			extra := modernMeta(true)
			extra["inputResponses"] = map[string]any{key: map[string]any{"action": action}}
			response = rawCall(t, fixture.ctx, fixture.srv, toolCall(ToolSetOrganizationModel, arguments, extra))
			require.Equal(t, true, response["result"].(map[string]any)["isError"], response)
			require.Contains(t, resultText(t, response), "Credential input was "+outcome)
			require.Nil(t, fixture.get(t, mcGVR, orgNS, "claude"))
			require.Nil(t, fixture.get(t, secretGVR, orgNS, "tenant-credential-claude"))
		})
	}
}

func TestCredentialFallbackAndDryRun(t *testing.T) {
	fixture := newOrgFixture(t)
	result := prepareModelOperation(t, fixture, nil)
	require.True(t, result.Pending)
	require.NotEmpty(t, result.Credentials[0].FormURL)
	require.Contains(t, result.Credentials[0].Next, "Never send the value")
	fixture = newOrgFixture(t)
	text, failed := fixture.call(t, ToolSetOrganizationModel, map[string]any{"namespace": orgNS, "name": "claude", "provider": "Anthropic", "model": "claude-sonnet-4-6", "dryRun": true})
	require.False(t, failed, text)
	require.NotContains(t, text, "credentialOperation")
	require.Nil(t, fixture.get(t, mcGVR, orgNS, "claude"))
}

func TestCredentialWithoutURLElicitationIsAStructuredResultWithAResourceLink(t *testing.T) {
	arguments := map[string]any{"namespace": orgNS, "name": "claude", "provider": "Anthropic", "model": "claude-sonnet-4-6"}

	t.Run("apply", func(t *testing.T) {
		fixture := newOrgFixture(t)
		structured, content := structuredCredentialResult(t, fixture, arguments)
		credential := structured["credentials"].([]any)[0].(map[string]any)
		formURL := credential["formUrl"].(string)
		require.Contains(t, formURL, "https://portal.example.com/credentials?operation=")
		require.Contains(t, credential["next"], formURL)
		require.Len(t, content, 2)
		link := content[1].(map[string]any)
		require.Equal(t, "resource_link", link["type"])
		require.Equal(t, formURL, link["uri"])
		require.Equal(t, map[string]any{"audience": []any{"user"}}, link["annotations"])
	})

	t.Run("before resource_link", func(t *testing.T) {
		fixture := newOrgFixture(t)
		ctx := legacyContext(t, fixture, mcp.ProtocolVersion20250326, false)
		response := rawCall(t, ctx, fixture.srv, toolCall(ToolSetOrganizationModel, arguments, nil))
		result := response["result"].(map[string]any)
		require.NotEqual(t, true, result["isError"], result)
		require.NotNil(t, result["structuredContent"])
		require.Len(t, result["content"].([]any), 1)
	})

	t.Run("dry run", func(t *testing.T) {
		dryRun := map[string]any{"dryRun": true}
		for key, value := range arguments {
			dryRun[key] = value
		}
		structuredCredentialResult(t, newOrgFixture(t), dryRun)
	})

	t.Run("commit", func(t *testing.T) {
		commit := map[string]any{"mode": "commit"}
		for key, value := range arguments {
			commit[key] = value
		}
		fixture, _ := commitFixture(t, nil)
		structured, _ := structuredCredentialResult(t, fixture, commit)
		require.NotEmpty(t, structured["credentials"])
	})
}

// structuredCredentialResult calls set_organization_model without URL
// elicitation and returns the structuredContent, validated against the
// outputSchema tools/list declares and equal to the JSON text, with the
// result's content.
func structuredCredentialResult(t *testing.T, fixture *orgFixture, arguments map[string]any) (map[string]any, []any) {
	t.Helper()
	response := rawCall(t, fixture.ctx, fixture.srv, toolCall(ToolSetOrganizationModel, arguments, modernMeta(false)))
	result := response["result"].(map[string]any)
	require.NotEqual(t, true, result["isError"], result)
	structured := result["structuredContent"].(map[string]any)
	require.NoError(t, outputSchema(t, fixture, ToolSetOrganizationModel).Validate(structured))
	content := result["content"].([]any)
	var text map[string]any
	require.NoError(t, json.Unmarshal([]byte(content[0].(map[string]any)["text"].(string)), &text))
	require.Equal(t, structured, text)
	return structured, content
}

// outputSchema compiles the outputSchema tools/list declares for the tool.
func outputSchema(t *testing.T, fixture *orgFixture, name string) *jsonschema.Schema {
	t.Helper()
	response := rawCall(t, fixture.ctx, fixture.srv, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	for _, tool := range response["result"].(map[string]any)["tools"].([]any) {
		tool := tool.(map[string]any)
		if tool["name"] != name {
			continue
		}
		compiler := jsonschema.NewCompiler()
		require.NoError(t, compiler.AddResource("output.schema.json", tool["outputSchema"]))
		compiled, err := compiler.Compile("output.schema.json")
		require.NoError(t, err)
		return compiled
	}
	t.Fatalf("tools/list has no tool %s", name)
	return nil
}

func TestCredentialWriteToolsDeclareTheirOutputSchema(t *testing.T) {
	fixture := newOrgFixture(t)
	response := rawCall(t, fixture.ctx, fixture.srv, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	declared := map[string]bool{}
	for _, tool := range response["result"].(map[string]any)["tools"].([]any) {
		tool := tool.(map[string]any)
		if schema, ok := tool["outputSchema"].(map[string]any); ok {
			_, hasCredentials := schema["properties"].(map[string]any)["credentials"]
			declared[tool["name"].(string)] = hasCredentials
		}
	}
	for _, name := range []string{ToolSetOrganizationModel, ToolSetOrganizationConnector, ToolSetOrganizationEgress} {
		require.True(t, declared[name], name)
	}
}
