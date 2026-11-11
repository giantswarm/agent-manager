package organizations

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"path"
	"reflect"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/kube"
)

// CredentialValue is accepted only by the HTTPS save operation.
type CredentialValue struct {
	Secret string `json:"secret"`
	Key    string `json:"key"`
	Value  string `json:"value"`
}

// credentialOperationAnnotation marks an object written directly to the API
// server by one credential operation, so a retry after an uncertain write can
// tell its own write from someone else's. Commit mode never sets it: Git
// carries only the desired state, and recovery there checks the branch.
const credentialOperationAnnotation = "agent-manager.giantswarm.io/credential-operation"

var secretGVR = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}

func credentialSaveError(err error) error {
	if errors.Is(err, ErrCredentialOperationChanged) {
		return ErrCredentialOperationChanged
	}
	for _, sentinel := range []error{agents.ErrUnauthenticated, agents.ErrAuthRequired, agents.ErrForbidden, agents.ErrNotFound, agents.ErrConflict, agents.ErrInvalid, agents.ErrUnsupported, agents.ErrGitOpsOwned} {
		if errors.Is(err, sentinel) {
			return fmt.Errorf("%w: credential save was refused; refresh the operation before retrying", sentinel)
		}
	}
	return errors.New("credential save failed; inspect the operation status before retrying")
}

func (s *Service) SaveCredentialOperation(ctx context.Context, id, version string, values []CredentialValue) (*ResourceResult, error) {
	result, err := s.saveCredentialOperation(ctx, id, version, values)
	if err != nil {
		return nil, credentialSaveError(err)
	}
	return result, nil
}

func (s *Service) saveCredentialOperation(ctx context.Context, id, version string, values []CredentialValue) (*ResourceResult, error) {
	operation, err := s.credentialOperation(ctx, id)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(90 * time.Second)
	if operation.ExpiresAt.Before(deadline) {
		deadline = operation.ExpiresAt
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if operation.State == operationComplete {
		return &operation.Result, nil
	}
	if operation.Mode == agents.ModeCommit {
		if s.cfg.Commit == nil || operation.GitTarget == nil || s.cfg.Commit.Objects.Repository.String() != operation.GitTarget.Repository || s.cfg.Commit.Objects.Branch != operation.GitTarget.Base || path.Join(s.cfg.Commit.ContentPrefix, operation.Namespace.Parent, operation.Namespace.Organization) != operation.GitTarget.Directory {
			return nil, fmt.Errorf("%w: the credential Git target changed", agents.ErrConflict)
		}
	}
	if operation.State == operationSaving {
		if operation.Mode == agents.ModeCommit {
			return s.recoverCredentialCommit(ctx, operation)
		}
		return s.recoverAppliedCredential(ctx, operation)
	}
	if (operation.State != operationPending && operation.State != operationUncertain) || version == "" || version != operation.Version {
		return nil, fmt.Errorf("%w: %w", agents.ErrConflict, ErrCredentialOperationChanged)
	}
	if operation.State == operationUncertain && operation.Mode == agents.ModeCommit {
		return s.recoverCredentialCommit(ctx, operation)
	}
	if len(values) != len(operation.Credentials) {
		return nil, invalidf("supply exactly the credentials in the confirmation")
	}
	caller, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	writes, err := remainingOperationWrites(ctx, caller, operation)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, credential := range operation.Credentials {
		index := slices.IndexFunc(values, func(value CredentialValue) bool {
			return value.Secret == credential.Secret && value.Key == credential.Key
		})
		if index < 0 || values[index].Value == "" || seen[credential.Secret] {
			return nil, invalidf("each credential needs one nonempty value")
		}
		seen[credential.Secret] = true
		references, live, err := s.references(ctx, caller, operation.Namespace.Name, credential.Secret)
		if err != nil {
			return nil, err
		}
		if operation.Result.Kind == KindCredential && !slices.ContainsFunc(references, func(reference CredentialReference) bool { return reference.Key == credential.Key }) {
			return nil, fmt.Errorf("%w: nothing in %s uses key %s of credential Secret %s any more, so there is no credential to replace", agents.ErrConflict, operation.Namespace.Name, credential.Key, credential.Secret)
		}
		for _, reference := range references {
			if !writesFor(operation.Result, reference) {
				return nil, fmt.Errorf("%w: another object references this Secret", agents.ErrConflict)
			}
			if operation.Result.Kind == KindCredential {
				if reference.Key != credential.Key {
					return nil, otherKey(reference, credential.Secret, credential.Key)
				}
				// The Secret lands where its consumers live, as at the
				// preparation: a consumer written live since then is refused
				// in commit mode, one Flux applies in apply mode.
				if err := (&contentWriter{mode: operation.Mode}).owned(live[reference.Kind+"/"+reference.Name], reference.Kind); err != nil {
					return nil, err
				}
			}
			if operation.Mode == agents.ModeApply && reference.FromGit {
				return nil, fmt.Errorf("%w: a credential whose consumer Flux applies must be changed in Git", agents.ErrGitOpsOwned)
			}
		}
		if operation.Result.Kind == KindCredential && !sameConsumers(references, operation.Consumers) {
			return nil, errConsumersChanged(operation.Namespace.Name, credential.Secret)
		}
		if credential.Exists && !credential.Replace {
			return nil, fmt.Errorf("%w: credential replacement requires explicit confirmation", agents.ErrConflict)
		}
		if operation.State == operationUncertain && operation.AppliedVersions[kindSecret+"/"+credential.Secret] != "" {
			continue
		}
		action := opCreate
		if credential.Exists {
			action = opUpdate
		} else if operation.State == operationUncertain {
			// An earlier attempt may have created the Secret before its write
			// was recorded; the retry carries the same value, so it writes it
			// whole either way.
			exists, err := secretExists(ctx, caller, operation.Namespace.Name, credential.Secret)
			if err != nil {
				return nil, err
			}
			if exists {
				action = opUpdate
			}
		}
		data := map[string]any{credential.Key: base64.StdEncoding.EncodeToString([]byte(values[index].Value))}
		writes = append([]write{credentialSecretWrite(operation.Namespace.Name, credential.Secret, action, data)}, writes...)
	}
	if err := apply(ctx, caller.Dynamic(), writes, true); err != nil {
		return nil, err
	}
	writer, err := s.contentWriter(ctx, operation.Namespace, operation.Mode)
	if err != nil {
		return nil, err
	}
	operation.State = operationSaving
	if err := s.cfg.CredentialOperations.put(ctx, operation); err != nil {
		return nil, err
	}
	if operation.Mode == agents.ModeCommit {
		var committed *CommitResult
		committed, err = s.commitCredentialOperation(ctx, operation, writer, writes)
		if err == nil {
			operation.Result.Commit = committed
		}
	} else {
		err = s.applyCredentialOperation(ctx, caller, operation, writes)
	}
	if err != nil {
		operation.State = operationUncertain
		_ = s.cfg.CredentialOperations.put(ctx, operation)
		return nil, err
	}
	completeCredentialOperation(operation)
	if err := s.cfg.CredentialOperations.put(ctx, operation); err != nil {
		return nil, err
	}
	return &operation.Result, nil
}

func operationVersionMatches(operation *credentialOperation, object *unstructured.Unstructured) bool {
	version := operation.AppliedVersions[object.GetKind()+"/"+object.GetName()]
	return version != "" && version == object.GetResourceVersion()
}

func (s *Service) applyCredentialOperation(ctx context.Context, caller kube.Client, operation *credentialOperation, writes []write) error {
	for _, planned := range writes {
		marked := unstructuredOf(planned.obj).DeepCopy()
		setAnnotation(marked, credentialOperationAnnotation, operation.ElicitationID)
		planned.obj = marked.Object
		object, err := do(ctx, caller.Dynamic(), planned, nil)
		if err != nil {
			return err
		}
		if operation.AppliedVersions == nil {
			operation.AppliedVersions = map[string]string{}
		}
		operation.AppliedVersions[planned.kind+"/"+planned.name] = object.GetResourceVersion()
		if err := s.cfg.CredentialOperations.put(ctx, operation); err != nil {
			return err
		}
	}
	return nil
}

func remainingOperationWrites(ctx context.Context, caller kube.Client, operation *credentialOperation) ([]write, error) {
	writes := []write{}
	for _, planned := range operationWrites(operation) {
		current, err := caller.Dynamic().Resource(planned.gvr).Namespace(planned.ns).Get(ctx, planned.name, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, kubeErr(err, "read credential consumer", wayOrganizationAdmin(planned.ns))
		}
		if current != nil && operation.State == operationUncertain && current.GetAnnotations()[credentialOperationAnnotation] == operation.ElicitationID {
			if !operationVersionMatches(operation, current) || !operationObjectApplied(current, planned.obj, operation.ElicitationID) {
				return nil, fmt.Errorf("%w: a partially written consumer changed or lacks a confirmed version", agents.ErrConflict)
			}
			continue
		}
		expected := unstructuredOf(planned.obj)
		if (planned.op == opCreate && !apierrors.IsNotFound(err)) || (planned.op != opCreate && (current == nil || current.GetResourceVersion() != expected.GetResourceVersion())) {
			return nil, fmt.Errorf("%w: credential consumer changed; prepare it again", agents.ErrConflict)
		}
		planned.obj = expected.DeepCopy().Object
		writes = append(writes, planned)
	}
	return writes, nil
}

func operationObjectApplied(current *unstructured.Unstructured, intended map[string]any, id string) bool {
	if current.GetAnnotations()[credentialOperationAnnotation] != id {
		return false
	}
	for key, value := range intended {
		if key == fieldMetadata {
			continue
		}
		if !operationValueMatches(value, current.Object[key]) {
			return false
		}
	}
	for key, value := range unstructuredOf(intended).GetAnnotations() {
		if current.GetAnnotations()[key] != value {
			return false
		}
	}
	return true
}

func operationValueMatches(expected, actual any) bool {
	switch value := expected.(type) {
	case map[string]any:
		current, ok := actual.(map[string]any)
		if !ok {
			return false
		}
		for key, child := range value {
			found, exists := current[key]
			if !exists || !operationValueMatches(child, found) {
				return false
			}
		}
		return true
	case []any:
		current, ok := actual.([]any)
		if !ok || len(value) != len(current) {
			return false
		}
		for index, child := range value {
			if !operationValueMatches(child, current[index]) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(expected, actual)
	}
}

func (s *Service) recoverAppliedCredential(ctx context.Context, operation *credentialOperation) (*ResourceResult, error) {
	caller, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	for _, planned := range operationWrites(operation) {
		current, err := caller.Dynamic().Resource(planned.gvr).Namespace(planned.ns).Get(ctx, planned.name, metav1.GetOptions{})
		if err != nil || !operationVersionMatches(operation, current) || !operationObjectApplied(current, planned.obj, operation.ElicitationID) {
			return nil, fmt.Errorf("%w: credential save is in progress; retry after it finishes", agents.ErrConflict)
		}
	}
	for _, credential := range operation.Credentials {
		if operation.AppliedVersions[kindSecret+"/"+credential.Secret] == "" {
			return nil, fmt.Errorf("%w: credential save is in progress; retry after it finishes", agents.ErrConflict)
		}
	}
	completeCredentialOperation(operation)
	if err := s.cfg.CredentialOperations.put(ctx, operation); err != nil {
		return nil, err
	}
	return &operation.Result, nil
}

func completeCredentialOperation(operation *credentialOperation) {
	operation.State = operationComplete
	operation.Result.Pending = false
	for index := range operation.Result.Credentials {
		operation.Result.Credentials[index].State = CredentialPresent
		if operation.Mode == agents.ModeCommit {
			operation.Result.Credentials[index].State = CredentialCommitted
		}
		operation.Result.Credentials[index].FormURL = ""
		operation.Result.Credentials[index].Next = ""
	}
}
