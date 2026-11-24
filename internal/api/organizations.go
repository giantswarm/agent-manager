package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"

	"github.com/giantswarm/mcp-toolkit/tooldesc"
	orgv1alpha1 "github.com/giantswarm/organization-operator/api/v1alpha1"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/organizations"
)

// The organization tools. Every name holds "organization", so a muster
// toolset preset excludes them all with OrganizationToolPattern: the curated
// agent-manager-assistant carries the agent tools and not these.
const (
	ToolListOrganizations             = "list_organizations"
	ToolGetOrganization               = "get_organization"
	ToolCreateOrganization            = "create_organization"
	ToolUpdateOrganization            = "update_organization"
	ToolDeleteOrganization            = "delete_organization"
	ToolListOrganizationMembers       = "list_organization_members"
	ToolAddOrganizationMember         = "add_organization_member"
	ToolRemoveOrganizationMember      = "remove_organization_member"
	ToolSetOrganizationModel          = "set_organization_model"
	ToolListOrganizationConnector     = "list_organization_connectors"
	ToolSetOrganizationConnector      = "set_organization_connector"
	ToolSetOrganizationEgress         = "set_organization_egress_policy"
	ToolDeleteOrganizationObject      = "delete_organization_object"
	ToolReplaceOrganizationCredential = "replace_organization_credential"

	// OrganizationToolPattern matches, as a muster toolset rule
	// (filepath.Match on x_<server>_<tool>), every organization tool of the
	// agent-manager server and no agent tool: the exclude rule of the
	// agent-manager-assistant's preset.
	OrganizationToolPattern = "x_agent-manager_*organization*"
)

// OrganizationToolNames lists the organization tools.
func OrganizationToolNames() []string {
	return []string{
		ToolListOrganizations, ToolGetOrganization, ToolCreateOrganization, ToolUpdateOrganization, ToolDeleteOrganization,
		ToolListOrganizationMembers, ToolAddOrganizationMember, ToolRemoveOrganizationMember,
		ToolSetOrganizationModel, ToolListOrganizationConnector, ToolSetOrganizationConnector, ToolSetOrganizationEgress,
		ToolDeleteOrganizationObject, ToolReplaceOrganizationCredential,
	}
}

const (
	argAdminGroups      = "adminGroups"
	argAdminUsers       = "adminUsers"
	argHarnesses        = "harnesses"
	argWorkerReplicas   = "workerReplicas"
	argConfirm          = "confirm"
	argRole             = "role"
	argGroup            = "group"
	argUser             = "user"
	argProvider         = "provider"
	argModel            = "model"
	argDefault          = "default"
	argCreate           = "create"
	argCredentialSecret = "credentialSecret" // #nosec G101 -- an argument name, not a credential
	argCredentialKey    = "credentialKey"    // #nosec G101 -- an argument name, not a credential
	argCredentialHeader = "credentialHeader" // #nosec G101 -- an argument name, not a credential
	argNoCredential     = "noCredential"     // #nosec G101 -- an argument name, not a credential
	argURL              = "url"
	argDestinations     = "destinations"
	argKind             = "kind"
	argSecret           = "secret"
	argKey              = "key"
	argMaxTokens        = "maxTokens"
	argPromptCaching    = "promptCaching"
	argCacheTTL         = "cacheTTL"
)

// The workerReplicas argument of create_organization and update_organization:
// the default of spec.agentPlatform.workers.replicas and when the workers run.
var (
	defaultWorkerReplicas           = strconv.Itoa(int(orgv1alpha1.DefaultAgentPlatformWorkerReplicas))
	workerReplicasRule              = "0 runs none. They run only while the organization has a model and an agent or a session"
	createWorkerReplicasDescription = "Warm workers of its WorkerPool; omitted means " + defaultWorkerReplicas + ", " + workerReplicasRule
	updateWorkerReplicasDescription = "Warm workers; omitted keeps the value (none set means " + defaultWorkerReplicas + "), " + workerReplicasRule
)

// JSON Schema keys of the destinations item.
const (
	schemaType       = "type"
	schemaObject     = "object"
	schemaProperties = "properties"
)

// Option configures NewMCPServer.
type Option func(*serverOptions)

type serverOptions struct {
	orgs      *organizations.Service
	lifecycle context.Context
}

func WithLifecycle(ctx context.Context) Option {
	return func(options *serverOptions) { options.lifecycle = ctx }
}

// WithOrganizations serves the organization tools and the credential route
// from orgs; without it they answer unsupported.
func WithOrganizations(orgs *organizations.Service) Option {
	return func(o *serverOptions) { o.orgs = orgs }
}

type orgTools struct {
	svc               *organizations.Service
	server            *mcpserver.MCPServer
	credentialWatches sync.Map
	lifecycle         context.Context
}

func (t *orgTools) addTool(server *mcpserver.MCPServer, tool mcp.Tool, handler mcpserver.ToolHandlerFunc) {
	if t.svc != nil && !t.svc.OrganizationToolsEnabled() {
		switch tool.Name {
		case ToolListOrganizations, ToolSetOrganizationModel, ToolReplaceOrganizationCredential, ToolDeleteOrganizationObject:
		default:
			return
		}
	}
	server.AddTool(tool, handler)
}

var errOrganizationsUnsupported = fmt.Errorf("%w: model management needs OAuth and downstream authorization; organization management also needs tenants.enabled", agents.ErrUnsupported)

// run answers unsupported without the service, else calls fn.
func (t *orgTools) run(fn func(context.Context, mcp.CallToolRequest) (any, error)) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if t.svc == nil {
			return errResult(errOrganizationsUnsupported), nil
		}
		out, err := fn(ctx, req)
		if err != nil {
			return errResult(err), nil
		}
		return jsonResult(out)
	}
}

func bindArgs(req mcp.CallToolRequest, into any) error {
	raw, err := json.Marshal(req.Params.Arguments)
	if err != nil {
		return fmt.Errorf("%w: arguments do not match the tool schema", agents.ErrInvalid)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return fmt.Errorf("%w: arguments do not match the tool schema", agents.ErrInvalid)
	}
	return nil
}

func addOrganizationTools(s *mcpserver.MCPServer, orgs *organizations.Service, lifecycle context.Context) {
	if lifecycle == nil {
		lifecycle = context.Background()
	}
	t := &orgTools{svc: orgs, server: s, lifecycle: lifecycle}
	orgNameProp := mcp.WithString(argName, mcp.Required(), mcp.Description("The organization's name: the name of its Organization (at most 56 characters, no dot)"))
	orgNSProp := mcp.WithString(argNamespace, mcp.Required(), mcp.Description("The organization namespace, e.g. agents-payments (list_organizations, get_organization)"))
	modelNSProp := mcp.WithString(argNamespace, mcp.Required(), mcp.Description("Organization namespace, or the configured installation namespace for cluster admins"))
	dryRun := mcp.WithBoolean(argDryRun, mcp.Description("Check every write with the API server as you and write nothing (default false)"))
	modeProp := mcp.WithString(argMode, mcp.Enum(agents.ModeApply, agents.ModeCommit), mcp.Description("apply (default): write live as you; refused with gitops_owned for what git applies. commit: dry-run as you, then open a pull request as you"))
	credentialRequestID := mcp.WithString(argRequestID, mcp.Description("Idempotency key, at most 128 characters. Reuse on retries; use a fresh key for another save or a refreshed confirmation."))
	stringList := map[string]any{"type": "string"}
	credSecretDesc := "Secret in the target namespace, by name; organizations need tenant-credential-...; never a value"

	t.addTool(s, mcp.NewTool(ToolListOrganizations,
		tooldesc.Describe("List the organizations (Organizations with the Agent Platform) you manage or belong to.",
			"Each with its namespace, Orphaned condition, deleting, and permissions (manage, member, createAgents: create on HelmReleases there, what create_agent needs), asked as you; a missing createAgents is not known. Where you manage it, organization is its summary (admins, harnesses, workers, Ready, DefaultModelConfigured, the last revision Flux applied (lastAppliedRevision) and in mode commit its commit and file (lastAppliedRevisionUrl, lastAppliedFile, lastAppliedFileUrl)). none says who can add you."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(false),
	), t.run(func(ctx context.Context, req mcp.CallToolRequest) (any, error) {
		return t.svc.List(ctx)
	}))

	t.addTool(s, mcp.NewTool(ToolGetOrganization,
		tooldesc.Describe("Read one organization and what deleting it would erase.",
			"erases lists, as you can read them in the organization namespace, its agents, HelmReleases, models, connectors, egress policies, credential Secrets (names only), member bindings and snapshot prefix; what you may not read is named in unreadable. sessions counts, per agent and across every person, the running and stopped sessions the erase deletes; when they cannot be listed it is null and sessionsUnavailable says why."),
		orgNameProp,
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(false),
	), t.run(func(ctx context.Context, req mcp.CallToolRequest) (any, error) {
		return t.svc.Get(ctx, req.GetString(argName, ""))
	}))

	t.addTool(s, mcp.NewTool(ToolCreateOrganization,
		tooldesc.Describe("Create an organization as you: an Organization with the Agent Platform enabled and its admins.",
			"Dry-runs as you first. The admins go in spec.admins: agent-platform-operator creates the organization namespace and binds them to agent-platform:admin there; list_organizations shows the namespace once it reports it. An Organization of that name that exists is refused. mode commit opens one pull request instead. Prefer a group as admin. Needs agent-platform:organizations-admin, bound cluster-wide."),
		orgNameProp,
		mcp.WithString(argDisplayName, mcp.Description("Name people see")),
		mcp.WithArray(argAdminGroups, mcp.Description("Admins: identity provider groups, as the API server sees them (preferred)"), mcp.Items(stringList)),
		mcp.WithArray(argAdminUsers, mcp.Description("Admins: users, as the API server sees them"), mcp.Items(stringList)),
		mcp.WithArray(argHarnesses, mcp.Description("Curated Harnesses the organization gets"), mcp.Items(stringList)),
		mcp.WithNumber(argWorkerReplicas, mcp.Description(createWorkerReplicasDescription)),
		dryRun, modeProp,
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(false),
	), t.run(func(ctx context.Context, req mcp.CallToolRequest) (any, error) {
		var in struct {
			Name           string   `json:"name"`
			DisplayName    string   `json:"displayName"`
			AdminGroups    []string `json:"adminGroups"`
			AdminUsers     []string `json:"adminUsers"`
			Harnesses      []string `json:"harnesses"`
			WorkerReplicas *int32   `json:"workerReplicas"`
			DryRun         bool     `json:"dryRun"`
			Mode           string   `json:"mode"`
		}
		if err := bindArgs(req, &in); err != nil {
			return nil, err
		}
		return t.svc.Create(ctx, organizations.CreateRequest{Name: in.Name, DisplayName: in.DisplayName,
			Harnesses: in.Harnesses, WorkerReplicas: in.WorkerReplicas, AdminGroups: in.AdminGroups, AdminUsers: in.AdminUsers, DryRun: in.DryRun, Mode: in.Mode})
	}))

	t.addTool(s, mcp.NewTool(ToolUpdateOrganization,
		tooldesc.Describe("Change an organization's display name, admins, harnesses or worker replicas as you.",
			"An omitted field stays; harnesses replaces the whole list, adminGroups the admin groups and adminUsers the admin users ([] removes them); displayName \"\" removes it. Dry-runs as you first. agent-platform-operator follows spec.admins with the admins' binding. An organization Flux applies from git is changed with mode commit. Needs agent-platform:organizations-admin."),
		orgNameProp,
		mcp.WithString(argDisplayName, mcp.Description("Name people see")),
		mcp.WithArray(argAdminGroups, mcp.Description("Admin groups, replacing the ones in spec.admins"), mcp.Items(stringList)),
		mcp.WithArray(argAdminUsers, mcp.Description("Admin users, replacing the ones in spec.admins"), mcp.Items(stringList)),
		mcp.WithArray(argHarnesses, mcp.Description("Curated Harnesses the organization gets"), mcp.Items(stringList)),
		mcp.WithNumber(argWorkerReplicas, mcp.Description(updateWorkerReplicasDescription)),
		dryRun, modeProp,
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
	), t.run(func(ctx context.Context, req mcp.CallToolRequest) (any, error) {
		var in struct {
			Name           string    `json:"name"`
			DisplayName    *string   `json:"displayName"`
			AdminGroups    *[]string `json:"adminGroups"`
			AdminUsers     *[]string `json:"adminUsers"`
			Harnesses      *[]string `json:"harnesses"`
			WorkerReplicas *int32    `json:"workerReplicas"`
			DryRun         bool      `json:"dryRun"`
			Mode           string    `json:"mode"`
		}
		if err := bindArgs(req, &in); err != nil {
			return nil, err
		}
		return t.svc.Update(ctx, organizations.UpdateRequest{Name: in.Name, DisplayName: in.DisplayName,
			AdminGroups: in.AdminGroups, AdminUsers: in.AdminUsers, Harnesses: in.Harnesses, WorkerReplicas: in.WorkerReplicas, DryRun: in.DryRun, Mode: in.Mode})
	}))

	t.addTool(s, mcp.NewTool(ToolDeleteOrganization,
		tooldesc.Describe("Delete an organization as you, which erases it: its namespace, agents, sessions, snapshots and credentials.",
			"Without confirm it only answers what would be erased: show that to the person and ask them to type its name, then call again with confirm set to it. Marks the object kustomize.toolkit.fluxcd.io/prune: enabled, the delete intent, then deletes it; the operator erases it. mode commit: without confirm it opens the pull request removing it from git and marks the object; once merged (Pending erase), confirm erases it. Session tombstones stay. Needs agent-platform:organizations-admin."),
		orgNameProp,
		mcp.WithString(argConfirm, mcp.Description("The organization's name, as the person typed it")),
		dryRun, modeProp,
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(false),
	), t.run(func(ctx context.Context, req mcp.CallToolRequest) (any, error) {
		return t.svc.Delete(ctx, organizations.DeleteRequest{Name: req.GetString(argName, ""),
			Confirm: req.GetString(argConfirm, ""), DryRun: req.GetBool(argDryRun, false), Mode: req.GetString(argMode, "")})
	}))

	t.addTool(s, mcp.NewTool(ToolListOrganizationMembers,
		tooldesc.Describe("List an organization's members: the groups and people bound to agent-platform:admin, editor or user, groups first.",
			"Read as you; every member role (user, editor or admin) may read them, from agent-platform chart 4.179.0 on. An admin's from says it comes from the Organization's spec.admins, which update_organization changes."),
		orgNSProp,
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(false),
	), t.run(func(ctx context.Context, req mcp.CallToolRequest) (any, error) {
		return t.svc.ListMembers(ctx, req.GetString(argNamespace, ""))
	}))

	memberProps := func() []mcp.ToolOption {
		return []mcp.ToolOption{
			orgNSProp,
			mcp.WithString(argGroup, mcp.Description("Identity provider group, as the API server sees it; offer a group first: membership then changes in the identity provider")),
			mcp.WithString(argUser, mcp.Description("A person, as the API server sees them; when no group fits")),
			dryRun, modeProp,
		}
	}
	addOpts := append(memberProps(),
		mcp.WithString(argRole, mcp.Required(), mcp.Description("editor creates agents; user uses them. Not admin: the admins are the Organization's spec.admins (update_organization)"), mcp.Enum("editor", "user")),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false))
	t.addTool(s, mcp.NewTool(ToolAddOrganizationMember, append([]mcp.ToolOption{
		tooldesc.Describe("Add a group or a person to an organization with a role, as you.",
			"Writes the RoleBinding organization-<role>s, dry run first; a member already holding the role is left as is. The admin role is refused: the admins are the Organization's spec.admins, changed with update_organization. Needs agent-platform:admin in the organization, or agent-platform:organizations-admin."),
	}, addOpts...)...), t.run(func(ctx context.Context, req mcp.CallToolRequest) (any, error) {
		return t.svc.AddMember(ctx, memberRequest(req))
	}))

	removeOpts := append(memberProps(),
		mcp.WithString(argRole, mcp.Description("Only this role; omit for editor and user"), mcp.Enum("editor", "user")),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false))
	t.addTool(s, mcp.NewTool(ToolRemoveOrganizationMember, append([]mcp.ToolOption{
		tooldesc.Describe("Remove a group or a person from an organization's roles, as you.",
			"From every binding of the role that holds them, dry run first; an emptied binding goes. An admin stays one (note says so): the admins are the Organization's spec.admins, changed with update_organization. Needs agent-platform:admin in the organization."),
	}, removeOpts...)...), t.run(func(ctx context.Context, req mcp.CallToolRequest) (any, error) {
		return t.svc.RemoveMember(ctx, memberRequest(req))
	}))

	t.addTool(s, mcp.NewTool(ToolSetOrganizationModel,
		tooldesc.Describe("Create or change an organization or installation model as you; credentials are Secret references.",
			"A new model becomes the default when none is set; default true moves the mark. New Anthropic models cache prompts ("+agents.DefaultCacheTTL+") unless promptCaching is false; omitted settings stay. Never pass a key: missing credentials open the form (credentials[].next); another login: 403 credential_operation_other_login. Vertex AI calls bypass the LLM gateway's metering, rate limits and spend reporting. list_model_configs lists them. Needs agent-platform:admin (organization) or cluster-admin (installation models)."),
		modelNSProp,
		mcp.WithString(argName, mcp.Required(), mcp.Description("ModelConfig name")),
		mcp.WithString(argProvider, mcp.Description("Provider as kagent's ModelConfig takes it, e.g. Anthropic, OpenAI; required to create; a change needs a new credentialSecret too")),
		mcp.WithString(argModel, mcp.Description("Model id, e.g. claude-sonnet-4-6; required to create")),
		mcp.WithString("projectID", mcp.Description("Vertex AI Google project ID; required to create a Vertex model")),
		mcp.WithString("location", mcp.Description("Vertex AI location: global, us, eu or a Google region")),
		mcp.WithString("endpoint", mcp.Description("Installation models only: OpenAI/Anthropic base URL or Ollama host, without userinfo, fragment or credential query; empty removes it")),
		mcp.WithBoolean("insecureSkipTLSVerify", mcp.Description("Installation models only: disable model endpoint certificate verification; default false")),
		mcp.WithBoolean("noCredential", mcp.Description("Installation models only: the endpoint requires no credential; never valid for Vertex AI")),
		mcp.WithString(argCredentialSecret, mcp.Description(credSecretDesc+"; default and only written: tenant-credential-<name>")),
		mcp.WithString(argCredentialKey, mcp.Description("Key of that Secret; default GOOGLE_APPLICATION_CREDENTIALS for Vertex AI, apiKey otherwise")),
		mcp.WithBoolean(argDefault, mcp.Description("Make this the namespace's default model, moving the mark (default false)")),
		mcp.WithBoolean(argCreate, mcp.Description("Only create: a model of that name, live or in git, is refused with already_exists, not changed (default false: create or change)")),
		mcp.WithString(argDisplayName, mcp.Description("Name people see; \"\" removes it")),
		mcp.WithNumber(argMaxTokens, mcp.Description("Token limit of an answer (Anthropic, Mistral, OpenAI); 0 removes it: the provider's default")),
		mcp.WithBoolean(argPromptCaching, mcp.Description("Prompt caching (Anthropic); a new Anthropic model has it on, false turns it off")),
		mcp.WithString(argCacheTTL, mcp.Description("How long a cached prompt stays while caching is on: 5m or 1h (1h costs more per write)")),
		credentialRequestID, dryRun, modeProp,
		mcp.WithOutputSchema[organizations.ResourceResult](),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
	), t.runWrite(func(ctx context.Context, req mcp.CallToolRequest) (*organizations.ResourceResult, error) {
		var in struct {
			Namespace             string  `json:"namespace"`
			Name                  string  `json:"name"`
			Provider              string  `json:"provider"`
			Model                 string  `json:"model"`
			ProjectID             string  `json:"projectID"`
			Location              string  `json:"location"`
			Endpoint              *string `json:"endpoint"`
			InsecureSkipTLSVerify *bool   `json:"insecureSkipTLSVerify"`
			NoCredential          bool    `json:"noCredential"`
			CredentialSecret      string  `json:"credentialSecret"`
			CredentialKey         string  `json:"credentialKey"`
			Default               bool    `json:"default"`
			Create                bool    `json:"create"`
			DryRun                bool    `json:"dryRun"`
			Mode                  string  `json:"mode"`
			DisplayName           *string `json:"displayName"`
			MaxTokens             *int64  `json:"maxTokens"`
			PromptCaching         *bool   `json:"promptCaching"`
			CacheTTL              string  `json:"cacheTTL"`
		}
		if err := bindArgs(req, &in); err != nil {
			return nil, err
		}
		return t.svc.SetModel(ctx, organizations.ModelRequest(in))
	}))

	t.addTool(s, mcp.NewTool(ToolListOrganizationConnector,
		tooldesc.Describe("List an organization's connectors (RemoteMCPServers): URL, description and the credential headers by Secret name.",
			"Read as you. A credential in a URL written elsewhere shows as REDACTED. curated marks a read-only copy of what the platform curates. A credential's state: present or missing, whether its Secret exists (a dry-run create as you, never a read; unknown if you may not create it), not whether it holds the key. accepted, reason, message: kagent's Accepted condition. musterBindingOf: the agent whose muster binding it is (no connector). create_agent takes the rest as connectors."),
		orgNSProp,
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(false),
	), t.run(func(ctx context.Context, req mcp.CallToolRequest) (any, error) {
		list, err := t.svc.ListConnectors(ctx, req.GetString(argNamespace, ""))
		return map[string]any{"connectors": list}, err
	}))

	t.addTool(s, mcp.NewTool(ToolSetOrganizationConnector,
		tooldesc.Describe("Create or change an organization's connector (a RemoteMCPServer) as you; its credential is a Secret named by reference.",
			"A credential field sets the Secret-backed header it names (default: the only one, else Authorization; Secret tenant-credential-<name>, key token, the whole value); other headers stay; noCredential removes them. A URL with userinfo, a fragment, a credential-like or unparsable query is refused. Never pass a value: a missing one is a URL elicitation of the credential UI (credentials[].next); another login: 403 credential_operation_other_login. Needs agent-platform:admin."),
		orgNSProp,
		mcp.WithString(argName, mcp.Required(), mcp.Description("RemoteMCPServer name")),
		mcp.WithString(argURL, mcp.Description("MCP endpoint URL, without a credential in it; required to create")),
		mcp.WithString(argDescription, mcp.Description("What the server offers")),
		mcp.WithString(argCredentialHeader, mcp.Description("Header the credential goes in; default Authorization")),
		mcp.WithString(argCredentialSecret, mcp.Description(credSecretDesc)),
		mcp.WithString(argCredentialKey, mcp.Description("Key of that Secret; default token")),
		mcp.WithBoolean(argNoCredential, mcp.Description("Remove the credential header (default false)")),
		mcp.WithBoolean(argCreate, mcp.Description("Only create: a connector of that name, live or in git, is refused with already_exists, not changed (default false: create or change)")),
		credentialRequestID, dryRun, modeProp,
		mcp.WithOutputSchema[organizations.ResourceResult](),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
	), t.runWrite(func(ctx context.Context, req mcp.CallToolRequest) (*organizations.ResourceResult, error) {
		var in struct {
			Namespace        string `json:"namespace"`
			Name             string `json:"name"`
			URL              string `json:"url"`
			Description      string `json:"description"`
			CredentialHeader string `json:"credentialHeader"`
			CredentialSecret string `json:"credentialSecret"`
			CredentialKey    string `json:"credentialKey"`
			NoCredential     bool   `json:"noCredential"`
			Create           bool   `json:"create"`
			DryRun           bool   `json:"dryRun"`
			Mode             string `json:"mode"`
		}
		if err := bindArgs(req, &in); err != nil {
			return nil, err
		}
		return t.svc.SetConnector(ctx, organizations.ConnectorRequest(in))
	}))

	t.addTool(s, mcp.NewTool(ToolSetOrganizationEgress,
		tooldesc.Describe("Create or replace an organization's egress policy as you: origins its agents may reach, credentials by Secret name.",
			"destinations replaces the whole list. A credentialHeader makes the egress gateway add that header from the Secret (default tenant-credential-<policy>, key token). Never pass a value: a missing credential is a URL elicitation of the credential UI (credentials[].next); another login: 403 credential_operation_other_login. list_egress_policies lists them. Needs agent-platform:admin."),
		orgNSProp,
		mcp.WithString(argName, mcp.Required(), mcp.Description("EgressPolicy name")),
		mcp.WithArray(argDestinations, mcp.Required(), mcp.Description("Origins, each {origin, credentialHeader, credentialPrefix, credentialSecret, credentialKey}"), mcp.Items(map[string]any{
			schemaType: schemaObject,
			schemaProperties: map[string]any{
				"origin":            schemaProp("string", "https://host[:port]; a host may start with *."),
				argCredentialHeader: schemaProp("string", "Header the gateway adds; omit for no credential"),
				"credentialPrefix":  schemaProp("string", "Text before the value, e.g. \"Bearer \""),
				argCredentialSecret: schemaProp("string", credSecretDesc),
				argCredentialKey:    schemaProp("string", "Key of that Secret; default token"),
			},
			"required": []string{"origin"},
		})),
		mcp.WithBoolean(argCreate, mcp.Description("Only create: a policy of that name, live or in git, is refused with already_exists, not replaced (default false: create or replace)")),
		credentialRequestID, dryRun, modeProp,
		mcp.WithOutputSchema[organizations.ResourceResult](),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
	), t.runWrite(func(ctx context.Context, req mcp.CallToolRequest) (*organizations.ResourceResult, error) {
		var in struct {
			Namespace    string                            `json:"namespace"`
			Name         string                            `json:"name"`
			Destinations []organizations.EgressDestination `json:"destinations"`
			Create       bool                              `json:"create"`
			DryRun       bool                              `json:"dryRun"`
			Mode         string                            `json:"mode"`
		}
		if err := bindArgs(req, &in); err != nil {
			return nil, err
		}
		return t.svc.SetEgressPolicy(ctx, organizations.EgressRequest(in))
	}))

	t.addTool(s, mcp.NewTool(ToolReplaceOrganizationCredential,
		tooldesc.Describe("Replace an organization credential or installation model credential by its Secret and key, as you.",
			"Only the Secret is written. The form (credentials[].next) lists its consumers and destinations; another login: 403 credential_operation_other_login. Refused if unused or another key is referenced: the one-key Secret is replaced whole. Use mode commit for consumers applied from git. Never pass a value. Needs agent-platform:admin in an organization, cluster-admin for installation models."),
		modelNSProp,
		mcp.WithString(argSecret, mcp.Required(), mcp.Description(credSecretDesc)),
		mcp.WithString(argKey, mcp.Required(), mcp.Description("Key of that Secret the objects read, e.g. apiKey or token")),
		credentialRequestID, dryRun, modeProp,
		mcp.WithOutputSchema[organizations.ResourceResult](),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
	), t.runWrite(func(ctx context.Context, req mcp.CallToolRequest) (*organizations.ResourceResult, error) {
		var in struct {
			Namespace string `json:"namespace"`
			Secret    string `json:"secret"`
			Key       string `json:"key"`
			DryRun    bool   `json:"dryRun"`
			Mode      string `json:"mode"`
		}
		if err := bindArgs(req, &in); err != nil {
			return nil, err
		}
		return t.svc.ReplaceCredential(ctx, organizations.CredentialRequest(in))
	}))

	t.addTool(s, mcp.NewTool(ToolDeleteOrganizationObject,
		tooldesc.Describe("Delete an organization object or installation model as you.",
			"Dry run first. Its credential Secret stays. Deleting the default model leaves its namespace without one until another is marked. Copies the platform curates are refused. Needs agent-platform:admin in an organization, cluster-admin for installation models."),
		modelNSProp,
		mcp.WithString(argKind, mcp.Required(), mcp.Enum(organizations.KindModel, organizations.KindConnector, organizations.KindEgressPolicy)),
		mcp.WithString(argName, mcp.Required(), mcp.Description("Object name")),
		dryRun, modeProp,
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(false),
	), t.run(func(ctx context.Context, req mcp.CallToolRequest) (any, error) {
		return t.svc.DeleteResource(ctx, req.GetString(argNamespace, ""), req.GetString(argKind, ""), req.GetString(argName, ""), req.GetBool(argDryRun, false), req.GetString(argMode, ""))
	}))
}

func memberRequest(req mcp.CallToolRequest) organizations.MemberRequest {
	return organizations.MemberRequest{Namespace: req.GetString(argNamespace, ""), Role: req.GetString(argRole, ""),
		Group: req.GetString(argGroup, ""), User: req.GetString(argUser, ""), DryRun: req.GetBool(argDryRun, false), Mode: req.GetString(argMode, "")}
}
