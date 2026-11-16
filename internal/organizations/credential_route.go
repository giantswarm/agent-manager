package organizations

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/kube"
)

var credentialKinds = []string{kubeKindModelConfig, kubeKindRemoteMCPServer, kubeKindEgressPolicy}

// CredentialReference is an object of an organization namespace that names
// a credential Secret.
type CredentialReference struct {
	ProjectID string `json:"projectID,omitempty"`
	Location  string `json:"location,omitempty"`
	// Kind is ModelConfig, RemoteMCPServer or EgressPolicy.
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Key is the key of the Secret the object reads.
	Key string `json:"key"`
	// Provider is the provider a ModelConfig sends the value to.
	Provider string `json:"provider,omitempty"`
	// URL is where a RemoteMCPServer sends the value, a credential in the
	// URL shown as REDACTED.
	URL string `json:"url,omitempty"`
	// Origins are where an EgressPolicy sends the value.
	Origins []string `json:"origins,omitempty"`
	// FromGit: a Flux Kustomization applies the object from git.
	FromGit bool `json:"fromGit,omitempty"`
}

// CredentialUse is every object of an organization namespace that names a
// credential Secret, as the caller can read them.
type CredentialUse struct {
	Namespace    string                `json:"namespace"`
	Parent       string                `json:"parent"`
	Organization string                `json:"organization"`
	Secret       string                `json:"secret"`
	References   []CredentialReference `json:"references"`
	// Mode is how a write for the object asked about lands, apply or
	// commit; empty when no object was named.
	Mode string `json:"mode,omitempty"`
}

func (s *Service) orgNamespace(ctx context.Context, parent, organization string) (Namespace, error) {
	if s.cfg.Namespaces == nil {
		return Namespace{}, fmt.Errorf("%w: organization management is disabled", agents.ErrUnsupported)
	}
	if parent == "" || organization == "" {
		return Namespace{}, invalidf("parent and organization are required")
	}
	org, ok, err := s.cfg.Namespaces.Find(ctx, parent, organization)
	if err != nil {
		return Namespace{}, fmt.Errorf("%w: %v", agents.ErrUnsupported, err)
	}
	if !ok {
		return Namespace{}, fmt.Errorf("%w: organization %s/%s has no organization namespace", agents.ErrNotFound, parent, organization)
	}
	return org, nil
}

// references reads, as the caller, every object of ns that names secret,
// and the live objects of the credential kinds by kind/name. A kind the
// caller may not list refuses the read: what is not read cannot be ruled
// out.
func (s *Service) references(ctx context.Context, c kube.Client, ns, secret string) ([]CredentialReference, map[string]*unstructured.Unstructured, error) {
	refs := []CredentialReference{}
	live := map[string]*unstructured.Unstructured{}
	for _, kind := range credentialKinds {
		resource := ""
		for _, info := range kinds {
			if info.kubeKind == kind {
				resource = info.resource
			}
		}
		items, err := s.existing(ctx, c, ns, s.kagentGVR(resource), kind+"s")
		if err != nil {
			return nil, nil, err
		}
		for i := range items {
			u := &items[i]
			live[kind+"/"+u.GetName()] = u
			refs = append(refs, referencesOf(kind, u, secret)...)
		}
	}
	return refs, live, nil
}

func referencesOf(kind string, u *unstructured.Unstructured, secret string) []CredentialReference {
	base := CredentialReference{Kind: kind, Name: u.GetName(), FromGit: fromGit(u)}
	var out []CredentialReference
	switch kind {
	case kubeKindModelConfig:
		name, _, _ := unstructured.NestedString(u.Object, "spec", "apiKeySecret")
		key, _, _ := unstructured.NestedString(u.Object, "spec", "apiKeySecretKey")
		if name == secret {
			ref := base
			ref.Key = key
			ref.Provider, _, _ = unstructured.NestedString(u.Object, "spec", "provider")
			if block := vertexBlock(u); block != "" {
				ref.ProjectID, _, _ = unstructured.NestedString(u.Object, "spec", block, "projectID")
				ref.Location, _, _ = unstructured.NestedString(u.Object, "spec", block, "location")
			}
			out = append(out, ref)
		}
	case kubeKindRemoteMCPServer:
		raw, _, _ := unstructured.NestedString(u.Object, "spec", "url")
		for _, cc := range connectorCredentials(u) {
			if cc.Secret == secret && !slices.ContainsFunc(out, func(r CredentialReference) bool { return r.Key == cc.Key }) {
				ref := base
				ref.Key, ref.URL = cc.Key, redactURL(raw)
				out = append(out, ref)
			}
		}
	case kubeKindEgressPolicy:
		for _, d := range egressDestinations(u) {
			if d.CredentialSecret != secret {
				continue
			}
			i := slices.IndexFunc(out, func(r CredentialReference) bool { return r.Key == d.CredentialKey })
			if i < 0 {
				ref := base
				ref.Key = d.CredentialKey
				out = append(out, ref)
				i = len(out) - 1
			}
			if !slices.Contains(out[i].Origins, d.Origin) {
				out[i].Origins = append(out[i].Origins, d.Origin)
			}
		}
	}
	return out
}

// writesFor says whether the operation of result may write the credential
// Secret reference reads: a credential result writes it for every object that
// reads it, any other result only for its own object.
func writesFor(result ResourceResult, reference CredentialReference) bool {
	return result.Kind == KindCredential || (reference.Kind == kinds[result.Kind].kubeKind && reference.Name == result.Name)
}

// writeMode is the mode of a credential write: the one asked for, else
// commit where git applies the object, or where the object is not live (a
// commit-mode write not merged yet) and commit mode is configured, else
// apply. A credential Secret lives in its consumer's content path, so the
// consumer decides; the Secret itself is never read.
func (s *Service) writeMode(requested string, object *unstructured.Unstructured) (string, error) {
	if requested != "" {
		return s.mode(requested)
	}
	if s.cfg.Commit != nil && (object == nil || fromGit(object)) {
		return agents.ModeCommit, nil
	}
	return agents.ModeApply, nil
}

func checkSecretName(secret string) error {
	if !strings.HasPrefix(secret, CredentialPrefix) {
		return invalidf("credential Secret %q must start with %s", secret, CredentialPrefix)
	}
	return segment("Secret", secret)
}

// CredentialReferences lists, as the caller, the objects of the
// organization parent/organization that name secret, and for an
// EgressPolicy the origins it sends the value to: what the credential form
// shows before it accepts a value. kind and name, when given, are the
// object that asked; Mode then says how a write for it lands.
func (s *Service) CredentialReferences(ctx context.Context, parent, organization, secret, kind, name string) (*CredentialUse, error) {
	if err := checkSecretName(secret); err != nil {
		return nil, err
	}
	org, err := s.orgNamespace(ctx, parent, organization)
	if err != nil {
		return nil, err
	}
	c, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	refs, live, err := s.references(ctx, c, org.Name, secret)
	if err != nil {
		return nil, err
	}
	out := &CredentialUse{Namespace: org.Name, Parent: org.Parent, Organization: org.Organization, Secret: secret, References: refs}
	if kind != "" && name != "" {
		if !slices.Contains(credentialKinds, kind) {
			return nil, invalidf("kind %q: %s", kind, strings.Join(credentialKinds, ", "))
		}
		if out.Mode, err = s.writeMode("", live[kind+"/"+name]); err != nil {
			return nil, err
		}
	}
	return out, nil
}
