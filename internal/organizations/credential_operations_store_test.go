package organizations

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/giantswarm/agent-manager/internal/agents"
)

func TestCredentialOperationMetadataExpires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := kubefake.NewClientset()
		store := NewCredentialOperations(client.CoreV1().ConfigMaps("operations"))
		operation := &credentialOperation{ID: strings.Repeat("a", 48), State: operationPending, ExpiresAt: time.Now().Add(credentialOperationTTL)}
		require.NoError(t, store.put(t.Context(), operation))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go store.Expire(ctx)
		time.Sleep(credentialOperationTTL + time.Minute)
		synctest.Wait()
		_, err := store.read(t.Context(), operation.ID)
		require.ErrorIs(t, err, agents.ErrNotFound)
		cancel()
		synctest.Wait()
	})
}

func TestCredentialUIURLHasAFixedOrigin(t *testing.T) {
	require.NoError(t, ValidateCredentialUIURL("https://portal.example.com/organizations/gazelle/{parent}/{organization}/credentials?operation={operation}"))
	for _, value := range []string{"http://portal.example.com/?operation={operation}", "https://{operation}.example.com/", "https://portal.example.com/#operation={operation}", "https://user:password@portal.example.com/?operation={operation}", "https://portal.example.com/{unknown}?operation={operation}", "https://portal.example.com/"} {
		require.Error(t, ValidateCredentialUIURL(value))
	}
}
