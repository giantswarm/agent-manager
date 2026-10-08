package api

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	mcpotel "github.com/mark3labs/mcp-go/otel"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/giantswarm/agent-manager/internal/agents"
)

// MCP tool names. Through muster they appear as x_<server>_<tool>, e.g.
// x_agent-manager_create_agent.
const (
	ToolGetInfo          = "get_info"
	ToolListAgents       = "list_agents"
	ToolGetAgent         = "get_agent"
	ToolCreateAgent      = "create_agent"
	ToolUpdateAgent      = "update_agent"
	ToolDeleteAgent      = "delete_agent"
	ToolGetAgentStatus   = "get_agent_status"
	ToolValidateAgent    = "validate_agent"
	ToolListModelConfigs = "list_model_configs"
	ToolListSkills       = "list_skills"
)

// ToolNames lists every tool the MCP server registers.
func ToolNames() []string {
	return []string{
		ToolGetInfo, ToolListAgents, ToolGetAgent, ToolCreateAgent, ToolUpdateAgent,
		ToolDeleteAgent, ToolGetAgentStatus, ToolValidateAgent, ToolListModelConfigs, ToolListSkills,
	}
}

const (
	argNamespace     = "namespace"
	argOrganization  = "organization"
	argCluster       = "cluster"
	argName          = "name"
	argDisplayName   = "displayName"
	argDescription   = "description"
	argSystemMessage = "systemMessage"
	argModelConfig   = "modelConfig"
	argHarness       = "harness"
	argIconURL       = "iconUrl"
	argSkills        = "skills"
	argGitAuthSecret = "gitAuthSecretName" // #nosec G101 -- an argument name, not a credential
	argRefreshSkills = "refreshSkills"
	argToolset       = "toolset"
	argLabels        = "labels"
	argAnnotations   = "annotations"
	argForce         = "force"
	argUpdate        = "update"
	argRepository    = "repository"
	argRef           = "ref"
	argRefresh       = "refresh"
	argMode          = "mode"
	argDryRun        = "dryRun"
	argBranch        = "branch"
	argPath          = "path"
)

var systemMessageLimit = fmt.Sprintf("At most %d characters; put long reference material in a skill.", agents.MaxSystemMessageLength)

const tracerName = "github.com/giantswarm/agent-manager"

// genAIToolName labels the mcp.tools/call server span with the called tool
// under the GenAI semantic-convention key.
func genAIToolName(next mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		trace.SpanFromContext(ctx).SetAttributes(attribute.String("gen_ai.tool.name", req.Params.Name))
		return next(ctx, req)
	}
}

// NewMCPServer builds an MCP server exposing the same operations as the REST
// API as tools. Results are JSON text with the same shapes as the REST bodies.
func NewMCPServer(svc *agents.Service, version string) *mcpserver.MCPServer {
	s := mcpserver.NewMCPServer("agent-manager", version,
		mcpserver.WithToolHandlerMiddleware(genAIToolName),
		mcpserver.WithToolHandlerMiddleware(toolDuration(otel.Meter(tracerName))),
		// The HTTP server span already joined the inbound traceparent, so the
		// MCP spans nest under it instead of extracting it a second time.
		mcpotel.WithServerTracingPropagator(otel.Tracer(tracerName), propagation.NewCompositeTextMapPropagator()),
		mcpserver.WithToolCapabilities(false),
		mcpserver.WithInstructions("Manage the agents of the Agent Platform. An agent is a Flux HelmRelease of the Generic agent chart (1.x; one release renders one kagent.dev/v1alpha3 AgentTemplate plus the agent's own muster RemoteMCPServer carrying its toolset) plus the shared per-namespace OCIRepository of that chart. Call get_info first for the managed namespaces, the chart range and version, the platform Harness and the muster URL; list_model_configs before create_agent (the modelConfig must exist in the namespace); list_skills for the skills an agent can mount — it reports each repository's head commit, and every skill is written pinned to a commit or a digest (a branch, tag or image tag is resolved at write time). Names are DNS-1123 labels the caller chooses and confirms — the service never derives a name from a display name. validate_agent is a dry run of create/update. Every agent declares a toolset — create_agent requires it: the selectors (preset:<name>, server:<name>, workflow:<name>, tool:<name>) that bound which of the gateway's tools the agent's meta-tools can see and call; presets shipped on every installation: read-only, none, infrastructure, agent-platform, full. list_agents reports the toolset of each agent, or implicitFullAccess: true for agents created before toolsets existed — assign them one with update_agent. Readiness is the verdict of the agent's own Harness on the AgentTemplate (get_agent_status). An agent runs on the platform Harness (the Go ADK) unless create_agent names another Harness of the namespace, such as claude for a coding agent; there is no runtime argument and no per-source skill credential. Every write tool takes dryRun (return the manifests, write nothing) and mode: apply (default) writes live as the caller and never touches an agent whose HelmRelease is applied from git (managed: gitops; refused with gitops_owned); commit writes the manifests as files into the git repository that owns the namespace and opens a pull request as the caller (get_info capabilities.commit says whether this installation offers it; without the caller's GitHub authorization it answers auth_required). organization and cluster (both or neither) place an agent on a workload cluster of the installation and address it in every read and write tool: its HelmRelease stays on the installation in org-<organization>, the agent, its ModelConfig and its Harness are on the cluster (get_info capabilities.targetCluster)."),
	)
	t := &tools{svc: svc}

	skillDesc := "Skills the agent mounts, a list of {name, path, git: {url, ref | commit}} or {name, oci: <reference>} entries. A git skill names its repository (http(s) URL) and either a commit (40 or 64 hex characters; list_skills reports it) or a ref (branch or tag) that is resolved to its head commit at write time; neither means the head of the default branch. An OCI skill is <registry>/<repository>:<tag> (resolved to its digest) or @sha256:<digest>. What is written is always the pin; get_agent reports it. There is no gitAuthSecretName: a private repository is resolved with the service's own GitHub token."
	skillEntry := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": schemaProp("string", "Name the skill mounts under; defaults to the last path segment, else the repository or image name"),
			"path": schemaProp("string", "Skill directory (holds SKILL.md), relative without '..'; empty for the repository root (git skills only)"),
			"git": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url":    schemaProp("string", "Repository URL (http or https)"),
					"ref":    schemaProp("string", "Branch or tag to resolve to its head commit at write time; omit for the default branch"),
					"commit": schemaProp("string", "Full commit id (40 or 64 hex characters) to pin; list_skills reports it"),
				},
				"required": []string{"url"},
			},
			"oci": schemaProp("string", "OCI reference: <registry>/<repository>:<tag> (resolved to its digest) or <registry>/<repository>@sha256:<digest>"),
		},
	}
	gitAuthDesc := "Secret in the agent's namespace whose key token reads the agent's private git skills (a GitHub token; chart value skillsGitAuthSecretRef.name, offered to every git skill's https host). Omit it: the installation's skills credential (get_info skillsGitAuthSecretName) is used for every agent with a git skill"
	gitAuthProp := mcp.WithString(argGitAuthSecret, mcp.Description(gitAuthDesc))
	gitAuthReplaceProp := mcp.WithString(argGitAuthSecret, mcp.Description(gitAuthDesc+"; \"\" goes back to the installation's"))
	skillsProp := mcp.WithArray(argSkills, mcp.Description(skillDesc), mcp.Items(skillEntry))
	skillsReplaceProp := mcp.WithArray(argSkills, mcp.Description("Replaces the agent's whole skill list. "+skillDesc), mcp.Items(skillEntry))
	refreshSkillsProp := mcp.WithBoolean(argRefreshSkills, mcp.Description("Re-resolve every git skill of the agent to the head of its repository's default branch (a skill passed in skills with a ref in the same call goes to that ref's head) and change nothing else on the release (default false)."))
	toolsetDesc := "Toolset: the selectors that bound which of the gateway's tools the agent's meta-tools can see and call. Each is preset:<name>, server:<name>, workflow:<name> or tool:<name> (exact names; at most 32 — define a preset for more). Shipped presets: preset:read-only, preset:none (a chat-only agent without tools), preset:infrastructure, preset:agent-platform, preset:full (every tool the gateway exposes). Composed as the chart's top-level toolset value, rendered as the X-Muster-Toolset header on the agent's own RemoteMCPServer; muster resolves it per caller. The former toolNames argument is gone: it never narrowed anything against muster."
	toolsetProp := mcp.WithArray(argToolset, mcp.Description(toolsetDesc), mcp.WithStringItems())
	toolsetRequiredProp := mcp.WithArray(argToolset, mcp.Required(), mcp.Description("REQUIRED. "+toolsetDesc), mcp.WithStringItems())
	toolsetReplaceProp := mcp.WithArray(argToolset, mcp.Description("Replaces the agent's whole toolset with this list (an empty list is refused: use [\"preset:none\"] for no tools). "+toolsetDesc), mcp.WithStringItems())
	labelsProp := mcp.WithObject(argLabels, mcp.Description("Extra labels on the AgentTemplate (string values)."), mcp.AdditionalProperties(map[string]any{"type": "string"}))
	annotationsProp := mcp.WithObject(argAnnotations, mcp.Description("Extra annotations on the AgentTemplate (string values)."), mcp.AdditionalProperties(map[string]any{"type": "string"}))
	nsProp := mcp.WithString(argNamespace, mcp.Description("Namespace of the agent; default: the installation's kagent namespace (get_info reports the managed ones)."))
	orgProp := mcp.WithString(argOrganization, mcp.Description("With cluster: the organization owning the workload cluster the agent runs on (its namespace org-<organization> holds the cluster's kubeconfig Secret and the agent's HelmRelease). Omit both for the installation's own cluster."))
	clusterProp := mcp.WithString(argCluster, mcp.Description("With organization: the workload cluster the agent runs on. The HelmRelease stays on the installation and installs on the cluster through its Cluster API kubeconfig Secret <cluster>-kubeconfig; the agent, its ModelConfig and its Harness are read on the cluster, as the caller (the cluster's apiserver must trust the installation's identity provider). namespace is then the namespace on the cluster."))
	modeProp := mcp.WithString(argMode, mcp.Enum(agents.ModeApply, agents.ModeCommit), mcp.Description("apply (default): write live as the caller; refused with gitops_owned for a HelmRelease a Flux Kustomization applies from git. commit: write the manifests as files into agent-manager's directory of the git repository that owns the namespace (from the Flux Kustomization labels, or repository/branch/path) and open a pull request as the caller; answers auth_required when the call carries no GitHub authorization. get_info capabilities.commit says whether commit is offered."))
	dryRunProp := mcp.WithBoolean(argDryRun, mcp.Description("Return what the write would do — the manifests and, with mode commit, the repository, directory, files and branch of the pull request — and write nothing (default false)."))
	repositoryProp := mcp.WithString(argRepository, mcp.Description("mode commit only: the GitHub repository (owner/name) to commit to where no Flux Kustomization owns the namespace yet; overrides the derived target. Needs branch."))
	branchProp := mcp.WithString(argBranch, mcp.Description("mode commit with repository: the base branch the pull request goes to."))
	pathProp := mcp.WithString(argPath, mcp.Description("mode commit with repository: the repository-relative directory a Flux Kustomization builds from; agent-manager writes into its agent-manager/ subdirectory (default: the root)."))

	// Every tool spells out all four hints: mcp-go pre-fills an unset hint with
	// the spec default, so a hint left out ships as a claim. openWorldHint is true
	// where a caller-named GitHub repository or OCI reference is resolved, false
	// for the tools that only read and write this installation's own resources.
	s.AddTool(mcp.NewTool(ToolGetInfo,
		mcp.WithDescription("Read-only. Report the service version, the agent chart (OCI URL, the tracked 1.x range, resolved latest version, which values schema validates right now), the managed namespaces, the capability flags (commit: whether the write tools take mode commit), the served API versions (apiVersions.agentTemplate, harness, remoteMcpServer, modelConfig, helmRelease, ociRepository), the platform Harness (harness.name), the muster MCP URL composed into every agent (muster.url; empty means the chart default), the Flux settings composed into every agent and how writes are authenticated. Call first."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(false),
	), t.getInfo)

	s.AddTool(mcp.NewTool(ToolListAgents,
		mcp.WithDescription("Read-only. List the agents of a namespace: display name, description, icon URL, model config, pinned skills, the declared toolset (or implicitFullAccess: true for an agent without one — it sees every tool the gateway exposes and still needs a toolset), the MCP bindings, ready (the platform Harness's verdict on the AgentTemplate) with the per-Harness status, the owning HelmRelease (Ready, chart version) and how each is managed (helmrelease: writable here; gitops: applied from git, read-only without force; none: a bare AgentTemplate). HelmReleases of the agent chart that have not rendered a template yet are listed too (exists: false)."),
		nsProp,
		orgProp,
		clusterProp,
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(false),
	), t.listAgents)

	s.AddTool(mcp.NewTool(ToolGetAgent,
		mcp.WithDescription("Read-only. Get one agent with its HelmRelease values (the chart contract), its pinned skills, its declared toolset (or implicitFullAccess: true) and the per-Harness status of its AgentTemplate."),
		mcp.WithString(argName, mcp.Required(), mcp.Description("Agent name")),
		nsProp,
		orgProp,
		clusterProp,
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(false),
	), t.getAgent)

	s.AddTool(mcp.NewTool(ToolCreateAgent,
		mcp.WithDescription("WRITES: creates a Flux HelmRelease of the Generic agent chart named after the agent (and the shared OCIRepository of the chart in the namespace, tracking 1.x, when it does not exist yet); helm-controller then renders the kagent.dev/v1alpha3 AgentTemplate and the agent's muster RemoteMCPServer, and the platform Harness compiles it. A toolset is required (refused without one). Every skill is pinned (a branch or tag to its head commit, an image tag to its digest), the values are validated against the chart's values.schema.json and the modelConfig must exist in the namespace before anything is applied — a failure writes nothing and lists the valid model configs. Returns the applied manifests and the initial status; poll get_agent_status until the verdict is ready. dryRun returns the manifests and writes nothing. mode commit writes the HelmRelease (and the OCIRepository when the namespace has none) as files of a pull request opened as the caller in the repository that owns the namespace, applies nothing live and returns commit {repository, directory, files, branch, pullRequest}. The name is the DNS-1123 technical name the caller chose (confirm it with the user; it is never derived from displayName). runtime is refused: the platform Harness is the runtime."),
		mcp.WithString(argName, mcp.Required(), mcp.Description("DNS-1123 technical name (max 63 chars): the HelmRelease and AgentTemplate name")),
		mcp.WithString(argModelConfig, mcp.Required(), mcp.Description("Name of an existing kagent ModelConfig in the namespace (list_model_configs)")),
		mcp.WithString(argHarness, mcp.Description("Harness of the namespace that runs the agent, e.g. claude for a Claude Code coding agent; omit for the platform Harness (get_info). Fixed at create: an unknown name is refused with the Harnesses that exist")),
		mcp.WithString(argDisplayName, mcp.Description("Friendly Unicode name (max 63 chars), shown by the portal")),
		mcp.WithString(argDescription, mcp.Description("What the agent is for")),
		mcp.WithString(argSystemMessage, mcp.Description("System prompt; omit for the chart's default prompt. "+systemMessageLimit)),
		mcp.WithString(argIconURL, mcp.Description("Avatar URL (chart agent.iconUrl, rendered as the ui.giantswarm.io/icon-url annotation); omit unless the installation serves avatars")),
		toolsetRequiredProp,
		skillsProp,
		gitAuthProp,
		labelsProp,
		annotationsProp,
		nsProp,
		orgProp,
		clusterProp,
		modeProp, dryRunProp, repositoryProp, branchProp, pathProp,
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(true),
	), t.createAgent)

	s.AddTool(mcp.NewTool(ToolUpdateAgent,
		mcp.WithDescription("WRITES: merges the given fields into the agent's HelmRelease values (only the arguments passed change; skills replace the whole list and are pinned, toolset replaces the whole list — the way to assign a toolset to an agent that reports implicitFullAccess; an empty string clears a field back to the chart default; refreshSkills re-pins every git skill to its default-branch head and changes nothing else), validates the result against the chart schema and updates the HelmRelease — helm-controller upgrades the AgentTemplate and the platform Harness compiles a new revision. Returns the values before and after and the changed paths. dryRun returns them and writes nothing. A GitOps-owned release (managed: gitops) is refused in mode apply with gitops_owned; mode commit rewrites its file in agent-manager's directory of the owning repository and opens a pull request as the caller (a release defined elsewhere in the repository is refused). A suspended release is refused unless force is true."),
		mcp.WithString(argName, mcp.Required(), mcp.Description("Agent name")),
		mcp.WithString(argDisplayName, mcp.Description("New friendly name; \"\" clears it")),
		mcp.WithString(argDescription, mcp.Description("New description; \"\" clears it")),
		mcp.WithString(argSystemMessage, mcp.Description("New system prompt; \"\" restores the chart default. "+systemMessageLimit)),
		mcp.WithString(argModelConfig, mcp.Description("Name of an existing ModelConfig in the namespace")),
		mcp.WithString(argIconURL, mcp.Description("New avatar URL; \"\" clears it")),
		skillsReplaceProp,
		gitAuthReplaceProp,
		refreshSkillsProp,
		toolsetReplaceProp,
		labelsProp,
		annotationsProp,
		mcp.WithBoolean(argForce, mcp.Description("Write even when the HelmRelease is suspended (default false). A GitOps-owned release is never written live: use mode commit")),
		nsProp,
		orgProp,
		clusterProp,
		modeProp, dryRunProp, repositoryProp, branchProp, pathProp,
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(true),
	), t.updateAgent)

	s.AddTool(mcp.NewTool(ToolDeleteAgent,
		mcp.WithDescription("WRITES (destructive): deletes the HelmRelease that owns the agent — helm-controller uninstalls the release and removes the AgentTemplate and the agent's RemoteMCPServer — and deletes the shared OCIRepository of the agent chart only when no other HelmRelease in the namespace references it (the result says why it was kept). A bare AgentTemplate without a HelmRelease is refused unless force is true (then the template is deleted directly); a suspended HelmRelease is refused unless force is true. A GitOps-owned release is refused in mode apply with gitops_owned; mode commit removes its file (and the chart source's when nothing else uses it) from agent-manager's directory of the owning repository with a pull request opened as the caller. dryRun reports what would be deleted and deletes nothing."),
		mcp.WithString(argName, mcp.Required(), mcp.Description("Agent name")),
		mcp.WithBoolean(argForce, mcp.Description("Also delete bare AgentTemplates and suspended releases (default false). A GitOps-owned release is never deleted live: use mode commit")),
		nsProp,
		orgProp,
		clusterProp,
		modeProp, dryRunProp, repositoryProp, branchProp, pathProp,
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(false),
	), t.deleteAgent)

	s.AddTool(mcp.NewTool(ToolGetAgentStatus,
		mcp.WithDescription("Read-only. One verdict (ready | progressing | failed | unknown) with a one-line summary, from the AgentTemplate's status entry for the platform Harness (ready: Ready and the desired revision is the latest successful one; progressing: a revision still compiling; failed: Accepted, ResolvedRefs or Compatible False with the condition's message, or no Harness admitting the template), the HelmRelease conditions and history, and the namespace's recent Warning events for the agent. No Deployment or pod is involved: agents run as Substrate actors."),
		mcp.WithString(argName, mcp.Required(), mcp.Description("Agent name")),
		nsProp,
		orgProp,
		clusterProp,
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(false),
	), t.getAgentStatus)

	s.AddTool(mcp.NewTool(ToolValidateAgent,
		mcp.WithDescription("Read-only dry run of create_agent (or of update_agent when update is true): composes the OCIRepository and HelmRelease, checks the name, the modelConfig, the toolset (required for a create; validated when given for an update), pins the skills and validates the values against the agent chart's values.schema.json, and returns the manifests and every violation. Nothing is written."),
		mcp.WithString(argName, mcp.Required(), mcp.Description("Agent name")),
		mcp.WithString(argModelConfig, mcp.Description("ModelConfig name (required for a create)")),
		mcp.WithString(argHarness, mcp.Description("Harness of the namespace that runs the agent, e.g. claude for a Claude Code coding agent; omit for the platform Harness (get_info). Fixed at create: an unknown name is refused with the Harnesses that exist")),
		mcp.WithString(argDisplayName, mcp.Description("Friendly name")),
		mcp.WithString(argDescription, mcp.Description("Description")),
		mcp.WithString(argSystemMessage, mcp.Description("System prompt. "+systemMessageLimit)),
		mcp.WithString(argIconURL, mcp.Description("Avatar URL")),
		toolsetProp,
		skillsProp,
		gitAuthProp,
		refreshSkillsProp,
		labelsProp,
		annotationsProp,
		mcp.WithBoolean(argUpdate, mcp.Description("Validate as an update of the existing agent instead of a create (default false)")),
		mcp.WithBoolean(argForce, mcp.Description("With update: ignore the GitOps/suspended guards (default false)")),
		nsProp,
		orgProp,
		clusterProp,
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(true),
	), t.validateAgent)

	s.AddTool(mcp.NewTool(ToolListModelConfigs,
		mcp.WithDescription("Read-only. List the kagent ModelConfigs of a namespace (name, provider, model, Accepted condition, who manages it) — the values create_agent accepts for modelConfig. ModelConfigs are platform-admin owned; agent-manager never writes them."),
		nsProp,
		orgProp,
		clusterProp,
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(false),
	), t.listModelConfigs)

	s.AddTool(mcp.NewTool(ToolListSkills,
		mcp.WithDescription("Read-only. Discover the skills of the configured skill repositories: per repository the ref read and the head commit it resolved to (the commit a skills entry of create_agent/update_agent pins), and its skills (every SKILL.md, by frontmatter name, description and directory path). Results are cached briefly; refresh re-reads GitHub."),
		mcp.WithString(argRepository, mcp.Description("Only this repository (https://github.com/<owner>/<repo>); default: every configured one")),
		mcp.WithString(argRef, mcp.Description("Git ref to read; default: the default branch (the ref refreshSkills follows)")),
		mcp.WithBoolean(argRefresh, mcp.Description("Bypass the cache (default false)")),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(true),
	), t.listSkills)

	return s
}

type tools struct {
	svc *agents.Service
}

// schemaProp is a JSON-schema leaf with a description.
func schemaProp(typ, desc string) map[string]any {
	return map[string]any{"type": typ, argDescription: desc}
}

func (t *tools) getInfo(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return jsonResult(t.svc.Info(ctx))
}

func (t *tools) listAgents(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	list, err := t.svc.List(ctx, locationFromArgs(req))
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(map[string]any{"agents": list})
}

func (t *tools) getAgent(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, err := req.RequireString(argName)
	if err != nil {
		return errResult(fmt.Errorf("%w: %v", agents.ErrInvalid, err)), nil
	}
	a, err := t.svc.Get(ctx, locationFromArgs(req), name)
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(a)
}

// locationFromArgs reads namespace, organization and cluster.
func locationFromArgs(req mcp.CallToolRequest) agents.Location {
	return agents.Location{
		Namespace: req.GetString(argNamespace, ""),
		Target:    agents.Target{Organization: req.GetString(argOrganization, ""), Cluster: req.GetString(argCluster, "")},
	}
}

// specFromArgs binds the flat tool arguments to a Spec.
func specFromArgs(req mcp.CallToolRequest) (agents.Spec, error) {
	var s agents.Spec
	if err := req.BindArguments(&s); err != nil {
		return s, fmt.Errorf("%w: %v", agents.ErrInvalid, err)
	}
	return s, nil
}

func (t *tools) createAgent(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	s, err := specFromArgs(req)
	if err != nil {
		return errResult(err), nil
	}
	res, err := t.svc.Create(ctx, s)
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(res)
}

func (t *tools) updateAgent(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// Bind the raw arguments so an explicit "" (clear) survives — SpecToUpdate
	// would read it as "unchanged".
	var upd agents.Update
	if err := req.BindArguments(&upd); err != nil {
		return errResult(fmt.Errorf("%w: %v", agents.ErrInvalid, err)), nil
	}
	if upd.Name == "" {
		return errResult(fmt.Errorf("%w: name is required", agents.ErrInvalid)), nil
	}
	res, err := t.svc.Update(ctx, upd)
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(res)
}

func (t *tools) deleteAgent(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, err := req.RequireString(argName)
	if err != nil {
		return errResult(fmt.Errorf("%w: %v", agents.ErrInvalid, err)), nil
	}
	var w agents.WriteOptions
	if err := req.BindArguments(&w); err != nil {
		return errResult(fmt.Errorf("%w: %v", agents.ErrInvalid, err)), nil
	}
	res, err := t.svc.Delete(ctx, locationFromArgs(req), name, req.GetBool(argForce, false), w)
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(res)
}

func (t *tools) getAgentStatus(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, err := req.RequireString(argName)
	if err != nil {
		return errResult(fmt.Errorf("%w: %v", agents.ErrInvalid, err)), nil
	}
	st, err := t.svc.Status(ctx, locationFromArgs(req), name)
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(st)
}

func (t *tools) validateAgent(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var (
		res *agents.ValidateResult
		err error
	)
	if req.GetBool(argUpdate, false) {
		var upd agents.Update
		if err := req.BindArguments(&upd); err != nil {
			return errResult(fmt.Errorf("%w: %v", agents.ErrInvalid, err)), nil
		}
		res, err = t.svc.ValidateUpdate(ctx, upd)
	} else {
		s, specErr := specFromArgs(req)
		if specErr != nil {
			return errResult(specErr), nil
		}
		res, err = t.svc.ValidateCreate(ctx, s)
	}
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(res)
}

func (t *tools) listModelConfigs(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	list, err := t.svc.ListModelConfigs(ctx, locationFromArgs(req))
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(map[string]any{"modelConfigs": list})
}

func (t *tools) listSkills(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	res, err := t.svc.ListSkills(ctx, req.GetString(argRepository, ""), req.GetString(argRef, ""), req.GetBool(argRefresh, false))
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(res)
}

func jsonResult(v any) (*mcp.CallToolResult, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("encode result: %v", err)), nil
	}
	return mcp.NewToolResultText(string(data)), nil
}

func errResult(err error) *mcp.CallToolResult {
	_, code := statusFor(err)
	return mcp.NewToolResultError(fmt.Sprintf("%s: %s", code, err.Error()))
}
