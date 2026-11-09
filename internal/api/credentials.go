package api

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/identity"
	"github.com/giantswarm/agent-manager/internal/organizations"
)

const credentialInputPrefix = "credential_" // #nosec G101 -- an input request key prefix, not a credential

// credentialInputOutcome words each elicitation answer that ends a credential
// write without writing; both answer isError.
var credentialInputOutcome = map[mcp.ElicitationResponseAction]string{
	mcp.ElicitationResponseActionDecline: "declined",
	mcp.ElicitationResponseActionCancel:  "cancelled",
}

// URL navigation does not confirm a save. Only the authenticated HTTPS save
// completes an operation and makes its receipt available to an MCP retry.
func (t *orgTools) runWrite(fn func(context.Context, mcp.CallToolRequest) (*organizations.ResourceResult, error)) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if t.svc == nil {
			return errResult(errOrganizationsUnsupported), nil
		}
		arguments, err := json.Marshal(req.Params.Arguments)
		if err != nil {
			return errResult(errCredentialBody), nil
		}
		if value, present := req.GetArguments()[argRequestID]; present {
			requestID, valid := value.(string)
			if !valid || requestID == "" || len(requestID) > 128 {
				return errResult(errCredentialBody), nil
			}
			bound := maps.Clone(req.GetArguments())
			delete(bound, argRequestID)
			req.Params.Arguments = bound
		}
		person, _ := identity.FromContext(ctx)
		client := ""
		if person != nil {
			client = person.Client
		}
		if session := mcpserver.ClientSessionFromContext(ctx); session != nil {
			client += "/" + session.SessionID()
		}
		ctx = organizations.ContextWithCredentialRequest(ctx, req.Params.Name, string(arguments), client)
		res, err := t.svc.CredentialReceipt(ctx)
		if err == nil && res == nil {
			res, err = fn(ctx, req)
		}
		if err != nil {
			return errResult(err), nil
		}
		if res.Pending {
			if response, ok := req.Params.InputResponses[credentialInputPrefix+res.ElicitationID]; ok {
				if response.DecodeFor(mcp.MethodElicitationCreate) != nil {
					return errResult(errCredentialBody), nil
				}
				if outcome, ends := credentialInputOutcome[elicitationAction(response.Elicitation)]; ends {
					operation, err := t.svc.ReadCredentialOperation(ctx, res.CredentialOperation)
					if err == nil {
						err = t.svc.CancelCredentialOperation(ctx, res.CredentialOperation, operation.Version)
					}
					if err != nil {
						return errResult(err), nil
					}
					return mcp.NewToolResultError(fmt.Sprintf("Credential input was %s; no resource was written.", outcome)), nil
				}
			}
		}
		if res.DryRun || !clientSupportsURLElicitation(ctx) {
			return credentialResult(res)
		}
		var elicitations []mcp.ElicitationParams
		for _, c := range res.Credentials {
			if c.NeedsInput() {
				elicitations = append(elicitations, mcp.ElicitationParams{
					Mode:          mcp.ElicitationModeURL,
					Message:       fmt.Sprintf("Enter the value of key %s of Secret %s, which %s %s uses, in the credential UI.", c.Key, c.Secret, res.Kind, res.Name),
					URL:           c.FormURL,
					ElicitationID: res.ElicitationID,
				})
				break
			}
		}
		if len(elicitations) == 0 {
			return credentialResult(res)
		}
		if mcp.IsModernProtocol(mcpserver.RequestProtocolVersion(ctx)) {
			b := mcpserver.NewInputRequestBuilder("")
			for _, e := range elicitations {
				b.Elicit(credentialInputPrefix+res.ElicitationID, e)
			}
			return b.ToolResult(), nil
		}
		t.watchCredentialCompletion(ctx, res)
		return nil, mcp.URLElicitationRequiredError{Elicitations: elicitations}
	}
}

func elicitationAction(response *mcp.ElicitationResult) mcp.ElicitationResponseAction {
	if response == nil {
		return ""
	}
	return response.Action
}

// credentialResult answers a credential write as the tools' outputSchema
// declares: the result as structuredContent, its JSON as text, and a
// resource_link to the credential UI for each credential waiting on a value,
// the link a client without URL elicitation offers the person.
func credentialResult(res *organizations.ResourceResult) (*mcp.CallToolResult, error) {
	data, err := json.Marshal(res)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("encode result: %v", err)), nil
	}
	result := mcp.NewToolResultStructured(res, string(data))
	for _, c := range res.Credentials {
		if c.FormURL != "" {
			link := mcp.NewResourceLink(c.FormURL, "credential-ui",
				fmt.Sprintf("Enter the value of key %s of Secret %s in the credential UI.", c.Key, c.Secret), "text/html")
			// The form is for the person; a client must not fetch it for the model.
			link.Annotations = &mcp.Annotations{Audience: []mcp.Role{mcp.RoleUser}}
			result.Content = append(result.Content, link)
		}
	}
	return result, nil
}

// clientSupportsURLElicitation reports whether the client declared URL mode
// elicitation: in the request's capabilities from protocol 2026-07-28, at
// initialize before.
func clientSupportsURLElicitation(ctx context.Context) bool {
	var capabilities *mcp.ClientCapabilities
	if info := mcpserver.RequestProtocolInfoFromContext(ctx); info != nil && info.Modern {
		capabilities = info.ClientCapabilities
	} else if session, ok := mcpserver.ClientSessionFromContext(ctx).(mcpserver.SessionWithClientInfo); ok {
		declared := session.GetClientCapabilities()
		capabilities = &declared
	}
	return capabilities != nil && capabilities.Elicitation != nil && capabilities.Elicitation.URL != nil
}

var errCredentialBody = fmt.Errorf("%w: invalid credential operation request", agents.ErrInvalid)

func (h *REST) getCredential(w http.ResponseWriter, r *http.Request) {
	if h.orgs == nil {
		h.writeError(w, errOrganizationsUnsupported)
		return
	}
	q := r.URL.Query()
	res, err := h.orgs.CredentialReferences(r.Context(), r.PathValue("parent"), r.PathValue("organization"), r.PathValue("secret"), q.Get("kind"), q.Get("name"))
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
