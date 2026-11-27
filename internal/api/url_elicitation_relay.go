package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/giantswarm/mcp-oauth/handler"
	"github.com/giantswarm/mcp-oauth/providers"
	"github.com/giantswarm/mcp-toolkit/urlelicitation"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/giantswarm/agent-manager/internal/mustermcp"
)

// URLElicitor relays URL elicitations with state bound to a validated caller.
type URLElicitor = urlelicitation.Elicitor[*providers.UserInfo]

type unavailableRequestState struct{}

func (unavailableRequestState) Error() string {
	return "requestState cannot be verified: URL elicitation relay is disabled"
}

func (unavailableRequestState) JSONRPCError() mcp.JSONRPCError {
	return mcp.JSONRPCError{
		JSONRPC: mcp.JSONRPC_VERSION,
		Error: mcp.JSONRPCErrorDetails{
			Code: mcp.INVALID_PARAMS, Message: unavailableRequestState{}.Error(),
		},
	}
}

// WithURLElicitationRelay relays URL input requests and caller-bound retries.
func WithURLElicitationRelay(elicitor *URLElicitor, log *slog.Logger) Option {
	return func(o *serverOptions) { o.urlElicitations, o.log = elicitor, log }
}

// relayURLElicitations validates continuations before handlers and relays the
// first pending URL interaction. Decline and cancel never run the handler.
func relayURLElicitations(elicitor *URLElicitor, log *slog.Logger) mcpserver.ToolHandlerMiddleware {
	return func(next mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
		return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if elicitor == nil {
				if request.Params.RequestState != "" {
					return nil, mcpserver.NewHandlerError(unavailableRequestState{})
				}
				return next(ctx, request)
			}
			user, _ := handler.UserInfoFromContext(ctx)
			answer, err := elicitor.Result(ctx, request, user)
			if err != nil {
				return nil, err
			}
			if user == nil {
				return next(ctx, request)
			}
			var retry *urlelicitation.Retry
			if answer != nil {
				if retry, err = answer.Retry(); err != nil {
					return nil, err
				}
				if retry.Action != mcp.ElicitationResponseActionAccept {
					return mcp.NewToolResultError(fmt.Sprintf("%s did not run: the URL interaction muster requested was answered with %s.", request.Params.Name, retry.Action)), nil
				}
			}
			relayCtx, relay := mustermcp.WithRelay(ctx, retry)
			result, err := next(relayCtx, request)
			elicitation := relay.Elicitation()
			if elicitation == nil || err != nil || result == nil || !result.IsError {
				return result, err
			}
			relayed, relayErr := elicitor.Relay(ctx, request, user, elicitation.Target, elicitation.Relayed)
			if relayErr != nil && !errors.As(relayErr, new(mcp.URLElicitationRequiredError)) {
				log.WarnContext(ctx, "URL elicitation relay failed", "tool", request.Params.Name, "error", relayErr)
				return nil, relayErr
			}
			return relayed, relayErr
		}
	}
}
