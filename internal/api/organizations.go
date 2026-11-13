package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"

	apov1alpha1 "github.com/giantswarm/agent-platform-operator/api/v1alpha1"
	"github.com/giantswarm/mcp-toolkit/tooldesc"
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
// the operator's default (agent-platform-operator's DefaultWorkerReplicas) and
// when the workers run.
const (
	workerReplicasRule              = "0 runs none. They run only while the organization has a model and an agent or a session"
	createWorkerReplicasDescription = "Warm workers of its WorkerPool; omitted means 1, " + workerReplicasRule
	updateWorkerReplicasDescription = "Warm workers; omitted keeps the value (none set means 1), " + workerReplicasRule
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

var errOrganizationsUnsupported = fmt.Errorf("%w: this installation does not serve the organization tools (agent-manager's tenants.enabled is off)", agents.ErrUnsupported)

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
	parentProp := mcp.WithString(argNamespace, mcp.Required(), mcp.Description("The parent namespace the organization lives in, e.g. org-acme"))
	orgNameProp := mcp.WithString(argName, mcp.Required(), mcp.Description("The organization's name (at most 50 characters, no dot)"))
	orgNSProp := mcp.WithString(argNamespace, mcp.Required(), mcp.Description("The organization namespace, e.g. agents-payments-x7k2q (list_organizations, get_organization)"))
	dryRun := mcp.WithBoolean(argDryRun, mcp.Description("Check every write with the API server as you and write nothing (default false)"))
	modeProp := mcp.WithString(argMode, mcp.Enum(agents.ModeApply, agents.ModeCommit), mcp.Description("apply (default): write live as you; refused with gitops_owned for what git applies. commit: dry-run as you, then open a pull request as you"))
	credentialRequestID := mcp.WithString(argRequestID, mcp.Description("Idempotency key, at most 128 characters. Reuse on retries; use a fresh key for another save or a refreshed confirmation."))
	stringList := map[string]any{"type": "string"}
	credSecretDesc := "Secret in the organization namespace that holds the credential, by name (tenant-credential-...); never a value"

	s.AddTool(mcp.NewTool(ToolListOrganizations,
		tooldesc.Describe("List the organizations you manage or belong to, of one parent namespace or of all.",
			"Each with its organization namespace, Orphaned condition, deleting, and permissions (manage, member, createAgents), asked with your own token; a New agent goes where createAgents is true. Where you manage it, organization is its summary: harnesses, workers, Ready, DefaultModelConfigured, the last revision Flux applied (lastAppliedRevision) and in mode commit its commit and the organization's file (lastAppliedRevisionUrl, lastAppliedFile, lastAppliedFileUrl). none says who can add you when there is none."),
		mcp.WithString(argNamespace, mcp.Description("Parent namespace; omit for all")),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(false),
	), t.run(func(ctx context.Context, req mcp.CallToolRequest) (any, error) {
		return t.svc.List(ctx, req.GetString(argNamespace, ""))
	}))

	s.AddTool(mcp.NewTool(ToolGetOrganization,
		tooldesc.Describe("Read one organization and what deleting it would erase.",
			"erases lists, as you can read them in the organization namespace, its agents, HelmReleases, models, connectors, egress policies, credential Secrets (names only), member bindings and snapshot prefix; what you may not read is named in unreadable. sessions counts, per agent and across every person, the running and stopped sessions the erase deletes; when they cannot be listed it is null and sessionsUnavailable says why."),
		parentProp, orgNameProp,
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(false),
	), t.run(func(ctx context.Context, req mcp.CallToolRequest) (any, error) {
		return t.svc.Get(ctx, req.GetString(argNamespace, ""), req.GetString(argName, ""))
	}))

	s.AddTool(mcp.NewTool(ToolCreateOrganization,
		tooldesc.Describe("Create an organization as you, then bind its first admins in its namespace.",
			"Before any write, dry run too, refuses when you may not bind them or your right will not reach the new namespace (it does cluster-wide, or by a parent RoleBinding naming you that the operator mirrors: organizations-admin, or rbac-operator's cluster-admin). Waits for Ready, then writes organization-admins to agent-platform:admin as you. pending: call again with the same arguments. mode commit opens one pull request instead. Prefer a group as admin. Needs agent-platform:organizations-admin in the parent namespace."),
		parentProp, orgNameProp,
		mcp.WithString(argDisplayName, mcp.Description("Name people see")),
		mcp.WithArray(argAdminGroups, mcp.Description("First admins: identity provider groups, as the API server sees them (preferred)"), mcp.Items(stringList)),
		mcp.WithArray(argAdminUsers, mcp.Description("First admins: users, as the API server sees them"), mcp.Items(stringList)),
		mcp.WithArray(argHarnesses, mcp.Description("Curated Harnesses the organization gets"), mcp.Items(stringList)),
		mcp.WithNumber(argWorkerReplicas, mcp.Description(createWorkerReplicasDescription)),
		dryRun, modeProp,
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(false),
	), t.run(func(ctx context.Context, req mcp.CallToolRequest) (any, error) {
		var in struct {
			Namespace      string   `json:"namespace"`
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
		return t.svc.Create(ctx, organizations.CreateRequest{Parent: in.Namespace, Name: in.Name, DisplayName: in.DisplayName,
			Harnesses: in.Harnesses, WorkerReplicas: in.WorkerReplicas, AdminGroups: in.AdminGroups, AdminUsers: in.AdminUsers, DryRun: in.DryRun, Mode: in.Mode})
	}))

	s.AddTool(mcp.NewTool(ToolUpdateOrganization,
		tooldesc.Describe("Change an organization's display name, harnesses or worker replicas as you.",
			"An omitted field stays; harnesses replaces the whole list; displayName \"\" removes it. Dry-runs as you first. An organization Flux applies from git is changed with mode commit. Needs agent-platform:organizations-admin."),
		parentProp, orgNameProp,
		mcp.WithString(argDisplayName, mcp.Description("Name people see")),
		mcp.WithArray(argHarnesses, mcp.Description("Curated Harnesses the organization gets"), mcp.Items(stringList)),
		mcp.WithNumber(argWorkerReplicas, mcp.Description(updateWorkerReplicasDescription)),
		dryRun, modeProp,
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
	), t.run(func(ctx context.Context, req mcp.CallToolRequest) (any, error) {
		var in struct {
			Namespace      string    `json:"namespace"`
			Name           string    `json:"name"`
			DisplayName    *string   `json:"displayName"`
			Harnesses      *[]string `json:"harnesses"`
			WorkerReplicas *int32    `json:"workerReplicas"`
			DryRun         bool      `json:"dryRun"`
			Mode           string    `json:"mode"`
		}
		if err := bindArgs(req, &in); err != nil {
			return nil, err
		}
		return t.svc.Update(ctx, organizations.UpdateRequest{Parent: in.Namespace, Name: in.Name, DisplayName: in.DisplayName,
			Harnesses: in.Harnesses, WorkerReplicas: in.WorkerReplicas, DryRun: in.DryRun, Mode: in.Mode})
	}))

	s.AddTool(mcp.NewTool(ToolDeleteOrganization,
		tooldesc.Describe("Delete an organization as you, which erases it: its namespace, agents, sessions, snapshots and credentials.",
			"Without confirm it only answers what would be erased: show that to the person and ask them to type <parent>/<name>, then call again with confirm set to it. Marks the object kustomize.toolkit.fluxcd.io/prune: enabled, the delete intent, then deletes it; the operator erases it. mode commit: without confirm it opens the pull request removing it from git and marks the object; once merged (Pending erase), confirm erases it. Session tombstones stay. Needs agent-platform:organizations-admin."),
		parentProp, orgNameProp,
		mcp.WithString(argConfirm, mcp.Description("<parent>/<name>, as the person typed it")),
		dryRun, modeProp,
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(false),
	), t.run(func(ctx context.Context, req mcp.CallToolRequest) (any, error) {
		return t.svc.Delete(ctx, organizations.DeleteRequest{Parent: req.GetString(argNamespace, ""), Name: req.GetString(argName, ""),
			Confirm: req.GetString(argConfirm, ""), DryRun: req.GetBool(argDryRun, false), Mode: req.GetString(argMode, "")})
	}))

	s.AddTool(mcp.NewTool(ToolListOrganizationMembers,
		tooldesc.Describe("List an organization's members: the groups and people bound to agent-platform:admin, editor or user, groups first.",
			"Read as you; every member role (user, editor or admin) may read them, from agent-platform chart 4.179.0 on."),
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
		mcp.WithString(argRole, mcp.Required(), mcp.Description("admin manages members, models and credentials; editor creates agents; user uses them"), mcp.Enum("admin", "editor", "user")),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false))
	s.AddTool(mcp.NewTool(ToolAddOrganizationMember, append([]mcp.ToolOption{
		tooldesc.Describe("Add a group or a person to an organization with a role, as you.",
			"Writes the RoleBinding organization-<role>s (admins: organization-admins), dry run first; a member already holding the role is left as is. Needs agent-platform:admin in the organization, or agent-platform:organizations-admin."),
	}, addOpts...)...), t.run(func(ctx context.Context, req mcp.CallToolRequest) (any, error) {
		return t.svc.AddMember(ctx, memberRequest(req))
	}))

	removeOpts := append(memberProps(),
		mcp.WithString(argRole, mcp.Description("Only this role; omit for every role"), mcp.Enum("admin", "editor", "user")),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false))
	s.AddTool(mcp.NewTool(ToolRemoveOrganizationMember, append([]mcp.ToolOption{
		tooldesc.Describe("Remove a group or a person from an organization's roles, as you.",
			"From every binding of the role that holds them, dry run first; an emptied binding goes, except organization-admins: admission keeps its last admin. Needs agent-platform:admin in the organization."),
	}, removeOpts...)...), t.run(func(ctx context.Context, req mcp.CallToolRequest) (any, error) {
		return t.svc.RemoveMember(ctx, memberRequest(req))
	}))

	s.AddTool(mcp.NewTool(ToolSetOrganizationModel,
		tooldesc.Describe("Create or change an organization's model (a ModelConfig) as you; its API key is a Secret named by reference.",
			"The first model of an organization without a default becomes its default (annotation "+apov1alpha1.DefaultModelConfigAnnotation+"); default true moves the mark here. A new Anthropic model caches prompts ("+agents.DefaultCacheTTL+") unless promptCaching is false; an omitted argument keeps the stored value. Never pass a key: a missing one is a URL elicitation of the credential UI, link in credentials[].next. list_model_configs lists the models and settings. Needs agent-platform:admin."),
		orgNSProp,
		mcp.WithString(argName, mcp.Required(), mcp.Description("ModelConfig name")),
		mcp.WithString(argProvider, mcp.Description("Provider as kagent's ModelConfig takes it, e.g. Anthropic, OpenAI; required to create; a change needs a new credentialSecret too")),
		mcp.WithString(argModel, mcp.Description("Model id, e.g. claude-sonnet-4-6; required to create")),
		mcp.WithString(argCredentialSecret, mcp.Description(credSecretDesc+"; default tenant-credential-<name>")),
		mcp.WithString(argCredentialKey, mcp.Description("Key of that Secret; default apiKey")),
		mcp.WithBoolean(argDefault, mcp.Description("Make this the organization's default model, moving the mark (default false)")),
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
			Namespace        string  `json:"namespace"`
			Name             string  `json:"name"`
			Provider         string  `json:"provider"`
			Model            string  `json:"model"`
			CredentialSecret string  `json:"credentialSecret"`
			CredentialKey    string  `json:"credentialKey"`
			Default          bool    `json:"default"`
			Create           bool    `json:"create"`
			DryRun           bool    `json:"dryRun"`
			Mode             string  `json:"mode"`
			DisplayName      *string `json:"displayName"`
			MaxTokens        *int64  `json:"maxTokens"`
			PromptCaching    *bool   `json:"promptCaching"`
			CacheTTL         string  `json:"cacheTTL"`
		}
		if err := bindArgs(req, &in); err != nil {
			return nil, err
		}
		return t.svc.SetModel(ctx, organizations.ModelRequest(in))
	}))

	s.AddTool(mcp.NewTool(ToolListOrganizationConnector,
		tooldesc.Describe("List an organization's connectors (RemoteMCPServers): URL, description and the credential headers by Secret name.",
			"Read as you. A credential in a URL written elsewhere is shown as REDACTED. curated marks a read-only copy of what the platform curates."),
		orgNSProp,
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(false),
	), t.run(func(ctx context.Context, req mcp.CallToolRequest) (any, error) {
		list, err := t.svc.ListConnectors(ctx, req.GetString(argNamespace, ""))
		return map[string]any{"connectors": list}, err
	}))

	s.AddTool(mcp.NewTool(ToolSetOrganizationConnector,
		tooldesc.Describe("Create or change an organization's connector (a RemoteMCPServer) as you; its credential is a Secret named by reference.",
			"A credential field sets the Secret-backed header it names (default: the only one, else Authorization; Secret tenant-credential-<name>, key token, the whole value); other headers stay; noCredential removes them. A URL with userinfo, a fragment, a credential-like query parameter or a query that does not parse is refused. Never pass a value: a missing credential is a URL elicitation of the credential UI, link in credentials[].next. Needs agent-platform:admin."),
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

	s.AddTool(mcp.NewTool(ToolSetOrganizationEgress,
		tooldesc.Describe("Create or replace an organization's egress policy as you: origins its agents may reach, credentials by Secret name.",
			"destinations replaces the whole list. A credentialHeader makes the egress gateway add that header from the Secret (default tenant-credential-<policy>, key token). Never pass a value: a missing credential is a URL elicitation of the credential UI, link in credentials[].next. list_egress_policies lists them. Needs agent-platform:admin."),
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

	s.AddTool(mcp.NewTool(ToolReplaceOrganizationCredential,
		tooldesc.Describe("Replace an organization's credential by its Secret and key, as you, writing only the Secret.",
			"No model, connector or egress policy is written. Answers with a URL elicitation of the credential UI, link in credentials[].next; the form lists every object that uses the Secret and where each sends the value. Refused when nothing uses that key of the Secret, and when an object reads another key of it: the Secret is written whole, holding the one key. A credential whose consumer git applies is replaced with mode commit. Never pass a value. Needs agent-platform:admin."),
		orgNSProp,
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

	s.AddTool(mcp.NewTool(ToolDeleteOrganizationObject,
		tooldesc.Describe("Delete an organization's model, connector or egress policy as you.",
			"Dry run first. Its credential Secret stays. Deleting the default model leaves the organization without one until another is marked. Copies the platform curates are refused. Needs agent-platform:admin."),
		orgNSProp,
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
