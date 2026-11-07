package organizations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/giantswarm/agent-manager/internal/agents"
)

const credentialOperationLabel = "agent-manager.giantswarm.io/credential-operation"
const credentialOperationTTL = 15 * time.Minute

// CredentialOperations stores only operation metadata in a dedicated namespace.
// Resource versions serialize saves across replicas. Secret values, ciphertext
// and authorization tokens never enter this store.
type CredentialOperations struct{ maps typedcore.ConfigMapInterface }

func NewCredentialOperations(maps typedcore.ConfigMapInterface) *CredentialOperations {
	return &CredentialOperations{maps: maps}
}

func (s *CredentialOperations) read(ctx context.Context, id string) (*credentialOperation, error) {
	if s == nil {
		return nil, fmt.Errorf("%w: credential operations are not configured", agents.ErrUnsupported)
	}
	if len(id) != 48 {
		return nil, fmt.Errorf("%w: credential operation", agents.ErrNotFound)
	}
	for _, letter := range id {
		if (letter < 'a' || letter > 'f') && (letter < '0' || letter > '9') {
			return nil, fmt.Errorf("%w: credential operation", agents.ErrNotFound)
		}
	}
	item, err := s.maps.Get(ctx, "credential-"+id, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("%w: credential operation", agents.ErrNotFound)
	}
	if err != nil {
		return nil, errors.New("credential operation store is unavailable")
	}
	var operation credentialOperation
	if json.Unmarshal([]byte(item.Data["operation"]), &operation) != nil || operation.ID != id || item.Labels[credentialOperationLabel] != "true" {
		return nil, errors.New("credential operation metadata is invalid")
	}
	operation.Version = item.ResourceVersion
	return &operation, nil
}

func (s *CredentialOperations) put(ctx context.Context, operation *credentialOperation) error {
	version := operation.Version
	operation.Version = ""
	body, err := json.Marshal(operation)
	operation.Version = version
	if err != nil || len(body) > 128*1024 {
		return invalidf("credential operation metadata exceeds its size limit")
	}
	item := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "credential-" + operation.ID, ResourceVersion: version, Labels: map[string]string{credentialOperationLabel: "true"}}, Data: map[string]string{"operation": string(body)}}
	if version == "" {
		item, err = s.maps.Create(ctx, item, metav1.CreateOptions{})
	} else {
		item, err = s.maps.Update(ctx, item, metav1.UpdateOptions{})
	}
	if apierrors.IsAlreadyExists(err) || apierrors.IsConflict(err) {
		return fmt.Errorf("%w: %w", agents.ErrConflict, ErrCredentialOperationChanged)
	}
	if err != nil {
		return errors.New("credential operation store is unavailable")
	}
	operation.Version = item.ResourceVersion
	return nil
}

func (s *CredentialOperations) prepare(ctx context.Context, operation *credentialOperation) (*credentialOperation, error) {
	if s == nil {
		return nil, fmt.Errorf("%w: credential operations are not configured", agents.ErrUnsupported)
	}
	previous, err := s.read(ctx, operation.ID)
	if err == nil {
		if time.Now().Before(previous.ExpiresAt) {
			return previous, nil
		}
		operation.Version = previous.Version
	} else if !errors.Is(err, agents.ErrNotFound) {
		return nil, err
	}
	items, err := s.maps.List(ctx, metav1.ListOptions{LabelSelector: credentialOperationLabel + "=true", Limit: 257})
	if err != nil {
		return nil, errors.New("credential operation store is unavailable")
	}
	active := 0
	for _, item := range items.Items {
		if item.Name == "credential-"+operation.ID {
			active++
			continue
		}
		var existing credentialOperation
		if json.Unmarshal([]byte(item.Data["operation"]), &existing) != nil || time.Now().Before(existing.ExpiresAt) {
			active++
			continue
		}
		version := item.ResourceVersion
		_ = s.maps.Delete(ctx, item.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{ResourceVersion: &version}})
	}
	if active >= 256 || items.Continue != "" {
		return nil, fmt.Errorf("%w: credential operation capacity reached; retry later", agents.ErrConflict)
	}
	if err := s.put(ctx, operation); err != nil {
		if errors.Is(err, agents.ErrConflict) {
			return s.read(ctx, operation.ID)
		}
		return nil, err
	}
	return operation, nil
}

// Expire removes expired metadata under the server's lifecycle context.
func (s *CredentialOperations) Expire(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			items, err := s.maps.List(ctx, metav1.ListOptions{LabelSelector: credentialOperationLabel + "=true"})
			if err != nil {
				continue
			}
			for _, item := range items.Items {
				var operation credentialOperation
				if json.Unmarshal([]byte(item.Data["operation"]), &operation) != nil || time.Now().Before(operation.ExpiresAt) {
					continue
				}
				version := item.ResourceVersion
				_ = s.maps.Delete(ctx, item.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{ResourceVersion: &version}})
			}
		}
	}
}
