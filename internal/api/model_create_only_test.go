package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/agent-manager/internal/organizations"
)

// createModel calls set_organization_model for a new model claude with
// extra arguments.
func createModel(t *testing.T, fixture *orgFixture, extra map[string]any) (string, bool) {
	t.Helper()
	arguments := map[string]any{"namespace": orgNS, "name": "claude", "provider": "OpenAI", "model": "gpt-5", "create": true}
	for key, value := range extra {
		arguments[key] = value
	}
	return fixture.call(t, ToolSetOrganizationModel, arguments)
}

// liveModelSpec is the model id of the live ModelConfig claude.
func liveModelSpec(t *testing.T, fixture *orgFixture) string {
	t.Helper()
	value, _, err := unstructured.NestedString(fixture.get(t, mcGVR, orgNS, "claude").Object, "spec", "model")
	require.NoError(t, err)
	return value
}

// With create, a model that exists live is refused with already_exists and
// left as it is; without it, the same call changes the model as before.
func TestSetModelCreateRefusesAnExistingModel(t *testing.T) {
	fixture := newOrgFixture(t, model("claude", true))
	seedSecret(t, fixture, "tenant-credential-claude", "apiKey")

	text, failed := createModel(t, fixture, nil)
	require.True(t, failed, text)
	require.True(t, strings.HasPrefix(text, "already_exists:"), text)
	require.Equal(t, "m", liveModelSpec(t, fixture))
	require.Zero(t, consumerWrites(fixture))

	text, failed = createModel(t, fixture, map[string]any{"create": false})
	require.False(t, failed, text)
	require.Equal(t, "gpt-5", liveModelSpec(t, fixture))
}

// With create, a new model is created.
func TestSetModelCreateCreatesANewModel(t *testing.T) {
	fixture := newOrgFixture(t)
	seedSecret(t, fixture, "tenant-credential-claude", "apiKey")
	text, failed := createModel(t, fixture, nil)
	require.False(t, failed, text)
	require.Equal(t, true, decode(t, text)["created"])
	require.Equal(t, "gpt-5", liveModelSpec(t, fixture))
}

// A model made by someone else between the read and the write is refused by
// the create itself, with the same code.
func TestSetModelCreateRefusesAModelMadeAfterTheRead(t *testing.T) {
	fixture := newOrgFixture(t)
	seedSecret(t, fixture, "tenant-credential-claude", "apiKey")
	fixture.dyn.PrependReactor("create", "modelconfigs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if len(action.(k8stesting.CreateActionImpl).CreateOptions.DryRun) > 0 {
			return false, nil, nil
		}
		return true, nil, apierrors.NewAlreadyExists(mcGVR.GroupResource(), "claude")
	})
	text, failed := createModel(t, fixture, nil)
	require.True(t, failed, text)
	require.True(t, strings.HasPrefix(text, "already_exists:"), text)
}

// In commit mode, with create, a model that git applies, or whose file is in
// git and not applied yet, is refused with already_exists and no pull
// request; with a credential to enter or without.
func TestSetModelCreateInCommitModeRefusesAnExistingModel(t *testing.T) {
	file := paymentsPath + "/modelconfig-claude.yaml"
	for _, scenario := range []struct {
		name   string
		live   bool
		secret bool
	}{
		{"applied from git", true, true},
		{"applied from git, credential missing", true, false},
		{"in git, not applied yet", false, true},
		{"in git, not applied yet, credential missing", false, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var objects []runtime.Object
			if scenario.live {
				live := model("claude", true)
				live.SetLabels(fromGitLabels("payments"))
				objects = append(objects, live)
			}
			fixture, forge := commitFixture(t, map[string][]byte{file: gitManifest(t, model("claude", true))}, objects...)
			if scenario.secret {
				seedSecret(t, fixture, "tenant-credential-claude", "apiKey")
			}
			text, failed := createModel(t, fixture, map[string]any{"mode": "commit"})
			require.True(t, failed, text)
			require.True(t, strings.HasPrefix(text, "already_exists:"), text)
			require.Empty(t, forge.PullRequests())
		})
	}
}

// A new model that waits for its key is refused at the save when a model of
// that name exists by then: the race does not move to the save.
func TestSetModelCreateSaveRefusesAModelMadeSincePreparation(t *testing.T) {
	fixture := newOrgFixture(t)
	result := prepareModelOperation(t, fixture, map[string]any{"create": true})
	confirmation := confirmOperation(t, fixture, result.CredentialOperation)
	_, err := fixture.dyn.Resource(mcGVR).Namespace(orgNS).Create(t.Context(), model("claude", false), metav1.CreateOptions{})
	require.NoError(t, err)
	status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, "first-value"), nil)
	require.Equal(t, http.StatusConflict, status, answer)
	require.Equal(t, "already_exists", errorCode(t, answer))
	require.NotContains(t, answer, "first-value")
	require.Equal(t, "m", liveModelSpec(t, fixture))
	requireNoSecret(t, fixture, "tenant-credential-claude")
}

// requireNoSecret says the credential Secret name was not written.
func requireNoSecret(t *testing.T, fixture *orgFixture, name string) {
	t.Helper()
	_, err := fixture.dyn.Tracker().Get(secretGVR, orgNS, name)
	require.True(t, apierrors.IsNotFound(err), "Secret %s: %v", name, err)
}

// A dry run with create in commit mode sees the model's file in git too,
// also when the key is missing: the dry run and the call agree.
func TestSetModelCreateDryRunInCommitModeSeesTheGitFile(t *testing.T) {
	fixture, forge := commitFixture(t, map[string][]byte{paymentsPath + "/modelconfig-claude.yaml": gitManifest(t, model("claude", true))})
	text, failed := createModel(t, fixture, map[string]any{"mode": "commit", "dryRun": true})
	require.True(t, failed, text)
	require.True(t, strings.HasPrefix(text, "already_exists:"), text)
	require.Empty(t, forge.PullRequests())
}

// A save of an operation that creates an object answers already_exists when
// the object exists by then: a model as its file in git (commit mode), a
// model made between the save's check and its create, and a connector (any
// kind, with or without create). Nothing is written.
func TestCredentialSaveAnswersAlreadyExists(t *testing.T) {
	t.Run("commit mode, the model's file appears in git", func(t *testing.T) {
		fixture, forge := commitFixture(t, nil)
		result := prepareModelOperation(t, fixture, map[string]any{"create": true, "mode": "commit"})
		confirmation := confirmOperation(t, fixture, result.CredentialOperation)
		require.NoError(t, forge.Commit(t.Context(), mcRepo, "main", "add the model", map[string][]byte{paymentsPath + "/modelconfig-claude.yaml": gitManifest(t, model("claude", false))}))
		status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, "first-value"), nil)
		require.Equal(t, http.StatusConflict, status, answer)
		require.Equal(t, "already_exists", errorCode(t, answer))
		require.Empty(t, forge.PullRequests())
	})
	t.Run("apply mode, the model is made after the save's check", func(t *testing.T) {
		fixture := newOrgFixture(t)
		result := prepareModelOperation(t, fixture, map[string]any{"create": true})
		confirmation := confirmOperation(t, fixture, result.CredentialOperation)
		fixture.dyn.PrependReactor("create", "modelconfigs", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewAlreadyExists(mcGVR.GroupResource(), "claude")
		})
		status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, "first-value"), nil)
		require.Equal(t, http.StatusConflict, status, answer)
		require.Equal(t, "already_exists", errorCode(t, answer))
		requireNoSecret(t, fixture, "tenant-credential-claude")
	})
	t.Run("a connector made since the preparation", func(t *testing.T) {
		fixture := newOrgFixture(t)
		text, failed := fixture.call(t, ToolSetOrganizationConnector, map[string]any{"namespace": orgNS, "name": "gh", "url": "https://mcp.github.example/mcp", "credentialHeader": "Authorization"})
		require.False(t, failed, text)
		var result organizations.ResourceResult
		require.NoError(t, json.Unmarshal([]byte(text), &result))
		require.NotEmpty(t, result.CredentialOperation, text)
		confirmation := confirmOperation(t, fixture, result.CredentialOperation)
		_, err := fixture.dyn.Resource(serverGVR).Namespace(orgNS).Create(t.Context(), connector("gh", "tenant-credential-gh", "token"), metav1.CreateOptions{})
		require.NoError(t, err)
		status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, "first-value"), nil)
		require.Equal(t, http.StatusConflict, status, answer)
		require.Equal(t, "already_exists", errorCode(t, answer))
		requireNoSecret(t, fixture, "tenant-credential-gh")
	})
}
