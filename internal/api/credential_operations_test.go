package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/gitops-commit/commit"

	"github.com/giantswarm/agent-manager/internal/identity"
	"github.com/giantswarm/agent-manager/internal/organizations"
)

func answerCredentialResourceVersions(t *testing.T, client *dynamicfake.FakeDynamicClient) {
	t.Helper()
	var lock sync.Mutex
	for _, verb := range []string{"create", "update"} {
		client.PrependReactor(verb, "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
			resource := action.GetResource().Resource
			if resource != "secrets" && resource != "modelconfigs" && resource != "remotemcpservers" && resource != "egresspolicies" {
				return false, nil, nil
			}
			var object *unstructured.Unstructured
			if action.GetVerb() == "create" {
				creation := action.(k8stesting.CreateActionImpl)
				if len(creation.CreateOptions.DryRun) > 0 {
					return false, nil, nil
				}
				object = creation.GetObject().(*unstructured.Unstructured).DeepCopy()
			} else {
				update := action.(k8stesting.UpdateActionImpl)
				if len(update.UpdateOptions.DryRun) > 0 {
					return false, nil, nil
				}
				object = update.GetObject().(*unstructured.Unstructured).DeepCopy()
			}
			lock.Lock()
			defer lock.Unlock()
			if action.GetVerb() == "create" {
				object.SetResourceVersion("1")
				return true, object, client.Tracker().Create(action.GetResource(), object, action.GetNamespace())
			}
			stored, err := client.Tracker().Get(action.GetResource(), action.GetNamespace(), object.GetName())
			if err != nil {
				return true, nil, err
			}
			if stored.(*unstructured.Unstructured).GetResourceVersion() != object.GetResourceVersion() {
				return true, nil, apierrors.NewConflict(action.GetResource().GroupResource(), object.GetName(), errors.New("stale version"))
			}
			version := 0
			if object.GetResourceVersion() != "" {
				version, err = strconv.Atoi(object.GetResourceVersion())
				require.NoError(t, err)
			}
			object.SetResourceVersion(strconv.Itoa(version + 1))
			return true, object, client.Tracker().Update(action.GetResource(), object, action.GetNamespace())
		})
	}
}

func operationMaps(t *testing.T) typedcore.ConfigMapInterface {
	t.Helper()
	client := kubefake.NewClientset()
	var lock sync.Mutex
	client.PrependReactor("create", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		lock.Lock()
		defer lock.Unlock()
		item := action.(k8stesting.CreateAction).GetObject().(*corev1.ConfigMap).DeepCopy()
		item.ResourceVersion = "1"
		err := client.Tracker().Create(corev1.SchemeGroupVersion.WithResource("configmaps"), item, action.GetNamespace())
		return true, item, err
	})
	client.PrependReactor("update", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		lock.Lock()
		defer lock.Unlock()
		item := action.(k8stesting.UpdateAction).GetObject().(*corev1.ConfigMap).DeepCopy()
		stored, err := client.Tracker().Get(corev1.SchemeGroupVersion.WithResource("configmaps"), action.GetNamespace(), item.Name)
		if err != nil {
			return true, nil, err
		}
		if stored.(*corev1.ConfigMap).ResourceVersion != item.ResourceVersion {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, item.Name, fmt.Errorf("stale version"))
		}
		version, err := strconv.Atoi(item.ResourceVersion)
		require.NoError(t, err)
		item.ResourceVersion = strconv.Itoa(version + 1)
		err = client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("configmaps"), item, action.GetNamespace())
		return true, item, err
	})
	return client.CoreV1().ConfigMaps("agent-manager")
}

func prepareModelOperation(t *testing.T, fixture *orgFixture, extra map[string]any) organizations.ResourceResult {
	t.Helper()
	arguments := map[string]any{"namespace": orgNS, "name": "claude", "provider": "Anthropic", "model": "claude-sonnet-4-6"}
	for key, value := range extra {
		arguments[key] = value
	}
	text, failed := fixture.call(t, ToolSetOrganizationModel, arguments)
	require.False(t, failed, text)
	var result organizations.ResourceResult
	require.NoError(t, json.Unmarshal([]byte(text), &result))
	require.NotEmpty(t, result.CredentialOperation, text)
	return result
}

func operationHTTP(t *testing.T, fixture *orgFixture, method, id, version, body string, person *identity.Identity) (int, string, http.Header) {
	t.Helper()
	service, _ := newService(t)
	mux := http.NewServeMux()
	NewREST(service, nil, WithOrganizations(fixture.orgs)).Register(mux)
	ctx := fixture.ctx
	if person != nil {
		ctx = identity.ContextWith(ctx, person)
	}
	request := httptest.NewRequestWithContext(ctx, method, Prefix+"/credential-operations/"+id, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if version != "" {
		request.Header.Set("If-Match", `"`+version+`"`)
	}
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	return recorder.Code, recorder.Body.String(), recorder.Header()
}

func confirmOperation(t *testing.T, fixture *orgFixture, id string) organizations.CredentialOperation {
	t.Helper()
	status, body, headers := operationHTTP(t, fixture, http.MethodGet, id, "", "", nil)
	require.Equal(t, http.StatusOK, status, body)
	require.Equal(t, "no-store", headers.Get("Cache-Control"))
	var result organizations.CredentialOperation
	require.NoError(t, json.Unmarshal([]byte(body), &result))
	require.Equal(t, `"`+result.Version+`"`, headers.Get("ETag"))
	return result
}

func valuesBody(t *testing.T, result organizations.ResourceResult, value string) string {
	t.Helper()
	values := []organizations.CredentialValue{}
	for _, credential := range result.Credentials {
		if credential.State == organizations.CredentialMissing || credential.State == organizations.CredentialReplace {
			values = append(values, organizations.CredentialValue{Secret: credential.Secret, Key: credential.Key, Value: value})
		}
	}
	raw, err := json.Marshal(map[string]any{"values": values})
	require.NoError(t, err)
	return string(raw)
}

func TestCredentialOperationCreatesAndRetriesWithoutAnotherWrite(t *testing.T) {
	fixture := newOrgFixture(t)
	result := prepareModelOperation(t, fixture, nil)
	require.Nil(t, fixture.get(t, mcGVR, orgNS, "claude"))
	require.Nil(t, fixture.get(t, secretGVR, orgNS, "tenant-credential-claude"))
	confirmation := confirmOperation(t, fixture, result.CredentialOperation)
	require.Equal(t, "pending", confirmation.State)
	require.Len(t, confirmation.References, 1)
	body := valuesBody(t, result, "synthetic-credential")
	status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, body, nil)
	require.Equal(t, http.StatusOK, status, answer)
	require.NotContains(t, answer, "synthetic-credential")
	require.NotNil(t, fixture.get(t, mcGVR, orgNS, "claude"))
	secret := fixture.get(t, secretGVR, orgNS, "tenant-credential-claude")
	require.NotNil(t, secret)
	before := len(fixture.dyn.Actions())
	status, answer, _ = operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, body, nil)
	require.Equal(t, http.StatusOK, status, answer)
	for _, action := range fixture.dyn.Actions()[before:] {
		require.NotContains(t, []string{"create", "update", "patch", "delete"}, action.GetVerb())
	}
	retry := prepareModelOperation(t, fixture, nil)
	require.Equal(t, result.CredentialOperation, retry.CredentialOperation)
	require.Equal(t, organizations.CredentialPresent, retry.Credentials[0].State)
}

func TestCredentialOperationOwnershipAndConfirmation(t *testing.T) {
	for _, scenario := range []string{"other subject", "other issuer", "delegated actor", "wrong version", "missing confirmation", "different Secret", "empty credential", "extra body", "trailing JSON", "cancelled", "stale consumer", "write denied", "multi-key Secret", "admission leaks value"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newOrgFixture(t)
			result := prepareModelOperation(t, fixture, nil)
			confirmation := confirmOperation(t, fixture, result.CredentialOperation)
			body := valuesBody(t, result, "sensitive-fixture")
			version := confirmation.Version
			var person *identity.Identity
			switch scenario {
			case "other subject":
				person = &identity.Identity{Issuer: "https://identity.example.com", Subject: "other"}
			case "other issuer":
				person = &identity.Identity{Issuer: "https://other.example.com", Subject: "a"}
			case "delegated actor":
				person = &identity.Identity{Issuer: "https://identity.example.com", Subject: "a", Delegated: true}
			case "wrong version":
				version = "999"
			case "missing confirmation":
				version = ""
			case "different Secret":
				body = strings.ReplaceAll(body, "tenant-credential-claude", "tenant-credential-other")
			case "empty credential":
				body = strings.ReplaceAll(body, "sensitive-fixture", "")
			case "extra body":
				body = strings.TrimSuffix(body, "}") + `,"extra":"sensitive-fixture"}`
			case "trailing JSON":
				body += ` {"value":"sensitive-fixture"}`
			case "cancelled":
				status, answer, _ := operationHTTP(t, fixture, http.MethodDelete, result.CredentialOperation, version, "", nil)
				require.Equal(t, http.StatusNoContent, status, answer)
			case "stale consumer":
				_, err := fixture.dyn.Resource(mcGVR).Namespace(orgNS).Create(t.Context(), model("claude", true), metav1.CreateOptions{})
				require.NoError(t, err)
			case "write denied":
				fixture.deny = func(_, _, resource, _ string) bool { return resource == "secrets" }
			case "multi-key Secret":
				_, err := fixture.dyn.Resource(secretGVR).Namespace(orgNS).Create(t.Context(), secretObject("tenant-credential-claude", map[string]any{"other": "b3RoZXI="}), metav1.CreateOptions{})
				require.NoError(t, err)
			case "admission leaks value":
				fixture.dryRuns.Refuse = func(object *unstructured.Unstructured) error {
					if object.GetKind() == "Secret" {
						return apierrors.NewInvalid(schema.GroupKind{Kind: "Secret"}, object.GetName(), field.ErrorList{field.Invalid(field.NewPath("data"), "c2Vuc2l0aXZlLWZpeHR1cmU=", "sensitive-fixture")})
					}
					return nil
				}
			}
			status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, version, body, person)
			require.GreaterOrEqual(t, status, 400, answer)
			require.NotContains(t, answer, "sensitive-fixture")
			require.NotContains(t, answer, "c2Vuc2l0aXZlLWZpeHR1cmU=")
		})
	}
}

func secretObject(name string, data map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": name, "namespace": orgNS}, "data": data}}
}

func TestCredentialOperationCommitsConsumerAndEncryptedSecretTogether(t *testing.T) {
	fixture, forge := commitFixture(t, nil)
	result := prepareModelOperation(t, fixture, map[string]any{"mode": "commit"})
	require.Empty(t, forge.PullRequests())
	confirmation := confirmOperation(t, fixture, result.CredentialOperation)
	status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, "synthetic-credential"), nil)
	require.Equal(t, http.StatusOK, status, answer)
	require.NotContains(t, answer, "synthetic-credential")
	require.Len(t, forge.PullRequests(), 1)
	files := forge.Files(mcRepo, forge.PullRequests()[0].Head)
	require.Contains(t, files, paymentsPath+"/modelconfig-claude.yaml")
	require.Contains(t, string(files[paymentsPath+"/tenant-credential-claude-secret.enc.yaml"]), "ENC[AES256_GCM,")
	for _, raw := range files {
		require.NotContains(t, string(raw), "synthetic-credential")
		require.NotContains(t, string(raw), "c3ludGhldGljLWNyZWRlbnRpYWw=")
	}
	require.Nil(t, fixture.get(t, mcGVR, orgNS, "claude"))
	require.Nil(t, fixture.get(t, secretGVR, orgNS, "tenant-credential-claude"))
	status, answer, _ = operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, "another-value"), nil)
	require.Equal(t, http.StatusOK, status, answer)
	require.Len(t, forge.PullRequests(), 1)
}

func TestCredentialOperationConcurrentSavesShareOneWrite(t *testing.T) {
	fixture := newOrgFixture(t)
	result := prepareModelOperation(t, fixture, nil)
	confirmation := confirmOperation(t, fixture, result.CredentialOperation)
	body := valuesBody(t, result, "synthetic-value")
	var writers sync.WaitGroup
	statuses := make(chan int, 2)
	for range 2 {
		writers.Go(func() {
			status, _, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, body, nil)
			statuses <- status
		})
	}
	writers.Wait()
	close(statuses)
	for status := range statuses {
		require.Contains(t, []int{http.StatusOK, http.StatusConflict, http.StatusPreconditionFailed}, status)
	}
	created := map[string]int{}
	for _, action := range fixture.dyn.Actions() {
		if action.GetVerb() == "create" {
			write := action.(k8stesting.CreateActionImpl)
			if len(write.CreateOptions.DryRun) == 0 {
				created[action.GetResource().Resource]++
			}
		}
	}
	require.Equal(t, 1, created["secrets"])
	require.Equal(t, 1, created["modelconfigs"])
}

func TestCredentialCommitRefusesChangedInputsAndUnsafeEncryption(t *testing.T) {
	for _, scenario := range []string{"changed policy", "unencrypted credential field", "encrypted identity", "other Git consumer", "JSON Git consumer", "multiple documents", "list document", "Git target changed"} {
		t.Run(scenario, func(t *testing.T) {
			base := map[string][]byte{}
			if scenario == "unencrypted credential field" {
				base[".sops.yaml"] = bytes.ReplaceAll(sopsCovering(t, `^agent-platform/organizations/.*\.enc\.yaml$`), []byte("^(data|stringData)$"), []byte("^metadata$"))
			}
			if scenario == "encrypted identity" {
				base[".sops.yaml"] = bytes.ReplaceAll(sopsCovering(t, `^agent-platform/organizations/.*\.enc\.yaml$`), []byte("    encrypted_regex: ^(data|stringData)$\n"), nil)
			}
			fixture, forge := commitFixture(t, base)
			result := prepareModelOperation(t, fixture, map[string]any{"mode": "commit"})
			confirmation := confirmOperation(t, fixture, result.CredentialOperation)
			if scenario == "changed policy" {
				require.NoError(t, forge.Commit(t.Context(), mcRepo, "main", "change policy", map[string][]byte{".sops.yaml": sopsCovering(t, `^changed/.*`)}))
			}
			if scenario == "other Git consumer" {
				require.NoError(t, forge.Commit(t.Context(), mcRepo, "main", "add another consumer", map[string][]byte{paymentsPath + "/other.yaml": []byte("apiVersion: api.kagent.dev/v1alpha3\nkind: ModelConfig\nmetadata:\n  name: other\nspec:\n  apiKeySecret: tenant-credential-claude\n  apiKeySecretKey: apiKey\n")}))
			}
			if scenario == "JSON Git consumer" {
				consumer := model("other", false)
				require.NoError(t, unstructured.SetNestedField(consumer.Object, "tenant-credential-claude", "spec", "apiKeySecret"))
				encoded, err := json.Marshal(consumer.Object)
				require.NoError(t, err)
				require.NoError(t, forge.Commit(t.Context(), mcRepo, "main", "add JSON consumer", map[string][]byte{paymentsPath + "/other.json": encoded}))
			}
			if scenario == "multiple documents" || scenario == "list document" {
				raw := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: other\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: another\n"
				if scenario == "list document" {
					raw = "apiVersion: v1\nkind: List\nitems: []\n"
				}
				require.NoError(t, forge.Commit(t.Context(), mcRepo, "main", "add unsupported consumer", map[string][]byte{paymentsPath + "/other.yml": []byte(raw)}))
			}
			if scenario == "Git target changed" {
				fixture.cfg.Commit.Objects.Repository.Name = "other"
			}
			status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, "synthetic-sensitive-value"), nil)
			require.GreaterOrEqual(t, status, 400, answer)
			require.NotContains(t, answer, "synthetic-sensitive-value")
			require.Empty(t, forge.PullRequests())
		})
	}
}

func TestCredentialCommitReplacesAnEncryptedSecret(t *testing.T) {
	fixture, forge := commitFixture(t, nil)
	first := prepareModelOperation(t, fixture, map[string]any{"mode": "commit"})
	confirmation := confirmOperation(t, fixture, first.CredentialOperation)
	status, answer, _ := operationHTTP(t, fixture, http.MethodPost, first.CredentialOperation, confirmation.Version, valuesBody(t, first, "first-synthetic-value"), nil)
	require.Equal(t, http.StatusOK, status, answer)
	base := forge.Files(mcRepo, forge.PullRequests()[0].Head)
	consumer := model("claude", true)
	consumer.SetLabels(fromGitLabels("payments"))
	fixture, forge = commitFixture(t, base, consumer)
	seedSecret(t, fixture, "tenant-credential-claude", "apiKey")
	replacement := prepareModelOperation(t, fixture, map[string]any{"mode": "commit", "replaceCredential": true})
	confirmation = confirmOperation(t, fixture, replacement.CredentialOperation)
	status, answer, _ = operationHTTP(t, fixture, http.MethodPost, replacement.CredentialOperation, confirmation.Version, valuesBody(t, replacement, "replacement-synthetic-value"), nil)
	require.Equal(t, http.StatusOK, status, answer)
	require.Len(t, forge.PullRequests(), 1)
	file := paymentsPath + "/tenant-credential-claude-secret.enc.yaml"
	updated := forge.Files(mcRepo, forge.PullRequests()[0].Head)[file]
	require.NotEqual(t, base[file], updated)
	require.Contains(t, string(updated), "ENC[AES256_GCM,")
	require.NotContains(t, string(updated), "replacement-synthetic-value")
}

func testCredentialPartialApply(t *testing.T, interveningEdit bool) {
	t.Helper()
	fixture := newOrgFixture(t)
	result := prepareModelOperation(t, fixture, nil)
	confirmation := confirmOperation(t, fixture, result.CredentialOperation)
	fail := true
	fixture.dyn.PrependReactor("create", "modelconfigs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if fail && len(action.(k8stesting.CreateActionImpl).CreateOptions.DryRun) == 0 {
			fail = false
			return true, nil, errors.New("synthetic admission refusal")
		}
		return false, nil, nil
	})
	status, _, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, "first-synthetic-value"), nil)
	require.GreaterOrEqual(t, status, 400)
	confirmation = confirmOperation(t, fixture, result.CredentialOperation)
	require.Equal(t, "uncertain", confirmation.State)
	secret := fixture.get(t, secretGVR, orgNS, "tenant-credential-claude")
	if interveningEdit {
		secret.SetAnnotations(map[string]string{"agent-manager.giantswarm.io/credential-operation": result.ElicitationID, "other-writer": "changed"})
		_, err := fixture.dyn.Resource(secretGVR).Namespace(orgNS).Update(t.Context(), secret, metav1.UpdateOptions{})
		require.NoError(t, err)
		status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, "replacement-synthetic-value"), nil)
		require.Equal(t, http.StatusConflict, status, answer)
		require.Nil(t, fixture.get(t, mcGVR, orgNS, "claude"))
		return
	}
	status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, "replacement-synthetic-value"), nil)
	require.Equal(t, http.StatusOK, status, answer)
	secret = fixture.get(t, secretGVR, orgNS, "tenant-credential-claude")
	value, _, err := unstructured.NestedString(secret.Object, "data", "apiKey")
	require.NoError(t, err)
	require.Equal(t, base64.StdEncoding.EncodeToString([]byte("first-synthetic-value")), value)
	require.NotNil(t, fixture.get(t, mcGVR, orgNS, "claude"))
}

func TestCredentialApplyRefusesAnInterveningSecretEdit(t *testing.T) {
	testCredentialPartialApply(t, true)
}

func TestCredentialApplyRetryPreservesTheFirstSecretValue(t *testing.T) {
	testCredentialPartialApply(t, false)
}

func TestCredentialCommitRecoversAnUncertainPullRequest(t *testing.T) {
	fixture, forge := commitFixture(t, nil)
	result := prepareModelOperation(t, fixture, map[string]any{"mode": "commit"})
	confirmation := confirmOperation(t, fixture, result.CredentialOperation)
	forge.Fail[commit.OpOpenPullRequest] = errors.New("the provider did not answer")
	status, _, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, "synthetic-value"), nil)
	require.GreaterOrEqual(t, status, 400)
	delete(forge.Fail, commit.OpOpenPullRequest)
	confirmation = confirmOperation(t, fixture, result.CredentialOperation)
	require.Equal(t, "uncertain", confirmation.State)
	status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, `{"values":[]}`, nil)
	require.Equal(t, http.StatusOK, status, answer)
	require.Len(t, forge.PullRequests(), 1)
	confirmation = confirmOperation(t, fixture, result.CredentialOperation)
	require.Equal(t, "complete", confirmation.State)
	require.Equal(t, organizations.CredentialCommitted, confirmation.Result.Credentials[0].State)
}

func TestCredentialReplacementKeepsOrdinaryEditsSeparate(t *testing.T) {
	fixture := newOrgFixture(t, model("claude", true))
	seedSecret(t, fixture, "tenant-credential-claude", "apiKey")
	before := len(fixture.dyn.Actions())
	text, failed := fixture.call(t, ToolSetOrganizationModel, map[string]any{"namespace": orgNS, "name": "claude", "model": "edited"})
	require.False(t, failed, text)
	require.NotContains(t, text, "credentialOperation")
	for _, action := range fixture.dyn.Actions()[before:] {
		if action.GetResource().Resource == "secrets" {
			require.NotContains(t, []string{"create", "update", "patch", "delete"}, action.GetVerb())
		}
	}
	result := prepareModelOperation(t, fixture, map[string]any{"replaceCredential": true, "requestId": "rotation"})
	confirmation := confirmOperation(t, fixture, result.CredentialOperation)
	status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, "replacement-value"), nil)
	require.Equal(t, http.StatusOK, status, answer)
	secret := fixture.get(t, secretGVR, orgNS, "tenant-credential-claude")
	data, _, err := unstructured.NestedString(secret.Object, "data", "apiKey")
	require.NoError(t, err)
	require.Equal(t, base64.StdEncoding.EncodeToString([]byte("replacement-value")), data)
}
