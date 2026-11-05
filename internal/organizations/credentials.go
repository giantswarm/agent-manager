package organizations

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/agent-manager/internal/kube"
)

// The states of a credential Secret, as the caller can read it.
const (
	// CredentialPresent: the Secret exists and holds the key.
	CredentialPresent = "present"
	// CredentialMissing: the Secret or its key does not exist.
	CredentialMissing = "missing"
	// CredentialReplace: the caller asked to replace it.
	CredentialReplace = "replace"
	// CredentialUnknown: the caller may not read the Secret.
	CredentialUnknown = "unknown"
)

// Credential is a credential Secret an object names, by reference only: the
// value is typed in the Dev Portal's credential form, never in a tool call,
// and no tool answer, log or trace carries it.
type Credential struct {
	// Secret and Key name where the value lives in the organization
	// namespace.
	Secret string `json:"secret"`
	Key    string `json:"key"`
	// State is present, missing, replace (asked for) or unknown (the caller
	// may not read the Secret).
	State string `json:"state"`
	// FormURL is the Dev Portal's credential form for this Secret and the
	// object that names it; it holds no value and no token. Empty without a
	// configured Dev Portal.
	FormURL string `json:"formUrl,omitempty"`
	// Next tells the person what to do when the value is missing or to be
	// replaced.
	Next string `json:"next,omitempty"`
}

// credentialRef checks a credential Secret name an argument gave.
func credentialRef(secret, key string) error {
	if !strings.HasPrefix(secret, CredentialPrefix) {
		return invalidf("credential Secret %q must start with %s: the Dev Portal's credential form writes only those, and admission lets an organization name only those", secret, CredentialPrefix)
	}
	if key == "" {
		return invalidf("credential Secret %s needs a key", secret)
	}
	return nil
}

// credential reports the state of a Secret an object of kind and name in org
// names. It GETs the Secret as the caller, which returns its data (the API
// has no read of a Secret's keys without their values), to tell whether the
// key holds a value; the value stays in this function, which reports only
// the state, and it is never returned, logged or traced. A caller who may not
// read the Secret gets the state unknown.
func (s *Service) credential(ctx context.Context, c kube.Client, org Namespace, kind, name, secret, key string, replace bool) Credential {
	cred := Credential{Secret: secret, Key: key, FormURL: s.formURL(org, secret, key, kind, name)}
	obj, err := c.Typed().CoreV1().Secrets(org.Name).Get(ctx, secret, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		cred.State = CredentialMissing
	case err != nil:
		cred.State = CredentialUnknown
	case len(obj.Data[key]) == 0 && obj.StringData[key] == "":
		cred.State = CredentialMissing
	default:
		cred.State = CredentialPresent
	}
	if replace {
		cred.State = CredentialReplace
	}
	switch cred.State {
	case CredentialMissing, CredentialReplace, CredentialUnknown:
		cred.Next = s.next(cred, kind, name)
	}
	return cred
}

// next is the answer to a missing or replaced credential: the form link as
// text. The URL elicitation that opens it in the client is
// giantswarm/agent-manager#293.
func (s *Service) next(c Credential, kind, name string) string {
	what := fmt.Sprintf("the value of key %s of Secret %s, which %s %s uses", c.Key, c.Secret, kind, name)
	lead := "A person enters " + what
	switch c.State {
	case CredentialMissing:
		lead = "The credential is missing: a person enters " + what
	case CredentialUnknown:
		lead = "You may not read the Secret, so whether it holds the value is unknown; if it does not, a person enters " + what
	}
	if c.FormURL == "" {
		return lead + " in the Dev Portal's credential form (no Dev Portal is configured, so there is no link). Never pass the value to a tool or a chat."
	}
	return lead + " in the Dev Portal's credential form: " + c.FormURL + " . Never pass the value to a tool or a chat."
}

// formURL is the Dev Portal's credential form for a Secret: the
// organization, the Secret, its key and the object that names it, in the
// path. No value and no token.
func (s *Service) formURL(org Namespace, secret, key, kind, name string) string {
	if s.cfg.DevPortalURL == "" {
		return ""
	}
	parts := []string{"agent-platform", "organizations", org.Parent, org.Organization, "credentials", secret, key, kind, name}
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return s.cfg.DevPortalURL + "/" + strings.Join(parts, "/")
}
