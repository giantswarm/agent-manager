package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"

	"github.com/giantswarm/agent-manager/internal/organizations"
)

// Secret names, no credentials.
const (
	sharedSecret = "tenant-credential-shared" // #nosec G101 -- a Secret's name
	claudeSecret = "tenant-credential-claude" // #nosec G101 -- a Secret's name
	ghSecret     = "tenant-credential-gh"     // #nosec G101 -- a Secret's name
)

func sharedModel(key string) *unstructured.Unstructured {
	consumer := model("claude", true)
	_ = unstructured.SetNestedField(consumer.Object, sharedSecret, "spec", "apiKeySecret")
	_ = unstructured.SetNestedField(consumer.Object, key, "spec", "apiKeySecretKey")
	return consumer
}

func sharedEgress() *unstructured.Unstructured {
	auth := map[string]any{"header": "Authorization", "secretKeyRef": map[string]any{"name": sharedSecret, "key": "token"}}
	return egressPolicy("github",
		map[string]any{"origin": "https://api.github.com", "authentication": auth},
		map[string]any{"origin": "https://uploads.github.com", "authentication": auth},
		map[string]any{"origin": "https://example.com"},
	)
}

func replaceCredential(t *testing.T, fixture *orgFixture, extra map[string]any) organizations.ResourceResult {
	t.Helper()
	arguments := map[string]any{"namespace": orgNS, "secret": sharedSecret, "key": "token"}
	for key, value := range extra {
		arguments[key] = value
	}
	text, failed := fixture.call(t, ToolReplaceOrganizationCredential, arguments)
	require.False(t, failed, text)
	var result organizations.ResourceResult
	require.NoError(t, json.Unmarshal([]byte(text), &result))
	require.True(t, result.Pending, text)
	require.NotEmpty(t, result.CredentialOperation, text)
	return result
}

// consumerWrites counts the writes, dry runs aside, of the kinds that read a
// credential Secret.
func consumerWrites(fixture *orgFixture) int {
	count := 0
	for _, action := range fixture.dyn.Actions() {
		switch action.GetResource().Resource {
		case "modelconfigs", "remotemcpservers", "egresspolicies":
		default:
			continue
		}
		switch typed := action.(type) {
		case k8stesting.CreateActionImpl:
			if len(typed.CreateOptions.DryRun) == 0 {
				count++
			}
		case k8stesting.UpdateActionImpl:
			if len(typed.UpdateOptions.DryRun) == 0 {
				count++
			}
		case k8stesting.PatchActionImpl, k8stesting.DeleteActionImpl:
			count++
		}
	}
	return count
}

// A Secret several objects read is replaced for all of them: the form lists
// each with where it sends the value, and only the Secret is written, whole.
func TestReplaceCredentialWritesTheSharedSecretOnly(t *testing.T) {
	egress := sharedEgress()
	fixture := newOrgFixture(t, sharedModel("token"), connector("gh", sharedSecret, "token"), egress)
	seedSecret(t, fixture, sharedSecret, "token")

	result := replaceCredential(t, fixture, nil)
	require.Equal(t, organizations.KindCredential, result.Kind)
	require.Equal(t, sharedSecret, result.Name)
	require.Len(t, result.Credentials, 1)
	require.Equal(t, organizations.CredentialReplace, result.Credentials[0].State)

	confirmation := confirmOperation(t, fixture, result.CredentialOperation)
	require.Empty(t, confirmation.Changes)
	require.Equal(t, []organizations.CredentialReference{
		{Kind: "EgressPolicy", Name: "github", Key: "token", Origins: []string{"https://api.github.com", "https://uploads.github.com"}},
		{Kind: "ModelConfig", Name: "claude", Key: "token", Provider: "Anthropic"},
		{Kind: "RemoteMCPServer", Name: "gh", Key: "token", URL: "https://mcp.example.com/mcp?api_key=REDACTED"},
	}, confirmation.References)

	status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, "rotated-value"), nil)
	require.Equal(t, http.StatusOK, status, answer)
	require.NotContains(t, answer, "rotated-value")
	data, _, err := unstructured.NestedMap(fixture.get(t, secretGVR, orgNS, sharedSecret).Object, "data")
	require.NoError(t, err)
	require.Equal(t, map[string]any{"token": base64.StdEncoding.EncodeToString([]byte("rotated-value"))}, data)
	require.Zero(t, consumerWrites(fixture))
	destinations, _, err := unstructured.NestedSlice(fixture.get(t, egressGVR, orgNS, "github").Object, "spec", "destinations")
	require.NoError(t, err)
	want, _, err := unstructured.NestedSlice(egress.Object, "spec", "destinations")
	require.NoError(t, err)
	require.Equal(t, want, destinations)
}

func TestReplaceCredentialRefusals(t *testing.T) {
	for _, scenario := range []struct {
		name, key, prefix, contains string
		objects                     func() []*unstructured.Unstructured
	}{
		{"another key of the Secret", "token", "conflict:", "ModelConfig claude reads key apiKey", func() []*unstructured.Unstructured {
			return []*unstructured.Unstructured{sharedModel("apiKey"), connector("gh", sharedSecret, "token")}
		}},
		{"unreferenced Secret", "token", "not_found:", "nothing in " + orgNS + " uses key token", func() []*unstructured.Unstructured {
			return []*unstructured.Unstructured{model("claude", true)}
		}},
		{"only another key referenced", "token", "not_found:", "uses key token", func() []*unstructured.Unstructured {
			return []*unstructured.Unstructured{sharedModel("apiKey")}
		}},
		{"consumer applied from git", "token", "gitops_owned:", "mode commit", func() []*unstructured.Unstructured {
			consumer := sharedModel("token")
			consumer.SetLabels(fromGitLabels("payments"))
			return []*unstructured.Unstructured{consumer, connector("gh", sharedSecret, "token")}
		}},
		{"not a credential Secret", "token", "invalid_request:", "must start with tenant-credential-", nil},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var objects []*unstructured.Unstructured
			if scenario.objects != nil {
				objects = scenario.objects()
			}
			fixture := newOrgFixture(t, runtimeObjects(objects)...)
			seedSecret(t, fixture, sharedSecret, "token")
			secret := sharedSecret
			if scenario.objects == nil {
				secret = "other-secret"
			}
			text, failed := fixture.call(t, ToolReplaceOrganizationCredential, map[string]any{"namespace": orgNS, "secret": secret, "key": scenario.key})
			require.True(t, failed, text)
			require.True(t, strings.HasPrefix(text, scenario.prefix), text)
			require.Contains(t, text, scenario.contains)
			require.NotContains(t, text, "credentialOperation")
			data, _, err := unstructured.NestedMap(fixture.get(t, secretGVR, orgNS, sharedSecret).Object, "data")
			require.NoError(t, err)
			require.Equal(t, map[string]any{"token": base64.StdEncoding.EncodeToString([]byte(liveValue))}, data)
		})
	}
}

// A consumer reading another key that appears after the preparation is
// refused at the save: the whole-Secret write would drop its key.
func TestReplaceCredentialSaveRefusesAnotherKeyAddedSincePreparation(t *testing.T) {
	fixture := newOrgFixture(t, connector("gh", sharedSecret, "token"))
	seedSecret(t, fixture, sharedSecret, "token")
	result := replaceCredential(t, fixture, nil)
	confirmation := confirmOperation(t, fixture, result.CredentialOperation)
	_, err := fixture.dyn.Resource(mcGVR).Namespace(orgNS).Create(t.Context(), sharedModel("apiKey"), metav1.CreateOptions{})
	require.NoError(t, err)
	status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, "rotated-value"), nil)
	require.Equal(t, http.StatusConflict, status, answer)
	data, _, err := unstructured.NestedMap(fixture.get(t, secretGVR, orgNS, sharedSecret).Object, "data")
	require.NoError(t, err)
	require.Equal(t, map[string]any{"token": base64.StdEncoding.EncodeToString([]byte(liveValue))}, data)
}

func runtimeObjects(objects []*unstructured.Unstructured) []runtime.Object {
	out := make([]runtime.Object, 0, len(objects))
	for _, object := range objects {
		out = append(out, object)
	}
	return out
}

func gitManifest(t *testing.T, object *unstructured.Unstructured) []byte {
	t.Helper()
	file := object.DeepCopy()
	file.SetNamespace("")
	raw, err := yaml.Marshal(file.Object)
	require.NoError(t, err)
	return raw
}

// In commit mode the pull request carries the encrypted Secret alone, for
// every git consumer of it; an encrypted Secret holding another key is
// refused before a value is entered.
func TestReplaceCredentialCommitsTheSecretOnly(t *testing.T) {
	for _, otherKeyInGit := range []bool{false, true} {
		t.Run(map[bool]string{false: "consumers in git", true: "encrypted Secret with another key"}[otherKeyInGit], func(t *testing.T) {
			modelFile, egressFile := paymentsPath+"/modelconfig-claude.yaml", paymentsPath+"/egresspolicy-github.yaml"
			base := map[string][]byte{
				modelFile:                            gitManifest(t, sharedModel("token")),
				egressFile:                           gitManifest(t, sharedEgress()),
				paymentsPath + "/kustomization.yaml": []byte("resources:\n  - modelconfig-claude.yaml\n  - egresspolicy-github.yaml\n"),
			}
			secretFile := paymentsPath + "/" + sharedSecret + "-secret.enc.yaml"
			if otherKeyInGit {
				base[secretFile] = []byte("apiVersion: v1\nkind: Secret\nmetadata:\n  name: " + sharedSecret + "\ndata:\n  other: ENC[AES256_GCM,data:x]\n")
			}
			liveModel, liveEgress := sharedModel("token"), sharedEgress()
			liveModel.SetLabels(fromGitLabels("payments"))
			liveEgress.SetLabels(fromGitLabels("payments"))
			fixture, forge := commitFixture(t, base, liveModel, liveEgress)
			if otherKeyInGit {
				text, failed := fixture.call(t, ToolReplaceOrganizationCredential, map[string]any{"namespace": orgNS, "secret": sharedSecret, "key": "token", "mode": "commit"})
				require.True(t, failed, text)
				require.True(t, strings.HasPrefix(text, "conflict:"), text)
				require.Contains(t, text, "other credential keys")
				require.Empty(t, forge.PullRequests())
				return
			}
			result := replaceCredential(t, fixture, map[string]any{"mode": "commit"})
			confirmation := confirmOperation(t, fixture, result.CredentialOperation)
			require.Len(t, confirmation.References, 2)
			status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, "rotated-value"), nil)
			require.Equal(t, http.StatusOK, status, answer)
			require.Len(t, forge.PullRequests(), 1)
			head := forge.Files(mcRepo, forge.PullRequests()[0].Head)
			require.Equal(t, base[modelFile], head[modelFile])
			require.Equal(t, base[egressFile], head[egressFile])
			require.Contains(t, string(head[secretFile]), "ENC[AES256_GCM,")
			for _, raw := range head {
				require.NotContains(t, string(raw), "rotated-value")
			}
			require.Zero(t, consumerWrites(fixture))
		})
	}
}

// In commit mode a consumer written live is refused, as the set tools refuse
// it: no file in git defines it, so its Secret stays out of git.
func TestReplaceCredentialCommitRefusesALiveConsumer(t *testing.T) {
	liveModel := sharedModel("token")
	liveModel.SetLabels(fromGitLabels("payments"))
	fixture, forge := commitFixture(t, map[string][]byte{paymentsPath + "/modelconfig-claude.yaml": gitManifest(t, sharedModel("token"))}, liveModel, connector("gh", sharedSecret, "token"))
	text, failed := fixture.call(t, ToolReplaceOrganizationCredential, map[string]any{"namespace": orgNS, "secret": sharedSecret, "key": "token", "mode": "commit"})
	require.True(t, failed, text)
	require.True(t, strings.HasPrefix(text, "conflict:"), text)
	require.Contains(t, text, "RemoteMCPServer "+orgNS+"/gh was written live")
	require.NotContains(t, text, "credentialOperation")
	require.Empty(t, forge.PullRequests())
}

// The confirmation of a consumer tool's operation lists every live object
// that names the Secret, the planned write in place of its live object.
func TestCredentialConfirmationListsLiveReferences(t *testing.T) {
	t.Run("planned write replaces its live object", func(t *testing.T) {
		fixture := newOrgFixture(t, model("claude", true))
		result := prepareModelOperation(t, fixture, map[string]any{"provider": "OpenAI"})
		confirmation := confirmOperation(t, fixture, result.CredentialOperation)
		require.Equal(t, []organizations.CredentialReference{{Kind: "ModelConfig", Name: "claude", Key: "apiKey", Provider: "OpenAI"}}, confirmation.References)
	})
	t.Run("a consumer added since the preparation", func(t *testing.T) {
		fixture := newOrgFixture(t)
		result := prepareModelOperation(t, fixture, nil)
		auth := map[string]any{"header": "x-api-key", "secretKeyRef": map[string]any{"name": "tenant-credential-claude", "key": "apiKey"}}
		_, err := fixture.dyn.Resource(egressGVR).Namespace(orgNS).Create(t.Context(), egressPolicy("anthropic", map[string]any{"origin": "https://api.anthropic.com", "authentication": auth}), metav1.CreateOptions{})
		require.NoError(t, err)
		confirmation := confirmOperation(t, fixture, result.CredentialOperation)
		require.Equal(t, []organizations.CredentialReference{
			{Kind: "EgressPolicy", Name: "anthropic", Key: "apiKey", Origins: []string{"https://api.anthropic.com"}},
			{Kind: "ModelConfig", Name: "claude", Key: "apiKey", Provider: "Anthropic"},
		}, confirmation.References)
	})
}

// A dry run asks admission about the Secret write, as the real call does: a
// Secret only git may change is refused before any operation exists.
func TestReplaceCredentialDryRunAsksAdmission(t *testing.T) {
	fixture := newOrgFixture(t, connector("gh", sharedSecret, "token"))
	seedSecret(t, fixture, sharedSecret, "token")
	fixture.dryRuns.Refuse = func(object *unstructured.Unstructured) error {
		if object.GetKind() == "Secret" {
			return apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, object.GetName(), errors.New("a Secret applied from an organization path in git is changed in git"))
		}
		return nil
	}
	text, failed := fixture.call(t, ToolReplaceOrganizationCredential, map[string]any{"namespace": orgNS, "secret": sharedSecret, "key": "token", "dryRun": true})
	require.True(t, failed, text)
	require.Contains(t, text, "changed in git")
}

// A save after every consumer of the key is gone is refused: there is no
// credential left to replace, and the Secret stays as it was.
func TestReplaceCredentialSaveRefusesWhenNothingUsesTheKeyAnyMore(t *testing.T) {
	fixture := newOrgFixture(t, sharedModel("token"))
	seedSecret(t, fixture, sharedSecret, "token")
	result := replaceCredential(t, fixture, nil)
	confirmation := confirmOperation(t, fixture, result.CredentialOperation)
	require.NoError(t, fixture.dyn.Resource(mcGVR).Namespace(orgNS).Delete(t.Context(), "claude", metav1.DeleteOptions{}))
	status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, "rotated-value"), nil)
	require.Equal(t, http.StatusConflict, status, answer)
	data, _, err := unstructured.NestedMap(fixture.get(t, secretGVR, orgNS, sharedSecret).Object, "data")
	require.NoError(t, err)
	require.Equal(t, map[string]any{"token": base64.StdEncoding.EncodeToString([]byte(liveValue))}, data)
}

// In commit mode a consumer written live after the preparation is refused at
// the save, as the preparation refuses one: no pull request is opened.
func TestReplaceCredentialCommitSaveRefusesALiveConsumerAddedSincePreparation(t *testing.T) {
	liveModel := sharedModel("token")
	liveModel.SetLabels(fromGitLabels("payments"))
	fixture, forge := commitFixture(t, map[string][]byte{paymentsPath + "/modelconfig-claude.yaml": gitManifest(t, sharedModel("token"))}, liveModel)
	result := replaceCredential(t, fixture, map[string]any{"mode": "commit"})
	confirmation := confirmOperation(t, fixture, result.CredentialOperation)
	_, err := fixture.dyn.Resource(serverGVR).Namespace(orgNS).Create(t.Context(), connector("gh", sharedSecret, "token"), metav1.CreateOptions{})
	require.NoError(t, err)
	status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, "rotated-value"), nil)
	require.Equal(t, http.StatusConflict, status, answer)
	require.Empty(t, forge.PullRequests())
}

// A Secret that does not exist yet is missing, not replaced: the person
// enters its first value, and the save creates it.
func TestReplaceCredentialOfAMissingSecretSaysMissing(t *testing.T) {
	fixture := newOrgFixture(t, connector("gh", sharedSecret, "token"))
	result := replaceCredential(t, fixture, nil)
	require.Len(t, result.Credentials, 1)
	require.Equal(t, organizations.CredentialMissing, result.Credentials[0].State)
	confirmation := confirmOperation(t, fixture, result.CredentialOperation)
	status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, "first-value"), nil)
	require.Equal(t, http.StatusOK, status, answer)
	data, _, err := unstructured.NestedMap(fixture.get(t, secretGVR, orgNS, sharedSecret).Object, "data")
	require.NoError(t, err)
	require.Equal(t, map[string]any{"token": base64.StdEncoding.EncodeToString([]byte("first-value"))}, data)
}
