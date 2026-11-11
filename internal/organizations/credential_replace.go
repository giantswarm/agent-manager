package organizations

import (
	"cmp"
	"context"
	"fmt"
	"reflect"
	"slices"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/identity"
)

// CredentialRequest is replace_organization_credential's arguments.
type CredentialRequest struct {
	Namespace string
	Secret    string
	Key       string
	DryRun    bool
	// Mode is apply (default) or commit.
	Mode string
}

// ReplaceCredential prepares a new value for key of the credential Secret
// that objects of an organization namespace read, as the caller: the
// operation writes the Secret and none of the objects. The Secret is written
// whole and never read, so a Secret another object reads by a different key
// is refused: that key would be lost.
func (s *Service) ReplaceCredential(ctx context.Context, req CredentialRequest) (*ResourceResult, error) {
	org, err := s.organization(ctx, req.Namespace)
	if err != nil {
		return nil, err
	}
	if err := credentialRef(req.Secret, req.Key); err != nil {
		return nil, err
	}
	c, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	w, err := s.contentWriter(ctx, org, req.Mode)
	if err != nil {
		return nil, err
	}
	references, live, err := s.references(ctx, c, org.Name, req.Secret)
	if err != nil {
		return nil, err
	}
	var users []string
	for _, reference := range references {
		if reference.Key == req.Key {
			users = append(users, reference.Kind+" "+reference.Name)
		}
	}
	if len(users) == 0 {
		return nil, fmt.Errorf("%w: nothing in %s uses key %s of credential Secret %s, so there is no credential to replace", agents.ErrNotFound, org.Name, req.Key, req.Secret)
	}
	for _, reference := range references {
		if reference.Key != req.Key {
			return nil, otherKey(reference, req.Secret, req.Key)
		}
		// The Secret lands where its consumers live: in git for objects Flux
		// applies, live for objects written live.
		if err := w.owned(live[reference.Kind+"/"+reference.Name], reference.Kind); err != nil {
			return nil, err
		}
	}
	exists, err := secretExists(ctx, c, org.Name, req.Secret)
	if err != nil {
		return nil, err
	}
	// A Secret that does not exist is missing, not replaced: the person
	// enters its first value.
	state := CredentialReplace
	if !exists {
		state = CredentialMissing
	}
	credential := Credential{Secret: req.Secret, Key: req.Key, State: state, exists: exists}
	credential.Next = s.next(credential, users...)
	res := &ResourceResult{Namespace: org.Name, Kind: KindCredential, Name: req.Secret, DryRun: req.DryRun, RequestedBy: identity.Caller(ctx), Credentials: []Credential{credential}}
	if err := s.landResource(ctx, w, nil, res, true, "credential-"+req.Secret, "chore(organizations): replace credential "+req.Secret); err != nil {
		return nil, err
	}
	return res, nil
}

// sameConsumers says whether two lists name the same objects reading the same
// keys and sending the value to the same places. Where an object comes from
// (FromGit) is left out: the ownership checks judge it.
func sameConsumers(a, b []CredentialReference) bool {
	normal := func(references []CredentialReference) []CredentialReference {
		out := make([]CredentialReference, 0, len(references))
		for _, reference := range references {
			reference.FromGit = false
			reference.Origins = slices.Clone(reference.Origins)
			slices.Sort(reference.Origins)
			if len(reference.Origins) == 0 {
				reference.Origins = nil
			}
			out = append(out, reference)
		}
		slices.SortFunc(out, func(x, y CredentialReference) int {
			return cmp.Or(cmp.Compare(x.Kind, y.Kind), cmp.Compare(x.Name, y.Name), cmp.Compare(x.Key, y.Key))
		})
		return out
	}
	return reflect.DeepEqual(normal(a), normal(b))
}

// errConsumersChanged refuses a save whose consumers differ from the ones the
// replacement was prepared for: the person confirmed other destinations.
func errConsumersChanged(ns, secret string) error {
	return fmt.Errorf("%w: the objects in %s that use Secret %s, or where they send its value, changed since the preparation; prepare it again", agents.ErrConflict, ns, secret)
}

// otherKey refuses a write of Secret holding only key that would drop the
// key reference reads.
func otherKey(reference CredentialReference, secret, key string) error {
	return fmt.Errorf("%w: %s %s reads key %s of Secret %s, which a write of key %s would drop: the Secret is written whole, holding that one key", agents.ErrConflict, reference.Kind, reference.Name, reference.Key, secret, key)
}

// operationReferences lists, as the caller, every object of the operation's
// namespace that names one of its credential Secrets, each planned write in
// place of the live object it changes.
func (s *Service) operationReferences(ctx context.Context, operation *credentialOperation) ([]CredentialReference, error) {
	c, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	planned := map[string]bool{}
	for _, write := range operation.Writes {
		planned[write.Kind+"/"+write.Name] = true
	}
	references := []CredentialReference{}
	for _, credential := range operation.Credentials {
		live, _, err := s.references(ctx, c, operation.Namespace.Name, credential.Secret)
		if err != nil {
			return nil, err
		}
		for _, reference := range live {
			if !planned[reference.Kind+"/"+reference.Name] {
				references = append(references, reference)
			}
		}
		for _, write := range operation.Writes {
			references = append(references, referencesOf(write.Kind, unstructuredOf(write.Object), credential.Secret)...)
		}
	}
	slices.SortStableFunc(references, func(a, b CredentialReference) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Name, b.Name), cmp.Compare(a.Key, b.Key))
	})
	return references, nil
}
