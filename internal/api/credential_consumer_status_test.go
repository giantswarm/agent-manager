package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/agent-manager/internal/organizations"
)

func prepareConnectorStatusOperation(t *testing.T, configure func(*organizations.Config)) (*orgFixture, organizations.ResourceResult) {
	t.Helper()
	consumer := connector("gh", "tenant-credential-gh", "token")
	consumer.SetUID(types.UID("confirmed-consumer"))
	consumer.SetResourceVersion("1")
	consumer.SetAnnotations(map[string]string{credentialOperationAnnotation: "previous-operation"})
	require.NoError(t, unstructured.SetNestedField(consumer.Object, "https://mcp.example.com/mcp", "spec", "url"))
	fixture := newOrgFixtureWith(t, configure, consumer)
	answer, failed := fixture.call(t, ToolSetOrganizationConnector, map[string]any{
		"namespace": orgNS, "name": "gh", "url": "https://updated.example.com/mcp",
	})
	require.False(t, failed, answer)
	var result organizations.ResourceResult
	require.NoError(t, json.Unmarshal([]byte(answer), &result))
	require.True(t, result.Pending)
	return fixture, result
}

func changeCredentialConsumer(t *testing.T, fixture *orgFixture, change func(*unstructured.Unstructured)) {
	t.Helper()
	consumer := fixture.get(t, connGVR, orgNS, "gh").DeepCopy()
	version, err := strconv.Atoi(consumer.GetResourceVersion())
	require.NoError(t, err)
	change(consumer)
	consumer.SetResourceVersion(strconv.Itoa(version + 1))
	require.NoError(t, fixture.dyn.Tracker().Update(connGVR, consumer, orgNS))
}

func TestCredentialSaveToleratesConsumerStatusUpdates(t *testing.T) {
	for _, timing := range []string{"before-save", "after-secret", "during-consumer-update"} {
		t.Run(timing, func(t *testing.T) {
			fixture, result := prepareConnectorStatusOperation(t, nil)
			confirmation := confirmOperation(t, fixture, result.CredentialOperation)
			change := func() {
				changeCredentialConsumer(t, fixture, func(consumer *unstructured.Unstructured) {
					require.NoError(t, unstructured.SetNestedField(consumer.Object, "discovered", "status", "phase"))
				})
			}
			switch timing {
			case "before-save":
				change()
			case "after-secret":
				fixture.dyn.PrependReactor("create", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
					if len(action.(k8stesting.CreateActionImpl).CreateOptions.DryRun) == 0 {
						change()
					}
					return false, nil, nil
				})
			case "during-consumer-update":
				changed := false
				fixture.dyn.PrependReactor("update", "remotemcpservers", func(action k8stesting.Action) (bool, runtime.Object, error) {
					if !changed && len(action.(k8stesting.UpdateActionImpl).UpdateOptions.DryRun) == 0 {
						changed = true
						change()
						return true, nil, apierrors.NewConflict(connGVR.GroupResource(), "gh", errors.New("status updated"))
					}
					return false, nil, nil
				})
			}
			status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, "synthetic-value"), nil)
			require.Equal(t, http.StatusOK, status, answer)
			consumer := fixture.get(t, connGVR, orgNS, "gh")
			address, _, err := unstructured.NestedString(consumer.Object, "spec", "url")
			require.NoError(t, err)
			require.Equal(t, "https://updated.example.com/mcp", address)
			phase, _, err := unstructured.NestedString(consumer.Object, "status", "phase")
			require.NoError(t, err)
			require.Equal(t, "discovered", phase)
			require.Equal(t, types.UID("confirmed-consumer"), consumer.GetUID())
		})
	}
}

func TestCredentialSaveRefusesConsumerConfigurationChanges(t *testing.T) {
	changes := map[string]func(*unstructured.Unstructured){
		"url": func(consumer *unstructured.Unstructured) {
			require.NoError(t, unstructured.SetNestedField(consumer.Object, "https://other.example.com/mcp", "spec", "url"))
		},
		"credential-binding": func(consumer *unstructured.Unstructured) {
			require.NoError(t, unstructured.SetNestedSlice(consumer.Object, []any{}, "spec", "headersFrom"))
		},
		"uid":    func(consumer *unstructured.Unstructured) { consumer.SetUID(types.UID("replacement-consumer")) },
		"labels": func(consumer *unstructured.Unstructured) { consumer.SetLabels(map[string]string{"owner": "other"}) },
	}
	for name, change := range changes {
		for _, timing := range []string{"before-save", "during-consumer-update"} {
			t.Run(name+"/"+timing, func(t *testing.T) {
				fixture, result := prepareConnectorStatusOperation(t, nil)
				confirmation := confirmOperation(t, fixture, result.CredentialOperation)
				if timing == "before-save" {
					changeCredentialConsumer(t, fixture, change)
				} else {
					changed := false
					fixture.dyn.PrependReactor("update", "remotemcpservers", func(action k8stesting.Action) (bool, runtime.Object, error) {
						if !changed && len(action.(k8stesting.UpdateActionImpl).UpdateOptions.DryRun) == 0 {
							changed = true
							changeCredentialConsumer(t, fixture, change)
							return true, nil, apierrors.NewConflict(connGVR.GroupResource(), "gh", errors.New("configuration updated"))
						}
						return false, nil, nil
					})
				}
				status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, "synthetic-value"), nil)
				require.Equal(t, http.StatusConflict, status, answer)
				consumer := fixture.get(t, connGVR, orgNS, "gh")
				require.Equal(t, "previous-operation", consumer.GetAnnotations()[credentialOperationAnnotation])
				if timing == "before-save" {
					require.Nil(t, fixture.get(t, secretGVR, orgNS, "tenant-credential-gh"))
				}
			})
		}
	}
}

func TestCredentialStatusRetryPreservesPartialSecretWrite(t *testing.T) {
	fixture, result := prepareConnectorStatusOperation(t, nil)
	confirmation := confirmOperation(t, fixture, result.CredentialOperation)
	refuse := true
	fixture.dyn.PrependReactor("update", "remotemcpservers", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if refuse && len(action.(k8stesting.UpdateActionImpl).UpdateOptions.DryRun) == 0 {
			refuse = false
			return true, nil, errors.New("synthetic admission refusal")
		}
		return false, nil, nil
	})
	status, _, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, "first-synthetic-value"), nil)
	require.GreaterOrEqual(t, status, 400)
	changeCredentialConsumer(t, fixture, func(consumer *unstructured.Unstructured) {
		require.NoError(t, unstructured.SetNestedField(consumer.Object, "discovered", "status", "phase"))
	})
	confirmation = confirmOperation(t, fixture, result.CredentialOperation)
	require.Equal(t, "uncertain", confirmation.State)
	status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, "second-synthetic-value"), nil)
	require.Equal(t, http.StatusOK, status, answer)
	secret := fixture.get(t, secretGVR, orgNS, "tenant-credential-gh")
	value, _, err := unstructured.NestedString(secret.Object, "data", "token")
	require.NoError(t, err)
	require.Equal(t, base64.StdEncoding.EncodeToString([]byte("first-synthetic-value")), value)
}

type refuseCredentialCompletion struct {
	typedcore.ConfigMapInterface
	refused bool
}

func (store *refuseCredentialCompletion) Update(ctx context.Context, item *corev1.ConfigMap, options metav1.UpdateOptions) (*corev1.ConfigMap, error) {
	var operation struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(item.Data["operation"]), &operation); err != nil {
		return nil, err
	}
	if !store.refused && operation.State == "complete" {
		store.refused = true
		return nil, errors.New("synthetic receipt storage failure")
	}
	return store.ConfigMapInterface.Update(ctx, item, options)
}

func TestCredentialRecoveryRechecksAppliedConsumerConfiguration(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(strconv.FormatBool(changed), func(t *testing.T) {
			store := &refuseCredentialCompletion{ConfigMapInterface: operationMaps(t)}
			fixture, result := prepareConnectorStatusOperation(t, func(configuration *organizations.Config) {
				configuration.CredentialOperations = organizations.NewCredentialOperations(store)
			})
			confirmation := confirmOperation(t, fixture, result.CredentialOperation)
			body := valuesBody(t, result, "synthetic-value")
			status, _, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, body, nil)
			require.GreaterOrEqual(t, status, 400)
			require.True(t, store.refused)
			consumer := fixture.get(t, connGVR, orgNS, "gh").DeepCopy()
			consumer.SetResourceVersion("3")
			require.NoError(t, unstructured.SetNestedField(consumer.Object, "ready", "status", "phase"))
			if changed {
				consumer.SetUID(types.UID("replacement-consumer"))
			}
			require.NoError(t, fixture.dyn.Tracker().Update(connGVR, consumer, orgNS))
			confirmation = confirmOperation(t, fixture, result.CredentialOperation)
			require.Equal(t, "saving", confirmation.State)
			fixture.dyn.ClearActions()
			status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, body, nil)
			if changed {
				require.Equal(t, http.StatusConflict, status, answer)
			} else {
				require.Equal(t, http.StatusOK, status, answer)
			}
			for _, action := range fixture.dyn.Actions() {
				require.NotEqual(t, "secrets", action.GetResource().Resource)
			}
		})
	}
}

func TestCredentialStatusChangeRetainsConfirmationAndPermissionChecks(t *testing.T) {
	fixture, result := prepareConnectorStatusOperation(t, nil)
	confirmation := confirmOperation(t, fixture, result.CredentialOperation)
	changeCredentialConsumer(t, fixture, func(consumer *unstructured.Unstructured) {
		require.NoError(t, unstructured.SetNestedField(consumer.Object, "ready", "status", "phase"))
	})
	body := valuesBody(t, result, "synthetic-value")
	status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, "", body, nil)
	require.Equal(t, http.StatusPreconditionRequired, status, answer)
	status, answer, _ = operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, "stale-version", body, nil)
	require.Equal(t, http.StatusPreconditionFailed, status, answer)
	fixture.deny = func(_, verb, resource, _ string) bool {
		return verb == "update" && resource == "remotemcpservers"
	}
	status, answer, _ = operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, body, nil)
	require.Equal(t, http.StatusForbidden, status, answer)
	require.Nil(t, fixture.get(t, secretGVR, orgNS, "tenant-credential-gh"))
}

func TestCredentialOperationsWithoutConsumerHashKeepVersionGuard(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(strconv.FormatBool(changed), func(t *testing.T) {
			store := operationMaps(t)
			fixture, result := prepareConnectorStatusOperation(t, func(configuration *organizations.Config) {
				configuration.CredentialOperations = organizations.NewCredentialOperations(store)
			})
			item, err := store.Get(t.Context(), "credential-"+result.CredentialOperation, metav1.GetOptions{})
			require.NoError(t, err)
			var operation map[string]any
			require.NoError(t, json.Unmarshal([]byte(item.Data["operation"]), &operation))
			for _, planned := range operation["writes"].([]any) {
				delete(planned.(map[string]any), "consumerHash")
			}
			body, err := json.Marshal(operation)
			require.NoError(t, err)
			item.Data["operation"] = string(body)
			_, err = store.Update(t.Context(), item, metav1.UpdateOptions{})
			require.NoError(t, err)
			confirmation := confirmOperation(t, fixture, result.CredentialOperation)
			if changed {
				changeCredentialConsumer(t, fixture, func(consumer *unstructured.Unstructured) {
					require.NoError(t, unstructured.SetNestedField(consumer.Object, "ready", "status", "phase"))
				})
			}
			status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, "synthetic-value"), nil)
			if changed {
				require.Equal(t, http.StatusConflict, status, answer)
				require.Nil(t, fixture.get(t, secretGVR, orgNS, "tenant-credential-gh"))
			} else {
				require.Equal(t, http.StatusOK, status, answer)
			}
		})
	}
}
