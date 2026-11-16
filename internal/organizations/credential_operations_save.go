package organizations

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/identity"
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
	if errors.Is(err, ErrCredentialOperationOtherLogin) {
		return ErrCredentialOperationOtherLogin
	}
	// The consumers changed since the preparation: a stable sentinel the
	// client switches on, naming nothing the confirmation did not show.
	if errors.As(err, new(consumersDiffer)) {
		return ErrCredentialConsumersChanged
	}
	// The object the operation creates exists by then: the name the person
	// confirmed is taken. The message names nothing the confirmation did not.
	if errors.As(err, new(nameTaken)) {
		return fmt.Errorf("%w: the object this operation creates exists now, live or in git; prepare it again", ErrAlreadyExists)
	}
	// The Git files or the encryption policy changed since the preparation,
	// before any branch: the operation stays pending, and only a fresh
	// preparation helps.
	if errors.As(err, new(gitInputsChanged)) {
		return fmt.Errorf("%w: the Git files or the encryption policy changed since the preparation; nothing was written; prepare the credential operation again", agents.ErrConflict)
	}
	for _, sentinel := range []error{agents.ErrUnauthenticated, agents.ErrAuthRequired, agents.ErrForbidden, agents.ErrNotFound, agents.ErrConflict, agents.ErrInvalid, agents.ErrUnsupported, agents.ErrGitOpsOwned} {
		if errors.Is(err, sentinel) {
			return fmt.Errorf("%w: credential save was refused; refresh the operation before retrying", sentinel)
		}
	}
	return errCredentialSaveFailed
}

// errCredentialSaveFailed answers a save that failed for a reason the caller
// cannot act on: agent-manager's log names the step and the cause.
var errCredentialSaveFailed = errors.New("credential save failed; inspect the operation status before retrying")

// credentialWriteBudget is how long the writes of a commit-mode save may take
// once they start. They no longer stop with the caller's request, so this is
// what ends them.
const credentialWriteBudget = 2 * time.Minute

// credentialRecordBudget is how long recording a commit-mode save's outcome
// may take, after its writes, whatever the caller does.
const credentialRecordBudget = 15 * time.Second

// saveStep names the step of a save that failed, for the log only: the
// answer to the caller stays what credentialSaveError makes of the cause.
type saveStep struct {
	step string
	err  error
}

func (e saveStep) Error() string { return e.err.Error() }
func (e saveStep) Unwrap() error { return e.err }

// atStep marks err with step, unless a deeper step already marked it.
func atStep(step string, err error) error {
	if err == nil || errors.As(err, new(saveStep)) {
		return err
	}
	return saveStep{step: step, err: err}
}

// credentialSaveCause is the cause of a failed save as the log may hold it.
// A Kubernetes API answer is logged by its reason and code only: the API
// server, a webhook or an admission policy may quote the object it refused,
// in any escaping, and a credential Secret carries the value. Any other
// cause is logged with every submitted value removed, as typed, quoted and
// base64 encoded.
func credentialSaveCause(err error, values []CredentialValue) string {
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		return fmt.Sprintf("the Kubernetes API answered %s (%d)", apierrors.ReasonForError(err), status.Status().Code)
	}
	message := err.Error()
	for _, value := range values {
		if value.Value == "" {
			continue
		}
		raw := []byte(value.Value)
		quoted := strconv.Quote(value.Value)
		escaped, _ := json.Marshal(value.Value)
		for _, form := range []string{
			value.Value,
			quoted[1 : len(quoted)-1],
			string(escaped[1 : len(escaped)-1]),
			base64.StdEncoding.EncodeToString(raw),
			base64.RawStdEncoding.EncodeToString(raw),
			base64.URLEncoding.EncodeToString(raw),
			base64.RawURLEncoding.EncodeToString(raw),
		} {
			message = strings.ReplaceAll(message, form, "[redacted]")
		}
	}
	return message
}

// detached is ctx without its cancellation and deadline, ended by budget
// instead: work that must not stop with the caller's request.
func detached(ctx context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), budget)
}

func (s *Service) SaveCredentialOperation(ctx context.Context, id, version string, values []CredentialValue) (*ResourceResult, error) {
	result, err := s.saveCredentialOperation(ctx, id, version, values)
	if err != nil {
		answer := credentialSaveError(err)
		step := "save"
		var failed saveStep
		if errors.As(err, &failed) {
			step = failed.step
		}
		// A refusal is the caller's to act on; a failure is the operator's.
		level := slog.LevelWarn
		if errors.Is(answer, errCredentialSaveFailed) {
			level = slog.LevelError
		}
		s.log.Log(ctx, level, "credential save failed", identity.LogAttr(ctx), "operation", id, "step", step, "cause", credentialSaveCause(err, values))
		return nil, answer
	}
	return result, nil
}

func (s *Service) saveCredentialOperation(ctx context.Context, id, version string, values []CredentialValue) (_ *ResourceResult, err error) {
	step := "read operation"
	defer func() { err = atStep(step, err) }()
	request := ctx
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
		step = "check Git target"
		writer, err := s.contentWriter(ctx, operation.Namespace, operation.Mode)
		if err != nil {
			return nil, err
		}
		if s.cfg.Commit == nil || operation.GitTarget == nil || s.cfg.Commit.Objects.Repository.String() != operation.GitTarget.Repository || s.cfg.Commit.Objects.Branch != operation.GitTarget.Base || writer.dir.Path() != operation.GitTarget.Directory {
			return nil, fmt.Errorf("%w: the credential Git target changed", agents.ErrConflict)
		}
	}
	if operation.State == operationSaving {
		if operation.Mode == agents.ModeCommit {
			step = "finish commit"
			return s.recoverCredentialCommit(ctx, operation)
		}
		step = "finish apply"
		return s.recoverAppliedCredential(ctx, operation)
	}
	step = "check version"
	if (operation.State != operationPending && operation.State != operationUncertain) || version == "" || version != operation.Version {
		return nil, fmt.Errorf("%w: %w", agents.ErrConflict, ErrCredentialOperationChanged)
	}
	if operation.State == operationUncertain && operation.Mode == agents.ModeCommit {
		step = "finish commit"
		return s.recoverCredentialCommit(ctx, operation)
	}
	step = "check values"
	if len(values) != len(operation.Credentials) {
		return nil, invalidf("supply exactly the credentials in the confirmation")
	}
	step = "authenticate caller"
	caller, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	step = "check consumers"
	writes, err := remainingOperationWrites(ctx, caller, operation)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	// created are the Secrets this save creates: only those it may take back.
	var created []string
	var notes []string
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
		// Consumers that differ from the prepared ones come first, whatever
		// else is wrong with them: the person must see them again.
		if operation.Result.Kind == KindCredential && !sameConsumers(references, operation.Consumers) {
			return nil, errConsumersChanged(operation.Namespace.Name, credential.Secret)
		}
		keyNotes, err := validateCredentialModels(operation, live, credential, values[index].Value)
		if err != nil {
			return nil, err
		}
		notes = append(notes, keyNotes...)
		// The preparation stored at least one consumer of the key and none of
		// another key, so for a replacement the comparison above already
		// answers both cases below; they stay as a defence.
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
		if action == opCreate {
			created = append(created, credential.Secret)
		}
		data := map[string]any{credential.Key: base64.StdEncoding.EncodeToString([]byte(values[index].Value))}
		writes = append([]write{credentialSecretWrite(operation.Namespace.Name, credential.Secret, action, data)}, writes...)
	}
	step = "dry run"
	for _, planned := range writes {
		if _, err := doCredentialWrite(ctx, caller, operation, planned, dryRunAll); err != nil {
			return nil, err
		}
	}
	step = "open writer"
	writer, err := s.contentWriter(ctx, operation.Namespace, operation.Mode)
	if err != nil {
		return nil, err
	}
	// Before a branch exists: every check of commit mode runs while the
	// operation is still pending, and a refusal returns it as it is. Nothing
	// was written, so a retry runs the same checks and gets the same answer.
	var prepared *credentialCommit
	if operation.Mode == agents.ModeCommit {
		step = "prepare commit"
		prepared, err = s.prepareCredentialCommit(ctx, operation, writer, writes)
		if err != nil {
			return nil, err
		}
	}
	// After this point a write may have happened: a failure marks the
	// operation uncertain, and the next save recovers it.
	step = "record saving"
	operation.State = operationSaving
	saving := ctx
	if operation.Mode == agents.ModeCommit {
		// The operation must outlive the writes and their record, so that a
		// repeated call finds it and gives the receipt.
		if until := time.Now().Add(credentialWriteBudget + 2*credentialRecordBudget); operation.ExpiresAt.Before(until) {
			operation.ExpiresAt = until
		}
		// A put the request cut could land without its answer, leaving a
		// saving operation with no recorded branch to recover.
		var cancelSaving context.CancelFunc
		saving, cancelSaving = detached(ctx, credentialRecordBudget)
		defer cancelSaving()
	}
	if err := s.cfg.CredentialOperations.put(saving, operation); err != nil {
		return nil, err
	}
	// In commit mode the branch, the commit and the pull request run to
	// their end once the branch is to be made, also when the caller goes
	// away: a request cut between the commit and the pull request would leave
	// a commit that only "Finish the save" opens. Their own budget ends them
	// instead.
	record := ctx
	if operation.Mode == agents.ModeCommit {
		writeCtx, cancelWrite := detached(ctx, credentialWriteBudget)
		var committed *CommitResult
		committed, err = s.writeCredentialCommit(writeCtx, operation, writer, prepared)
		cancelWrite()
		if err == nil {
			operation.Result.Commit = committed
		}
		var cancelRecord context.CancelFunc
		record, cancelRecord = detached(ctx, credentialRecordBudget)
		defer cancelRecord()
	} else {
		step = "apply"
		err = s.applyCredentialOperation(ctx, caller, operation, writes)
		if errors.As(err, new(nameTaken)) {
			s.removeCreatedSecrets(ctx, caller, operation, created)
		}
	}
	if err != nil {
		operation.State = operationUncertain
		if recordErr := s.cfg.CredentialOperations.put(record, operation); recordErr != nil {
			s.log.Warn("a failed credential save could not be recorded as uncertain", identity.LogAttr(ctx), "operation", id, "cause", recordErr.Error())
		}
		return nil, err
	}
	step = "record result"
	completeCredentialOperation(operation)
	operation.Result.Note = strings.Join(slices.DeleteFunc(append([]string{operation.Result.Note}, notes...), func(note string) bool { return note == "" }), "; ")
	if err := s.cfg.CredentialOperations.put(record, operation); err != nil {
		return nil, err
	}
	if request.Err() != nil {
		s.log.Info("a credential save completed after its request ended", identity.LogAttr(ctx), "operation", id, "mode", operation.Mode)
	}
	return &operation.Result, nil
}

// removeCreatedSecrets takes back, as the caller, the Secrets of created that
// this save wrote before a consumer create was refused because its object
// exists by then (nameTaken): that object may name the Secret, and the value
// the person typed for their own new object must not stay for it to send. A
// Secret is deleted only at the version this save wrote (a precondition), so
// one changed since is left, and never one that existed before the save,
// which is updated, not created. The delete sends no propagation policy:
// the API server answers it with a Status, never the Secret. A clear
// refusal (the Secret changed since, Conflict; or no delete right,
// Forbidden) leaves the Secret and its recorded version for the uncertain
// operation and is logged; any other result forgets the recorded version,
// so a retry checks whether the Secret exists and writes it whole instead
// of trusting a write that may be gone.
func (s *Service) removeCreatedSecrets(ctx context.Context, caller kube.Client, operation *credentialOperation, created []string) {
	for _, name := range created {
		version := operation.AppliedVersions[kindSecret+"/"+name]
		if version == "" {
			continue
		}
		err := caller.Dynamic().Resource(secretGVR).Namespace(operation.Namespace.Name).Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{ResourceVersion: &version}})
		if apierrors.IsConflict(err) || apierrors.IsForbidden(err) {
			s.log.Warn("a credential save could not take back the Secret it created; it stays", identity.LogAttr(ctx), "namespace", operation.Namespace.Name, "secret", name, "reason", string(apierrors.ReasonForError(err)))
			continue
		}
		delete(operation.AppliedVersions, kindSecret+"/"+name)
	}
}

func appliedCredentialConsumerMatches(operation *credentialOperation, object *unstructured.Unstructured) bool {
	if hash := operation.AppliedConsumerHashes[object.GetKind()+"/"+object.GetName()]; hash != "" && object.GetKind() != kindSecret {
		current, err := credentialConsumerHash(object)
		return err == nil && hash == current
	}
	version := operation.AppliedVersions[object.GetKind()+"/"+object.GetName()]
	return version != "" && version == object.GetResourceVersion()
}

func (s *Service) applyCredentialOperation(ctx context.Context, caller kube.Client, operation *credentialOperation, writes []write) error {
	for _, planned := range writes {
		marked := unstructuredOf(planned.obj).DeepCopy()
		setAnnotation(marked, credentialOperationAnnotation, operation.ElicitationID)
		planned.obj = marked.Object
		object, err := doCredentialWrite(ctx, caller, operation, planned, nil)
		if err != nil {
			return err
		}
		if operation.AppliedVersions == nil {
			operation.AppliedVersions = map[string]string{}
		}
		operation.AppliedVersions[planned.kind+"/"+planned.name] = object.GetResourceVersion()
		if planned.kind != kindSecret {
			hash, err := credentialConsumerHash(object)
			if err != nil {
				return err
			}
			if operation.AppliedConsumerHashes == nil {
				operation.AppliedConsumerHashes = map[string]string{}
			}
			operation.AppliedConsumerHashes[planned.kind+"/"+planned.name] = hash
		}
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
			if !appliedCredentialConsumerMatches(operation, current) || !operationObjectApplied(current, planned.obj, operation.ElicitationID) {
				return nil, fmt.Errorf("%w: a partially written consumer changed or lacks a confirmed version", agents.ErrConflict)
			}
			continue
		}
		expected := unstructuredOf(planned.obj)
		if planned.op == opCreate && !apierrors.IsNotFound(err) {
			return nil, nameTaken{fmt.Errorf("%w: %s exists now; prepare it again", agents.ErrConflict, planned.what)}
		}
		if planned.op != opCreate && (current == nil || !confirmedCredentialConsumer(operation, planned, current)) {
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
		if key == fieldMetadata || key == "status" {
			continue
		}
		if !operationValueMatches(value, current.Object[key]) {
			return false
		}
	}
	for key, value := range unstructuredOf(intended).GetAnnotations() {
		if key != credentialOperationAnnotation && current.GetAnnotations()[key] != value {
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
		if err != nil || !appliedCredentialConsumerMatches(operation, current) || !operationObjectApplied(current, planned.obj, operation.ElicitationID) {
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
