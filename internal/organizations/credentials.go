package organizations

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/giantswarm/agent-manager/internal/kube"
)

// The states of a credential Secret. Credentials are write-only: neither the
// caller nor agent-manager reads a Secret, so a state says whether the Secret
// exists, never what it holds.
const (
	// CredentialPresent: the Secret exists.
	CredentialPresent   = "present"
	CredentialCommitted = "committed"
	// CredentialMissing: the Secret does not exist, or its consumer reports
	// that it lacks the key (a ModelConfig's ResolvedRefs condition).
	CredentialMissing = "missing"
	// CredentialReplace: the caller asked to replace it.
	CredentialReplace = "replace"
)

// Credential is a credential Secret an object names, by reference only: the
// value is typed in the credential UI, never in a tool call,
// and no tool answer, log or trace carries it.
type Credential struct {
	// Secret and Key name where the value lives in the organization
	// namespace.
	Secret string `json:"secret"`
	Key    string `json:"key"`
	// Committed means a pull request exists, without asserting a live value.
	State string `json:"state"`
	// FormURL grants no authority; the UI authenticates ownership separately.
	FormURL string `json:"formUrl,omitempty"`
	// Next tells the person what to do when the value is missing or to be
	// replaced.
	Next string `json:"next,omitempty"`

	// exists says the Secret exists, whatever State the caller asked for.
	exists bool
}

// credentialRef checks a credential Secret name an argument gave.
func credentialRef(secret, key string) error {
	if err := segment("credential Secret", secret); err != nil {
		return err
	}
	if !strings.HasPrefix(secret, CredentialPrefix) {
		return invalidf("credential Secret %q must start with %s: the credential UI writes only those, and admission lets an organization name only those", secret, CredentialPrefix)
	}
	if key == "" {
		return invalidf("credential Secret %s needs a key", secret)
	}
	return nil
}

// credential reports the state of a Secret an object of kind and name in org
// names. It never reads the Secret: get, list and watch return its data, and
// so does a patch, so an organization admin holds none of them on Secrets.
// It asks the API server instead with a dry-run create of the Secret as the
// caller, which needs only the create the admin holds and answers
// AlreadyExists when the Secret exists. A refusal of the dry run (the caller
// may not create the Secret, or admission refuses its name) is the error.
func (s *Service) credential(ctx context.Context, c kube.Client, org Namespace, kind, name, secret, key string, replace bool) (Credential, error) {
	exists, err := secretExists(ctx, c, org.Name, secret)
	if err != nil {
		return Credential{}, err
	}
	cred := Credential{Secret: secret, Key: key, State: CredentialMissing, exists: exists}
	switch {
	case replace:
		cred.State = CredentialReplace
	case exists:
		cred.State = CredentialPresent
	}
	if cred.State != CredentialPresent {
		cred.Next = s.next(cred, kind, name)
	}
	return cred, nil
}

// modelKeyNotFound says kagent reports, for the current generation of the
// live ModelConfig, that its apiKeySecret secret exists without the key: the
// ModelConfig's ResolvedRefs condition is False with reason
// APIKeySecretKeyNotFound. It is the only word on a Secret's keys that needs
// no read of the Secret.
func modelKeyNotFound(live *unstructured.Unstructured, secret, key string) bool {
	if live == nil {
		return false
	}
	liveSecret, _, _ := unstructured.NestedString(live.Object, "spec", "apiKeySecret")
	liveKey, _, _ := unstructured.NestedString(live.Object, "spec", "apiKeySecretKey")
	if liveSecret != secret || liveKey != key {
		return false
	}
	raw, _, _ := unstructured.NestedSlice(live.Object, "status", "conditions")
	conditions := make([]metav1.Condition, 0, len(raw))
	for _, item := range raw {
		fields, ok := item.(map[string]any)
		if !ok {
			continue
		}
		var condition metav1.Condition
		if runtime.DefaultUnstructuredConverter.FromUnstructured(fields, &condition) == nil {
			conditions = append(conditions, condition)
		}
	}
	resolved := apimeta.FindStatusCondition(conditions, conditionResolvedRefs)
	return resolved != nil && resolved.Status == metav1.ConditionFalse && resolved.Reason == reasonAPIKeySecretKeyNotFound && resolved.ObservedGeneration == live.GetGeneration()
}

// The ModelConfig condition and reason kagent writes when its apiKeySecret
// lacks apiKeySecretKey (kagent go/core/internal/translator/modelconfig.go).
const (
	conditionResolvedRefs         = "ResolvedRefs"
	reasonAPIKeySecretKeyNotFound = "APIKeySecretKeyNotFound"
)

// secretExists says whether the Secret ns/name exists, by a dry-run create
// as the caller: nothing is written and nothing of the Secret is returned.
func secretExists(ctx context.Context, c kube.Client, ns, name string) (bool, error) {
	_, err := do(ctx, c.Dynamic(), credentialSecretWrite(ns, name, opCreate, nil), dryRunAll)
	switch {
	case err == nil:
		return false, nil
	case apierrors.IsAlreadyExists(err):
		return true, nil
	}
	return false, err
}

// credentialSecretWrite is the write of credential Secret ns/name holding
// data, all of the Secret: an update replaces it whole, so the caller never
// needs to read it, and any key data does not name is dropped.
func credentialSecretWrite(ns, name, op string, data map[string]any) write {
	secret := newObject("v1", kindSecret, ns, name)
	delete(secret.Object, "spec")
	secret.Object["type"] = "Opaque"
	if data != nil {
		secret.Object["data"] = data
	}
	return write{gvr: secretGVR, kind: kindSecret, op: op, obj: secret.Object, ns: ns, name: name, what: "credential Secret " + ns + "/" + name, reason: wayOrganizationAdmin(ns)}
}

// NeedsInput reports whether a person has to enter the credential's value in
// the credential UI, and the form has a link: the credential
// is missing or the caller asked to replace it.
func (c Credential) NeedsInput() bool {
	return c.FormURL != "" && (c.State == CredentialMissing || c.State == CredentialReplace)
}

func (s *Service) next(c Credential, kind, name string) string {
	what := fmt.Sprintf("the value of key %s of Secret %s, which %s %s uses", c.Key, c.Secret, kind, name)
	lead := "A person enters " + what
	if c.State == CredentialMissing {
		lead = "The credential is missing: a person enters " + what
	}
	if c.FormURL == "" {
		return lead + " in the credential UI (prepare a credential operation to obtain a link). Never pass the value to a tool or a chat."
	}
	return lead + " in the credential UI: " + c.FormURL + " . Never pass the value to a tool or a chat."
}
