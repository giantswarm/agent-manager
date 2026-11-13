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

func (c *orgCommit) credentialDirectory(target Namespace) (layout.Directory, error) {
	if target.Scope != scopePlatform {
		return c.contentDir(target.Parent, target.Organization), nil
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
		if project == "" || location == "" {
			return invalidf("Vertex AI requires projectID and location (global, us, eu or a Google region)")
		}
	} else if request.ProjectID != "" || request.Location != "" {
		return invalidf("projectID and location apply only to Vertex AI models")
	}
	if request.Endpoint != nil {
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

func validateGoogleCredential(raw, project string) error {
	var key struct {
		Type        string `json:"type"`
		ProjectID   string `json:"project_id"`
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
		TokenURI    string `json:"token_uri"`
	}
	if len(raw) > 64*1024 || json.Unmarshal([]byte(raw), &key) != nil || key.Type != "service_account" || key.ProjectID == "" || key.ProjectID != project || key.ClientEmail == "" {
		return invalidf("upload a Google service-account JSON key for the model's project")
	}
	if key.TokenURI != "" {
		endpoint, err := url.Parse(key.TokenURI)
		if err != nil || endpoint.Scheme != "https" || endpoint.Host != "oauth2.googleapis.com" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Path != "/token" {
			return invalidf("Google credentials must use https://oauth2.googleapis.com/token")
		}
	}
	block, rest := pem.Decode([]byte(key.PrivateKey))
	if block == nil || len(strings.TrimSpace(string(rest))) > 0 || block.Type != "PRIVATE KEY" {
		return invalidf("Google credentials require a valid RSA private key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return invalidf("Google credentials require a valid RSA private key")
	}
	privateKey, ok := parsed.(*rsa.PrivateKey)
	if !ok || privateKey.Validate() != nil {
		return invalidf("Google credentials require a valid RSA private key")
	}
	return nil
}

func validateCredentialModels(operation *credentialOperation, live map[string]*unstructured.Unstructured, credential operationCredential, value string) error {
	models := maps.Clone(live)
	for _, planned := range operation.Writes {
		models[planned.Kind+"/"+planned.Name] = unstructuredOf(planned.Object)
	}
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
		if err := validateGoogleCredential(value, project); err != nil {
			return err
		}
	}
	return nil
}
