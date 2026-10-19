package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/giantswarm/mcp-toolkit/tooldesc"
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
	ToolListSessions     = "list_sessions"
	ToolGetSession       = "get_session"
	ToolGetSessionTurn   = "get_session_turn"
	ToolStartSession     = "start_session"
	ToolDeleteSession    = "delete_session"
)

// ToolNames lists every tool the MCP server registers.
func ToolNames() []string {
	return []string{
		ToolGetInfo, ToolListAgents, ToolGetAgent, ToolCreateAgent, ToolUpdateAgent,
		ToolDeleteAgent, ToolGetAgentStatus, ToolValidateAgent, ToolListModelConfigs, ToolListSkills,
		ToolListSessions, ToolGetSession, ToolGetSessionTurn, ToolStartSession, ToolDeleteSession,
	}
}

const (
	argNamespace      = "namespace"
	argOrganization   = "organization"
	argCluster        = "cluster"
	argName           = "name"
	argDisplayName    = "displayName"
	argDescription    = "description"
	argSystemMessage  = "systemMessage"
	argModelConfig    = "modelConfig"
	argHarness        = "harness"
	argIconURL        = "iconUrl"
	argSkills         = "skills"
	argPlugins        = "plugins"
	argEgress         = "egress"
	argGitAuthSecret  = "gitAuthSecretName" // #nosec G101 -- an argument name, not a credential
	argRefreshSkills  = "refreshSkills"
	argToolset        = "toolset"
	argLabels         = "labels"
	argAnnotations    = "annotations"
	argForce          = "force"
	argUpdate         = "update"
	argRepository     = "repository"
	argRef            = "ref"
	argRefresh        = "refresh"
	argDetails        = "details"
	argMode           = "mode"
	argDryRun         = "dryRun"
	argBranch         = "branch"
	argPath           = "path"
	argAllCreators    = "allCreators"
	argSessionID      = "sessionId"
	argMessage        = "message"
	argRequestID      = "requestId"
	argSessionName    = "sessionName"
	argWorkspaceGrant = "workspaceGrant"
	argDetail         = "detail"
	argLastTurns      = "lastTurns"
	argMaxBytes       = "maxBytes"
	argTurn           = "turn"
	argToolCall       = "toolCall"
	argFromMessage    = "fromMessage"
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
		mcpserver.WithInstructions("Manage the agents of the Agent Platform. An agent is a Flux HelmRelease of the Generic agent chart, which renders a kagent Agent, its Harness reference and the agent's muster RemoteMCPServer carrying its toolset. Call get_info first, and list_model_configs and list_skills before create_agent. Names are DNS-1123 labels the person chooses and confirms. Every agent needs a toolset. create_agent, update_agent and delete_agent take dryRun and mode: apply writes live as you, commit opens a pull request as you in the repository that owns the namespace. organization and cluster (both or neither) place an agent on a workload cluster and address it in every later call of a tool that takes them. Talk to an agent with start_session and get_session."),
	)
	t := &tools{svc: svc}

	skillShape := "{name, path, git: {url, ref | commit}} or {name, oci} entries, pinned at write time"
	skillDesc := "Skills to mount: " + skillShape + "."
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
	gitAuthDesc := "Secret in the agent's namespace whose token key reads private git skills and plugins; omit for the installation's (get_info)"
	gitAuthProp := mcp.WithString(argGitAuthSecret, mcp.Description(gitAuthDesc))
	gitAuthReplaceProp := mcp.WithString(argGitAuthSecret, mcp.Description(gitAuthDesc+"; \"\" goes back to it"))
	skillsProp := mcp.WithArray(argSkills, mcp.Description(skillDesc), mcp.Items(skillEntry))
	skillsReplaceProp := mcp.WithArray(argSkills, mcp.Description("Replaces the whole skill list: "+skillShape+"."), mcp.Items(skillEntry))
	refreshSkillsProp := mcp.WithBoolean(argRefreshSkills, mcp.Description("Re-pin every git skill and git plugin to its default-branch head and change nothing else on the release (default false)."))
	selectorShape := "preset:<name>, server:<name>, workflow:<name> or tool:<name> selectors, at most 32"
	toolsetDesc := "Gateway tools the agent may use: " + selectorShape + "; preset:none means no tools."
	presetsSentence := "Presets: " + strings.Join(agents.PresetSelectors(), ", ") + "."
	toolsetProp := mcp.WithArray(argToolset, mcp.Description(toolsetDesc), mcp.WithStringItems())
	toolsetRequiredProp := mcp.WithArray(argToolset, mcp.Required(), mcp.Description(toolsetDesc), mcp.WithStringItems())
	toolsetReplaceProp := mcp.WithArray(argToolset, mcp.Description("Replaces the whole toolset: "+selectorShape+"; [] is refused: use preset:none."), mcp.WithStringItems())
	labelsProp := mcp.WithObject(argLabels, mcp.Description("Extra labels on the Agent object (string values)."), mcp.AdditionalProperties(stringSchema))
	annotationsProp := mcp.WithObject(argAnnotations, mcp.Description("Extra annotations on the Agent object (string values)."), mcp.AdditionalProperties(stringSchema))
	labelsReplaceProp := mcp.WithObject(argLabels, mcp.Description("Replaces all extra labels on the Agent object ({} clears them); pass every label to keep."), mcp.AdditionalProperties(stringSchema))
	annotationsReplaceProp := mcp.WithObject(argAnnotations, mcp.Description("Replaces all extra annotations on the Agent ({} clears them; the creator annotation always stays); pass every one to keep."), mcp.AdditionalProperties(stringSchema))
	pluginShape := "{git: {url, ref | commit} or oci: <reference>, path, skills} entries, pinned at write time"
	pluginDesc := "Plugins the agent loads: " + pluginShape + "."
	pluginEntry := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": schemaProp("string", "Directory of the plugin within the artifact, relative without '..'; empty for the root"),
			"git": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url":    schemaProp("string", "Repository URL (http or https)"),
					"ref":    schemaProp("string", "Branch or tag to resolve to its head commit at write time; omit for the default branch"),
					"commit": schemaProp("string", "Full commit id (40 or 64 hex characters) to pin"),
				},
				"required": []string{"url"},
			},
			"oci":    schemaProp("string", "OCI reference: <registry>/<repository>:<tag> (resolved to its digest) or <registry>/<repository>@sha256:<digest>"),
			"skills": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, argDescription: "Skills to enable from an Agent Plugins format package, unique, never a name skills or another plugin uses; omit for a Claude Code format plugin"},
		},
	}
	pluginsProp := mcp.WithArray(argPlugins, mcp.Description(pluginDesc), mcp.Items(pluginEntry))
	pluginsReplaceProp := mcp.WithArray(argPlugins, mcp.Description("Replaces the whole plugin list ([] clears it): "+pluginShape+"."), mcp.Items(pluginEntry))
	originShape := "<scheme>://<host>[:<port>] origins, at most 64; a wildcard replaces the leftmost label"
	egressDesc := "Extra HTTP(S) origins the agent may reach: " + originShape + "."
	egressProp := mcp.WithArray(argEgress, mcp.Description(egressDesc), mcp.Items(stringSchema))
	egressReplaceProp := mcp.WithArray(argEgress, mcp.Description("Replaces the whole egress list ([] clears it): "+originShape+"."), mcp.Items(stringSchema))
	harnessDesc := "Harness that runs the agent; omit for the platform Harness. A coding Harness (claude, codex, …) fixes egress, refuses extra bindings."
	maxBytesProp := mcp.WithNumber(argMaxBytes, mcp.Description(fmt.Sprintf("Max answer size in bytes, %d-%d, default %d", agents.MinSessionMaxBytes, agents.MaxSessionMaxBytes, agents.DefaultSessionMaxBytes)))
	nsProp := mcp.WithString(argNamespace, mcp.Description("Namespace of the agent; default: the installation's kagent namespace (get_info lists the managed ones)."))
	orgProp := mcp.WithString(argOrganization, mcp.Description("With cluster: the organization owning the workload cluster the agent runs on. Omit both for the installation's own cluster."))
	clusterProp := mcp.WithString(argCluster, mcp.Description("With organization: the workload cluster the agent runs on; its HelmRelease stays on the installation. namespace is on the cluster."))
	modeProp := mcp.WithString(argMode, mcp.Enum(agents.ModeApply, agents.ModeCommit), mcp.Description("apply (default): write live as you; a release applied from git is refused with gitops_owned. commit: open a pull request as you."))
	dryRunProp := mcp.WithBoolean(argDryRun, mcp.Description("Return what the write would do, the manifests and with mode commit the pull request's files, and write nothing (default false)."))
	repositoryProp := mcp.WithString(argRepository, mcp.Description("mode commit: the repository (owner/name) to commit to where no Flux Kustomization owns the namespace yet. Needs branch."))
	branchProp := mcp.WithString(argBranch, mcp.Description("mode commit with repository: the base branch the pull request goes to."))
	pathProp := mcp.WithString(argPath, mcp.Description("mode commit with repository: the directory a Flux Kustomization builds from; files go into its agent-manager/ subdirectory."))

	// Every tool spells out all four hints: mcp-go pre-fills an unset hint with
	// the spec default, so a hint left out ships as a claim. openWorldHint is true
	// where a caller-named GitHub repository or OCI reference is resolved, false
	// for the tools that only read and write this installation's own resources.
	s.AddTool(mcp.NewTool(ToolGetInfo,
		tooldesc.Describe("Where you can create agents and what this installation offers: managed namespaces, chart, Harness, capabilities.",
			"Call it first: capabilities says whether this installation offers mode commit, the session tools and workload clusters. Also the service version, the served API versions, the muster URL and Flux settings composed into every agent, and how writes are authenticated."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(false),
	), t.getInfo)

	s.AddTool(mcp.NewTool(ToolListAgents,
		tooldesc.Describe("List the agents of a namespace, one summary each: model, Harness, toolset, readiness and how it is managed.",
			"managed: helmrelease is writable here, gitops changes only with mode commit, none is a bare Agent object. implicitFullAccess: true means the agent sees every gateway tool and still needs a toolset (update_agent). An agent's prompt, skills, plugins and values come from get_agent."),
		nsProp,
		orgProp,
		clusterProp,
		mcp.WithBoolean(argDetails, mcp.Description("Report every agent in full, as get_agent does (default false). Large: prefer get_agent for the agents you need.")),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(false),
	), t.listAgents)

	s.AddTool(mcp.NewTool(ToolGetAgent,
		tooldesc.Describe("Get one agent: its HelmRelease values, Harness, pinned skills and plugins, toolset and Agent status.",
			""),
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
		tooldesc.Describe("Create an agent: a HelmRelease of the Generic agent chart. It needs a toolset; confirm its name with the person.",
			"name is the DNS-1123 name the person chose, never derived from displayName. modelConfig must exist in the namespace (list_model_configs; a refusal names the valid ones). Skills and plugins are pinned at write time (list_skills) and the values are checked against the chart schema; a failure writes nothing. "+presetsSentence+" Poll get_agent_status until ready."),
		mcp.WithString(argName, mcp.Required(), mcp.Description("DNS-1123 technical name (max 63 chars): the HelmRelease and Agent name")),
		mcp.WithString(argModelConfig, mcp.Required(), mcp.Description("Name of an existing kagent ModelConfig in the namespace (list_model_configs)")),
		mcp.WithString(argHarness, mcp.Description(harnessDesc)),
		mcp.WithString(argDisplayName, mcp.Description("Friendly Unicode name (max 63 chars), shown by the portal")),
		mcp.WithString(argDescription, mcp.Description("What the agent is for")),
		mcp.WithString(argSystemMessage, mcp.Description("System prompt; omit for the chart's default prompt. "+systemMessageLimit)),
		mcp.WithString(argIconURL, mcp.Description("Avatar URL (chart agent.iconUrl, rendered as the ui.giantswarm.io/icon-url annotation); omit unless the installation serves avatars")),
		toolsetRequiredProp,
		skillsProp,
		pluginsProp,
		egressProp,
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
		tooldesc.Describe("Change an agent: only the fields passed change; a list, labels or annotations replaces the whole value.",
			"Lists are skills, plugins, egress and toolset. \"\" clears a text field back to the chart default; gitAuthSecretName \"\" goes back to the installation's. "+presetsSentence+" refreshSkills re-pins every git skill and git plugin. harness re-runs the create-time checks on the new Harness. Returns the values before and after. A suspended release is refused unless force is true; a GitOps-owned one needs mode commit."),
		mcp.WithString(argName, mcp.Required(), mcp.Description("Agent name")),
		mcp.WithString(argDisplayName, mcp.Description("New friendly name; \"\" clears it")),
		mcp.WithString(argDescription, mcp.Description("New description; \"\" clears it")),
		mcp.WithString(argSystemMessage, mcp.Description("New system prompt; \"\" restores the chart default. "+systemMessageLimit)),
		mcp.WithString(argModelConfig, mcp.Description("Name of an existing ModelConfig in the namespace")),
		mcp.WithString(argHarness, mcp.Description("Move the agent to another Harness of the namespace; \"\" selects the platform Harness. Existing Sessions keep their runtime")),
		mcp.WithString(argIconURL, mcp.Description("New avatar URL; \"\" clears it")),
		skillsReplaceProp,
		pluginsReplaceProp,
		egressReplaceProp,
		gitAuthReplaceProp,
		refreshSkillsProp,
		toolsetReplaceProp,
		labelsReplaceProp,
		annotationsReplaceProp,
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
		tooldesc.Describe("Delete an agent: its HelmRelease, which removes the Agent and its muster RemoteMCPServer.",
			"The chart's shared OCIRepository goes too when nothing else in the namespace uses it; the answer says why it was kept. A bare Agent or a suspended release is refused unless force is true; a GitOps-owned one needs mode commit."),
		mcp.WithString(argName, mcp.Required(), mcp.Description("Agent name")),
		mcp.WithBoolean(argForce, mcp.Description("Also delete bare Agent objects and suspended releases (default false). A GitOps-owned release is never deleted live: use mode commit")),
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
		tooldesc.Describe("One readiness verdict for an agent: ready, progressing, failed or unknown, with a one-line summary.",
			"From the Agent's conditions and warnings, the HelmRelease's conditions and history, and recent Warning events. Poll it after a create or update until ready. No Deployment or pod is involved: agents run as Substrate actors."),
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
		tooldesc.Describe("Dry run of create_agent, or of update_agent with update: true: the manifests and every violation. Writes nothing.",
			"Checks the name, modelConfig, harness and toolset (required for a create), pins the skills and plugins and validates the values against the chart schema. mode answers as the write would. With update: true, lists, labels and annotations replace the whole value, as in update_agent."),
		mcp.WithString(argName, mcp.Required(), mcp.Description("Agent name")),
		mcp.WithString(argModelConfig, mcp.Description("ModelConfig name (required for a create)")),
		mcp.WithString(argHarness, mcp.Description(harnessDesc)),
		mcp.WithString(argDisplayName, mcp.Description("Friendly name")),
		mcp.WithString(argDescription, mcp.Description("Description")),
		mcp.WithString(argSystemMessage, mcp.Description("System prompt. "+systemMessageLimit)),
		mcp.WithString(argIconURL, mcp.Description("Avatar URL")),
		toolsetProp,
		skillsProp,
		pluginsProp,
		egressProp,
		gitAuthProp,
		refreshSkillsProp,
		labelsProp,
		annotationsProp,
		mcp.WithBoolean(argUpdate, mcp.Description("Validate as an update of the existing agent instead of a create (default false)")),
		mcp.WithBoolean(argForce, mcp.Description("With update: validate a change to a suspended HelmRelease anyway (default false). A GitOps-owned release is never written live: use mode commit")),
		nsProp,
		orgProp,
		clusterProp,
		modeProp,
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(true),
	), t.validateAgent)

	s.AddTool(mcp.NewTool(ToolListModelConfigs,
		tooldesc.Describe("List the ModelConfigs of a namespace: the values create_agent accepts for modelConfig.",
			"Each with provider, model, its Accepted condition and who manages it; agent-manager never writes them. In a tenant namespace without agents yet, the platform default ModelConfig comes with the first agent."),
		nsProp,
		orgProp,
		clusterProp,
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(false),
	), t.listModelConfigs)

	s.AddTool(mcp.NewTool(ToolListSkills,
		tooldesc.Describe("List the skills agents can mount, from the configured skill repositories, with each repository's head commit.",
			"Per repository: the ref read, the commit it resolved to (what a skills entry pins) and each SKILL.md's name, description and path. Cached briefly; refresh re-reads GitHub."),
		mcp.WithString(argRepository, mcp.Description("Only this repository (https://github.com/<owner>/<repo>); default: every configured one")),
		mcp.WithString(argRef, mcp.Description("Git ref to read; default: the default branch (the ref refreshSkills follows)")),
		mcp.WithBoolean(argRefresh, mcp.Description("Bypass the cache (default false)")),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(true),
	), t.listSkills)

	s.AddTool(mcp.NewTool(ToolListSessions,
		tooldesc.Describe("List your conversations with one agent: id, name, state, failure and timestamps.",
			"Read through agentgateway as you. allCreators includes every person's sessions, when the controller authorizes you for it."),
		mcp.WithString(argName, mcp.Required(), mcp.Description("Agent name")),
		nsProp,
		mcp.WithBoolean(argAllCreators, mcp.Description("Include every person's sessions of the agent, when the controller authorizes the caller for it (default false)")),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(false),
	), t.listSessions)

	s.AddTool(mcp.NewTool(ToolGetSession,
		tooldesc.Describe("Read a conversation's turns, oldest first. To poll, use lastTurns: 1 until the last turn's state is final.",
			fmt.Sprintf("Final states: COMPLETED, FAILED, CANCELED, REJECTED, INPUT_REQUIRED. detail: summary (default) gives per turn the messages, state and one stub per tool call; transcript every message and artifact, tool results as stubs; full the raw A2A tasks. The oldest turns are dropped to fit maxBytes (default %d; omittedTurns counts them). One turn or tool call whole: get_session_turn.", agents.DefaultSessionMaxBytes)),
		mcp.WithString(argSessionID, mcp.Required(), mcp.Description("Session id (from start_session or list_sessions)")),
		mcp.WithString(argDetail, mcp.Enum(agents.DetailSummary, agents.DetailTranscript, agents.DetailFull)),
		mcp.WithNumber(argLastTurns, mcp.Description("Only the last n turns")),
		maxBytesProp,
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(false),
	), t.getSession)

	s.AddTool(mcp.NewTool(ToolGetSessionTurn,
		tooldesc.Describe("Read one turn of a conversation, or with toolCall one tool call's arguments and full response.",
			"detail: transcript (default) clips tool arguments and gives tool results as stubs (read one with toolCall); full gives the raw A2A objects. If the answer is cut to fit maxBytes, call again with fromMessage set to nextMessage."),
		mcp.WithString(argSessionID, mcp.Required(), mcp.Description("Session id")),
		mcp.WithString(argTurn, mcp.Required(), mcp.Description("taskId, index (from 0) or last")),
		mcp.WithString(argDetail, mcp.Enum(agents.DetailTranscript, agents.DetailFull)),
		mcp.WithString(argToolCall, mcp.Description("Index or id of a tool call in this turn")),
		mcp.WithNumber(argFromMessage, mcp.Description("First message index, default 0")),
		maxBytesProp,
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(false),
	), t.getSessionTurn)

	s.AddTool(mcp.NewTool(ToolStartSession,
		tooldesc.Describe("Start a conversation with an agent as you, optionally sending the first message. Read replies with get_session.",
			"Refused with conflict while the agent has no ready revision (get_agent_status). With message it answers once the agent accepted the turn; the agent keeps working. Without it, send the first message yourself over A2A (tenant <namespace>/<name>, the session's contextId). The agent runs its tools as you. Pass a requestId and reuse it on a retry: it never starts a second turn."),
		mcp.WithString(argName, mcp.Required(), mcp.Description("Agent name")),
		nsProp,
		mcp.WithString(argMessage, mcp.Description(fmt.Sprintf("The first user message, text only, at most %d characters; omit it to send the first message yourself.", agents.MaxSessionMessageLength))),
		mcp.WithString(argRequestID, mcp.Description("Idempotency key (at most 128 characters): pass one and reuse it on a retry; without it a retry starts a second session.")),
		mcp.WithString(argSessionName, mcp.Description("Display name of the conversation (at most 200 characters); default: unnamed")),
		mcp.WithString(argWorkspaceGrant, mcp.Description("The grant authorize_workspace returned to you: the session works in that workspace, its sandbox bound to you in muster. Others' are refused.")),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(true),
	), t.startSession)

	s.AddTool(mcp.NewTool(ToolDeleteSession,
		tooldesc.Describe("Delete one of your conversations and its state for good; it cannot be read back.",
			"Another person's session is refused as not_found. While another operation on it runs, it is refused with conflict: retry shortly. A workspace session is unbound in muster; warning says when that failed."),
		mcp.WithString(argSessionID, mcp.Required(), mcp.Description("Session id (a UUID, from start_session or list_sessions)")),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(false),
	), t.deleteSession)

	return s
}

type tools struct {
	svc *agents.Service
}

// schemaProp is a JSON-schema leaf with a description.
// stringSchema is the JSON Schema of one string item.
var stringSchema = map[string]any{"type": "string"}

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
	if req.GetBool(argDetails, false) {
		return jsonResult(map[string]any{"agents": list})
	}
	return jsonResult(map[string]any{"agents": agents.Summaries(list)})
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

func (t *tools) listSessions(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, err := req.RequireString(argName)
	if err != nil {
		return errResult(fmt.Errorf("%w: %v", agents.ErrInvalid, err)), nil
	}
	res, err := t.svc.ListSessions(ctx, req.GetString(argNamespace, ""), name, req.GetBool(argAllCreators, false))
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(res)
}

func (t *tools) getSession(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in agents.SessionQuery
	if err := req.BindArguments(&in); err != nil {
		return errResult(fmt.Errorf("%w: %v", agents.ErrInvalid, err)), nil
	}
	res, err := t.svc.GetSession(ctx, in)
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(res)
}

func (t *tools) getSessionTurn(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in agents.TurnQuery
	if err := req.BindArguments(&in); err != nil {
		return errResult(fmt.Errorf("%w: %v", agents.ErrInvalid, err)), nil
	}
	res, err := t.svc.GetSessionTurn(ctx, in)
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(res)
}

func (t *tools) deleteSession(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString(argSessionID)
	if err != nil {
		return errResult(fmt.Errorf("%w: %v", agents.ErrInvalid, err)), nil
	}
	res, err := t.svc.DeleteSession(ctx, id)
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(res)
}

func (t *tools) startSession(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in agents.StartSession
	if err := req.BindArguments(&in); err != nil {
		return errResult(fmt.Errorf("%w: %v", agents.ErrInvalid, err)), nil
	}
	res, err := t.svc.StartSession(ctx, in)
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

// errResult answers "<code>: <message>". A domain error's message leads with
// its sentinel ("forbidden: create HelmRelease …"); that lead is dropped so
// the code stands once.
func errResult(err error) *mcp.CallToolResult {
	_, code, sentinel := classify(err)
	msg := err.Error()
	if sentinel != nil {
		msg = strings.TrimPrefix(msg, sentinel.Error()+": ")
	}
	return mcp.NewToolResultError(code + ": " + msg)
}
