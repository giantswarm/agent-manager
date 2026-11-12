package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/agent-manager/internal/organizations"
)

// newConsumerSubject is a set tool call that creates a consumer whose key is
// missing, so it prepares a credential operation.
type newConsumerSubject struct {
	tool, resource, name, secret string
	arguments                    map[string]any
}

func commitlessCreateSubjects() []newConsumerSubject {
	return []newConsumerSubject{ // #nosec G101 -- Secret names, no credentials
		{ToolSetOrganizationModel, "modelconfigs", "claude", "tenant-credential-claude", map[string]any{"namespace": orgNS, "name": "claude", "provider": "OpenAI", "model": "gpt-5", "create": true}},
		{ToolSetOrganizationConnector, "remotemcpservers", "gh", "tenant-credential-gh", map[string]any{"namespace": orgNS, "name": "gh", "url": "https://mcp.example.com/mcp", "credentialHeader": "Authorization", "create": true}},
		{ToolSetOrganizationEgress, "egresspolicies", "github", "tenant-credential-github", map[string]any{"namespace": orgNS, "name": "github", "destinations": []any{map[string]any{"origin": "https://api.github.com", "credentialHeader": "Authorization"}}, "create": true}},
	}
}

func decodeResult(t *testing.T, text string) organizations.ResourceResult {
	t.Helper()
	var result organizations.ResourceResult
	require.NoError(t, json.Unmarshal([]byte(text), &result))
	return result
}

// An apply-mode save that creates the credential Secret and then finds its
// new consumer made by someone else between its dry run and its create
// deletes the Secret it created: the value does not stay behind for the
// other object to send. It answers already_exists.
func TestCredentialSaveDeletesTheSecretItCreatedWhenTheConsumerCreateFails(t *testing.T) {
	for _, subject := range commitlessCreateSubjects() {
		t.Run(subject.tool, func(t *testing.T) {
			fixture := newOrgFixture(t)
			text, failed := fixture.call(t, subject.tool, subject.arguments)
			require.False(t, failed, text)
			result := decodeResult(t, text)
			require.NotEmpty(t, result.CredentialOperation, text)
			confirmation := confirmOperation(t, fixture, result.CredentialOperation)
			fixture.dyn.PrependReactor("create", subject.resource, func(action k8stesting.Action) (bool, runtime.Object, error) {
				if len(action.(k8stesting.CreateActionImpl).CreateOptions.DryRun) > 0 {
					return false, nil, nil
				}
				return true, nil, apierrors.NewAlreadyExists(action.GetResource().GroupResource(), subject.name)
			})
			status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, "first-value"), nil)
			require.Equal(t, http.StatusConflict, status, answer)
			require.Equal(t, "already_exists", errorCode(t, answer))
			requireNoSecret(t, fixture, subject.secret)
		})
	}
}

// A save that replaces a Secret that existed before never deletes it, even
// when a later write of the save fails.
func TestCredentialSaveKeepsASecretThatExistedWhenAWriteFails(t *testing.T) {
	fixture := newOrgFixture(t, sharedModel("token"))
	seedSecret(t, fixture, sharedSecret, "token")
	result := replaceCredential(t, fixture, nil)
	confirmation := confirmOperation(t, fixture, result.CredentialOperation)
	fixture.dyn.PrependReactor("update", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if len(action.(k8stesting.UpdateActionImpl).UpdateOptions.DryRun) > 0 {
			return false, nil, nil
		}
		return true, nil, apierrors.NewAlreadyExists(action.GetResource().GroupResource(), sharedSecret)
	})
	status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, "rotated-value"), nil)
	require.GreaterOrEqual(t, status, 400, answer)
	_, err := fixture.dyn.Tracker().Get(secretGVR, orgNS, sharedSecret)
	require.NoError(t, err, "the Secret that existed stays")
}
