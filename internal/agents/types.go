// Package agents is the domain of agent-manager: an agent on the platform is a
// kagent.dev/v1alpha3 AgentTemplate in the kagent namespace — its description,
// system prompt, model config, immutable skills and the MCP server it binds —
// plus, for the toolset it declares, a per-agent copy of the platform's muster
// RemoteMCPServer that carries the X-Muster-Toolset header (the toolset
// carrier). kagent compiles the template for every Harness whose admission
// selector matches its labels and reports readiness per Harness; instances are
// created from the compiled template over kagent's gRPC API, not here. The
// package composes both objects, validates what can be expressed on kagent
// main before anything is applied, and reads an agent back from the template
// and its carrier.
package agents

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Sentinel errors the API layers map to HTTP statuses and MCP error codes.
var (
	// ErrNotFound: the agent, its template or a ModelConfig does not exist.
	ErrNotFound = errors.New("not found")
	// ErrInvalid: the request is malformed or cannot be expressed on kagent main.
	ErrInvalid = errors.New("invalid request")
	// ErrConflict: the object exists, is owned by GitOps or by someone else —
	// a write would be refused or undone; force overrides where documented.
	ErrConflict = errors.New("conflict")
	// ErrForbidden: the Kubernetes API refused the call for the identity
	// agent-manager runs with.
	ErrForbidden = errors.New("forbidden")
	// ErrUnsupported: the operation is not available on this installation.
	ErrUnsupported = errors.New("unsupported")
	// ErrUnauthenticated: the request reached the service without the caller
	// token it needs to act (the server runs as the caller only). 401.
	ErrUnauthenticated = errors.New("unauthenticated")
)

// Labels and annotations the platform agrees on.
const (
	// DisplayNameAnnotation carries the friendly name (the portal's contract).
	DisplayNameAnnotation = "ui.giantswarm.io/display-name"
	// RequestedByAnnotation records the caller whose write produced the object.
	RequestedByAnnotation = "agent-manager.giantswarm.io/requested-by"
	// AgentLabel on a toolset carrier names the agent it belongs to.
	AgentLabel = "agent-manager.giantswarm.io/agent"
	// HarnessLabel is the label the platform's Harnesses select templates by
	// (Harness.spec.allowedAgentTemplates.selector.matchLabels).
	HarnessLabel = "kagent.dev/harness"
	// DiscoveryLabel opts a RemoteMCPServer out of controller-side tool
	// discovery (muster is an OAuth resource server the controller cannot
	// authenticate to); copied from the platform server onto every carrier.
	DiscoveryLabel = "kagent.dev/discovery"
	// KustomizationNameLabel marks an object whose desired state lives in git
	// (applied by a Flux Kustomization): a live write would be undone on the
	// next reconciliation.
	KustomizationNameLabel = "kustomize.toolkit.fluxcd.io/name"
	// ManagedByLabel / ManagedByValue mark the objects agent-manager created.
	ManagedByLabel = "app.kubernetes.io/managed-by"
	ManagedByValue = "agent-manager"
	// FieldManager is the manager every write is recorded under.
	FieldManager = "agent-manager"
)

// How an agent is managed.
const (
	// ManagedAgentManager: agent-manager created the template and writes it.
	ManagedAgentManager = "agent-manager"
	// ManagedGitOps: the template is applied by a Flux Kustomization; changes
	// belong in git (a meta agent opens a PR instead). force overrides.
	ManagedGitOps = "gitops"
	// ManagedExternal: the template was written by someone else (kubectl, a
	// chart); agent-manager overwrites it only with force.
	ManagedExternal = "external"
)

// SkillGitRef is one skill in a git repository: the repository, the
// subdirectory that is the skill, and the commit it is pinned to.
type SkillGitRef struct {
	// URL of the git repository (http or https).
	URL string `json:"url"`
	// Path is the subdirectory holding SKILL.md; empty for the repository root.
	Path string `json:"path,omitempty"`
	// Ref must be a full commit id (40 or 64 hex characters): kagent main
	// reads skills from immutable sources only. list_skills reports each
	// skill's commit.
	Ref string `json:"ref,omitempty"`
	// Name is the skill's name under the template (spec.skills[].name);
	// defaults to the last path segment, else the repository name.
	Name string `json:"name,omitempty"`
}

// Skills is what an agent mounts.
type Skills struct {
	// Refs are digest-pinned OCI references (<repository>@sha256:<digest>).
	Refs []string `json:"refs,omitempty"`
	// GitRefs are git repository skills pinned to a commit.
	GitRefs []SkillGitRef `json:"gitRefs,omitempty"`
	// RemovedGitAuthSecretName only exists so that a caller still passing
	// gitAuthSecretName is told why it is gone (kagent main reads skill
	// sources anonymously) instead of getting an "unknown field" error.
	RemovedGitAuthSecretName json.RawMessage `json:"gitAuthSecretName,omitempty"`
}

// IsEmpty reports whether no skill is referenced.
func (s *Skills) IsEmpty() bool {
	return s == nil || (len(s.Refs) == 0 && len(s.GitRefs) == 0)
}

// Spec is what a caller provides to create an agent. Everything but Name,
// ModelConfig and Toolset is optional.
type Spec struct {
	// Namespace the agent lives in; empty selects the default managed namespace.
	Namespace string `json:"namespace,omitempty"`
	// Name is the DNS-1123 technical name of the AgentTemplate. The caller
	// confirms it; agent-manager never derives it.
	Name string `json:"name"`
	// DisplayName is the friendly Unicode name (max 63 chars).
	DisplayName string `json:"displayName,omitempty"`
	// Description goes to AgentTemplate.spec.description.
	Description string `json:"description,omitempty"`
	// SystemMessage is the system prompt (spec.systemPrompt).
	SystemMessage string `json:"systemMessage,omitempty"`
	// ModelConfig names an existing kagent ModelConfig in the namespace.
	ModelConfig string `json:"modelConfig"`
	// Harness names the kagent Harness that runs the agent: the template is
	// labelled kagent.dev/harness=<harness> and a Harness whose admission
	// selector matches must exist. Empty selects the installation default.
	Harness string `json:"harness,omitempty"`
	// Skills the agent mounts (immutable sources only).
	Skills *Skills `json:"skills,omitempty"`
	// Toolset is the list of selectors that bounds which of the gateway's
	// tools the agent can use, carried as the X-Muster-Toolset header of the
	// agent's toolset carrier. Required on create; see ValidateToolset.
	Toolset []string `json:"toolset,omitempty"`
	// Labels / Annotations are merged onto the AgentTemplate (never over the
	// platform's own).
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`

	// Removed arguments: they only exist so that a caller still passing them
	// is told why they are gone (see rejectRemoved) instead of getting an
	// "unknown field" error.
	RemovedToolNames json.RawMessage `json:"toolNames,omitempty"`
	RemovedRuntime   json.RawMessage `json:"runtime,omitempty"`
	RemovedIconURL   json.RawMessage `json:"iconUrl,omitempty"`
}

// Update is a partial change to an existing agent: nil pointers leave the
// current value; a pointer to an empty value clears it. Skills replace the
// whole block; Toolset replaces the whole list (an empty list is refused: use
// preset:none).
type Update struct {
	Namespace     string             `json:"namespace,omitempty"`
	Name          string             `json:"name"`
	DisplayName   *string            `json:"displayName,omitempty"`
	Description   *string            `json:"description,omitempty"`
	SystemMessage *string            `json:"systemMessage,omitempty"`
	ModelConfig   *string            `json:"modelConfig,omitempty"`
	Harness       *string            `json:"harness,omitempty"`
	Skills        *Skills            `json:"skills,omitempty"`
	Toolset       *[]string          `json:"toolset,omitempty"`
	Labels        *map[string]string `json:"labels,omitempty"`
	Annotations   *map[string]string `json:"annotations,omitempty"`
	// Removed arguments: see Spec.
	RemovedToolNames json.RawMessage `json:"toolNames,omitempty"`
	RemovedRuntime   json.RawMessage `json:"runtime,omitempty"`
	RemovedIconURL   json.RawMessage `json:"iconUrl,omitempty"`
	// Force writes to a GitOps-owned or externally written template anyway,
	// and overwrites template fields agent-manager does not compose.
	Force bool `json:"force,omitempty"`
}

// Condition is a Kubernetes-style status condition, flattened.
type Condition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
	LastTransitionTime string `json:"lastTransitionTime,omitempty"`
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
}

// HarnessStatus is what kagent reports for the template on one admitting
// Harness (AgentTemplate.status.harnesses[]).
type HarnessStatus struct {
	Harness string `json:"harness"`
	// Ready mirrors the Ready condition; nil while unreported.
	Ready        *bool `json:"ready"`
	Accepted     *bool `json:"accepted"`
	ResolvedRefs *bool `json:"resolvedRefs"`
	Compatible   *bool `json:"compatible"`
	// DesiredRevision is the revision compiled from the current generation;
	// LatestSuccessfulRevision the last one that became ready. Instances pin
	// the revision they were created with.
	DesiredRevision          string      `json:"desiredRevision,omitempty"`
	LatestSuccessfulRevision string      `json:"latestSuccessfulRevision,omitempty"`
	Conditions               []Condition `json:"conditions,omitempty"`
	// Warnings are kagent's non-blocking compatibility decisions (compile
	// downgrades).
	Warnings []string `json:"warnings,omitempty"`
}

// ToolsetCarrier is the per-agent RemoteMCPServer that carries the toolset
// header.
type ToolsetCarrier struct {
	Name   string `json:"name"`
	Exists bool   `json:"exists"`
	// Accepted mirrors kagent's Accepted condition; nil while unreported.
	Accepted *bool  `json:"accepted"`
	Message  string `json:"message,omitempty"`
	// Header is the X-Muster-Toolset value the carrier sends.
	Header string `json:"header,omitempty"`
}

// Agent is the read model of one agent.
type Agent struct {
	Name          string `json:"name"`
	Namespace     string `json:"namespace"`
	DisplayName   string `json:"displayName,omitempty"`
	Description   string `json:"description,omitempty"`
	ModelConfig   string `json:"modelConfig,omitempty"`
	SystemMessage string `json:"systemMessage,omitempty"`
	// Harness is the kagent.dev/harness label; "" when the template carries none.
	Harness string  `json:"harness,omitempty"`
	Skills  *Skills `json:"skills,omitempty"`
	// Toolset is the toolset the agent declares: the X-Muster-Toolset header
	// of its toolset carrier. Absent when none is declared.
	Toolset []string `json:"toolset,omitempty"`
	// ImplicitFullAccess is true when the template binds the platform's
	// muster server directly, without a toolset carrier: its tools are every
	// tool the gateway exposes to the caller. Such agents still need a
	// toolset assigned (update_agent).
	ImplicitFullAccess bool            `json:"implicitFullAccess,omitempty"`
	ToolsetCarrier     *ToolsetCarrier `json:"toolsetCarrier,omitempty"`
	// Ready is true when any admitting Harness reports Ready; nil while no
	// Harness has reported.
	Ready     *bool           `json:"ready"`
	Harnesses []HarnessStatus `json:"harnesses"`
	// Managed is agent-manager, gitops or external.
	Managed            string `json:"managed"`
	Generation         int64  `json:"generation,omitempty"`
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
	// UnmanagedFields lists template fields agent-manager does not compose
	// (bucket skills, plugins, prompt templates, agent tool bindings, other
	// MCP servers): an update overwrites them only with force.
	UnmanagedFields []string `json:"unmanagedFields,omitempty"`
	// Spec is the AgentTemplate spec as served (the kagent contract).
	Spec map[string]any `json:"spec,omitempty"`
}

// ModelConfig is a kagent ModelConfig an agent can reference.
type ModelConfig struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Provider  string `json:"provider,omitempty"`
	Model     string `json:"model,omitempty"`
	// Accepted mirrors the kagent Accepted condition; nil while unreported.
	Accepted  *bool  `json:"accepted"`
	Message   string `json:"message,omitempty"`
	ManagedBy string `json:"managedBy,omitempty"`
}

// Manifests are the objects a create/update applies, as YAML.
type Manifests struct {
	AgentTemplate string `json:"agentTemplate"`
	// ToolsetCarrier is empty when the template binds the platform server
	// directly (no toolset declared).
	ToolsetCarrier string `json:"toolsetCarrier,omitempty"`
}

// ValidateResult is a dry run: nothing was written.
type ValidateResult struct {
	Valid bool `json:"valid"`
	// Mode is create or update.
	Mode string `json:"mode"`
	// Errors lists every precondition failure.
	Errors    []string  `json:"errors,omitempty"`
	Manifests Manifests `json:"manifests"`
}

// CreateResult reports what a create applied.
type CreateResult struct {
	Agent     Agent     `json:"agent"`
	Manifests Manifests `json:"manifests"`
	Created   struct {
		AgentTemplate  bool `json:"agentTemplate"`
		ToolsetCarrier bool `json:"toolsetCarrier"`
	} `json:"created"`
	Status *Status `json:"status,omitempty"`
	// RequestedBy is the authenticated caller the write ran as (email, else
	// subject); empty when the server runs without OAuth.
	RequestedBy string `json:"requestedBy,omitempty"`
}

// UpdateResult reports before/after declarations of an update.
type UpdateResult struct {
	Agent Agent `json:"agent"`
	// Before / After are the agent's declaration (displayName, description,
	// systemMessage, modelConfig, harness, skills, toolset, labels,
	// annotations) before and after the change.
	Before  map[string]any `json:"before"`
	After   map[string]any `json:"after"`
	Changed []string       `json:"changed"`
	// Note explains a consequence of the change worth telling the user.
	Note      string    `json:"note,omitempty"`
	Manifests Manifests `json:"manifests"`
	// RequestedBy is the authenticated caller the write ran as.
	RequestedBy string `json:"requestedBy,omitempty"`
}

// DeleteResult reports what a delete removed.
type DeleteResult struct {
	Name                  string `json:"name"`
	Namespace             string `json:"namespace"`
	AgentTemplateDeleted  bool   `json:"agentTemplateDeleted"`
	ToolsetCarrierDeleted bool   `json:"toolsetCarrierDeleted"`
	// ToolsetCarrierKept explains why a carrier of that name stays.
	ToolsetCarrierKept string `json:"toolsetCarrierKept,omitempty"`
	// RequestedBy is the authenticated caller the delete ran as.
	RequestedBy string `json:"requestedBy,omitempty"`
}

// Verdicts of a status check.
const (
	VerdictReady       = "ready"
	VerdictProgressing = "progressing"
	VerdictFailed      = "failed"
	VerdictUnknown     = "unknown"
)

// Status is the compact verdict of get_agent_status.
type Status struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// Verdict is ready, progressing, failed or unknown.
	Verdict string `json:"verdict"`
	// Summary is one sentence a human or an agent can act on.
	Summary        string          `json:"summary"`
	Template       *TemplateStatus `json:"template,omitempty"`
	ToolsetCarrier *ToolsetCarrier `json:"toolsetCarrier,omitempty"`
}

// TemplateStatus is the AgentTemplate's status: one entry per admitting Harness.
type TemplateStatus struct {
	Exists             bool            `json:"exists"`
	Generation         int64           `json:"generation,omitempty"`
	ObservedGeneration int64           `json:"observedGeneration,omitempty"`
	Harnesses          []HarnessStatus `json:"harnesses"`
}

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

func conflictf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrConflict, fmt.Sprintf(format, args...))
}

func notFoundf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrNotFound, fmt.Sprintf(format, args...))
}
