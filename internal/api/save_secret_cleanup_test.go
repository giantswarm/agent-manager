package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/agent-manager/internal/organizations"
)

// newConsumerSubject is a set tool call that creates a consumer whose key is
// missing, so it prepares a credential operation, and the object of that
// name another writer makes.
type newConsumerSubject struct {
	tool, name, secret string
	gvr                schema.GroupVersionResource
	arguments          map[string]any
	other              func() *unstructured.Unstructured
}

func newConsumerSubjects() []newConsumerSubject {
	return []newConsumerSubject{ // #nosec G101 -- Secret names, no credentials
		{ToolSetOrganizationModel, "claude", "tenant-credential-claude", mcGVR,
			map[string]any{"namespace": orgNS, "name": "claude", "provider": "OpenAI", "model": "gpt-5", "create": true},
			func() *unstructured.Unstructured { return model("claude", false) }},
		{ToolSetOrganizationConnector, "gh", "tenant-credential-gh", serverGVR,
			map[string]any{"namespace": orgNS, "name": "gh", "url": "https://mcp.example.com/mcp", "credentialHeader": "Authorization", "create": true},
			func() *unstructured.Unstructured { return connector("gh", "tenant-credential-gh", "token") }},
		{ToolSetOrganizationEgress, "github", "tenant-credential-github", egressGVR,
			map[string]any{"namespace": orgNS, "name": "github", "destinations": []any{map[string]any{"origin": "https://api.github.com", "credentialHeader": "Authorization"}}, "create": true},
			func() *unstructured.Unstructured {
				return egressPolicy("github", map[string]any{"origin": "https://api.github.com"})
			}},
	}
}

// prepareNewConsumer prepares subject's operation and makes another writer
// create its object between the save's dry run and its create.
func prepareNewConsumer(t *testing.T, fixture *orgFixture, subject newConsumerSubject) organizations.ResourceResult {
	t.Helper()
	text, failed := fixture.call(t, subject.tool, subject.arguments)
	require.False(t, failed, text)
	var result organizations.ResourceResult
	require.NoError(t, json.Unmarshal([]byte(text), &result))
	require.NotEmpty(t, result.CredentialOperation, text)
	fixture.dyn.PrependReactor("create", subject.gvr.Resource, func(action k8stesting.Action) (bool, runtime.Object, error) {
		if len(action.(k8stesting.CreateActionImpl).CreateOptions.DryRun) > 0 {
			return false, nil, nil
		}
		if err := fixture.dyn.Tracker().Create(subject.gvr, subject.other(), orgNS); err != nil && !apierrors.IsAlreadyExists(err) {
			return true, nil, err
		}
		return true, nil, apierrors.NewAlreadyExists(subject.gvr.GroupResource(), subject.name)
	})
	return result
}

// An apply-mode save that creates the credential Secret and then finds its
// new consumer made by someone else between its dry run and its create
// deletes the Secret it created, at the version it wrote and with no
// propagation policy (the API server answers that with a Status, never the
// Secret): the value does not stay for the other object to send. It answers
// already_exists, and a retry while that object exists writes nothing.
func TestCredentialSaveDeletesTheSecretItCreatedWhenTheConsumerCreateFails(t *testing.T) {
	for _, subject := range newConsumerSubjects() {
		t.Run(subject.tool, func(t *testing.T) {
			fixture := newOrgFixture(t)
			result := prepareNewConsumer(t, fixture, subject)
			confirmation := confirmOperation(t, fixture, result.CredentialOperation)
			deletes := 0
			fixture.dyn.PrependReactor("delete", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
				deletes++
				options := action.(k8stesting.DeleteActionImpl).DeleteOptions
				assert.Nil(t, options.PropagationPolicy, "a delete without a propagation policy is answered with a Status")
				assert.Empty(t, options.DryRun)
				stored, err := fixture.dyn.Tracker().Get(secretGVR, orgNS, subject.secret)
				require.NoError(t, err)
				written := stored.(*unstructured.Unstructured).GetResourceVersion()
				require.NotNil(t, options.Preconditions)
				require.NotNil(t, options.Preconditions.ResourceVersion)
				assert.Equal(t, written, *options.Preconditions.ResourceVersion, "only the version this save wrote")
				return false, nil, nil
			})
			status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, "first-value"), nil)
			require.Equal(t, http.StatusConflict, status, answer)
			require.Equal(t, "already_exists", errorCode(t, answer))
			require.Equal(t, 1, deletes)
			requireNoSecret(t, fixture, subject.secret)

			retry := confirmOperation(t, fixture, result.CredentialOperation)
			status, answer, _ = operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, retry.Version, valuesBody(t, result, "first-value"), nil)
			require.Equal(t, http.StatusConflict, status, answer)
			require.Equal(t, "already_exists", errorCode(t, answer))
			requireNoSecret(t, fixture, subject.secret)
		})
	}
}

// A delete the API server refuses because the Secret changed since the
// save's write leaves the Secret as it is.
func TestCredentialSaveLeavesASecretChangedSinceItsWrite(t *testing.T) {
	subject := newConsumerSubjects()[0]
	fixture := newOrgFixture(t)
	result := prepareNewConsumer(t, fixture, subject)
	confirmation := confirmOperation(t, fixture, result.CredentialOperation)
	fixture.dyn.PrependReactor("delete", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(secretGVR.GroupResource(), subject.secret, assert.AnError)
	})
	status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, "first-value"), nil)
	require.Equal(t, http.StatusConflict, status, answer)
	require.Equal(t, "already_exists", errorCode(t, answer))
	_, err := fixture.dyn.Tracker().Get(secretGVR, orgNS, subject.secret)
	require.NoError(t, err, "a Secret changed since the write stays")
}
