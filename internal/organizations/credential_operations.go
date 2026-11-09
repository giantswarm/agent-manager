package organizations

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/identity"
)

type credentialRequestKey struct{}

const (
	operationPending   = "pending"
	operationSaving    = "saving"
	operationUncertain = "uncertain"
	operationComplete  = "complete"
	operationCancelled = "cancelled"
)

var ErrCredentialOperationChanged = errors.New("credential operation changed; refresh the confirmation")

type credentialRequest struct{ Tool, Intent, Client string }

// ContextWithCredentialRequest binds preparation and retries to the original
// nonsecret tool arguments and authenticated client context.
func ContextWithCredentialRequest(ctx context.Context, tool, intent, client string) context.Context {
	return context.WithValue(ctx, credentialRequestKey{}, credentialRequest{tool, intent, client})
}

// ValidateCredentialUIURL keeps the origin fixed while allowing domain identifiers in the path or query.
func ValidateCredentialUIURL(template string) error {
	const marker = "credential-operation-placeholder"
	validationURL := strings.NewReplacer("{operation}", marker, "{parent}", marker, "{organization}", marker).Replace(template)
	parsed, err := url.Parse(validationURL)
	if err != nil || strings.ContainsAny(validationURL, "{}") || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || strings.Contains(parsed.Host, marker) || strings.Count(template, "{operation}") != 1 {
		return errors.New("credential UI requires an HTTPS URL containing exactly one {operation} placeholder in its path or query")
	}
	return nil
}

type operationWrite struct {
	Resource schema.GroupVersionResource `json:"resource"`
	Kind     string                      `json:"kind"`
	Action   string                      `json:"action"`
	Object   map[string]any              `json:"object"`
	Name     string                      `json:"name"`
	Version  string                      `json:"version"`
}

type operationCredential struct {
	Secret  string `json:"secret"`
	Key     string `json:"key"`
	Version string `json:"version,omitempty"`
	Replace bool   `json:"replace"`
	Exists  bool   `json:"exists"`
}

type credentialOperation struct {
	ID              string                `json:"id"`
	Version         string                `json:"-"`
	Issuer          string                `json:"issuer"`
	Subject         string                `json:"subject"`
	Request         credentialRequest     `json:"request"`
	ElicitationID   string                `json:"elicitationId"`
	ExpiresAt       time.Time             `json:"expiresAt"`
	State           string                `json:"state"`
	Namespace       Namespace             `json:"namespace"`
	Mode            string                `json:"mode"`
	Writes          []operationWrite      `json:"writes"`
	Credentials     []operationCredential `json:"credentials"`
	Result          ResourceResult        `json:"result"`
	GitFiles        map[string]string     `json:"gitFiles,omitempty"`
	WrittenFiles    map[string]string     `json:"writtenFiles,omitempty"`
	GitTarget       *CredentialGitTarget  `json:"gitTarget,omitempty"`
	AppliedVersions map[string]string     `json:"appliedVersions,omitempty"`
}

type CredentialGitTarget struct {
	Repository string `json:"repository"`
	Base       string `json:"base"`
	Directory  string `json:"directory"`
}

type CredentialChange struct {
	Action string         `json:"action"`
	Object map[string]any `json:"object"`
}

// CredentialOperation is the authenticated UI's confirmation. Its consumer
// manifests contain only nonsecret configuration and Secret references.
type CredentialOperation struct {
	ID            string                `json:"id"`
	Version       string                `json:"version"`
	ElicitationID string                `json:"elicitationId"`
	ExpiresAt     time.Time             `json:"expiresAt"`
	State         string                `json:"state"`
	Mode          string                `json:"mode"`
	Result        ResourceResult        `json:"result"`
	References    []CredentialReference `json:"references"`
	GitTarget     *CredentialGitTarget  `json:"gitTarget,omitempty"`
	Organization  Namespace             `json:"organization"`
	Changes       []CredentialChange    `json:"changes"`
}

func credentialOperationID(ctx context.Context) (string, error) {
	person, ok := identity.FromContext(ctx)
	request, bound := ctx.Value(credentialRequestKey{}).(credentialRequest)
	if !ok || person.Delegated || person.Issuer == "" || person.Subject == "" || !bound || request.Client == "" {
		return "", fmt.Errorf("%w: credential preparation needs a verified identity and client", agents.ErrUnauthenticated)
	}
	raw, err := json.Marshal([]string{person.Issuer, person.Subject, request.Client, request.Tool, request.Intent})
	if err != nil {
		return "", invalidf("credential preparation arguments are invalid")
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:24]), nil
}

func (s *Service) credentialOperation(ctx context.Context, id string) (*credentialOperation, error) {
	operation, err := s.cfg.CredentialOperations.read(ctx, id)
	if err != nil {
		return nil, err
	}
	person, ok := identity.FromContext(ctx)
	if !ok || person.Delegated || operation.Issuer != person.Issuer || operation.Subject != person.Subject {
		return nil, fmt.Errorf("%w: credential operation", agents.ErrNotFound)
	}
	if time.Now().After(operation.ExpiresAt) {
		return nil, fmt.Errorf("%w: credential operation expired; prepare it again", agents.ErrConflict)
	}
	organization, err := s.orgNamespace(ctx, operation.Namespace.Parent, operation.Namespace.Organization)
	if err != nil {
		return nil, err
	}
	if organization.Name != operation.Namespace.Name {
		return nil, fmt.Errorf("%w: organization namespace changed", agents.ErrConflict)
	}
	caller, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	var checks []access
	for _, planned := range operation.Writes {
		checks = append(checks, access{namespace: organization.Name, verb: planned.Action, group: planned.Resource.Group, resource: planned.Resource.Resource, name: planned.Name})
	}
	for _, credential := range operation.Credentials {
		verb := opCreate
		if credential.Exists {
			verb = opUpdate
		}
		checks = append(checks, access{namespace: organization.Name, verb: verb, resource: "secrets", name: credential.Secret})
	}
	if err := requireAccess(ctx, caller, wayOrganizationAdmin(organization.Name), checks...); err != nil {
		return nil, err
	}
	return operation, nil
}

func (s *Service) ReadCredentialOperation(ctx context.Context, id string) (*CredentialOperation, error) {
	operation, err := s.credentialOperation(ctx, id)
	if err != nil {
		return nil, err
	}
	var references []CredentialReference
	changes := make([]CredentialChange, 0, len(operation.Writes))
	for _, planned := range operation.Writes {
		changes = append(changes, CredentialChange{Action: planned.Action, Object: unstructuredOf(planned.Object).DeepCopy().Object})
		for _, credential := range operation.Credentials {
			references = append(references, referencesOf(planned.Kind, unstructuredOf(planned.Object), credential.Secret)...)
		}
	}
	return &CredentialOperation{ID: operation.ID, Version: operation.Version, ElicitationID: operation.ElicitationID, ExpiresAt: operation.ExpiresAt, State: operation.State, Mode: operation.Mode, Result: operation.Result, References: references, GitTarget: operation.GitTarget, Organization: operation.Namespace, Changes: changes}, nil
}

func (s *Service) CredentialReceipt(ctx context.Context) (*ResourceResult, error) {
	if s.cfg.CredentialOperations == nil {
		return nil, nil
	}
	id, err := credentialOperationID(ctx)
	if err != nil {
		return nil, err
	}
	stored, err := s.cfg.CredentialOperations.read(ctx, id)
	if errors.Is(err, agents.ErrNotFound) || (err == nil && time.Now().After(stored.ExpiresAt)) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	operation, err := s.credentialOperation(ctx, id)
	if errors.Is(err, agents.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if operation.State == operationComplete {
		return &operation.Result, nil
	}
	if operation.State == operationCancelled {
		return nil, fmt.Errorf("%w: credential operation was cancelled", agents.ErrConflict)
	}
	return &operation.Result, nil
}

func (s *Service) landResource(ctx context.Context, writer *contentWriter, writes []write, result *ResourceResult, replace bool, verb, title string) error {
	if slices.ContainsFunc(result.Credentials, func(credential Credential) bool { return credential.State == CredentialUnknown }) {
		return fmt.Errorf("%w: credential state cannot be verified", agents.ErrForbidden)
	}
	needsCredential := slices.ContainsFunc(result.Credentials, func(credential Credential) bool {
		return credential.State == CredentialMissing || credential.State == CredentialReplace
	})
	if !needsCredential {
		var err error
		result.Commit, err = writer.land(ctx, s, writes, result.DryRun, verb, title)
		return err
	}
	caller, err := s.caller(ctx)
	if err != nil {
		return err
	}
	if err := apply(ctx, caller.Dynamic(), writes, true); err != nil {
		return err
	}
	for _, planned := range writes {
		if containsInlineCredential(planned.obj) {
			return invalidf("inline credentials cannot enter a credential operation; use Secret references")
		}
		if planned.kind == kubeKindRemoteMCPServer {
			raw, _, _ := unstructured.NestedString(planned.obj, "spec", "url")
			if err := refuseCredentialInURL(raw); err != nil {
				return err
			}
		}
	}
	if result.DryRun {
		return nil
	}
	if s.cfg.CredentialUIURL == "" {
		return fmt.Errorf("%w: no credential UI URL is configured; no resource was written", agents.ErrUnsupported)
	}
	if err := ValidateCredentialUIURL(s.cfg.CredentialUIURL); err != nil {
		return invalidf("%s", err)
	}
	id, err := credentialOperationID(ctx)
	if err != nil {
		return err
	}
	person, _ := identity.FromContext(ctx)
	request, _ := ctx.Value(credentialRequestKey{}).(credentialRequest)
	operation := &credentialOperation{ID: id, Issuer: person.Issuer, Subject: person.Subject, Request: request, ElicitationID: uuid.NewString(), ExpiresAt: time.Now().Add(credentialOperationTTL), State: operationPending, Namespace: writer.org, Mode: writer.mode, Result: *result}
	for _, planned := range writes {
		object := unstructuredOf(planned.obj)
		operation.Writes = append(operation.Writes, operationWrite{Resource: planned.gvr, Kind: planned.kind, Action: planned.op, Object: object.DeepCopy().Object, Name: planned.name, Version: object.GetResourceVersion()})
	}
	seenSecrets := map[string]bool{}
	for _, credential := range result.Credentials {
		if credential.State != CredentialMissing && credential.State != CredentialReplace {
			continue
		}
		if seenSecrets[credential.Secret] {
			return invalidf("each credential Secret must contain one credential key")
		}
		seenSecrets[credential.Secret] = true
		references, _, err := s.references(ctx, caller, writer.org.Name, credential.Secret)
		if err != nil {
			return err
		}
		for _, reference := range references {
			if reference.Kind != kinds[result.Kind].kubeKind || reference.Name != result.Name {
				return fmt.Errorf("%w: another consumer references the credential Secret", agents.ErrConflict)
			}
		}
		secret, err := liveSecret(ctx, caller, writer.org.Name, credential.Secret)
		if err != nil {
			return err
		}
		version := ""
		if secret != nil {
			version = secret.GetResourceVersion()
			for _, field := range []string{"data", "stringData"} {
				data, _, _ := unstructured.NestedMap(secret.Object, field)
				for key := range data {
					if key != credential.Key {
						return fmt.Errorf("%w: credential replacement requires a Secret with one key", agents.ErrConflict)
					}
				}
			}
		}
		operation.Credentials = append(operation.Credentials, operationCredential{Secret: credential.Secret, Key: credential.Key, Version: version, Replace: replace, Exists: secret != nil})
	}
	if writer.commit != nil {
		operation.GitTarget = &CredentialGitTarget{Repository: writer.commit.cfg.Objects.Repository.String(), Base: writer.commit.cfg.Objects.Branch, Directory: writer.dir.Path()}
		operation.GitFiles, err = writer.commit.credentialSnapshot(ctx, operation)
		if err != nil {
			return err
		}
	}
	link := strings.NewReplacer("{operation}", url.PathEscape(id), "{parent}", url.PathEscape(writer.org.Parent), "{organization}", url.PathEscape(writer.org.Organization)).Replace(s.cfg.CredentialUIURL)
	for index := range operation.Result.Credentials {
		credential := &operation.Result.Credentials[index]
		if credential.State == CredentialMissing || credential.State == CredentialReplace {
			credential.FormURL = link
			credential.Next = "Enter the credential in the authenticated credential UI: " + link + " . Never send the value to a tool or a chat."
		}
	}
	operation.Result.CredentialOperation = id
	operation.Result.ElicitationID = operation.ElicitationID
	operation.Result.Pending = true
	stored, err := s.cfg.CredentialOperations.prepare(ctx, operation)
	if err != nil {
		return err
	}
	*result = stored.Result
	return nil
}

func containsInlineCredential(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			switch strings.ToLower(key) {
			case "apikey", "password", "token", "accesstoken", "refreshtoken", "clientsecret", "privatekey", "headers":
				if child != nil && child != "" {
					return true
				}
			}
			if key == "headersFrom" {
				if headers, ok := child.([]any); ok {
					for _, raw := range headers {
						if header, ok := raw.(map[string]any); ok {
							if _, inline := header["value"]; inline {
								return true
							}
						}
					}
				}
			}
			if containsInlineCredential(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if containsInlineCredential(child) {
				return true
			}
		}
	}
	return false
}

func (s *Service) CancelCredentialOperation(ctx context.Context, id, version string) error {
	operation, err := s.credentialOperation(ctx, id)
	if err != nil {
		return err
	}
	if version == "" || version != operation.Version || operation.State != operationPending {
		return fmt.Errorf("%w: %w", agents.ErrConflict, ErrCredentialOperationChanged)
	}
	operation.State = operationCancelled
	return s.cfg.CredentialOperations.put(ctx, operation)
}

func operationWrites(operation *credentialOperation) []write {
	writes := make([]write, 0, len(operation.Writes))
	for _, planned := range operation.Writes {
		writes = append(writes, write{gvr: planned.Resource, kind: planned.Kind, op: planned.Action, obj: planned.Object, ns: operation.Namespace.Name, name: planned.Name, what: planned.Kind + " " + operation.Namespace.Name + "/" + planned.Name, reason: wayOrganizationAdmin(operation.Namespace.Name)})
	}
	return writes
}
