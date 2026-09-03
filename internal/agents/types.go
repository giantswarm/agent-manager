// Package agents is the domain of agent-manager: an agent on the platform is a
// Flux HelmRelease of the agent chart (one release renders one kagent Agent),
// sharing a per-namespace OCIRepository that tracks the chart. The package
// composes those two objects exactly like the portal's create flow, validates
// the values against the chart's schema before anything is applied, and reads
// the agent back from the Agent CR, its owning HelmRelease and the workload
// kagent runs for it.
package agents

import (
	"errors"
	"fmt"
)

// Sentinel errors the API layers map to HTTP statuses and MCP error codes.
var (
	// ErrNotFound: the agent, HelmRelease or ModelConfig does not exist.
	ErrNotFound = errors.New("not found")
	// ErrInvalid: the request is malformed or fails the chart's values schema.
	ErrInvalid = errors.New("invalid request")
	// ErrConflict: the object exists, is owned by GitOps, is suspended, or is
	// a bare Agent CR — a write would be refused or undone; force overrides
	// where documented.
	ErrConflict = errors.New("conflict")
	// ErrForbidden: the Kubernetes API refused the write for the identity
	// agent-manager runs with.
	ErrForbidden = errors.New("forbidden")
	// ErrUnsupported: the operation is not available on this installation.
	ErrUnsupported = errors.New("unsupported")
)

// Labels and annotations the platform agrees on.
const (
	// DisplayNameAnnotation is the agent chart's contract for the friendly name.
	DisplayNameAnnotation = "ui.giantswarm.io/display-name"
	// HelmReleaseNameLabel / HelmReleaseNamespaceLabel are the Flux provenance
	// labels helm-controller stamps on every object a release renders.
	HelmReleaseNameLabel      = "helm.toolkit.fluxcd.io/name"
	HelmReleaseNamespaceLabel = "helm.toolkit.fluxcd.io/namespace"
	// KustomizationNameLabel marks a HelmRelease whose desired state lives in
	// git (applied by a Flux Kustomization): a live write would be undone on
	// the next reconciliation.
	KustomizationNameLabel = "kustomize.toolkit.fluxcd.io/name"
	// ManagedByLabel / ManagedByValue mark the HelmReleases and
	// OCIRepositories agent-manager created.
	ManagedByLabel = "app.kubernetes.io/managed-by"
	ManagedByValue = "agent-manager"
)

// kindOCIRepository is the chartRef kind every composed HelmRelease uses.
const kindOCIRepository = "OCIRepository"

// How an agent is managed.
const (
	// ManagedHelmRelease: a HelmRelease owns the agent and can be written live.
	ManagedHelmRelease = "helmrelease"
	// ManagedGitOps: the owning HelmRelease is applied by a Flux Kustomization;
	// changes belong in git (the meta agent opens a PR instead).
	ManagedGitOps = "gitops"
	// ManagedNone: a bare Agent CR with no HelmRelease behind it.
	ManagedNone = "none"
)

// SkillGitRef is one kagent spec.skills.gitRefs entry (chart values
// skills.gitRefs[]): a git repository plus the subdirectory that is the skill.
type SkillGitRef struct {
	// URL of the git repository.
	URL string `json:"url"`
	// Path is the subdirectory holding SKILL.md; empty for the repository root.
	Path string `json:"path,omitempty"`
	// Ref is a branch, tag or commit; empty means the default branch.
	Ref string `json:"ref,omitempty"`
	// Name is the directory the skill is mounted under (/skills/<name>);
	// defaults to the last path segment, else the repository name.
	Name string `json:"name,omitempty"`
}

// Skills mirrors the agent chart's skills block.
type Skills struct {
	// Refs are OCI skill image references.
	Refs []string `json:"refs,omitempty"`
	// GitRefs are git repository references.
	GitRefs []SkillGitRef `json:"gitRefs,omitempty"`
	// GitAuthSecretName names a Secret in the agent's namespace for private
	// gitRefs (key token, or a kubernetes.io/ssh-auth secret).
	GitAuthSecretName string `json:"gitAuthSecretName,omitempty"`
}

// IsEmpty reports whether no skill is referenced.
func (s *Skills) IsEmpty() bool {
	return s == nil || (len(s.Refs) == 0 && len(s.GitRefs) == 0)
}

// Spec is what a caller provides to create an agent. Everything but Name and
// ModelConfig is optional; omitted fields keep the chart's defaults.
type Spec struct {
	// Namespace the agent lives in; empty selects the default managed namespace.
	Namespace string `json:"namespace,omitempty"`
	// Name is the DNS-1123 technical name: the HelmRelease name and the Agent
	// name. The caller confirms it; agent-manager never derives it.
	Name string `json:"name"`
	// DisplayName is the friendly Unicode name (max 63 chars).
	DisplayName string `json:"displayName,omitempty"`
	// Description goes to Agent.spec.description.
	Description string `json:"description,omitempty"`
	// SystemMessage is the system prompt; empty keeps the chart default.
	SystemMessage string `json:"systemMessage,omitempty"`
	// ModelConfig names an existing kagent ModelConfig in the namespace.
	ModelConfig string `json:"modelConfig"`
	// IconURL is the avatar URL (chart agent.iconUrl).
	IconURL string `json:"iconUrl,omitempty"`
	// Runtime is go or python; empty keeps the chart default (go).
	Runtime string `json:"runtime,omitempty"`
	// Skills the agent mounts.
	Skills *Skills `json:"skills,omitempty"`
	// ToolNames narrows the muster tools; empty means every tool the gateway
	// exposes (chart default).
	ToolNames []string `json:"toolNames,omitempty"`
	// Labels / Annotations are merged onto the Agent by the chart.
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Update is a partial change to an existing agent: nil pointers leave the
// current value; a pointer to an empty value clears it (falls back to the
// chart default). Skills and ToolNames replace the whole block.
type Update struct {
	Namespace     string             `json:"namespace,omitempty"`
	Name          string             `json:"name"`
	DisplayName   *string            `json:"displayName,omitempty"`
	Description   *string            `json:"description,omitempty"`
	SystemMessage *string            `json:"systemMessage,omitempty"`
	ModelConfig   *string            `json:"modelConfig,omitempty"`
	IconURL       *string            `json:"iconUrl,omitempty"`
	Runtime       *string            `json:"runtime,omitempty"`
	Skills        *Skills            `json:"skills,omitempty"`
	ToolNames     *[]string          `json:"toolNames,omitempty"`
	Labels        *map[string]string `json:"labels,omitempty"`
	Annotations   *map[string]string `json:"annotations,omitempty"`
	// Force writes to a GitOps-owned or suspended HelmRelease anyway.
	Force bool `json:"force,omitempty"`
}

// Condition is a Kubernetes-style status condition, flattened.
type Condition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
	LastTransitionTime string `json:"lastTransitionTime,omitempty"`
}

// ChartRef is a HelmRelease's spec.chartRef.
type ChartRef struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

// HelmReleaseRef is what an agent view says about its owning release.
type HelmReleaseRef struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// Ready mirrors the Ready condition; nil while unreported.
	Ready   *bool  `json:"ready"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	// ChartVersion is the deployed chart version (last history entry), else
	// the last attempted revision.
	ChartVersion          string `json:"chartVersion,omitempty"`
	LastAttemptedRevision string `json:"lastAttemptedRevision,omitempty"`
	Suspended             bool   `json:"suspended"`
	GitOpsOwned           bool   `json:"gitOpsOwned"`
	// Deleting is true while helm-controller uninstalls the release (the
	// HelmRelease carries a deletionTimestamp); the Agent disappears with it.
	Deleting bool      `json:"deleting"`
	ChartRef *ChartRef `json:"chartRef,omitempty"`
}

// Agent is the read model of one agent.
type Agent struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// Exists is false while the HelmRelease has not rendered the Agent CR yet
	// (or failed to).
	Exists        bool     `json:"exists"`
	DisplayName   string   `json:"displayName,omitempty"`
	Description   string   `json:"description,omitempty"`
	ModelConfig   string   `json:"modelConfig,omitempty"`
	Runtime       string   `json:"runtime,omitempty"`
	IconURL       string   `json:"iconUrl,omitempty"`
	SystemMessage string   `json:"systemMessage,omitempty"`
	Skills        *Skills  `json:"skills,omitempty"`
	ToolNames     []string `json:"toolNames,omitempty"`
	// Ready / Accepted mirror the Agent CR's conditions; nil while unreported
	// or when the CR is absent.
	Ready      *bool       `json:"ready"`
	Accepted   *bool       `json:"accepted"`
	Conditions []Condition `json:"conditions,omitempty"`
	// Managed is helmrelease, gitops or none.
	Managed     string          `json:"managed"`
	HelmRelease *HelmReleaseRef `json:"helmRelease,omitempty"`
	// Values are the HelmRelease's inline values (the chart contract), when a
	// HelmRelease owns the agent.
	Values map[string]any `json:"values,omitempty"`
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

// Manifests are the objects a create/update applies, as YAML, plus the values.
type Manifests struct {
	OCIRepository string         `json:"ociRepository"`
	HelmRelease   string         `json:"helmRelease"`
	Values        map[string]any `json:"values"`
}

// ValidateResult is a dry run: nothing was written.
type ValidateResult struct {
	Valid bool `json:"valid"`
	// Mode is create or update.
	Mode string `json:"mode"`
	// Errors lists every schema violation and precondition failure.
	Errors []string `json:"errors,omitempty"`
	// SchemaVersion / SchemaSource say which chart schema judged the values.
	SchemaVersion string    `json:"schemaVersion"`
	SchemaSource  string    `json:"schemaSource"`
	Manifests     Manifests `json:"manifests"`
}

// CreateResult reports what a create applied.
type CreateResult struct {
	Agent     Agent     `json:"agent"`
	Manifests Manifests `json:"manifests"`
	// Created says which objects were new; the OCIRepository is shared per
	// namespace and reused when it exists.
	Created struct {
		OCIRepository bool `json:"ociRepository"`
		HelmRelease   bool `json:"helmRelease"`
	} `json:"created"`
	Status *Status `json:"status,omitempty"`
}

// UpdateResult reports before/after values of an update.
type UpdateResult struct {
	Agent     Agent          `json:"agent"`
	Before    map[string]any `json:"before"`
	After     map[string]any `json:"after"`
	Changed   []string       `json:"changed"`
	Manifests Manifests      `json:"manifests"`
}

// DeleteResult reports what a delete removed.
type DeleteResult struct {
	Name                 string `json:"name"`
	Namespace            string `json:"namespace"`
	HelmReleaseDeleted   bool   `json:"helmReleaseDeleted"`
	AgentDeleted         bool   `json:"agentDeleted"`
	OCIRepositoryDeleted bool   `json:"ociRepositoryDeleted"`
	// OCIRepositoryKept explains why the chart source stays (other agents
	// reference it, or it could not be checked).
	OCIRepositoryKept string `json:"ociRepositoryKept,omitempty"`
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
	Summary     string             `json:"summary"`
	Agent       *AgentStatus       `json:"agent,omitempty"`
	HelmRelease *HelmReleaseStatus `json:"helmRelease,omitempty"`
	Deployment  *DeploymentStatus  `json:"deployment,omitempty"`
	Pods        []PodStatus        `json:"pods,omitempty"`
	Events      []Event            `json:"events,omitempty"`
}

// AgentStatus is the Agent CR's status.
type AgentStatus struct {
	Exists     bool        `json:"exists"`
	Ready      *bool       `json:"ready"`
	Accepted   *bool       `json:"accepted"`
	Conditions []Condition `json:"conditions,omitempty"`
}

// HelmReleaseStatus is the release's conditions and recent history.
type HelmReleaseStatus struct {
	Exists                bool                 `json:"exists"`
	Ready                 *bool                `json:"ready"`
	Suspended             bool                 `json:"suspended"`
	GitOpsOwned           bool                 `json:"gitOpsOwned"`
	Deleting              bool                 `json:"deleting"`
	Conditions            []Condition          `json:"conditions,omitempty"`
	History               []HelmReleaseHistory `json:"history,omitempty"`
	LastAttemptedRevision string               `json:"lastAttemptedRevision,omitempty"`
}

// HelmReleaseHistory is one Helm release revision from status.history.
type HelmReleaseHistory struct {
	Version      int64  `json:"version"`
	ChartVersion string `json:"chartVersion,omitempty"`
	Status       string `json:"status,omitempty"`
	LastDeployed string `json:"lastDeployed,omitempty"`
}

// DeploymentStatus is the kagent-managed Deployment of the agent.
type DeploymentStatus struct {
	Exists            bool        `json:"exists"`
	Replicas          int32       `json:"replicas"`
	ReadyReplicas     int32       `json:"readyReplicas"`
	AvailableReplicas int32       `json:"availableReplicas"`
	Conditions        []Condition `json:"conditions,omitempty"`
}

// PodStatus is one agent pod with what its containers are waiting on.
type PodStatus struct {
	Name  string `json:"name"`
	Phase string `json:"phase"`
	Ready bool   `json:"ready"`
	// Containers lists non-running containers with their waiting/terminated
	// reason (CrashLoopBackOff, ImagePullBackOff, ...).
	Containers []ContainerStatus `json:"containers,omitempty"`
	Restarts   int32             `json:"restarts"`
}

// ContainerStatus is a container's state when it is not running normally.
type ContainerStatus struct {
	Name    string `json:"name"`
	State   string `json:"state"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	// Init marks an init container (skills-init).
	Init bool `json:"init,omitempty"`
}

// Event is a recent Warning event on the agent's objects.
type Event struct {
	Type    string `json:"type"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
	Object  string `json:"object"`
	Count   int32  `json:"count,omitempty"`
	Last    string `json:"lastTimestamp,omitempty"`
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
