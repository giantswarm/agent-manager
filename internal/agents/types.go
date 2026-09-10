// Package agents is the domain of agent-manager: an agent on the platform is a
// Flux HelmRelease of the Generic agent chart (1.x) — one release renders one
// kagent.dev/v1alpha3 AgentTemplate plus the agent's own muster
// RemoteMCPServer, the toolset carrier — sharing a per-namespace OCIRepository
// that tracks the chart. The package composes those two Flux objects exactly
// like the portal's create flow, pins every skill to an immutable source,
// validates the values against the chart's schema before anything is applied,
// and reads the agent back from the AgentTemplate, its RemoteMCPServer and its
// owning HelmRelease. Readiness is what the platform Harness reports on the
// template (status.harnesses[]); there is no per-agent Deployment or pod.
package agents

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Sentinel errors the API layers map to HTTP statuses and MCP error codes.
var (
	// ErrNotFound: the agent, HelmRelease or ModelConfig does not exist.
	ErrNotFound = errors.New("not found")
	// ErrInvalid: the request is malformed, fails the chart's values schema,
	// or names a skill reference that cannot be pinned.
	ErrInvalid = errors.New("invalid request")
	// ErrConflict: the object exists, is owned by GitOps, is suspended, or is
	// a bare AgentTemplate — a write would be refused or undone; force
	// overrides where documented.
	ErrConflict = errors.New("conflict")
	// ErrForbidden: the Kubernetes API refused the write for the identity
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
	// DisplayNameAnnotation is the agent chart's contract for the friendly
	// name; IconURLAnnotation for the avatar (the AgentTemplate has no icon
	// field). Both are rendered by Generic chart 1.x and read by the Dev
	// Portal, Swarmgeist and agent-manager.
	DisplayNameAnnotation = "ui.giantswarm.io/display-name"
	IconURLAnnotation     = "ui.giantswarm.io/icon-url"
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

// The kagent.dev kinds an agent renders to.
const (
	kindOCIRepository   = "OCIRepository"
	kindRemoteMCPServer = "RemoteMCPServer"
)

// How an agent is managed.
const (
	// ManagedHelmRelease: a HelmRelease owns the agent and can be written live.
	ManagedHelmRelease = "helmrelease"
	// ManagedGitOps: the owning HelmRelease is applied by a Flux Kustomization;
	// changes belong in git (the meta agent opens a PR instead).
	ManagedGitOps = "gitops"
	// ManagedNone: a bare AgentTemplate with no HelmRelease behind it.
	ManagedNone = "none"
)

// GitSkill is a skill in a git repository. A write takes the repository and,
// optionally, a Ref to resolve or a Commit to pin; what is written and read
// back is always the Commit.
type GitSkill struct {
	// URL of the git repository (http or https).
	URL string `json:"url"`
	// Ref is a branch, tag or commit a write resolves to its head commit at
	// write time; empty (and no Commit) means the repository's default
	// branch. Never written: the pin is Commit.
	Ref string `json:"ref,omitempty"`
	// Commit is the full commit id (40 or 64 hex characters) the skill is
	// pinned to. Given by a caller who has one (list_skills reports it),
	// resolved from Ref otherwise.
	Commit string `json:"commit,omitempty"`
}

// Skill is one skills[] entry of the Generic chart 1.x values: a name, a
// subdirectory and exactly one source, git or OCI.
type Skill struct {
	// Name is the skill's name under the template (spec.skills[].name), the
	// directory it mounts under; defaults to the last path segment, else the
	// repository name. Unique per agent.
	Name string `json:"name,omitempty"`
	// Path is the subdirectory holding SKILL.md (git skills); empty for the
	// repository root. Relative, without "..".
	Path string `json:"path,omitempty"`
	// Git is the git source.
	Git *GitSkill `json:"git,omitempty"`
	// OCI is the OCI source: <registry>/<repository>@sha256:<digest> once
	// written; a write also takes <registry>/<repository>:<tag> and resolves
	// the digest.
	OCI string `json:"oci,omitempty"`
}

// Skills is an agent's skill list.
type Skills []Skill

// UnmarshalJSON accepts the list and explains the 0.x object shape
// ({refs, gitRefs, gitAuthSecretName}) to a caller still sending it.
func (s *Skills) UnmarshalJSON(b []byte) error {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var legacy struct {
			GitAuthSecretName json.RawMessage `json:"gitAuthSecretName"`
		}
		_ = json.Unmarshal(trimmed, &legacy)
		if len(legacy.GitAuthSecretName) > 0 && string(legacy.GitAuthSecretName) != "null" {
			return errors.New(gitAuthRefRemoved)
		}
		return errors.New(skillsShapeChanged)
	}
	var list []Skill
	if err := json.Unmarshal(b, &list); err != nil {
		return err
	}
	*s = list
	return nil
}

// Spec is what a caller provides to create an agent. Everything but Name,
// ModelConfig and Toolset is optional; omitted fields keep the chart's
// defaults.
type Spec struct {
	// Namespace the agent lives in; empty selects the default managed namespace.
	Namespace string `json:"namespace,omitempty"`
	// Name is the DNS-1123 technical name: the HelmRelease name and the
	// AgentTemplate name. The caller confirms it; agent-manager never derives
	// it.
	Name string `json:"name"`
	// DisplayName is the friendly Unicode name (max 63 chars).
	DisplayName string `json:"displayName,omitempty"`
	// Description goes to AgentTemplate.spec.description.
	Description string `json:"description,omitempty"`
	// SystemMessage is the system prompt (spec.systemPrompt); empty keeps the
	// chart default.
	SystemMessage string `json:"systemMessage,omitempty"`
	// ModelConfig names an existing kagent ModelConfig in the namespace.
	ModelConfig string `json:"modelConfig"`
	// IconURL is the avatar URL (chart agent.iconUrl, rendered as the
	// ui.giantswarm.io/icon-url annotation).
	IconURL string `json:"iconUrl,omitempty"`
	// Skills the agent mounts; every entry is pinned before it is written.
	Skills Skills `json:"skills,omitempty"`
	// Toolset is the list of selectors that bounds which of the gateway's
	// tools the agent can use (chart value `toolset`). Required on create;
	// see ValidateToolset for the grammar.
	Toolset []string `json:"toolset,omitempty"`
	// Labels / Annotations are merged onto the AgentTemplate by the chart.
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`

	// Removed arguments: they only exist so that a caller still passing them
	// is told why they are gone (see rejectRemoved) instead of getting an
	// "unknown field" error.
	RemovedToolNames json.RawMessage `json:"toolNames,omitempty"`
	RemovedRuntime   json.RawMessage `json:"runtime,omitempty"`
}

// Update is a partial change to an existing agent: nil pointers leave the
// current value; a pointer to an empty value clears it (falls back to the
// chart default). Skills replace the whole list; Toolset replaces the whole
// list (an empty list is refused: use preset:none).
type Update struct {
	Namespace     string             `json:"namespace,omitempty"`
	Name          string             `json:"name"`
	DisplayName   *string            `json:"displayName,omitempty"`
	Description   *string            `json:"description,omitempty"`
	SystemMessage *string            `json:"systemMessage,omitempty"`
	ModelConfig   *string            `json:"modelConfig,omitempty"`
	IconURL       *string            `json:"iconUrl,omitempty"`
	Skills        *Skills            `json:"skills,omitempty"`
	Toolset       *[]string          `json:"toolset,omitempty"`
	Labels        *map[string]string `json:"labels,omitempty"`
	Annotations   *map[string]string `json:"annotations,omitempty"`
	// RefreshSkills re-resolves every git skill of the agent to the head of
	// its repository's default branch — a skill passed in Skills with a Ref
	// goes to that ref's head — and changes nothing else on the release.
	RefreshSkills bool `json:"refreshSkills,omitempty"`
	// Removed arguments: see Spec.
	RemovedToolNames json.RawMessage `json:"toolNames,omitempty"`
	RemovedRuntime   json.RawMessage `json:"runtime,omitempty"`
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
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
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
	// HelmRelease carries a deletionTimestamp); the template disappears with
	// it.
	Deleting bool      `json:"deleting"`
	ChartRef *ChartRef `json:"chartRef,omitempty"`
}

// HarnessStatus is what kagent reports for the template on one admitting
// Harness (AgentTemplate.status.harnesses[]).
type HarnessStatus struct {
	Harness string `json:"harness"`
	// Ready / Accepted / ResolvedRefs / Compatible mirror the conditions; nil
	// while unreported.
	Ready        *bool `json:"ready"`
	Accepted     *bool `json:"accepted"`
	ResolvedRefs *bool `json:"resolvedRefs"`
	Compatible   *bool `json:"compatible"`
	// DesiredRevision is the revision compiled from the current generation;
	// LatestSuccessfulRevision the last one that became ready.
	DesiredRevision          string      `json:"desiredRevision,omitempty"`
	LatestSuccessfulRevision string      `json:"latestSuccessfulRevision,omitempty"`
	Conditions               []Condition `json:"conditions,omitempty"`
	// Warnings are the Harness's non-blocking compatibility decisions.
	Warnings []string `json:"warnings,omitempty"`
}

// ToolBinding is one MCP tool binding of the template (spec.tools[].mcp).
type ToolBinding struct {
	// Server is the RemoteMCPServer bound (same namespace).
	Server string `json:"server"`
	// Tools narrows the binding to these tools; empty is every tool.
	Tools []string `json:"tools,omitempty"`
}

// Agent is the read model of one agent.
type Agent struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// Exists is false while the HelmRelease has not rendered the
	// AgentTemplate yet (or failed to).
	Exists        bool   `json:"exists"`
	DisplayName   string `json:"displayName,omitempty"`
	Description   string `json:"description,omitempty"`
	ModelConfig   string `json:"modelConfig,omitempty"`
	IconURL       string `json:"iconUrl,omitempty"`
	SystemMessage string `json:"systemMessage,omitempty"`
	// Skills as pinned: git skills by commit, OCI skills by digest.
	Skills Skills `json:"skills,omitempty"`
	// Toolset is the toolset the agent declares: the owning HelmRelease's
	// `toolset` value, or — for a bare AgentTemplate — the X-Muster-Toolset
	// header of the agent's RemoteMCPServer. Absent when none is declared.
	Toolset []string `json:"toolset,omitempty"`
	// ImplicitFullAccess is true when the agent declares no toolset but binds
	// muster: its meta-tools see every tool the gateway exposes to the
	// caller. Such agents predate toolsets and still need one assigned.
	ImplicitFullAccess bool `json:"implicitFullAccess,omitempty"`
	// Tools are the template's MCP bindings.
	Tools []ToolBinding `json:"tools,omitempty"`
	// Ready is the platform Harness's verdict on the template (Ready and the
	// desired revision is the latest successful one); nil while the template
	// is absent or the Harness has not reported.
	Ready *bool `json:"ready"`
	// Harnesses is the template's per-Harness status.
	Harnesses []HarnessStatus `json:"harnesses,omitempty"`
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
	// RequestedBy is the authenticated caller the write ran as (email, else
	// subject); empty when the server runs without OAuth.
	RequestedBy string `json:"requestedBy,omitempty"`
}

// UpdateResult reports before/after values of an update.
type UpdateResult struct {
	Agent     Agent          `json:"agent"`
	Before    map[string]any `json:"before"`
	After     map[string]any `json:"after"`
	Changed   []string       `json:"changed"`
	Manifests Manifests      `json:"manifests"`
	// RequestedBy is the authenticated caller the write ran as.
	RequestedBy string `json:"requestedBy,omitempty"`
}

// DeleteResult reports what a delete removed.
type DeleteResult struct {
	Name               string `json:"name"`
	Namespace          string `json:"namespace"`
	HelmReleaseDeleted bool   `json:"helmReleaseDeleted"`
	// AgentTemplateDeleted: the template was deleted directly (a bare one, or
	// the rendered objects of a suspended release, both with force).
	AgentTemplateDeleted   bool `json:"agentTemplateDeleted"`
	RemoteMCPServerDeleted bool `json:"remoteMcpServerDeleted"`
	OCIRepositoryDeleted   bool `json:"ociRepositoryDeleted"`
	// OCIRepositoryKept explains why the chart source stays (other agents
	// reference it, or it could not be checked).
	OCIRepositoryKept string `json:"ociRepositoryKept,omitempty"`
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
	Summary     string             `json:"summary"`
	Template    *TemplateStatus    `json:"template,omitempty"`
	HelmRelease *HelmReleaseStatus `json:"helmRelease,omitempty"`
	Events      []Event            `json:"events,omitempty"`
}

// TemplateStatus is the AgentTemplate's status: one entry per admitting
// Harness, plus the generations that say whether kagent has caught up.
type TemplateStatus struct {
	Exists             bool            `json:"exists"`
	Generation         int64           `json:"generation,omitempty"`
	ObservedGeneration int64           `json:"observedGeneration,omitempty"`
	Harnesses          []HarnessStatus `json:"harnesses"`
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
