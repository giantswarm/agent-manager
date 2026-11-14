package organizations

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	apov1alpha1 "github.com/giantswarm/agent-platform-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/identity"
	"github.com/giantswarm/agent-manager/internal/kube"
)

// The kinds the organization tools write in an organization namespace, by
// the names the tools take.
const (
	KindModel        = "model"
	KindConnector    = "connector"
	KindEgressPolicy = "egressPolicy"
	// KindCredential is the kind of a ResourceResult that names a credential
	// Secret, written for every object that reads it and without any of them.
	KindCredential = "credential"
)

// The default keys of a credential Secret.
const (
	defaultModelKey = "apiKey"
	defaultTokenKey = "token" // #nosec G101 -- a key name, not a credential
)

type kindInfo struct {
	resource, kubeKind string
}

// The Kubernetes kinds of the objects the tools write in an organization
// namespace.
const (
	kubeKindModelConfig     = "ModelConfig"
	kubeKindRemoteMCPServer = "RemoteMCPServer"
	kubeKindEgressPolicy    = "EgressPolicy"
)

var kinds = map[string]kindInfo{
	KindModel:        {"modelconfigs", kubeKindModelConfig},
	KindConnector:    {"remotemcpservers", kubeKindRemoteMCPServer},
	KindEgressPolicy: {"egresspolicies", kubeKindEgressPolicy},
}

// ResourceResult is the answer of a model, connector or egress policy write.
type ResourceResult struct {
	Namespace string `json:"namespace"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	// Created: the object did not exist.
	Created bool `json:"created"`
	// Deleted: the object was deleted.
	Deleted bool `json:"deleted,omitempty"`
	// DefaultModel is the namespace's default model after the write
	// (models only); empty when none is marked.
	DefaultModel string `json:"defaultModel,omitempty"`
	// Unmarked are the models whose default mark the write moved away.
	Unmarked []string `json:"unmarked,omitempty"`
	// Credentials are the credential Secrets the object names, by
	// reference, with what to do about a missing one.
	Credentials         []Credential `json:"credentials,omitempty"`
	CredentialOperation string       `json:"credentialOperation,omitempty"`
	ElicitationID       string       `json:"elicitationId,omitempty"`
	Pending             bool         `json:"pending,omitempty"`
	Note                string       `json:"note,omitempty"`
	DryRun              bool         `json:"dryRun,omitempty"`
	RequestedBy         string       `json:"requestedBy,omitempty"`
	// Commit is the pull request of a write in mode commit.
	Commit *CommitResult `json:"commit,omitempty"`
}

// existing reads the objects of kind in ns as the caller.
func (s *Service) existing(ctx context.Context, c kube.Client, ns string, gvr schema.GroupVersionResource, what string) ([]unstructured.Unstructured, error) {
	list, err := c.Dynamic().Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, kubeErr(err, "list "+what+" in "+ns, wayOrganizationAdmin(ns))
	}
	return list.Items, nil
}

func find(items []unstructured.Unstructured, name string) *unstructured.Unstructured {
	for i := range items {
		if items[i].GetName() == name {
			return items[i].DeepCopy()
		}
	}
	return nil
}

func newObject(apiVersion, kind, ns, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion, "kind": kind,
		fieldMetadata: map[string]any{"name": name, fieldNamespace: ns, "labels": map[string]any{agents.ManagedByLabel: agents.ManagedByValue}},
		"spec":        map[string]any{},
	}}
}

func (s *Service) apiVersion() string { return agents.KagentAPIGroup + "/" + s.cfg.KagentAPIVersion }

// ---- models -----------------------------------------------------------------------

// ModelRequest is set_organization_model's arguments. On an existing model an
// empty field keeps its value.
type ModelRequest struct {
	Namespace             string
	Name                  string
	Provider              string
	Model                 string
	ProjectID             string
	Location              string
	Endpoint              *string
	InsecureSkipTLSVerify *bool
	NoCredential          bool
	// CredentialSecret and CredentialKey name the Secret that holds the API
	// key; default tenant-credential-<name> and apiKey.
	CredentialSecret string
	CredentialKey    string
	// Default moves the organization's default mark to this model.
	Default bool
	// Create only creates: a model of that name, live or in git, is refused
	// with ErrAlreadyExists instead of changed.
	Create bool
	DryRun bool
	// Mode is apply (default) or commit.
	Mode string
	// DisplayName is the name people see (annotation
	// agents.DisplayNameAnnotation); nil keeps it, "" removes it.
	DisplayName *string
	// MaxTokens is the token limit of an answer, the provider block's
	// maxTokens; nil keeps it, 0 removes it (the provider's default).
	MaxTokens *int64
	// PromptCaching and CacheTTL are the provider block's prompt caching
	// (Anthropic, Bedrock); nil and "" keep the stored values. A new
	// Anthropic model caches, with cacheTTL agents.DefaultCacheTTL, unless
	// PromptCaching is false.
	PromptCaching *bool
	CacheTTL      string
}

// SetModel creates or changes a ModelConfig as the caller, dry run first. The
// first model of a namespace without a default carries the default mark;
// Default moves it here. A namespace that holds two marked models has no
// default, so a new model is refused there until one is chosen.
func (s *Service) SetModel(ctx context.Context, req ModelRequest) (*ResourceResult, error) {
	if req.Name == "" {
		return nil, invalidf("name is required")
	}
	org, err := s.modelNamespace(ctx, req.Namespace)
	if err != nil {
		return nil, err
	}
	c, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ns := req.Namespace
	gvr := s.kagentGVR("modelconfigs")
	items, err := s.existing(ctx, c, ns, gvr, "ModelConfigs")
	if err != nil {
		return nil, err
	}
	w, err := s.contentWriter(ctx, org, req.Mode)
	if err != nil {
		return nil, err
	}
	if req.Create {
		if err := refuseTaken(ctx, w, items, "ModelConfig", ns, req.Name); err != nil {
			return nil, err
		}
	}
	obj, err := w.source(ctx, find(items, req.Name), "ModelConfig")
	if err != nil {
		return nil, err
	}
	res := &ResourceResult{Namespace: ns, Kind: KindModel, Name: req.Name, DryRun: req.DryRun, RequestedBy: identity.Caller(ctx)}
	if obj == nil {
		if req.Provider == "" || req.Model == "" {
			return nil, invalidf("provider and model are required for a new model (provider as kagent's ModelConfig takes it, e.g. Anthropic or OpenAI)")
		}
		obj = newObject(s.apiVersion(), "ModelConfig", ns, req.Name)
		res.Created = true
	}
	// The provider before the write: a file in git may leave it out, and then
	// the live object says what the API server defaulted it to.
	previous := storedProvider(obj, find(items, req.Name))
	if !res.Created {
		if err := refuseProviderChangeOnSameSecret(obj, previous, req); err != nil {
			return nil, err
		}
	}
	providerChanged := !res.Created && req.Provider != "" && !strings.EqualFold(req.Provider, previous)
	var carried map[string]any
	if providerChanged {
		carried = dropProviderBlocks(obj, previous, req.Provider)
	}
	if req.Provider != "" {
		_ = unstructured.SetNestedField(obj.Object, req.Provider, "spec", "provider")
	}
	if req.Model != "" {
		_ = unstructured.SetNestedField(obj.Object, req.Model, "spec", "model")
	}
	if err := configureModel(obj, req); err != nil {
		return nil, err
	}
	secret, _, _ := unstructured.NestedString(obj.Object, "spec", "apiKeySecret")
	key, _, _ := unstructured.NestedString(obj.Object, "spec", "apiKeySecretKey")
	if !res.Created && secret == "" && req.CredentialSecret == "" {
		req.NoCredential = true
	}
	if previous := find(items, req.Name); previous != nil && isVertexModel(previous) && isVertexModel(obj) {
		previousProject, _, _ := unstructured.NestedString(previous.Object, "spec", vertexBlock(previous), "projectID")
		project, _, _ := unstructured.NestedString(obj.Object, "spec", vertexBlock(obj), "projectID")
		if previousProject != project && (req.CredentialSecret == "" || req.CredentialSecret == secret) {
			return nil, invalidf("changing a Vertex project requires a new credentialSecret; its key must belong to that project")
		}
	}
	secret = firstOf(req.CredentialSecret, secret, CredentialPrefix+req.Name)
	key = firstOf(req.CredentialKey, key, modelCredentialKey(obj))
	if err := modelCredentialRef(org, secret, key); err != nil {
		return nil, err
	}
	_ = unstructured.SetNestedField(obj.Object, secret, "spec", "apiKeySecret")
	_ = unstructured.SetNestedField(obj.Object, key, "spec", "apiKeySecretKey")
	provider := firstOf(req.Provider, previous)
	if err := setModelSettings(obj, req, provider, res.Created || providerChanged, carried); err != nil {
		return nil, err
	}
	if req.NoCredential {
		if isVertexModel(obj) {
			return nil, invalidf("Vertex AI requires a service account credential")
		}
		unstructured.RemoveNestedField(obj.Object, "spec", "apiKeySecret")
		unstructured.RemoveNestedField(obj.Object, "spec", "apiKeySecretKey")
	}

	var marks []string
	for i := range items {
		if agents.IsMarkedDefault(&items[i]) && items[i].GetName() != req.Name {
			marks = append(marks, items[i].GetName())
		}
	}
	way := wayOrganizationAdmin(ns)
	var writes, unmarks []write
	switch {
	case req.Default:
		// The mark is written here first and taken off the others after: a
		// failure in between leaves two marks, which the operator reports
		// and default true fixes, never an organization without a default.
		for _, name := range marks {
			other, err := w.source(ctx, find(items, name), "ModelConfig")
			if err != nil {
				return nil, fmt.Errorf("moving the default mark takes it off %s: %w", name, err)
			}
			annotations := other.GetAnnotations()
			delete(annotations, apov1alpha1.DefaultModelConfigAnnotation)
			other.SetAnnotations(annotations)
			unmarks = append(unmarks, write{gvr: gvr, kind: "ModelConfig", op: opUpdate, obj: other.Object, ns: ns, name: name, what: "ModelConfig " + ns + "/" + name + " (default mark removed)", reason: way})
		}
		res.Unmarked = marks
		setMark(obj)
	case res.Created && len(marks) == 0:
		setMark(obj)
	case res.Created && len(marks) > 1:
		return nil, fmt.Errorf("%w: namespace %s has %d models marked as its default (%s), so it has none: choose one first (set_organization_model name <one of them> default true), then add %s", agents.ErrConflict, ns, len(marks), strings.Join(marks, ", "), req.Name)
	}
	op := opUpdate
	if res.Created {
		op = opCreate
	}
	writes = append(writes, write{gvr: gvr, kind: "ModelConfig", op: op, obj: obj.Object, ns: ns, name: req.Name, what: "ModelConfig " + ns + "/" + req.Name, reason: way})
	writes = append(writes, unmarks...)
	switch {
	case agents.IsMarkedDefault(obj):
		res.DefaultModel = req.Name
	case len(marks) == 1:
		res.DefaultModel = marks[0]
	case len(marks) > 1:
		res.Note = fmt.Sprintf("the namespace has %d models marked as its default (%s), so it has none: set_organization_model with default true on one of them", len(marks), strings.Join(marks, ", "))
	}
	if req.NoCredential {
		err = s.landResource(ctx, w, writes, res, "model-"+req.Name, "chore(models): set model "+req.Name)
		return res, createOnly(err, req.Create, "ModelConfig", ns, req.Name)
	}
	cred, err := s.credential(ctx, c, org, "ModelConfig", req.Name, secret, key)
	if err != nil {
		return nil, err
	}
	if cred.State == CredentialPresent && modelKeyNotFound(find(items, req.Name), secret, key) {
		cred.State = CredentialMissing
		cred.Next = s.next(cred, "ModelConfig "+req.Name)
	}
	res.Credentials = []Credential{cred}
	if isVertexModel(obj) {
		res.Credentials[0].InputFormat = "google-service-account-json"
	}
	if err = s.landResource(ctx, w, writes, res, "model-"+req.Name, "chore(organizations): set model "+req.Name); err != nil {
		return nil, createOnly(err, req.Create, "ModelConfig", ns, req.Name)
	}
	if !req.DryRun && res.Commit == nil && res.CredentialOperation == "" {
		s.log.Info("organization model written", identity.LogAttr(ctx), fieldNamespace, ns, "name", req.Name, "created", res.Created, "default", agents.IsMarkedDefault(obj))
	}
	return res, nil
}

// refuseProviderChangeOnSameSecret refuses a change of the provider of the
// existing model obj (live, or its file in git in commit mode) that keeps its
// credential Secret: the Secret holds the old provider's key, and no order of
// a provider write and a key write is safe, as one of the two providers
// receives the other's key for a time. A new Secret is safe: when it does not
// exist, the save writes it first and then the provider and the reference in
// one write of the model. A model that names no Secret has no key to send to
// another provider, so its change is not refused; a provider that differs only
// in case is no change here, and admission judges its spelling. current is
// the stored provider (storedProvider): a file in git that leaves it out has
// the one the API server defaulted live.
func refuseProviderChangeOnSameSecret(obj *unstructured.Unstructured, current string, req ModelRequest) error {
	if req.Provider == "" || current == "" || strings.EqualFold(req.Provider, current) {
		return nil
	}
	secret, _, _ := unstructured.NestedString(obj.Object, "spec", "apiKeySecret")
	if secret == "" || firstOf(req.CredentialSecret, secret) != secret {
		return nil
	}
	return fmt.Errorf("%w: ModelConfig %s/%s sends the key in credential Secret %s to %s; a change of its provider to %s that keeps that Secret sends one provider's key to the other for a time, whatever the order of the provider and the key. Give a new credentialSecret that does not exist yet (you enter the %s key, and the save writes the Secret before it changes the provider and the reference), or create a new model",
		agents.ErrConflict, req.Namespace, req.Name, secret, current, req.Provider, req.Provider)
}

// setModelSettings writes the display name, the token limit and the prompt
// caching of req into the model obj, in the block of its provider (after
// the write). A setting req omits keeps its stored value. fresh says the
// provider's block is new (a new model, or a provider change): a new
// Anthropic block caches by default, as the platform's models do, and the
// token limit of the old block, carried, moves into it when req gives none.
// Caching that is on gets agents.DefaultCacheTTL when no cache time is
// stored or given. A setting the provider's block does not have is refused:
// the API server would prune it and the model would run without it in
// silence. The values themselves (the cacheTTL enum, a minimum) are judged
// by the API server's dry run.
func setModelSettings(obj *unstructured.Unstructured, req ModelRequest, provider string, fresh bool, carried map[string]any) error {
	if req.DisplayName != nil {
		annotations := obj.GetAnnotations()
		if *req.DisplayName == "" {
			delete(annotations, agents.DisplayNameAnnotation)
		} else {
			if annotations == nil {
				annotations = map[string]string{}
			}
			annotations[agents.DisplayNameAnnotation] = *req.DisplayName
		}
		obj.SetAnnotations(annotations)
	}
	block, _ := agents.SettingsBlockOf(provider)
	if limit := agents.IntValue(carried["maxTokens"]); limit > 0 && req.MaxTokens == nil && block.MaxTokens {
		_ = unstructured.SetNestedField(obj.Object, limit, "spec", block.Key, "maxTokens")
	}
	if req.MaxTokens != nil {
		switch {
		case *req.MaxTokens < 0:
			return invalidf("maxTokens must be 0 (the provider's default) or more, not %d", *req.MaxTokens)
		case !block.MaxTokens:
			return invalidf("provider %s has no maxTokens in kagent's ModelConfig (Anthropic, AnthropicVertexAI, AzureOpenAI, Mistral and OpenAI have one): leave maxTokens out", provider)
		case *req.MaxTokens == 0:
			removeBlockField(obj, block.Key, "maxTokens")
		default:
			_ = unstructured.SetNestedField(obj.Object, *req.MaxTokens, "spec", block.Key, "maxTokens")
		}
	}
	caching := req.PromptCaching
	if caching == nil && fresh && agents.IsAnthropic(provider) {
		on := true
		caching = &on
	}
	if (caching != nil || req.CacheTTL != "") && !block.Caching {
		return invalidf("provider %s has no promptCaching or cacheTTL in kagent's ModelConfig (Anthropic and Bedrock have them): leave them out", provider)
	}
	if caching != nil {
		_ = unstructured.SetNestedField(obj.Object, *caching, "spec", block.Key, "promptCaching")
	}
	ttl := req.CacheTTL
	if stored, _, _ := unstructured.NestedString(obj.Object, "spec", block.Key, "cacheTTL"); ttl == "" && stored == "" && caching != nil && *caching {
		ttl = agents.DefaultCacheTTL
	}
	if ttl != "" {
		_ = unstructured.SetNestedField(obj.Object, ttl, "spec", block.Key, "cacheTTL")
	}
	return nil
}

// storedProvider is the provider of the model obj (live, or its file in git)
// before the write; a file that leaves it out has the one the API server
// defaulted on the live object, live.
func storedProvider(obj, live *unstructured.Unstructured) string {
	provider, _, _ := unstructured.NestedString(obj.Object, "spec", "provider")
	if provider == "" && live != nil {
		provider, _, _ = unstructured.NestedString(live.Object, "spec", "provider")
	}
	return provider
}

// dropProviderBlocks removes from the model obj every provider block but
// the one of provider: kagent's ModelConfig refuses a block of another
// provider than spec.provider, so the old provider's block goes with a
// provider change. It answers the old provider's block, whose settings the
// new block may take.
func dropProviderBlocks(obj *unstructured.Unstructured, previous, provider string) map[string]any {
	old, _, _ := unstructured.NestedMap(obj.Object, "spec", agents.ProviderBlockKey(previous))
	keep := agents.ProviderBlockKey(provider)
	for _, key := range agents.ProviderBlockKeys() {
		if key != keep {
			unstructured.RemoveNestedField(obj.Object, "spec", key)
		}
	}
	return old
}

// removeBlockField removes field from the provider block key of obj, and the
// block when nothing is left in it.
func removeBlockField(obj *unstructured.Unstructured, key, field string) {
	unstructured.RemoveNestedField(obj.Object, "spec", key, field)
	if rest, found, _ := unstructured.NestedMap(obj.Object, "spec", key); found && len(rest) == 0 {
		unstructured.RemoveNestedField(obj.Object, "spec", key)
	}
}

// ErrAlreadyExists refuses a create of an object that exists by then, live or
// as its file in git: the name is taken. It is not ErrConflict, so a client
// tells it from the other refusals by its code.
var ErrAlreadyExists = errors.New("already exists")

// nameTaken marks a refusal, keeping its text and code, because the object a
// write creates exists by then: its file is in git, or it is live (the save's
// check sees it, or the create itself refuses it).
// A tool call with create, and a save, answer ErrAlreadyExists for it.
type nameTaken struct{ err error }

func (e nameTaken) Error() string { return e.err.Error() }
func (e nameTaken) Unwrap() error { return e.err }

// alreadyExists refuses a create-only write of kind ns/name.
func alreadyExists(kind, ns, name string) error {
	return fmt.Errorf("%w: %s %s/%s exists already: pick another name, or call without create to change or replace it", ErrAlreadyExists, kind, ns, name)
}

// refuseTaken refuses a create-only write of kind ns/name when an object of
// that name is live (items) or, in commit mode, its file is in git, a dry run
// included.
func refuseTaken(ctx context.Context, w *contentWriter, items []unstructured.Unstructured, kind, ns, name string) error {
	taken := find(items, name) != nil
	if !taken && w.commit != nil {
		var err error
		if taken, err = w.commit.exists(ctx, contentFile(w.dir, kind, name)); err != nil {
			return err
		}
	}
	if taken {
		return alreadyExists(kind, ns, name)
	}
	return nil
}

// createOnly is err of a write of kind ns/name; with create, an object of
// that name made since the read, live (the create itself refuses it) or as
// its file in git, answers ErrAlreadyExists too.
func createOnly(err error, create bool, kind, ns, name string) error {
	if create && errors.As(err, new(nameTaken)) {
		return alreadyExists(kind, ns, name)
	}
	return err
}

func setMark(obj *unstructured.Unstructured) {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[apov1alpha1.DefaultModelConfigAnnotation] = "true"
	obj.SetAnnotations(annotations)
}

func firstOf(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// ---- connectors -------------------------------------------------------------------

// ConnectorRequest is set_organization_connector's arguments. On an existing
// connector an empty field keeps its value.
type ConnectorRequest struct {
	Namespace   string
	Name        string
	URL         string
	Description string
	// CredentialHeader, CredentialSecret and CredentialKey name the request
	// header the egress gateway adds and the Secret its whole value comes
	// from; any of them sets the credential (defaults Authorization,
	// tenant-credential-<name>, token).
	CredentialHeader string
	CredentialSecret string
	CredentialKey    string
	// NoCredential removes the Secret-backed headers.
	NoCredential bool
	// Create only creates: a connector of that name, live or in git, is
	// refused with ErrAlreadyExists instead of changed.
	Create bool
	DryRun bool
	// Mode is apply (default) or commit.
	Mode string
}

// ConnectorCredential is a Secret-backed header of a connector.
type ConnectorCredential struct {
	Header string `json:"header"`
	Secret string `json:"secret"`
	Key    string `json:"key"`
}

// Connector is a RemoteMCPServer of an organization.
type Connector struct {
	Name        string                `json:"name"`
	URL         string                `json:"url"`
	Description string                `json:"description,omitempty"`
	Credentials []ConnectorCredential `json:"credentials"`
	// Curated: a copy agent-platform-operator keeps; read-only.
	Curated   bool   `json:"curated,omitempty"`
	ManagedBy string `json:"managedBy,omitempty"`
}

func connectorCredentials(u *unstructured.Unstructured) []ConnectorCredential {
	raw, _, _ := unstructured.NestedSlice(u.Object, "spec", "headersFrom")
	out := []ConnectorCredential{}
	for _, v := range raw {
		h, _ := v.(map[string]any)
		from, _ := h["valueFrom"].(map[string]any)
		if from == nil || from["type"] != "Secret" {
			continue
		}
		cc := ConnectorCredential{}
		cc.Header, _ = h["name"].(string)
		cc.Secret, _ = from["name"].(string)
		cc.Key, _ = from["key"].(string)
		out = append(out, cc)
	}
	return out
}

// ListConnectors lists the RemoteMCPServers of an organization namespace as
// the caller, each credential by reference.
func (s *Service) ListConnectors(ctx context.Context, ns string) ([]Connector, error) {
	if _, err := s.organization(ctx, ns); err != nil {
		return nil, err
	}
	c, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	items, err := s.existing(ctx, c, ns, s.kagentGVR("remotemcpservers"), "RemoteMCPServers")
	if err != nil {
		return nil, err
	}
	out := make([]Connector, 0, len(items))
	for i := range items {
		u := &items[i]
		conn := Connector{Name: u.GetName(), Credentials: connectorCredentials(u), ManagedBy: u.GetLabels()[agents.ManagedByLabel]}
		raw, _, _ := unstructured.NestedString(u.Object, "spec", "url")
		conn.URL = redactURL(raw)
		conn.Description, _, _ = unstructured.NestedString(u.Object, "spec", "description")
		_, conn.Curated = u.GetLabels()[agents.OperatorObjectLabel]
		out = append(out, conn)
	}
	slices.SortFunc(out, func(a, b Connector) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// SetConnector creates or changes a RemoteMCPServer as the caller, dry run
// first. Its credential is a Secret-backed header the egress gateway adds.
func (s *Service) SetConnector(ctx context.Context, req ConnectorRequest) (*ResourceResult, error) {
	if req.Name == "" {
		return nil, invalidf("name is required")
	}
	withCredential := req.CredentialHeader != "" || req.CredentialSecret != "" || req.CredentialKey != ""
	if withCredential && req.NoCredential {
		return nil, invalidf("noCredential and a credential field: pick one")
	}
	org, err := s.organization(ctx, req.Namespace)
	if err != nil {
		return nil, err
	}
	c, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ns := req.Namespace
	gvr := s.kagentGVR("remotemcpservers")
	items, err := s.existing(ctx, c, ns, gvr, "RemoteMCPServers")
	if err != nil {
		return nil, err
	}
	w, err := s.contentWriter(ctx, org, req.Mode)
	if err != nil {
		return nil, err
	}
	if req.Create {
		if err := refuseTaken(ctx, w, items, "RemoteMCPServer", ns, req.Name); err != nil {
			return nil, err
		}
	}
	obj, err := w.source(ctx, find(items, req.Name), "RemoteMCPServer")
	if err != nil {
		return nil, err
	}
	res := &ResourceResult{Namespace: ns, Kind: KindConnector, Name: req.Name, DryRun: req.DryRun, RequestedBy: identity.Caller(ctx)}
	if obj == nil {
		if req.URL == "" {
			return nil, invalidf("url is required for a new connector")
		}
		obj = newObject(s.apiVersion(), "RemoteMCPServer", ns, req.Name)
		res.Created = true
	}
	if req.URL != "" {
		if err := refuseCredentialInURL(req.URL); err != nil {
			return nil, err
		}
		_ = unstructured.SetNestedField(obj.Object, req.URL, "spec", "url")
	}
	if desc, _, _ := unstructured.NestedString(obj.Object, "spec", "description"); req.Description != "" || desc == "" {
		_ = unstructured.SetNestedField(obj.Object, firstOf(req.Description, "MCP server "+req.Name), "spec", "description")
	}
	if withCredential || req.NoCredential {
		raw, _, _ := unstructured.NestedSlice(obj.Object, "spec", "headersFrom")
		secretBacked := connectorCredentials(obj)
		// The header a credential field sets: the one named, else the only
		// Secret-backed one there is, else Authorization. Every other header
		// stays; noCredential removes the Secret-backed ones.
		header := req.CredentialHeader
		if header == "" && len(secretBacked) == 1 {
			header = secretBacked[0].Header
		}
		header = firstOf(header, "Authorization")
		var prev ConnectorCredential
		kept := make([]any, 0, len(raw)+1)
		for _, v := range raw {
			h, _ := v.(map[string]any)
			from, _ := h["valueFrom"].(map[string]any)
			isSecret := from != nil && from["type"] == "Secret"
			name, _ := h["name"].(string)
			if isSecret && (req.NoCredential || strings.EqualFold(name, header)) {
				prev.Secret, _ = from["name"].(string)
				prev.Key, _ = from["key"].(string)
				continue
			}
			kept = append(kept, v)
		}
		if withCredential {
			cred := ConnectorCredential{
				Header: header,
				Secret: firstOf(req.CredentialSecret, prev.Secret, CredentialPrefix+req.Name),
				Key:    firstOf(req.CredentialKey, prev.Key, defaultTokenKey),
			}
			if err := credentialRef(cred.Secret, cred.Key); err != nil {
				return nil, err
			}
			kept = append(kept, map[string]any{"name": cred.Header, "valueFrom": map[string]any{"type": "Secret", "name": cred.Secret, "key": cred.Key}})
		}
		if len(kept) == 0 {
			unstructured.RemoveNestedField(obj.Object, "spec", "headersFrom")
		} else {
			_ = unstructured.SetNestedSlice(obj.Object, kept, "spec", "headersFrom")
		}
	}
	op := opUpdate
	if res.Created {
		op = opCreate
	}
	wr := write{gvr: gvr, kind: "RemoteMCPServer", op: op, obj: obj.Object, ns: ns, name: req.Name, what: "RemoteMCPServer " + ns + "/" + req.Name, reason: wayOrganizationAdmin(ns)}
	seenCredentials := map[string]bool{}
	for _, cc := range connectorCredentials(obj) {
		pair := cc.Secret + "/" + cc.Key
		if seenCredentials[pair] {
			continue
		}
		seenCredentials[pair] = true
		cred, err := s.credential(ctx, c, org, "RemoteMCPServer", req.Name, cc.Secret, cc.Key)
		if err != nil {
			return nil, err
		}
		res.Credentials = append(res.Credentials, cred)
	}
	if err = s.landResource(ctx, w, []write{wr}, res, "connector-"+req.Name, "chore(organizations): set connector "+req.Name); err != nil {
		return nil, createOnly(err, req.Create, "RemoteMCPServer", ns, req.Name)
	}
	if !req.DryRun && res.Commit == nil && res.CredentialOperation == "" {
		s.log.Info("organization connector written", identity.LogAttr(ctx), fieldNamespace, ns, "name", req.Name, "created", res.Created)
	}
	return res, nil
}

// ---- egress policies --------------------------------------------------------------

// EgressDestination is one origin of an egress policy, with the credential
// the egress gateway adds there, by reference.
type EgressDestination struct {
	Origin string `json:"origin"`
	// CredentialHeader is the request header the gateway adds; empty: the
	// origin is reached without a credential.
	CredentialHeader string `json:"credentialHeader,omitempty"`
	// CredentialPrefix goes before the value (e.g. "Bearer ").
	CredentialPrefix string `json:"credentialPrefix,omitempty"`
	// CredentialSecret and CredentialKey name the Secret the value comes
	// from; default tenant-credential-<policy> and token.
	CredentialSecret string `json:"credentialSecret,omitempty"`
	CredentialKey    string `json:"credentialKey,omitempty"`
}

func egressDestinations(u *unstructured.Unstructured) []EgressDestination {
	raw, _, _ := unstructured.NestedSlice(u.Object, "spec", "destinations")
	out := []EgressDestination{}
	for _, v := range raw {
		d, _ := v.(map[string]any)
		dest := EgressDestination{}
		dest.Origin, _ = d["origin"].(string)
		if auth, _ := d["authentication"].(map[string]any); auth != nil {
			dest.CredentialHeader, _ = auth["header"].(string)
			dest.CredentialPrefix, _ = auth["prefix"].(string)
			if ref, _ := auth["secretKeyRef"].(map[string]any); ref != nil {
				dest.CredentialSecret, _ = ref["name"].(string)
				dest.CredentialKey, _ = ref["key"].(string)
			}
		}
		out = append(out, dest)
	}
	return out
}

// EgressRequest is set_organization_egress_policy's arguments: the whole
// destination list.
type EgressRequest struct {
	Namespace    string
	Name         string
	Destinations []EgressDestination
	// Create only creates: an egress policy of that name, live or in git, is
	// refused with ErrAlreadyExists instead of replaced.
	Create bool
	DryRun bool
	// Mode is apply (default) or commit.
	Mode string
}

// SetEgressPolicy creates or replaces an EgressPolicy as the caller, dry run
// first.
func (s *Service) SetEgressPolicy(ctx context.Context, req EgressRequest) (*ResourceResult, error) {
	if req.Name == "" {
		return nil, invalidf("name is required")
	}
	if len(req.Destinations) == 0 {
		return nil, invalidf("destinations is required: the origins the policy lets an agent reach")
	}
	org, err := s.organization(ctx, req.Namespace)
	if err != nil {
		return nil, err
	}
	c, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ns := req.Namespace
	gvr := s.kagentGVR("egresspolicies")
	items, err := s.existing(ctx, c, ns, gvr, "EgressPolicies")
	if err != nil {
		return nil, err
	}
	w, err := s.contentWriter(ctx, org, req.Mode)
	if err != nil {
		return nil, err
	}
	if req.Create {
		if err := refuseTaken(ctx, w, items, "EgressPolicy", ns, req.Name); err != nil {
			return nil, err
		}
	}
	obj, err := w.source(ctx, find(items, req.Name), "EgressPolicy")
	if err != nil {
		return nil, err
	}
	res := &ResourceResult{Namespace: ns, Kind: KindEgressPolicy, Name: req.Name, DryRun: req.DryRun, RequestedBy: identity.Caller(ctx)}
	if obj == nil {
		obj = newObject(s.apiVersion(), "EgressPolicy", ns, req.Name)
		res.Created = true
	}
	dests := make([]any, 0, len(req.Destinations))
	for _, d := range req.Destinations {
		if d.Origin == "" {
			return nil, invalidf("every destination needs an origin")
		}
		entry := map[string]any{"origin": d.Origin}
		if d.CredentialHeader != "" {
			secret := firstOf(d.CredentialSecret, CredentialPrefix+req.Name)
			key := firstOf(d.CredentialKey, defaultTokenKey)
			if err := credentialRef(secret, key); err != nil {
				return nil, err
			}
			auth := map[string]any{"header": d.CredentialHeader, "secretKeyRef": map[string]any{"name": secret, "key": key}}
			if d.CredentialPrefix != "" {
				auth["prefix"] = d.CredentialPrefix
			}
			entry["authentication"] = auth
		} else if d.CredentialSecret != "" || d.CredentialKey != "" || d.CredentialPrefix != "" {
			return nil, invalidf("destination %s: credentialHeader names where the credential goes; without it the origin is reached without one", d.Origin)
		}
		dests = append(dests, entry)
	}
	_ = unstructured.SetNestedSlice(obj.Object, dests, "spec", "destinations")
	op := opUpdate
	if res.Created {
		op = opCreate
	}
	wr := write{gvr: gvr, kind: "EgressPolicy", op: op, obj: obj.Object, ns: ns, name: req.Name, what: "EgressPolicy " + ns + "/" + req.Name, reason: wayOrganizationAdmin(ns)}
	seen := map[string]bool{}
	for _, d := range egressDestinations(obj) {
		if d.CredentialSecret == "" || seen[d.CredentialSecret+"/"+d.CredentialKey] {
			continue
		}
		seen[d.CredentialSecret+"/"+d.CredentialKey] = true
		cred, err := s.credential(ctx, c, org, "EgressPolicy", req.Name, d.CredentialSecret, d.CredentialKey)
		if err != nil {
			return nil, err
		}
		res.Credentials = append(res.Credentials, cred)
	}
	if err = s.landResource(ctx, w, []write{wr}, res, "egress-"+req.Name, "chore(organizations): set egress policy "+req.Name); err != nil {
		return nil, createOnly(err, req.Create, "EgressPolicy", ns, req.Name)
	}
	if !req.DryRun && res.Commit == nil && res.CredentialOperation == "" {
		s.log.Info("organization egress policy written", identity.LogAttr(ctx), fieldNamespace, ns, "name", req.Name, "created", res.Created)
	}
	return res, nil
}

// ---- delete -----------------------------------------------------------------------

// DeleteResource deletes a model, connector or egress policy of an
// organization namespace as the caller, dry run first. Its credential
// Secret stays: another object may name it.
func (s *Service) DeleteResource(ctx context.Context, ns, kind, name string, dryRun bool, mode string) (*ResourceResult, error) {
	info, ok := kinds[kind]
	if !ok {
		return nil, invalidf("kind %q: %s, %s or %s", kind, KindModel, KindConnector, KindEgressPolicy)
	}
	if name == "" {
		return nil, invalidf("name is required")
	}
	var org Namespace
	var err error
	if kind == KindModel {
		org, err = s.modelNamespace(ctx, ns)
	} else {
		org, err = s.organization(ctx, ns)
	}
	if err != nil {
		return nil, err
	}
	w, err := s.contentWriter(ctx, org, mode)
	if err != nil {
		return nil, err
	}
	c, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	gvr := s.kagentGVR(info.resource)
	what := info.kubeKind + " " + ns + "/" + name
	obj, err := c.Dynamic().Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, kubeErr(err, "get "+what, wayOrganizationAdmin(ns))
	}
	if _, err := w.source(ctx, obj, info.kubeKind); err != nil {
		return nil, err
	}
	wr := write{gvr: gvr, kind: info.kubeKind, op: opDelete, ns: ns, name: name, what: what, reason: wayOrganizationAdmin(ns)}
	committed, err := w.land(ctx, s, []write{wr}, dryRun, "delete-"+strings.ToLower(info.kubeKind)+"-"+name, "chore(organizations): remove "+kind+" "+name)
	if err != nil {
		return nil, err
	}
	res := &ResourceResult{Namespace: ns, Kind: kind, Name: name, Deleted: !dryRun && committed == nil, DryRun: dryRun, RequestedBy: identity.Caller(ctx),
		Note: "its credential Secret, if any, stays: another object may name it", Commit: committed}
	if kind == KindModel && agents.IsMarkedDefault(obj) {
		res.Note = "it was the organization's default model, so the organization has none now: set_organization_model with default true on another; " + res.Note
	}
	if !dryRun && committed == nil {
		s.log.Info("organization object deleted", identity.LogAttr(ctx), fieldNamespace, ns, "kind", kind, "name", name)
	}
	return res, nil
}

// credentialQuery matches a query parameter name that carries a credential.
var credentialQuery = regexp.MustCompile(`(?i)(key|token|secret|password|passwd|auth|sig|credential)`)

// refuseCredentialInURL refuses a destination URL that carries a credential:
// userinfo, or a query parameter named like one. A URL or a query that does
// not parse is refused too: what it hides cannot be checked. A credential
// is referenced through a Secret, and no refusal
// echoes the URL or its parse error, which would carry it back.
func refuseCredentialInURL(raw string) error {
	const way = "credentials require a Secret reference, never a value in the URL"
	u, err := url.Parse(raw)
	if err != nil {
		return invalidf("url: not a valid URL (the URL is not repeated here: it may carry a credential)")
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return invalidf("url: requires an absolute HTTP(S) URL")
	}
	if u.User != nil {
		return invalidf("url: carries user information; %s", way)
	}
	if u.Fragment != "" || u.RawFragment != "" {
		return invalidf("url: carries a fragment (#...), which can hold a credential; %s", way)
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return invalidf("url: its query does not parse as name=value pairs joined by &, so it cannot be checked for a credential; %s", way)
	}
	for name := range query {
		if credentialQuery.MatchString(name) {
			return invalidf("url: a query parameter looks like a credential; %s", way)
		}
	}
	return nil
}

// redactURL hides what refuseCredentialInURL refuses, in a URL written
// elsewhere (git, kubectl), so a read never returns it: userinfo, a query
// parameter named like a credential, a whole query that does not parse, and
// the fragment.
// A URL that does not parse is not returned at all.
func redactURL(raw string) string {
	const redacted = "REDACTED"
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if u.User != nil {
		u.User = url.User(redacted)
	}
	u.Fragment, u.RawFragment = "", ""
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		u.RawQuery = redacted
		return u.String()
	}
	changed := false
	for name := range query {
		if credentialQuery.MatchString(name) {
			query.Set(name, redacted)
			changed = true
		}
	}
	if changed {
		u.RawQuery = query.Encode()
	}
	return u.String()
}
