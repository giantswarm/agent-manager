package organizations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/retry"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/kube"
)

// credentialConsumerHash binds confirmation to configuration and object identity.
// Controller status and its bookkeeping do not change the confirmed intent.
func credentialConsumerHash(object *unstructured.Unstructured) (string, error) {
	configuration := object.DeepCopy()
	delete(configuration.Object, "status")
	unstructured.RemoveNestedField(configuration.Object, fieldMetadata, "resourceVersion")
	unstructured.RemoveNestedField(configuration.Object, fieldMetadata, "managedFields")
	body, err := json.Marshal(configuration.Object)
	if err != nil {
		return "", errors.New("credential consumer configuration cannot be inspected")
	}
	return fileDigest(body), nil
}

func confirmedCredentialConsumer(operation *credentialOperation, planned write, current *unstructured.Unstructured) bool {
	index := slices.IndexFunc(operation.Writes, func(stored operationWrite) bool {
		return stored.Resource == planned.gvr && stored.Name == planned.name
	})
	if index < 0 {
		return false
	}
	if operation.Writes[index].ConsumerHash == "" {
		return current.GetResourceVersion() == operation.Writes[index].Version
	}
	hash, err := credentialConsumerHash(current)
	return err == nil && hash == operation.Writes[index].ConsumerHash
}

func doCredentialWrite(ctx context.Context, caller kube.Client, operation *credentialOperation, planned write, dryRun []string) (*unstructured.Unstructured, error) {
	if planned.kind == kindSecret || planned.op != opUpdate {
		return do(ctx, caller.Dynamic(), planned, dryRun)
	}
	var written *unstructured.Unstructured
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := caller.Dynamic().Resource(planned.gvr).Namespace(planned.ns).Get(ctx, planned.name, metav1.GetOptions{})
		if err != nil {
			return kubeErr(err, "read credential consumer", planned.reason)
		}
		if !confirmedCredentialConsumer(operation, planned, current) {
			return fmt.Errorf("%w: credential consumer changed; prepare it again", agents.ErrConflict)
		}
		updated := unstructuredOf(planned.obj).DeepCopy()
		updated.SetResourceVersion(current.GetResourceVersion())
		updated.SetManagedFields(current.GetManagedFields())
		if status, exists := current.Object["status"]; exists {
			updated.Object["status"] = status
		} else {
			delete(updated.Object, "status")
		}
		attempt := planned
		attempt.obj = updated.Object
		written, err = do(ctx, caller.Dynamic(), attempt, dryRun)
		return err
	})
	return written, err
}
