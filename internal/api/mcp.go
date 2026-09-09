package api

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

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
	argName          = "name"
	argDisplayName   = "displayName"
	argDescription   = "description"
	argSystemMessage = "systemMessage"
	argModelConfig   = "modelConfig"
	argHarness       = "harness"
	argSkills        = "skills"
	argToolset       = "toolset"
	argLabels        = "labels"
	argAnnotations   = "annotations"
	argForce         = "force"
	argUpdate        = "update"
	argRepository    = "repository"
	argRef           = "ref"
	argRefresh       = "refresh"
)

// NewMCPServer builds an MCP server exposing the same operations as the REST
// API as tools. Results are JSON text with the same shapes as the REST bodies.
func NewMCPServer(svc *agents.Service, version string) *mcpserver.MCPServer {
	s := mcpserver.NewMCPServer("agent-manager", version,
		mcpserver.WithToolCapabilities(false),
		mcpserver.WithInstructions("Manage the agents of the Agent Platform on kagent main. An agent is a kagent.dev/v1alpha3 AgentTemplate in the kagent namespace (description, system prompt, model config, immutable skills, one MCP binding to the platform's muster gateway) that kagent compiles for every Harness admitting it; conversations are AgentInstances created from the template over kagent's own API, not here. Call get_info first for the managed namespaces, the default Harness and the capability flags; list_model_configs before create_agent (the modelConfig must exist in the namespace); list_skills for the skills an agent can mount (it reports each skill's commit — skills are pinned to immutable sources). Names are DNS-1123 labels the caller chooses and confirms — the service never derives a name from a display name. validate_agent is a dry run of create/update. Every agent declares a toolset — create_agent requires it: the selectors (preset:<name>, server:<name>, workflow:<name>, tool:<name>) that bound which of the gateway's tools the agent's meta-tools can see and call; presets shipped on every installation: read-only, none, infrastructure, agent-platform, full. The toolset travels as a per-agent copy of the platform's muster RemoteMCPServer (the toolset carrier, muster-<agent>) carrying the X-Muster-Toolset header; a toolset change never compiles a new template revision. list_agents reports each agent's toolset, or implicitFullAccess: true for a template that binds the platform server directly — assign it one with update_agent. Templates applied from git (managed: gitops) or written by someone else (managed: external) are read-only here unless force is passed."),
	)
	t := &tools{svc: svc}

	skillsProp := mcp.WithObject(argSkills,
		mcp.Description("Skills the agent mounts, each pinned to an immutable source: {gitRefs: [{url, path, ref, name}], refs: [digest-pinned OCI references]}. ref must be a full git commit id (40 or 64 hex characters) — take gitRefs entries from list_skills, which resolves the commit; refs must be <repository>@sha256:<digest>. A branch, a tag or an OCI tag is refused."),
		mcp.Properties(map[string]any{
			"refs": schemaArray(schemaProp("string", "Digest-pinned OCI skill reference (<repository>@sha256:<64 hex digits>)"), "OCI skill references"),
			"gitRefs": schemaArray(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url":  schemaProp("string", "Repository URL (http or https)"),
					"path": schemaProp("string", "Skill directory (holds SKILL.md); empty for the repository root"),
					"ref":  schemaProp("string", "Full git commit id (40 or 64 hex characters); list_skills reports it as commit"),
					"name": schemaProp("string", "Skill name under the template; defaults to the directory name"),
				},
				"required": []string{"url", "ref"},
			}, "Git repository skills"),
		}),
	)
	toolsetDesc := "Toolset: the selectors that bound which of the gateway's tools the agent's meta-tools can see and call. Each is preset:<name>, server:<name>, workflow:<name> or tool:<name> (exact names; at most 32 — define a preset for more). Shipped presets: preset:read-only, preset:none (a chat-only agent without tools), preset:infrastructure, preset:agent-platform, preset:full (every tool the gateway exposes). Carried as the X-Muster-Toolset header of the agent's toolset carrier (RemoteMCPServer muster-<agent>, a copy of the platform's muster server); muster resolves it per caller. The former toolNames argument is gone: it never narrowed anything against muster."
	toolsetProp := mcp.WithArray(argToolset, mcp.Description(toolsetDesc), mcp.WithStringItems())
	toolsetRequiredProp := mcp.WithArray(argToolset, mcp.Required(), mcp.Description("REQUIRED. "+toolsetDesc), mcp.WithStringItems())
	toolsetReplaceProp := mcp.WithArray(argToolset, mcp.Description("Replaces the agent's whole toolset with this list (an empty list is refused: use [\"preset:none\"] for no tools); changes the carrier only, never the template. "+toolsetDesc), mcp.WithStringItems())
	harnessProp := mcp.WithString(argHarness, mcp.Description("Name of the kagent Harness that runs the agent (the runtime: kagent for the Go ADK, claude for the Claude harness, …); the template is labelled kagent.dev/harness=<name> and a Harness admitting it must exist — refused otherwise, naming the Harnesses and what they admit. Omit for the installation default (get_info: kagent.defaultHarness). The former runtime argument is gone."))
	labelsProp := mcp.WithObject(argLabels, mcp.Description("Extra labels on the AgentTemplate (string values); a Harness with a foreign admission selector is matched through them."), mcp.AdditionalProperties(map[string]any{"type": "string"}))
	annotationsProp := mcp.WithObject(argAnnotations, mcp.Description("Extra annotations on the AgentTemplate (string values)."), mcp.AdditionalProperties(map[string]any{"type": "string"}))
	nsProp := mcp.WithString(argNamespace, mcp.Description("Namespace of the agent; default: the installation's kagent namespace (get_info reports the managed ones)."))

	s.AddTool(mcp.NewTool(ToolGetInfo,
		mcp.WithDescription("Read-only. Report the service version, the managed namespaces, the served kagent.dev API versions, the platform's muster server and default Harness, the capability flags (what an AgentTemplate can and cannot express: perToolSelection, runtime, iconUrl, skillGitAuthSecret, mutableSkillRefs are false on kagent main) and how writes are authenticated. Call first."),
		mcp.WithReadOnlyHintAnnotation(true),
	), t.getInfo)

	s.AddTool(mcp.NewTool(ToolListAgents,
		mcp.WithDescription("Read-only. List the AgentTemplates of a namespace: display name, description, model config, harness, skills, the declared toolset (or implicitFullAccess: true for a template that binds the platform server directly — it sees every tool the gateway exposes and still needs a toolset), the toolset carrier, ready (any admitting Harness Ready) with the per-Harness status, and how each is managed (agent-manager: created here, writable; gitops: applied from git, read-only without force; external: written by someone else, taken over with force). Templates carrying fields agent-manager does not compose list them under unmanagedFields."),
		nsProp,
		mcp.WithReadOnlyHintAnnotation(true),
	), t.listAgents)

	s.AddTool(mcp.NewTool(ToolGetAgent,
		mcp.WithDescription("Read-only. Get one agent: its declaration, its AgentTemplate spec (the kagent contract), its declared toolset (or implicitFullAccess: true), its toolset carrier and the per-Harness status."),
		mcp.WithString(argName, mcp.Required(), mcp.Description("Agent name")),
		nsProp,
		mcp.WithReadOnlyHintAnnotation(true),
	), t.getAgent)

	s.AddTool(mcp.NewTool(ToolCreateAgent,
		mcp.WithDescription("WRITES: creates the agent's toolset carrier (RemoteMCPServer muster-<name>: a copy of the platform's muster server plus the X-Muster-Toolset header) and its kagent.dev/v1alpha3 AgentTemplate (labelled kagent.dev/harness=<harness>, binding the carrier), both in the kagent namespace as the caller; kagent then compiles the template for every admitting Harness. A toolset is required (refused without one); the modelConfig must exist, a Harness must admit the template, skills must be immutable references — every check runs before anything is written, and a template that fails to apply takes its carrier with it. Returns the applied manifests and the initial status; poll get_agent_status until the verdict is ready. The name is the DNS-1123 technical name the caller chose (confirm it with the user; it is never derived from displayName). Conversations are AgentInstances created from the template through kagent's own API (the portal), not here."),
		mcp.WithString(argName, mcp.Required(), mcp.Description("DNS-1123 technical name (max 63 chars): the AgentTemplate name")),
		mcp.WithString(argModelConfig, mcp.Required(), mcp.Description("Name of an existing kagent ModelConfig in the namespace (list_model_configs)")),
		mcp.WithString(argDisplayName, mcp.Description("Friendly Unicode name (max 63 chars), shown by the portal (annotation ui.giantswarm.io/display-name)")),
		mcp.WithString(argDescription, mcp.Description("What the agent is for (spec.description)")),
		mcp.WithString(argSystemMessage, mcp.Description("System prompt (spec.systemPrompt); omit for the Harness default")),
		harnessProp,
		toolsetRequiredProp,
		skillsProp,
		labelsProp,
		annotationsProp,
		nsProp,
	), t.createAgent)

	s.AddTool(mcp.NewTool(ToolUpdateAgent,
		mcp.WithDescription("WRITES: merges the given fields into the agent's declaration (only the arguments passed change; skills replace their whole block, toolset replaces the whole list — the way to assign a toolset to an agent that reports implicitFullAccess; an empty string clears a field), re-composes and writes what differs: a toolset change updates the carrier only, anything else updates the AgentTemplate — which makes kagent compile a NEW REVISION for every admitting Harness while running AgentInstances keep the revision they were created with (the result's note says so). Returns the declaration before and after and the changed paths. Refused for GitOps-owned (managed: gitops) or externally written (managed: external) templates, and for templates carrying fields agent-manager does not compose, unless force is true."),
		mcp.WithString(argName, mcp.Required(), mcp.Description("Agent name")),
		mcp.WithString(argDisplayName, mcp.Description("New friendly name; \"\" clears it")),
		mcp.WithString(argDescription, mcp.Description("New description; \"\" clears it")),
		mcp.WithString(argSystemMessage, mcp.Description("New system prompt; \"\" clears it")),
		mcp.WithString(argModelConfig, mcp.Description("Name of an existing ModelConfig in the namespace")),
		mcp.WithString(argHarness, mcp.Description("Move the agent to another Harness (a Harness admitting the relabelled template must exist); \"\" restores the installation default")),
		skillsProp,
		toolsetReplaceProp,
		labelsProp,
		annotationsProp,
		mcp.WithBoolean(argForce, mcp.Description("Write even when the template is GitOps-owned or externally written, and overwrite fields agent-manager does not compose (default false)")),
		nsProp,
		mcp.WithIdempotentHintAnnotation(true),
	), t.updateAgent)

	s.AddTool(mcp.NewTool(ToolDeleteAgent,
		mcp.WithDescription("WRITES (destructive): deletes the agent's AgentTemplate and its toolset carrier (the RemoteMCPServer muster-<name>, only when agent-manager created it; the result says why one is kept). A GitOps-owned or externally written template is refused unless force is true."),
		mcp.WithString(argName, mcp.Required(), mcp.Description("Agent name")),
		mcp.WithBoolean(argForce, mcp.Description("Also delete GitOps-owned or externally written templates (default false)")),
		nsProp,
		mcp.WithDestructiveHintAnnotation(true),
	), t.deleteAgent)

	s.AddTool(mcp.NewTool(ToolGetAgentStatus,
		mcp.WithDescription("Read-only. One verdict (ready | progressing | failed | unknown) with a one-line summary, from the AgentTemplate's per-Harness status (Accepted, ResolvedRefs, Compatible, Ready, desired and latest successful revision, compile warnings; ready = any admitting Harness reports Ready for the current generation) and the toolset carrier's acceptance."),
		mcp.WithString(argName, mcp.Required(), mcp.Description("Agent name")),
		nsProp,
		mcp.WithReadOnlyHintAnnotation(true),
	), t.getAgentStatus)

	s.AddTool(mcp.NewTool(ToolValidateAgent,
		mcp.WithDescription("Read-only dry run of create_agent (or of update_agent when update is true): composes the AgentTemplate and the toolset carrier, checks the name, the modelConfig, the toolset (required for a create), the Harness admission and the skill sources, and returns the manifests and every violation. Nothing is written."),
		mcp.WithString(argName, mcp.Required(), mcp.Description("Agent name")),
		mcp.WithString(argModelConfig, mcp.Description("ModelConfig name (required for a create)")),
		mcp.WithString(argDisplayName, mcp.Description("Friendly name")),
		mcp.WithString(argDescription, mcp.Description("Description")),
		mcp.WithString(argSystemMessage, mcp.Description("System prompt")),
		harnessProp,
		toolsetProp,
		skillsProp,
		labelsProp,
		annotationsProp,
		mcp.WithBoolean(argUpdate, mcp.Description("Validate as an update of the existing agent instead of a create (default false)")),
		mcp.WithBoolean(argForce, mcp.Description("With update: ignore the ownership guards (default false)")),
		nsProp,
		mcp.WithReadOnlyHintAnnotation(true),
	), t.validateAgent)

	s.AddTool(mcp.NewTool(ToolListModelConfigs,
		mcp.WithDescription("Read-only. List the kagent ModelConfigs of a namespace (name, provider, model, Accepted condition, who manages it) — the values create_agent accepts for modelConfig. ModelConfigs are platform-admin owned; agent-manager never writes them."),
		nsProp,
		mcp.WithReadOnlyHintAnnotation(true),
	), t.listModelConfigs)

	s.AddTool(mcp.NewTool(ToolListSkills,
		mcp.WithDescription("Read-only. Discover the skills of the configured skill repositories (every SKILL.md in a GitHub repository, with its frontmatter name and description, the branch and the commit it resolves to) as gitRefs entries for create_agent/update_agent — pass the commit as ref. Results are cached briefly; refresh re-reads GitHub."),
		mcp.WithString(argRepository, mcp.Description("Only this repository (https://github.com/<owner>/<repo>); default: every configured one")),
		mcp.WithString(argRef, mcp.Description("Git ref to read; default: the default branch")),
		mcp.WithBoolean(argRefresh, mcp.Description("Bypass the cache (default false)")),
		mcp.WithReadOnlyHintAnnotation(true),
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

// schemaArray is a JSON-schema array of items with a description.
func schemaArray(items map[string]any, desc string) map[string]any {
	return map[string]any{"type": "array", "items": items, argDescription: desc}
}

func (t *tools) getInfo(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return jsonResult(t.svc.Info(ctx))
}

func (t *tools) listAgents(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	list, err := t.svc.List(ctx, req.GetString(argNamespace, ""))
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
	a, err := t.svc.Get(ctx, req.GetString(argNamespace, ""), name)
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(a)
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
	res, err := t.svc.Delete(ctx, req.GetString(argNamespace, ""), name, req.GetBool(argForce, false))
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
	st, err := t.svc.Status(ctx, req.GetString(argNamespace, ""), name)
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
	list, err := t.svc.ListModelConfigs(ctx, req.GetString(argNamespace, ""))
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
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("encode result: %v", err)), nil
	}
	return mcp.NewToolResultText(string(data)), nil
}

func errResult(err error) *mcp.CallToolResult {
	_, code := statusFor(err)
	return mcp.NewToolResultError(fmt.Sprintf("%s: %s", code, err.Error()))
}
