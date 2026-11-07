package api

import (
	"context"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/giantswarm/agent-manager/internal/organizations"
)

func (t *orgTools) watchCredentialCompletion(ctx context.Context, result *organizations.ResourceResult) {
	session := mcpserver.ClientSessionFromContext(ctx)
	if session == nil || t.server == nil {
		return
	}
	if _, active := t.credentialWatches.LoadOrStore(result.ElicitationID, struct{}{}); active {
		return
	}
	operationID, elicitationID := result.CredentialOperation, result.ElicitationID
	go func() {
		defer t.credentialWatches.Delete(elicitationID)
		watchContext, cancel := context.WithTimeout(t.lifecycle, 15*time.Minute)
		defer cancel()
		if t.svc.AwaitCredentialCompletion(watchContext, operationID, elicitationID) {
			_ = t.server.SendElicitationComplete(watchContext, session, elicitationID)
		}
	}()
}
