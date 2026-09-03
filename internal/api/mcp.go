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
	argIconURL       = "iconUrl"
	argRuntime       = "runtime"
	argSkills        = "skills"
	argToolNames     = "toolNames"
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
		mcpserver.WithInstructions("Manage the agents of the Agent Platform. An agent is a Flux HelmRelease of the agent chart (one release renders one kagent Agent) plus the shared per-namespace OCIRepository of that chart. Call get_info first for the managed namespaces and the chart version; list_model_configs before create_agent (the modelConfig must exist in the namespace); list_skills for the skills an agent can mount. Names are DNS-1123 labels the caller chooses and confirms — the service never derives a name from a display name. validate_agent is a dry run of create/update. Agents whose HelmRelease is applied from git (managed: gitops) are read-only here unless force is passed: change them in the GitOps repository instead."),
	)
	t := &tools{svc: svc}

	skillsProp := mcp.WithObject(argSkills,
		mcp.Description("Skills the agent mounts under /skills: {refs: [OCI image references], gitRefs: [{url, path, ref, name}], gitAuthSecretName}. Take gitRefs entries from list_skills."),
		mcp.Properties(map[string]any{
			"refs": schemaArray(schemaProp("string", "OCI skill image reference"), "OCI skill image references"),
			"gitRefs": schemaArray(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url":  schemaProp("string", "Repository URL"),
					"path": schemaProp("string", "Skill directory (holds SKILL.md); empty for the repository root"),
					"ref":  schemaProp("string", "Branch, tag or commit"),
					"name": schemaProp("string", "Mount name under /skills; defaults to the directory name"),
				},
				"required": []string{"url"},
			}, "Git repository skills"),
			"gitAuthSecretName": schemaProp("string", "Secret in the agent's namespace for private gitRefs (key token, or a kubernetes.io/ssh-auth secret)"),
		}),
	)
	toolNamesProp := mcp.WithArray(argToolNames, mcp.Description("Narrow the muster tools the agent sees to these names; omit (or pass []) for every tool the gateway exposes, the chart default."), mcp.WithStringItems())
	labelsProp := mcp.WithObject(argLabels, mcp.Description("Extra labels on the Agent (string values)."), mcp.AdditionalProperties(map[string]any{"type": "string"}))
	annotationsProp := mcp.WithObject(argAnnotations, mcp.Description("Extra annotations on the Agent (string values)."), mcp.AdditionalProperties(map[string]any{"type": "string"}))
	nsProp := mcp.WithString(argNamespace, mcp.Description("Namespace of the agent; default: the installation's kagent namespace (get_info reports the managed ones)."))

	s.AddTool(mcp.NewTool(ToolGetInfo,
		mcp.WithDescription("Read-only. Report the service version, the agent chart (OCI URL, tracked semver range, resolved latest version, which values schema validates right now), the managed namespaces, the capability flags, the Flux settings composed into every agent and how writes are authenticated. Call first."),
		mcp.WithReadOnlyHintAnnotation(true),
	), t.getInfo)

	s.AddTool(mcp.NewTool(ToolListAgents,
		mcp.WithDescription("Read-only. List the agents of a namespace: display name, description, model config, runtime, skills, tool names, Agent Ready/Accepted conditions, the owning HelmRelease (Ready, chart version) and how each is managed (helmrelease: writable here; gitops: applied from git, read-only without force; none: a bare Agent CR). HelmReleases of the agent chart that have not rendered an Agent yet are listed too (exists: false)."),
		nsProp,
		mcp.WithReadOnlyHintAnnotation(true),
	), t.listAgents)

	s.AddTool(mcp.NewTool(ToolGetAgent,
		mcp.WithDescription("Read-only. Get one agent with its HelmRelease values (the chart contract) and conditions."),
		mcp.WithString(argName, mcp.Required(), mcp.Description("Agent name")),
		nsProp,
		mcp.WithReadOnlyHintAnnotation(true),
	), t.getAgent)

	s.AddTool(mcp.NewTool(ToolCreateAgent,
		mcp.WithDescription("WRITES: creates a Flux HelmRelease of the agent chart named after the agent (and the shared OCIRepository of the chart in the namespace when it does not exist yet); helm-controller then renders the kagent Agent and kagent runs it. The values are validated against the chart's values.schema.json and the modelConfig must exist in the namespace before anything is applied — a failure writes nothing and lists the valid model configs. Returns the applied manifests and the initial status; poll get_agent_status until the verdict is ready. The name is the DNS-1123 technical name the caller chose (confirm it with the user; it is never derived from displayName)."),
		mcp.WithString(argName, mcp.Required(), mcp.Description("DNS-1123 technical name (max 63 chars): the HelmRelease and Agent name")),
		mcp.WithString(argModelConfig, mcp.Required(), mcp.Description("Name of an existing kagent ModelConfig in the namespace (list_model_configs)")),
		mcp.WithString(argDisplayName, mcp.Description("Friendly Unicode name (max 63 chars), shown by the portal")),
		mcp.WithString(argDescription, mcp.Description("What the agent is for")),
		mcp.WithString(argSystemMessage, mcp.Description("System prompt; omit for the chart's default prompt")),
		mcp.WithString(argIconURL, mcp.Description("Avatar URL (chart agent.iconUrl); omit unless the installation serves avatars")),
		mcp.WithString(argRuntime, mcp.Description("kagent runtime: go (default) or python"), mcp.Enum("go", "python")),
		skillsProp,
		toolNamesProp,
		labelsProp,
		annotationsProp,
		nsProp,
	), t.createAgent)

	s.AddTool(mcp.NewTool(ToolUpdateAgent,
		mcp.WithDescription("WRITES: merges the given fields into the agent's HelmRelease values (only the arguments passed change; skills and toolNames replace their whole block; an empty string clears a field back to the chart default), validates the result against the chart schema and updates the HelmRelease — helm-controller upgrades the Agent. Returns the values before and after and the changed paths. Refused for GitOps-owned (managed: gitops) or suspended releases unless force is true."),
		mcp.WithString(argName, mcp.Required(), mcp.Description("Agent name")),
		mcp.WithString(argDisplayName, mcp.Description("New friendly name; \"\" clears it")),
		mcp.WithString(argDescription, mcp.Description("New description; \"\" clears it")),
		mcp.WithString(argSystemMessage, mcp.Description("New system prompt; \"\" restores the chart default")),
		mcp.WithString(argModelConfig, mcp.Description("Name of an existing ModelConfig in the namespace")),
		mcp.WithString(argIconURL, mcp.Description("New avatar URL; \"\" clears it")),
		mcp.WithString(argRuntime, mcp.Description("kagent runtime: go or python"), mcp.Enum("go", "python")),
		skillsProp,
		toolNamesProp,
		labelsProp,
		annotationsProp,
		mcp.WithBoolean(argForce, mcp.Description("Write even when the HelmRelease is GitOps-owned or suspended (default false)")),
		nsProp,
		mcp.WithIdempotentHintAnnotation(true),
	), t.updateAgent)

	s.AddTool(mcp.NewTool(ToolDeleteAgent,
		mcp.WithDescription("WRITES (destructive): deletes the HelmRelease that owns the agent — helm-controller uninstalls the release and removes the Agent, kagent stops the pods — and deletes the shared OCIRepository of the agent chart only when no other HelmRelease in the namespace references it (the result says why it was kept). A bare Agent CR without a HelmRelease is refused unless force is true (then the CR is deleted directly); a GitOps-owned or suspended HelmRelease is refused unless force is true."),
		mcp.WithString(argName, mcp.Required(), mcp.Description("Agent name")),
		mcp.WithBoolean(argForce, mcp.Description("Also delete bare Agent CRs, and GitOps-owned or suspended releases (default false)")),
		nsProp,
		mcp.WithDestructiveHintAnnotation(true),
	), t.deleteAgent)

	s.AddTool(mcp.NewTool(ToolGetAgentStatus,
		mcp.WithDescription("Read-only. One verdict (ready | progressing | failed | unknown) with a one-line summary, from the Agent conditions, the HelmRelease conditions and history, the Deployment readiness, the pods with their waiting reasons (a CrashLoopBackOff pod is phase Running; containerStatuses tell) and the recent Warning events (BackOff, Failed*)."),
		mcp.WithString(argName, mcp.Required(), mcp.Description("Agent name")),
		nsProp,
		mcp.WithReadOnlyHintAnnotation(true),
	), t.getAgentStatus)

	s.AddTool(mcp.NewTool(ToolValidateAgent,
		mcp.WithDescription("Read-only dry run of create_agent (or of update_agent when update is true): composes the OCIRepository and HelmRelease, checks the name, the modelConfig and the values against the agent chart's values.schema.json, and returns the manifests and every violation. Nothing is written."),
		mcp.WithString(argName, mcp.Required(), mcp.Description("Agent name")),
		mcp.WithString(argModelConfig, mcp.Description("ModelConfig name (required for a create)")),
		mcp.WithString(argDisplayName, mcp.Description("Friendly name")),
		mcp.WithString(argDescription, mcp.Description("Description")),
		mcp.WithString(argSystemMessage, mcp.Description("System prompt")),
		mcp.WithString(argIconURL, mcp.Description("Avatar URL")),
		mcp.WithString(argRuntime, mcp.Description("kagent runtime: go or python"), mcp.Enum("go", "python")),
		skillsProp,
		toolNamesProp,
		labelsProp,
		annotationsProp,
		mcp.WithBoolean(argUpdate, mcp.Description("Validate as an update of the existing agent instead of a create (default false)")),
		mcp.WithBoolean(argForce, mcp.Description("With update: ignore the GitOps/suspended guards (default false)")),
		nsProp,
		mcp.WithReadOnlyHintAnnotation(true),
	), t.validateAgent)

	s.AddTool(mcp.NewTool(ToolListModelConfigs,
		mcp.WithDescription("Read-only. List the kagent ModelConfigs of a namespace (name, provider, model, Accepted condition, who manages it) — the values create_agent accepts for modelConfig. ModelConfigs are platform-admin owned; agent-manager never writes them."),
		nsProp,
		mcp.WithReadOnlyHintAnnotation(true),
	), t.listModelConfigs)

	s.AddTool(mcp.NewTool(ToolListSkills,
		mcp.WithDescription("Read-only. Discover the skills of the configured skill repositories (every SKILL.md in a GitHub repository, with its frontmatter name and description) as gitRefs entries for create_agent/update_agent. Results are cached briefly; refresh re-reads GitHub."),
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
