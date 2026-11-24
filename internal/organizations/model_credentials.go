package organizations

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"maps"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/giantswarm/gitops-commit/layout"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/giantswarm/agent-manager/internal/agents"
)

func (s *Service) modelNamespace(ctx context.Context, namespace string) (Namespace, error) {
	if namespace == "" || namespace != s.cfg.PlatformNamespace {
		return s.organization(ctx, namespace)
	}
	if !CredentialUIURLServesInstallations(s.cfg.CredentialUIURL) {
		return Namespace{}, fmt.Errorf("%w: installation model management requires a credential UI URL with a {namespace} expression and no {organization}", agents.ErrUnsupported)
	}
	caller, err := s.caller(ctx)
	if err != nil {
		return Namespace{}, err
	}
	if err := requireAccess(ctx, caller, "installation model management requires cluster-admin authorization", access{verb: "*", group: "*", resource: "*"}); err != nil {
		return Namespace{}, err
	}
	return Namespace{Name: namespace, Scope: scopePlatform}, nil
}

func modelCredentialRef(target Namespace, secret, key string) error {
	if target.Scope != scopePlatform {
		return credentialRef(secret, key)
	}
	if len(validation.IsDNS1123Subdomain(secret)) > 0 || len(validation.IsConfigMapKey(key)) > 0 {
		return invalidf("model credential requires a valid Secret name and key")
	}
	return nil
}

// writableModelCredential refuses a credential write to an installation
// Secret agent-manager does not own. The write replaces the Secret whole and
// never reads it, so another reader of the Secret (a Pod, a Helm release, a
// key under another name) would lose its keys or get them back from Helm.
// Owned Secrets carry CredentialPrefix; any other Secret is only referenced,
// and its value is replaced where it is managed.
func writableModelCredential(target Namespace, secret string) error {
	if target.Scope != scopePlatform || strings.HasPrefix(secret, CredentialPrefix) {
		return nil
	}
	return invalidf("agent-manager writes only %s* Secrets in the installation namespace: replace the value of Secret %s where it is managed", CredentialPrefix, secret)
}

func (c *orgCommit) credentialDirectory(target Namespace) (layout.Directory, error) {
	if target.Scope != scopePlatform {
		return c.contentDir(target.Organization), nil
	}
	configured := c.cfg.PlatformModelsPath
	if configured == "" || path.IsAbs(configured) || path.Clean(configured) != configured || configured == "." || configured == ".." || strings.HasPrefix(configured, "../") {
		return layout.Directory{}, fmt.Errorf("%w: installation commit mode requires a trusted platform-models-path", agents.ErrUnsupported)
	}
	if within(c.cfg.ContentPrefix, configured) || within(configured, c.cfg.ContentPrefix) {
		return layout.Directory{}, invalidf("installation model directory and organization content prefix must not contain each other")
	}
	location := c.cfg.Objects
	location.Directory = path.Dir(configured)
	return layout.Directory{Location: location, Name: path.Base(configured)}, nil
}

// kagent builds the Vertex host from location and the request path from
// projectID, so both are held to Google's formats; the same patterns are in
// the Agent Platform chart's ModelConfig admission.
var (
	vertexLocation  = regexp.MustCompile(`^(global|us|eu|[a-z]+-[a-z]+[0-9]+)$`)
	googleProjectID = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
)

func vertexBlock(model *unstructured.Unstructured) string {
	if model == nil {
		return ""
	}
	provider, _, _ := unstructured.NestedString(model.Object, "spec", "provider")
	switch provider {
	case "AnthropicVertexAI":
		return "anthropicVertexAI"
	case "GeminiVertexAI":
		return "geminiVertexAI"
	}
	return ""
}

func isVertexModel(model *unstructured.Unstructured) bool { return vertexBlock(model) != "" }

func modelCredentialKey(model *unstructured.Unstructured) string {
	if isVertexModel(model) {
		return "GOOGLE_APPLICATION_CREDENTIALS"
	}
	return defaultModelKey
}

func configureModel(model *unstructured.Unstructured, request ModelRequest) error {
	provider, _, _ := unstructured.NestedString(model.Object, "spec", "provider")
	if block := vertexBlock(model); block != "" {
		if request.ProjectID != "" {
			_ = unstructured.SetNestedField(model.Object, request.ProjectID, "spec", block, "projectID")
		}
		if request.Location != "" {
			_ = unstructured.SetNestedField(model.Object, request.Location, "spec", block, "location")
		}
		project, _, _ := unstructured.NestedString(model.Object, "spec", block, "projectID")
		location, _, _ := unstructured.NestedString(model.Object, "spec", block, "location")
		if !vertexLocation.MatchString(location) {
			return invalidf("Vertex AI requires a location: global, us, eu or a Google region such as europe-west1")
		}
		if !googleProjectID.MatchString(project) {
			return invalidf("Vertex AI requires a Google project ID: 6 to 30 lowercase letters, digits or hyphens, starting with a letter")
		}
	} else if request.ProjectID != "" || request.Location != "" {
		return invalidf("projectID and location apply only to Vertex AI models")
	}
	if request.Endpoint != nil {
		if *request.Endpoint != "" {
			if err := refuseCredentialInURL(*request.Endpoint); err != nil {
				return err
			}
		}
		block, field := "", "baseUrl"
		switch provider {
		case "OpenAI":
			block = "openAI"
		case "Anthropic":
			block = "anthropic"
		case "Ollama":
			block, field = "ollama", "host"
		default:
			if *request.Endpoint != "" {
				return invalidf("endpoint is not supported for this provider")
			}
		}
		if block != "" {
			if *request.Endpoint == "" {
				unstructured.RemoveNestedField(model.Object, "spec", block, field)
			} else {
				_ = unstructured.SetNestedField(model.Object, *request.Endpoint, "spec", block, field)
			}
		}
	}
	if request.InsecureSkipTLSVerify != nil {
		_ = unstructured.SetNestedField(model.Object, *request.InsecureSkipTLSVerify, "spec", "tls", "disableVerify")
	}
	return nil
}

// validateGoogleCredential checks a service-account key for a Vertex model
// on project. A key of another project is accepted: a central project's
// service account may hold Vertex AI access in the model's project. The
// returned note names the two projects so the person can check that access.
func validateGoogleCredential(raw, project string) (string, error) {
	var key struct {
		Type        string `json:"type"`
		ProjectID   string `json:"project_id"`
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
		TokenURI    string `json:"token_uri"`
	}
	if len(raw) > 64*1024 || json.Unmarshal([]byte(raw), &key) != nil || key.Type != "service_account" || key.ProjectID == "" || key.ClientEmail == "" {
		return "", invalidf("upload a Google service-account JSON key")
	}
	if key.TokenURI != "" {
		endpoint, err := url.Parse(key.TokenURI)
		if err != nil || endpoint.Scheme != "https" || endpoint.Host != "oauth2.googleapis.com" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Path != "/token" {
			return "", invalidf("Google credentials must use https://oauth2.googleapis.com/token")
		}
	}
	block, rest := pem.Decode([]byte(key.PrivateKey))
	if block == nil || len(strings.TrimSpace(string(rest))) > 0 || block.Type != "PRIVATE KEY" {
		return "", invalidf("Google credentials require a valid RSA private key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return "", invalidf("Google credentials require a valid RSA private key")
	}
	privateKey, ok := parsed.(*rsa.PrivateKey)
	if !ok || privateKey.Validate() != nil {
		return "", invalidf("Google credentials require a valid RSA private key")
	}
	if key.ProjectID != project {
		return fmt.Sprintf("the key's service account belongs to project %s and the model calls Vertex AI in project %s: it needs Vertex AI access in %s", key.ProjectID, project, project), nil
	}
	return "", nil
}

func validateCredentialModels(operation *credentialOperation, live map[string]*unstructured.Unstructured, credential operationCredential, value string) ([]string, error) {
	models := maps.Clone(live)
	for _, planned := range operation.Writes {
		models[planned.Kind+"/"+planned.Name] = unstructuredOf(planned.Object)
	}
	var notes []string
	for _, model := range models {
		if model.GetKind() != kubeKindModelConfig || !isVertexModel(model) {
			continue
		}
		secret, _, _ := unstructured.NestedString(model.Object, "spec", "apiKeySecret")
		key, _, _ := unstructured.NestedString(model.Object, "spec", "apiKeySecretKey")
		if secret != credential.Secret || key != credential.Key {
			continue
		}
		project, _, _ := unstructured.NestedString(model.Object, "spec", vertexBlock(model), "projectID")
		note, err := validateGoogleCredential(value, project)
		if err != nil {
			return nil, err
		}
		if note != "" && !slices.Contains(notes, note) {
			notes = append(notes, note)
		}
	}
	slices.Sort(notes)
	return notes, nil
}
