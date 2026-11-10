package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
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
	"sigs.k8s.io/yaml"

	"github.com/giantswarm/gitops-commit/commit"

	"github.com/giantswarm/agent-manager/internal/identity"
	"github.com/giantswarm/agent-manager/internal/organizations"
)

const credentialOperationAnnotation = "agent-manager.giantswarm.io/credential-operation"

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
			// An update without a resourceVersion is unconditional, as the API
			// server allows for these kinds; one with a stale version conflicts.
			current := stored.(*unstructured.Unstructured).GetResourceVersion()
			if object.GetResourceVersion() != "" && object.GetResourceVersion() != current {
				return true, nil, apierrors.NewConflict(action.GetResource().GroupResource(), object.GetName(), errors.New("stale version"))
			}
			version := 0
			if current != "" {
				version, err = strconv.Atoi(current)
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
	consumer := fixture.get(t, mcGVR, orgNS, "claude")
	require.NotNil(t, consumer)
	secret := fixture.get(t, secretGVR, orgNS, "tenant-credential-claude")
	require.NotNil(t, secret)
	require.Equal(t, result.ElicitationID, consumer.GetAnnotations()[credentialOperationAnnotation])
	require.Equal(t, result.ElicitationID, secret.GetAnnotations()[credentialOperationAnnotation])
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
		require.NotContains(t, string(raw), credentialOperationAnnotation)
		require.NotContains(t, string(raw), result.ElicitationID)
	}
	require.Nil(t, fixture.get(t, mcGVR, orgNS, "claude"))
	require.Nil(t, fixture.get(t, secretGVR, orgNS, "tenant-credential-claude"))
	status, answer, _ = operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, "another-value"), nil)
	require.Equal(t, http.StatusOK, status, answer)
	require.Len(t, forge.PullRequests(), 1)
}

func TestCredentialPreparationRefusesAnUnappliedGitConsumer(t *testing.T) {
	consumer := model("claude", true)
	require.NoError(t, unstructured.SetNestedField(consumer.Object, "existing-git-model", "spec", "model"))
	consumer.SetNamespace("")
	manifest, err := yaml.Marshal(consumer.Object)
	require.NoError(t, err)
	file := paymentsPath + "/modelconfig-claude.yaml"
	fixture, forge := commitFixture(t, map[string][]byte{
		file:                                 manifest,
		paymentsPath + "/kustomization.yaml": []byte("resources:\n  - modelconfig-claude.yaml\n"),
	})
	answer, failed := fixture.call(t, ToolSetOrganizationModel, map[string]any{
		"namespace": orgNS, "name": "claude", "provider": "Anthropic", "model": "replacement-model", "mode": "commit",
	})
	require.True(t, failed, answer)
	require.True(t, strings.HasPrefix(answer, "conflict:"), answer)
	require.Contains(t, answer, "already and not applied yet")
	require.Equal(t, manifest, forge.Files(mcRepo, "main")[file])
	require.Empty(t, forge.PullRequests())
}

func TestCredentialCommitRefusesAConsumerAddedAfterConfirmation(t *testing.T) {
	fixture, forge := commitFixture(t, nil)
	result := prepareModelOperation(t, fixture, map[string]any{"mode": "commit"})
	confirmation := confirmOperation(t, fixture, result.CredentialOperation)
	consumer := model("claude", true)
	require.NoError(t, unstructured.SetNestedField(consumer.Object, "intervening-git-model", "spec", "model"))
	consumer.SetNamespace("")
	manifest, err := yaml.Marshal(consumer.Object)
	require.NoError(t, err)
	file := paymentsPath + "/modelconfig-claude.yaml"
	require.NoError(t, forge.Commit(t.Context(), mcRepo, "main", "add model", map[string][]byte{file: manifest}))
	status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, "synthetic-value"), nil)
	require.Equal(t, http.StatusConflict, status, answer)
	require.Equal(t, manifest, forge.Files(mcRepo, "main")[file])
	require.Empty(t, forge.PullRequests())
}

func TestConnectorCredentialOperationDeduplicatesSharedHeaderReferences(t *testing.T) {
	for _, mode := range []string{"apply", "commit"} {
		for _, replace := range []bool{false, true} {
			t.Run(fmt.Sprintf("mode=%s/replace=%v", mode, replace), func(t *testing.T) {
				consumer := connector("gh", "tenant-credential-gh", "token")
				require.NoError(t, unstructured.SetNestedField(consumer.Object, "https://mcp.example.com/mcp", "spec", "url"))
				headers, _, err := unstructured.NestedSlice(consumer.Object, "spec", "headersFrom")
				require.NoError(t, err)
				headers = append(headers, map[string]any{"name": "X-API-Key", "valueFrom": map[string]any{"type": "Secret", "name": "tenant-credential-gh", "key": "token"}})
				require.NoError(t, unstructured.SetNestedSlice(consumer.Object, headers, "spec", "headersFrom"))
				var fixture *orgFixture
				var forge *commit.Fake
				if mode == "commit" {
					file := consumer.DeepCopy()
					file.SetNamespace("")
					manifest, err := yaml.Marshal(file.Object)
					require.NoError(t, err)
					consumer.SetLabels(fromGitLabels("payments"))
					fixture, forge = commitFixture(t, map[string][]byte{paymentsPath + "/remotemcpserver-gh.yaml": manifest}, consumer)
				} else {
					fixture = newOrgFixture(t, consumer)
				}
				if replace {
					seedSecret(t, fixture, "tenant-credential-gh", "token")
				}
				answer, failed := fixture.call(t, ToolSetOrganizationConnector, map[string]any{
					"namespace": orgNS, "name": "gh", "replaceCredential": replace, "mode": mode,
				})
				require.False(t, failed, answer)
				var result organizations.ResourceResult
				require.NoError(t, json.Unmarshal([]byte(answer), &result))
				require.True(t, result.Pending)
				require.Len(t, result.Credentials, 1)
				confirmation := confirmOperation(t, fixture, result.CredentialOperation)
				status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, "synthetic-value"), nil)
				require.Equal(t, http.StatusOK, status, answer)
				var saved *unstructured.Unstructured
				if mode == "commit" {
					require.Len(t, forge.PullRequests(), 1)
					saved = parseFile(t, forge.Files(mcRepo, forge.PullRequests()[0].Head)[paymentsPath+"/remotemcpserver-gh.yaml"])
				} else {
					saved = fixture.get(t, connGVR, orgNS, "gh")
				}
				savedHeaders, _, err := unstructured.NestedSlice(saved.Object, "spec", "headersFrom")
				require.NoError(t, err)
				require.Equal(t, headers, savedHeaders)
			})
		}
	}
}

func TestConnectorCredentialOperationRefusesDifferentKeysInOneSecret(t *testing.T) {
	consumer := connector("gh", "tenant-credential-gh", "token")
	require.NoError(t, unstructured.SetNestedField(consumer.Object, "https://mcp.example.com/mcp", "spec", "url"))
	headers, _, err := unstructured.NestedSlice(consumer.Object, "spec", "headersFrom")
	require.NoError(t, err)
	headers = append(headers, map[string]any{"name": "X-API-Key", "valueFrom": map[string]any{"type": "Secret", "name": "tenant-credential-gh", "key": "apiKey"}})
	require.NoError(t, unstructured.SetNestedSlice(consumer.Object, headers, "spec", "headersFrom"))
	fixture := newOrgFixture(t, consumer)
	answer, failed := fixture.call(t, ToolSetOrganizationConnector, map[string]any{"namespace": orgNS, "name": "gh"})
	require.True(t, failed, answer)
	require.Contains(t, answer, "each credential Secret must contain one credential key")
	require.Nil(t, fixture.get(t, secretGVR, orgNS, "tenant-credential-gh"))
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
		secret.SetAnnotations(map[string]string{credentialOperationAnnotation: result.ElicitationID, "other-writer": "changed"})
		err := fixture.dyn.Tracker().Update(secretGVR, secret, orgNS)
		require.NoError(t, err)
	}
	status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, "replacement-synthetic-value"), nil)
	require.Equal(t, http.StatusOK, status, answer)
	secret = fixture.get(t, secretGVR, orgNS, "tenant-credential-claude")
	value, _, err := unstructured.NestedString(secret.Object, "data", "apiKey")
	require.NoError(t, err)
	require.Equal(t, base64.StdEncoding.EncodeToString([]byte("first-synthetic-value")), value)
	require.NotNil(t, fixture.get(t, mcGVR, orgNS, "claude"))
	if interveningEdit {
		require.Equal(t, "changed", secret.GetAnnotations()["other-writer"], "the retry neither reads nor rewrites a Secret an earlier attempt wrote")
	}
}

// The retry after a partial apply leaves the Secret its first attempt wrote
// as it finds it: it never reads it, so an edit in between is not judged.
func TestCredentialApplyRetryLeavesAWrittenSecretAlone(t *testing.T) {
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
		if action.GetResource().Resource != "secrets" {
			continue
		}
		// The dry-run create that tells the Secret exists writes nothing.
		if creation, ok := action.(k8stesting.CreateActionImpl); ok && slices.Equal(creation.CreateOptions.DryRun, []string{metav1.DryRunAll}) {
			continue
		}
		require.NotContains(t, []string{"create", "update", "patch", "delete"}, action.GetVerb())
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

func TestCredentialOperationRequiresIfMatch(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			fixture := newOrgFixture(t)
			result := prepareModelOperation(t, fixture, nil)
			confirmOperation(t, fixture, result.CredentialOperation)
			status, answer, headers := operationHTTP(t, fixture, method, result.CredentialOperation, "", valuesBody(t, result, "sensitive-fixture"), nil)
			require.Equal(t, http.StatusPreconditionRequired, status, answer)
			require.Contains(t, answer, `"precondition_required"`)
			require.Equal(t, "no-store", headers.Get("Cache-Control"))
			require.NotContains(t, answer, "sensitive-fixture")
			require.Nil(t, fixture.get(t, secretGVR, orgNS, "tenant-credential-claude"))
		})
	}
}

// A Secret that appears between the preparation and the save is refused at
// the save's dry-run create: nothing overwrites it and no consumer is written.
func TestCredentialSaveRefusesASecretCreatedSinceThePreparation(t *testing.T) {
	fixture := newOrgFixture(t)
	result := prepareModelOperation(t, fixture, nil)
	confirmation := confirmOperation(t, fixture, result.CredentialOperation)
	seedSecret(t, fixture, "tenant-credential-claude", "apiKey")

	status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, "synthetic-value"), nil)
	require.Equal(t, http.StatusConflict, status, answer)
	secret := fixture.get(t, secretGVR, orgNS, "tenant-credential-claude")
	value, _, err := unstructured.NestedString(secret.Object, "data", "apiKey")
	require.NoError(t, err)
	require.Equal(t, base64.StdEncoding.EncodeToString([]byte(liveValue)), value)
	require.Nil(t, fixture.get(t, mcGVR, orgNS, "claude"))
}

// A Secret its ModelConfig reports without the key (kagent's ResolvedRefs
// False, APIKeySecretKeyNotFound, for the current generation) is missing: the
// person enters the value and the Secret is written whole. A condition of an
// older generation says nothing about the current one.
func TestCredentialKeyMissingFromAnExistingSecret(t *testing.T) {
	for _, current := range []bool{true, false} {
		t.Run(fmt.Sprintf("current generation %t", current), func(t *testing.T) {
			live := model("claude", true)
			live.SetGeneration(2)
			observed := int64(2)
			if !current {
				observed = 1
			}
			require.NoError(t, unstructured.SetNestedSlice(live.Object, []any{map[string]any{
				"type": "ResolvedRefs", "status": "False", "reason": "APIKeySecretKeyNotFound", "message": "secret tenant-credential-claude does not contain key apiKey",
				"observedGeneration": observed, "lastTransitionTime": "2026-11-10T00:00:00Z",
			}}, "status", "conditions"))
			fixture := newOrgFixture(t, live)
			seedSecret(t, fixture, "tenant-credential-claude", "other")

			text, failed := fixture.call(t, ToolSetOrganizationModel, map[string]any{"namespace": orgNS, "name": "claude", "provider": "Anthropic", "model": "claude-sonnet-4-6"})
			require.False(t, failed, text)
			var result organizations.ResourceResult
			require.NoError(t, json.Unmarshal([]byte(text), &result))
			if !current {
				require.Equal(t, organizations.CredentialPresent, result.Credentials[0].State, text)
				require.Empty(t, result.CredentialOperation, text)
				return
			}
			require.Equal(t, organizations.CredentialMissing, result.Credentials[0].State, text)
			require.NotEmpty(t, result.CredentialOperation, text)

			confirmation := confirmOperation(t, fixture, result.CredentialOperation)
			status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, "entered-value"), nil)
			require.Equal(t, http.StatusOK, status, answer)
			data, _, err := unstructured.NestedMap(fixture.get(t, secretGVR, orgNS, "tenant-credential-claude").Object, "data")
			require.NoError(t, err)
			require.Equal(t, map[string]any{"apiKey": base64.StdEncoding.EncodeToString([]byte("entered-value"))}, data)
		})
	}
}

// A replacement writes the credential Secret whole, as the caller, without
// reading it first: the one key the operation names, nothing else.
func TestCredentialReplacementWritesTheSecretWhole(t *testing.T) {
	fixture := newOrgFixture(t, model("claude", true))
	seedSecret(t, fixture, "tenant-credential-claude", "apiKey")
	stored := fixture.get(t, secretGVR, orgNS, "tenant-credential-claude")
	require.NoError(t, unstructured.SetNestedField(stored.Object, base64.StdEncoding.EncodeToString([]byte(liveValue)), "data", "other"))
	require.NoError(t, fixture.dyn.Tracker().Update(secretGVR, stored, orgNS))

	result := prepareModelOperation(t, fixture, map[string]any{"replaceCredential": true})
	confirmation := confirmOperation(t, fixture, result.CredentialOperation)
	status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, "replacement-value"), nil)
	require.Equal(t, http.StatusOK, status, answer)

	data, _, err := unstructured.NestedMap(fixture.get(t, secretGVR, orgNS, "tenant-credential-claude").Object, "data")
	require.NoError(t, err)
	require.Equal(t, map[string]any{"apiKey": base64.StdEncoding.EncodeToString([]byte("replacement-value"))}, data)
}
