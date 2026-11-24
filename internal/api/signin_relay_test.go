package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/giantswarm/mcp-oauth/handler"
	"github.com/giantswarm/mcp-oauth/providers"
	"github.com/giantswarm/mcp-oauth/requeststate"
	"github.com/giantswarm/mcp-oauth/security"
	"github.com/giantswarm/mcp-toolkit/urlelicitation"
	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/agent-manager/internal/mustermcp"
)

const (
	relayTool      = "authorize"
	musterTarget   = "x_workspace-manager_authorize_workspace"
	musterKey      = "muster_sign_in"
	musterState    = "muster-state"
	musterSignInAt = "https://muster.example/sign-in/workspace-manager"
)

// musterCall is one call_tool muster served.
type musterCall struct {
	Tool         string
	Args         map[string]any
	DeclaredURL  bool
	RequestState string
	Answer       mcp.ElicitationResponseAction
}

// fakeMuster asks for a sign-in the way muster does: an InputRequiredResult
// to a 2026-07-28 caller that declares URL mode elicitation, auth_required
// to any other; an accepted answer with its requestState runs the tool.
type fakeMuster struct {
	mu    sync.Mutex
	calls []musterCall
}

func (f *fakeMuster) Calls() []musterCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]musterCall(nil), f.calls...)
}

func (f *fakeMuster) start(t *testing.T) string {
	t.Helper()
	s := mcpserver.NewMCPServer("muster", "test")
	s.AddTool(mcp.NewTool(mustermcp.MetaCallTool), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		call := musterCall{RequestState: req.Params.RequestState}
		call.Tool, _ = args["name"].(string)
		call.Args, _ = args["arguments"].(map[string]any)
		if info := mcpserver.RequestProtocolInfoFromContext(ctx); info != nil && info.ClientCapabilities != nil {
			call.DeclaredURL = info.ClientCapabilities.Elicitation != nil && info.ClientCapabilities.Elicitation.URL != nil
		}
		answer := mcpserver.ElicitationResponse(req.Params.InputResponses, musterKey)
		if answer != nil {
			call.Answer = answer.Action
		}
		f.mu.Lock()
		f.calls = append(f.calls, call)
		f.mu.Unlock()
		switch {
		case answer != nil && req.Params.RequestState == musterState && answer.Action == mcp.ElicitationResponseActionAccept:
			return envelope(mcp.NewToolResultStructured(map[string]any{"outcome": "granted"}, "granted"))
		case answer != nil:
			return mcp.NewToolResultError("muster: the sign-in was not accepted"), nil
		case call.DeclaredURL:
			return mcpserver.NewInputRequestBuilder(musterState).Elicit(musterKey, mcp.ElicitationParams{
				Mode:    mcp.ElicitationModeURL,
				Message: "Sign in to workspace-manager in muster.",
				URL:     musterSignInAt,
			}).ToolResult(), nil
		default:
			res := mcp.NewToolResultStructured(map[string]any{"status": "auth_required", "server": "workspace-manager", "authUrl": musterSignInAt}, "auth_required")
			res.IsError = true
			return envelope(res)
		}
	})
	srv := httptest.NewServer(mcpserver.NewStreamableHTTPServer(s))
	t.Cleanup(srv.Close)
	return srv.URL + "/mcp"
}

func envelope(res *mcp.CallToolResult) (*mcp.CallToolResult, error) {
	raw, err := json.Marshal(res)
	if err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(string(raw)), nil
}

// relayServer is agent-manager's MCP server with one tool that calls muster
// as the caller and refuses with the sign-in muster asked for, the way the
// workspace and toolchain checks do. The caller is the user its X-User
// header names.
func relayServer(t *testing.T, musterURL string, elicitor *SignInElicitor) string {
	t.Helper()
	muster, err := mustermcp.New(musterURL, "x_workspace-manager_", "test", "workspace-manager")
	require.NoError(t, err)
	opts := []mcpserver.ServerOption{}
	if elicitor != nil {
		opts = append(opts, mcpserver.WithToolHandlerMiddleware(relaySignIns(elicitor, slog.New(slog.DiscardHandler))))
	}
	s := mcpserver.NewMCPServer("agent-manager", "test", opts...)
	s.AddTool(mcp.NewTool(relayTool, mcp.WithString("workspace")), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		res, err := muster.Call(ctx, "id-token", musterTarget, map[string]any{"workspace": req.GetString("workspace", "")})
		var challenge *mustermcp.AuthRequiredError
		switch {
		case errors.As(err, &challenge):
			return mcp.NewToolResultError("auth_required: " + challenge.Error()), nil
		case err != nil:
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(res.Text), nil
	})
	mcpHandler := mcpserver.NewStreamableHTTPServer(s)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := &providers.UserInfo{Issuer: "https://dex.example", ID: r.Header.Get("X-User"), ClientID: "muster"}
		mcpHandler.ServeHTTP(w, r.WithContext(handler.ContextWithUserInfo(r.Context(), user)))
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/mcp"
}

func newElicitor(t *testing.T) *SignInElicitor {
	t.Helper()
	secret := make([]byte, 32)
	_, err := rand.Read(secret)
	require.NoError(t, err)
	ring, err := security.NewKeyRing(security.Key{ID: security.KeyID(secret), Secret: secret})
	require.NoError(t, err)
	sealer, err := requeststate.New(ring)
	require.NoError(t, err)
	elicitor, err := urlelicitation.New(sealer, urlelicitation.WithExpired(func(err error) bool {
		return errors.Is(err, requeststate.ErrExpired)
	}))
	require.NoError(t, err)
	return elicitor
}

type caller struct {
	user     string
	url      bool
	protocol string
}

func (c caller) connect(t *testing.T, endpoint string) *mcpclient.Client {
	t.Helper()
	tr, err := transport.NewStreamableHTTP(endpoint, transport.WithHTTPHeaders(map[string]string{"X-User": c.user}))
	require.NoError(t, err)
	opts := []mcpclient.ClientOption{mcpclient.WithManualInputRoundTrips(), mcpclient.WithDiscoverTimeout(-1)}
	if c.url {
		opts = append(opts, mcpclient.WithClientCapabilities(mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapability{URL: &struct{}{}}}))
	}
	client := mcpclient.NewClient(tr, opts...)
	require.NoError(t, client.Start(t.Context()))
	t.Cleanup(func() { _ = client.Close() })
	init := mcp.InitializeRequest{}
	init.Params.ProtocolVersion = c.protocol
	init.Params.ClientInfo = mcp.Implementation{Name: "caller"}
	_, err = client.Initialize(t.Context(), init)
	require.NoError(t, err)
	return client
}

func callRelayTool(t *testing.T, client *mcpclient.Client, roundTrip mcp.MultiRoundTripParams) (*mcp.CallToolResult, error) {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Name = relayTool
	req.Params.Arguments = map[string]any{"workspace": "platform"}
	req.Params.MultiRoundTripParams = roundTrip
	return client.CallTool(t.Context(), req)
}

func answer(state string, action mcp.ElicitationResponseAction) mcp.MultiRoundTripParams {
	return mcp.MultiRoundTripParams{
		RequestState: state,
		InputResponses: mcp.InputResponses{urlelicitation.RelayKey: mcp.NewElicitationInputResponse(mcp.ElicitationResult{
			ElicitationResponse: mcp.ElicitationResponse{Action: action},
		})},
	}
}

func textOfResult(res *mcp.CallToolResult) string {
	var out string
	for _, c := range res.Content {
		if t, ok := mcp.AsTextContent(c); ok {
			out += t.Text
		}
	}
	return out
}

func TestMusterSignInIsRelayedAndAcceptRetriesMustersCall(t *testing.T) {
	muster := &fakeMuster{}
	endpoint := relayServer(t, muster.start(t), newElicitor(t))
	client := caller{user: "alice", url: true, protocol: mcp.LATEST_PROTOCOL_VERSION}.connect(t, endpoint)

	res, err := callRelayTool(t, client, mcp.MultiRoundTripParams{})
	require.NoError(t, err)
	require.True(t, res.NeedsInput())
	require.Len(t, res.InputRequests, 1)
	asked := res.InputRequests[urlelicitation.RelayKey].Elicitation
	require.NotNil(t, asked)
	require.Equal(t, mcp.ElicitationModeURL, asked.Mode)
	require.Equal(t, musterSignInAt, asked.URL)
	require.NotEmpty(t, res.RequestState)
	require.NotEqual(t, musterState, res.RequestState)

	res, err = callRelayTool(t, client, answer(res.RequestState, mcp.ElicitationResponseActionAccept))
	require.NoError(t, err)
	require.False(t, res.IsError, textOfResult(res))
	require.Equal(t, "granted", textOfResult(res))

	calls := muster.Calls()
	require.Len(t, calls, 2)
	require.True(t, calls[0].DeclaredURL)
	require.Empty(t, calls[0].RequestState)
	require.Equal(t, musterTarget, calls[1].Tool)
	require.Equal(t, map[string]any{"workspace": "platform"}, calls[1].Args)
	require.Equal(t, musterState, calls[1].RequestState)
	require.Equal(t, mcp.ElicitationResponseActionAccept, calls[1].Answer)
}

func TestDeclinedSignInRunsNothing(t *testing.T) {
	for _, action := range []mcp.ElicitationResponseAction{mcp.ElicitationResponseActionDecline, mcp.ElicitationResponseActionCancel} {
		t.Run(string(action), func(t *testing.T) {
			muster := &fakeMuster{}
			endpoint := relayServer(t, muster.start(t), newElicitor(t))
			client := caller{user: "alice", url: true, protocol: mcp.LATEST_PROTOCOL_VERSION}.connect(t, endpoint)

			res, err := callRelayTool(t, client, mcp.MultiRoundTripParams{})
			require.NoError(t, err)
			require.True(t, res.NeedsInput())

			res, err = callRelayTool(t, client, answer(res.RequestState, action))
			require.NoError(t, err)
			require.True(t, res.IsError)
			require.Contains(t, textOfResult(res), "did not run")
			require.Len(t, muster.Calls(), 1)
		})
	}
}

func TestRequestStateIsBoundToTheCaller(t *testing.T) {
	muster := &fakeMuster{}
	endpoint := relayServer(t, muster.start(t), newElicitor(t))
	alice := caller{user: "alice", url: true, protocol: mcp.LATEST_PROTOCOL_VERSION}.connect(t, endpoint)

	res, err := callRelayTool(t, alice, mcp.MultiRoundTripParams{})
	require.NoError(t, err)
	require.True(t, res.NeedsInput())
	state := res.RequestState

	t.Run("tampered", func(t *testing.T) {
		_, err := callRelayTool(t, alice, answer(state[:len(state)-2]+"AA", mcp.ElicitationResponseActionAccept))
		require.ErrorContains(t, err, "invalid requestState")
	})
	t.Run("another user", func(t *testing.T) {
		bob := caller{user: "bob", url: true, protocol: mcp.LATEST_PROTOCOL_VERSION}.connect(t, endpoint)
		_, err := callRelayTool(t, bob, answer(state, mcp.ElicitationResponseActionAccept))
		require.ErrorContains(t, err, "invalid requestState")
	})
	require.Len(t, muster.Calls(), 1)
}

func TestCallerWithoutURLElicitationGetsTheSignInAsAToolError(t *testing.T) {
	muster := &fakeMuster{}
	endpoint := relayServer(t, muster.start(t), newElicitor(t))
	client := caller{user: "alice", protocol: mcp.LATEST_PROTOCOL_VERSION}.connect(t, endpoint)

	res, err := callRelayTool(t, client, mcp.MultiRoundTripParams{})
	require.NoError(t, err)
	require.False(t, res.NeedsInput())
	require.True(t, res.IsError)
	require.Contains(t, textOfResult(res), "Open "+musterSignInAt+" and then retry.")
}

func TestLegacyCallerWithURLElicitationGetsURLElicitationRequired(t *testing.T) {
	muster := &fakeMuster{}
	endpoint := relayServer(t, muster.start(t), newElicitor(t))
	client := caller{user: "alice", url: true, protocol: mcp.LATEST_LEGACY_PROTOCOL_VERSION}.connect(t, endpoint)

	_, err := callRelayTool(t, client, mcp.MultiRoundTripParams{})
	var required mcp.URLElicitationRequiredError
	require.ErrorAs(t, err, &required)
	require.Len(t, required.Elicitations, 1)
	require.Equal(t, musterSignInAt, required.Elicitations[0].URL)
}

func TestWithoutAKeyMusterAnswersAuthRequired(t *testing.T) {
	muster := &fakeMuster{}
	endpoint := relayServer(t, muster.start(t), nil)
	client := caller{user: "alice", url: true, protocol: mcp.LATEST_PROTOCOL_VERSION}.connect(t, endpoint)

	res, err := callRelayTool(t, client, mcp.MultiRoundTripParams{})
	require.NoError(t, err)
	require.False(t, res.NeedsInput())
	require.True(t, res.IsError)
	require.Contains(t, textOfResult(res), "auth_required: muster needs your sign-in to workspace-manager: "+musterSignInAt)
	calls := muster.Calls()
	require.Len(t, calls, 1)
	require.False(t, calls[0].DeclaredURL)
}
