package organizations

import (
	"context"
	"encoding/json"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

// AwaitCredentialCompletion observes shared metadata without retaining the
// initiating request's identity token or broadcasting to other MCP sessions.
func (s *Service) AwaitCredentialCompletion(ctx context.Context, id, elicitationID string) bool {
	store := s.cfg.CredentialOperations
	for ctx.Err() == nil {
		operation, err := store.read(ctx, id)
		if err != nil || operation.ElicitationID != elicitationID || time.Now().After(operation.ExpiresAt) {
			return false
		}
		if operation.State == operationComplete {
			return true
		}
		if operation.State == operationCancelled {
			return false
		}
		stream, err := store.maps.Watch(ctx, metav1.ListOptions{FieldSelector: "metadata.name=credential-" + id, ResourceVersion: operation.Version, TimeoutSeconds: new(int64(60))})
		if err != nil {
			return false
		}
		completed, terminal := awaitCredentialWatch(ctx, stream, elicitationID)
		stream.Stop()
		if terminal {
			return completed
		}
	}
	return false
}

func awaitCredentialWatch(ctx context.Context, stream watch.Interface, elicitationID string) (bool, bool) {
	for {
		select {
		case <-ctx.Done():
			return false, true
		case event, open := <-stream.ResultChan():
			if !open {
				return false, false
			}
			if event.Type == watch.Deleted {
				return false, true
			}
			if event.Type == watch.Error {
				return false, false
			}
			item, ok := event.Object.(*corev1.ConfigMap)
			if !ok {
				continue
			}
			var operation credentialOperation
			if json.Unmarshal([]byte(item.Data["operation"]), &operation) != nil || operation.ElicitationID != elicitationID {
				return false, true
			}
			if operation.State == operationComplete {
				return true, true
			}
			if operation.State == operationCancelled {
				return false, true
			}
		}
	}
}
