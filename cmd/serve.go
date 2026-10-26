package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/giantswarm/gitops-commit/commit"
	"github.com/giantswarm/mcp-toolkit/metrics"
	"github.com/giantswarm/mcp-toolkit/tracing"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/api"
	"github.com/giantswarm/agent-manager/internal/chart"
	"github.com/giantswarm/agent-manager/internal/kagent"
	"github.com/giantswarm/agent-manager/internal/kube"
	"github.com/giantswarm/agent-manager/internal/migrate"
	"github.com/giantswarm/agent-manager/internal/muster"
	"github.com/giantswarm/agent-manager/internal/oci"
	"github.com/giantswarm/agent-manager/internal/server"
	"github.com/giantswarm/agent-manager/internal/skills"
	"github.com/giantswarm/agent-manager/internal/tenants"
	"github.com/giantswarm/agent-manager/internal/toolchainmanager"
	"github.com/giantswarm/agent-manager/internal/workspaces"
)

type serveOptions struct {
	listen string

	kubeconfig  string
	kubeContext string
	inCluster   bool

	kagentNamespace   string
	managedNamespaces string
	kagentAPIVersion  string
	harnessName       string
	harnessEgress     string
	toolchainRepo     string
	toolchainRegCfg   string
	musterURL         string
	targetMusterURL   string
	musterBindingsURL string
	musterActorBearer bool
	musterMCPURL      string
	workspacesPrefix  string
	toolchainsPrefix  string
	helmReleaseAPI    string
	ociRepositoryAPI  string

	chartOCIURL           string
	chartSemver           string
	chartSemverFilter     string
	chartRefresh          time.Duration
	helmReleaseInterval   string
	ociRepositoryInterval string
	helmReleaseSA         string

	skillsRepositories string
	skillsGitHubAPI    string
	skillsToken        string
	skillsAppID        string
	skillsAppInstall   string
	skillsAppKeyFile   string
	skillsGitAuth      string
	skillsGitAuthPrev  string
	skillsGitAuthMint  bool
	skillsCacheTTL     time.Duration

	kagentTarget string

	multiTenant                   bool
	tenantModelConfig             string
	tenantModelSecret             string
	tenantWorkerPoolReplicas      int64
	tenantFluxClusterRole         string
	tenantNetworkPolicy           string
	tenantNetworkPolicies         string
	tenantInterval                time.Duration
	tenantLeaderElectionNamespace string

	mcpEnabled bool
	mcpPath    string

	oauthEnabled                  bool
	oauthBaseURL                  string
	oauthProvider                 string
	dexIssuerURL                  string
	dexClientID                   string
	dexClientSecret               string
	dexCAFile                     string
	dexAllowPrivateIP             bool
	googleClientID                string
	googleClientSecret            string
	oauthTrustedAudiences         string
	ssoAllowPrivateIPs            bool
	allowPublicClientRegistration bool
	downstreamOAuth               bool
	githubAuthorizationServer     string
	githubAPIURL                  string
}

func newServeCmd() *cobra.Command {
	o := &serveOptions{}
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the REST + MCP server",
		Long: `Run the agent-manager server. Every flag can also be set through the
environment variable named next to it; flags win over the environment.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServe(cmd.Context(), o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.listen, "listen", envOr("AGENT_MANAGER_LISTEN", ":8080"), "Listen address (AGENT_MANAGER_LISTEN)")
	f.StringVar(&o.kubeconfig, "kubeconfig", envOr("KUBECONFIG", ""), "Kubeconfig path; empty uses the default loading rules or in-cluster auth (KUBECONFIG)")
	f.StringVar(&o.kubeContext, "kube-context", envOr("KUBE_CONTEXT", ""), "Kubeconfig context override (KUBE_CONTEXT)")
	f.BoolVar(&o.inCluster, "in-cluster", envBool("KUBERNETES_IN_CLUSTER", false), "Force in-cluster Kubernetes auth (KUBERNETES_IN_CLUSTER)")
	f.StringVar(&o.kagentNamespace, "kagent-namespace", envOr("KAGENT_NAMESPACE", "kagent"), "Default namespace agents are created in and listed from (KAGENT_NAMESPACE)")
	f.StringVar(&o.managedNamespaces, "managed-namespaces", envOr("AGENT_MANAGER_MANAGED_NAMESPACES", ""), "Comma-separated additional namespaces agents may live in; RBAC must exist there (AGENT_MANAGER_MANAGED_NAMESPACES)")
	f.StringVar(&o.kagentAPIVersion, "kagent-api-version", envOr("KAGENT_API_VERSION", "auto"), "api.kagent.dev API version for Agents, Harnesses, RemoteMCPServers and ModelConfigs; auto discovers the version serving agents, default "+agents.DefaultKagentAPIVersion+" (KAGENT_API_VERSION)")
	f.StringVar(&o.harnessName, "harness-name", envOr("AGENT_HARNESS_NAME", agents.DefaultHarnessName), "Name of the platform Harness an agent runs on unless create_agent names another: composed as the chart value agent.harness, the Agent's spec.harnessRef.name (AGENT_HARNESS_NAME)")
	f.StringVar(&o.toolchainRepo, "toolchain-repository", envOr("AGENT_MANAGER_TOOLCHAIN_REPOSITORY", ""), "The OCI repository the installation's toolchains are published under, <registry>/<path> (gsoci.azurecr.io/giantswarm/agent-toolchains): toolchain <name> is the image <registry>/<path>/<name>, read anonymously unless --toolchain-registry-config has credentials for its host, which start_session mounts into a session. A version the toolchain-manager lists (--toolchains-tools-prefix) is read from the image of the source that offers it. Empty: a start that names toolchains, or on an agent with default toolchains, is refused. Needs --kagent-target (AGENT_MANAGER_TOOLCHAIN_REPOSITORY)")
	f.StringVar(&o.toolchainRegCfg, "toolchain-registry-config", envOr("AGENT_MANAGER_TOOLCHAIN_REGISTRY_CONFIG", ""), "A Docker config file, the .dockerconfigjson of a kubernetes.io/dockerconfigjson Secret: the toolchains of a registry host it has credentials for are read with them, a private source's. Empty, or no file at the path: every registry is read anonymously. Read at start. Needs --toolchain-repository (AGENT_MANAGER_TOOLCHAIN_REGISTRY_CONFIG)")
	f.StringVar(&o.harnessEgress, "harness-egress", envOr("AGENT_MANAGER_HARNESS_EGRESS", ""), "JSON object of the origins a coding agent (on a Harness whose runtime is not kagent) reaches, by Harness name ({\"claude\": [\"https://github.com\"]}): written into the Agent's spec.egress, and the only origins such an agent may name; a coding Harness without an entry is refused, an empty list reaches none (AGENT_MANAGER_HARNESS_EGRESS)")
	f.StringVar(&o.musterURL, "muster-url", envOr("AGENT_MUSTER_URL", ""), "The platform's muster MCP URL, composed into every agent as the chart value muster.url; empty composes nothing and the chart default applies (AGENT_MUSTER_URL)")
	f.BoolVar(&o.musterActorBearer, "muster-actor-bearer", envBool("AGENT_MANAGER_MUSTER_ACTOR_BEARER", false), "muster trusts the actor JWT of a bound session: get_agent, list_agents and get_info report an agent's toolset as bound to each session at its start, which every session start_session binds carries. Needs --muster-bindings-url (AGENT_MANAGER_MUSTER_ACTOR_BEARER)")
	f.StringVar(&o.musterBindingsURL, "muster-bindings-url", envOr("AGENT_MANAGER_MUSTER_BINDINGS_URL", ""), "muster's session bindings endpoint (http(s)://host[:port]/session-bindings), which start_session binds the sandbox of every Session with providers to the caller at and delete_session unbinds it from, as the caller. Empty: start_session refuses a workspace grant. Needs --kagent-target (AGENT_MANAGER_MUSTER_BINDINGS_URL)")
	f.StringVar(&o.musterMCPURL, "muster-mcp-url", envOr("AGENT_MANAGER_MUSTER_MCP_URL", ""), "muster's MCP endpoint (http(s)://host[:port]/mcp), where start_session calls, as the caller, the workspace-manager's authorize_workspace for an agent's default workspace when the start names no workspace, and the toolchain-manager's list_toolchains. Needs --workspaces-tools-prefix or --toolchains-tools-prefix (AGENT_MANAGER_MUSTER_MCP_URL)")
	f.StringVar(&o.workspacesPrefix, "workspaces-tools-prefix", envOr("AGENT_MANAGER_WORKSPACES_TOOLS_PREFIX", ""), "The workspace-manager's tool prefix in muster (x_workspace-manager_). Empty: a start_session on an agent with a default workspace that names no workspace is refused. Needs --muster-mcp-url and --muster-bindings-url (AGENT_MANAGER_WORKSPACES_TOOLS_PREFIX)")
	f.StringVar(&o.toolchainsPrefix, "toolchains-tools-prefix", envOr("AGENT_MANAGER_TOOLCHAINS_TOOLS_PREFIX", ""), "The toolchain-manager's tool prefix in muster (x_toolchain-manager_): start_session asks its list_toolchains, as the caller, and starts a toolchain only at a version it lists, the newest listed when none is named. Empty: any version the registry carries starts. Needs --muster-mcp-url and --toolchain-repository (AGENT_MANAGER_TOOLCHAINS_TOOLS_PREFIX)")
	f.StringVar(&o.targetMusterURL, "target-muster-url", envOr("AGENT_TARGET_MUSTER_URL", ""), "The muster MCP URL composed into an agent on a workload cluster: one those clusters reach (the installation's public muster endpoint); empty refuses target clusters (AGENT_TARGET_MUSTER_URL)")
	f.StringVar(&o.helmReleaseAPI, "flux-helmrelease-api-version", envOr("FLUX_HELMRELEASE_API_VERSION", "auto"), "helm.toolkit.fluxcd.io API version composed into HelmReleases; auto discovers it (FLUX_HELMRELEASE_API_VERSION)")
	f.StringVar(&o.ociRepositoryAPI, "flux-ocirepository-api-version", envOr("FLUX_OCIREPOSITORY_API_VERSION", "auto"), "source.toolkit.fluxcd.io API version composed into OCIRepositories; auto discovers it (FLUX_OCIREPOSITORY_API_VERSION)")
	f.StringVar(&o.chartOCIURL, "agent-chart-oci-url", envOr("AGENT_CHART_OCI_URL", agents.DefaultChartOCIURL), "OCI URL of the agent chart every agent renders from (AGENT_CHART_OCI_URL)")
	f.StringVar(&o.chartSemver, "agent-chart-semver", envOr("AGENT_CHART_SEMVER", agents.DefaultChartSemver), "Semver range the OCIRepository tracks; 2.x follows every 2.x release of the Generic chart and never a pre-release (AGENT_CHART_SEMVER)")
	f.StringVar(&o.chartSemverFilter, "agent-chart-semver-filter", envOr("AGENT_CHART_SEMVER_FILTER", ""), "Regular expression the agent chart's tags must match before the range is evaluated, as the OCIRepository's ref.semverFilter; empty filters nothing (AGENT_CHART_SEMVER_FILTER)")
	f.DurationVar(&o.chartRefresh, "agent-chart-refresh", envDuration("AGENT_CHART_REFRESH", 10*time.Minute), "How often the chart registry is re-read for the latest version and its values schema (AGENT_CHART_REFRESH)")
	f.StringVar(&o.helmReleaseInterval, "helmrelease-interval", envOr("HELMRELEASE_INTERVAL", agents.DefaultHelmReleaseInterval), "HelmRelease.spec.interval of composed agents (HELMRELEASE_INTERVAL)")
	f.StringVar(&o.ociRepositoryInterval, "ocirepository-interval", envOr("OCIREPOSITORY_INTERVAL", agents.DefaultOCIRepositoryInterval), "OCIRepository.spec.interval of the shared chart source (OCIREPOSITORY_INTERVAL)")
	f.StringVar(&o.helmReleaseSA, "helmrelease-service-account", envOr("HELMRELEASE_SERVICE_ACCOUNT", ""), "HelmRelease.spec.serviceAccountName, required by a Flux multi-tenancy admission policy in tenant namespaces; empty omits it (HELMRELEASE_SERVICE_ACCOUNT)")
	f.StringVar(&o.skillsRepositories, "skills-repositories", envOr("AGENT_MANAGER_SKILLS_REPOSITORIES", ""), "Comma-separated GitHub repository URLs whose SKILL.md files are offered by list_skills (AGENT_MANAGER_SKILLS_REPOSITORIES)")
	f.StringVar(&o.skillsGitHubAPI, "skills-github-api", envOr("AGENT_MANAGER_SKILLS_GITHUB_API", "https://api.github.com"), "GitHub API base URL for skill discovery and for resolving a skill's branch or tag to its head commit (AGENT_MANAGER_SKILLS_GITHUB_API)")
	f.StringVar(&o.skillsToken, "skills-github-token", envOr("GITHUB_TOKEN", ""), "GitHub token for private skill repositories and a higher rate limit; prefer the environment (GITHUB_TOKEN)")
	f.StringVar(&o.skillsAppID, "skills-github-app-id", envOr("AGENT_MANAGER_SKILLS_GITHUB_APP_ID", ""), "ID of the read-only GitHub App skills are read with: installation tokens instead of a personal token, at the App's rate limit. Mutually exclusive with --skills-github-token (AGENT_MANAGER_SKILLS_GITHUB_APP_ID)")
	f.StringVar(&o.skillsAppInstall, "skills-github-app-installation-id", envOr("AGENT_MANAGER_SKILLS_GITHUB_APP_INSTALLATION_ID", ""), "Installation ID of the skills GitHub App (AGENT_MANAGER_SKILLS_GITHUB_APP_INSTALLATION_ID)")
	f.StringVar(&o.skillsAppKeyFile, "skills-github-app-private-key-file", envOr("AGENT_MANAGER_SKILLS_GITHUB_APP_PRIVATE_KEY_FILE", ""), "PEM private key file of the skills GitHub App (AGENT_MANAGER_SKILLS_GITHUB_APP_PRIVATE_KEY_FILE)")
	f.StringVar(&o.skillsGitAuth, "skills-git-auth-secret-name", envOr("AGENT_MANAGER_SKILLS_GIT_AUTH_SECRET_NAME", ""), "Secret (key token) in the agent's namespace every agent with a git skill fetches its skills with, unless the agent names its own gitAuthSecretName; composed as the chart value skillsGitAuthSecretRef.name. Empty: such agents fetch anonymously (AGENT_MANAGER_SKILLS_GIT_AUTH_SECRET_NAME)")
	f.StringVar(&o.skillsGitAuthPrev, "skills-git-auth-previous-secret-names", envOr("AGENT_MANAGER_SKILLS_GIT_AUTH_PREVIOUS_SECRET_NAMES", ""), "Comma-separated earlier names of the --skills-git-auth-secret-name Secret: an agent that names one is composed with the current name on its next update (agent-manager migrate moves the rest), and with --skills-git-auth-mint their Secrets are kept filled too until no agent names them (AGENT_MANAGER_SKILLS_GIT_AUTH_PREVIOUS_SECRET_NAMES)")
	f.BoolVar(&o.skillsGitAuthMint, "skills-git-auth-mint", envBool("AGENT_MANAGER_SKILLS_GIT_AUTH_MINT", false), "Keep the Secret --skills-git-auth-secret-name names filled with the skills GitHub App's installation token in every managed namespace, refreshed before it expires; the Secret must exist (the chart renders it) and the ServiceAccount may update it. Needs the skills GitHub App (AGENT_MANAGER_SKILLS_GIT_AUTH_MINT)")
	f.DurationVar(&o.skillsCacheTTL, "skills-cache-ttl", envDuration("AGENT_MANAGER_SKILLS_CACHE_TTL", 5*time.Minute), "How long a repository's discovered skills are reused (AGENT_MANAGER_SKILLS_CACHE_TTL)")
	f.StringVar(&o.kagentTarget, "kagent-target", envOr("AGENT_MANAGER_KAGENT_TARGET", ""), "The kagent controller's gRPC target through agentgateway, which list_sessions, get_session, get_session_turn, start_session and delete_session call as the caller: grpc://host:port (h2c, the in-cluster agentgateway Service) or grpcs://host[:port] (TLS). Empty: the session tools answer unsupported. Needs --enable-oauth for a caller token (AGENT_MANAGER_KAGENT_TARGET)")
	f.BoolVar(&o.multiTenant, "multi-tenant", envBool("AGENT_MANAGER_MULTI_TENANT", false), "Manage the tenant namespaces (labelled "+tenants.OrganizationLabel+") beside the configured ones, and keep in each, as the ServiceAccount, what an agent needs there: the agent chart's OCIRepository, the --helmrelease-service-account ServiceAccount bound to --tenant-flux-cluster-role, the --tenant-network-policies, and while it holds an agent copies of the platform Harness, its WorkerPool and the default ModelConfig with its key Secret, all copied from --kagent-namespace (AGENT_MANAGER_MULTI_TENANT)")
	f.StringVar(&o.tenantModelConfig, "tenant-model-config", envOr("AGENT_MANAGER_TENANT_MODEL_CONFIG", "default-model-config"), "The platform's default ModelConfig a tenant receives with its first agent (AGENT_MANAGER_TENANT_MODEL_CONFIG)")
	f.StringVar(&o.tenantModelSecret, "tenant-model-secret", envOr("AGENT_MANAGER_TENANT_MODEL_SECRET", "kagent-anthropic"), "The Secret that holds that ModelConfig's API key: the only Secret the reconciler copies (AGENT_MANAGER_TENANT_MODEL_SECRET)")
	f.Int64Var(&o.tenantWorkerPoolReplicas, "tenant-worker-pool-replicas", envInt64("AGENT_MANAGER_TENANT_WORKER_POOL_REPLICAS", 1), "Warm workers of a tenant's WorkerPool while it holds an agent (AGENT_MANAGER_TENANT_WORKER_POOL_REPLICAS)")
	f.StringVar(&o.tenantFluxClusterRole, "tenant-flux-cluster-role", envOr("AGENT_MANAGER_TENANT_FLUX_CLUSTER_ROLE", "agent-platform-tenant-flux"), "ClusterRole bound to the agents' Flux ServiceAccount in a tenant (AGENT_MANAGER_TENANT_FLUX_CLUSTER_ROLE)")
	f.StringVar(&o.tenantNetworkPolicy, "tenant-network-policy-flavor", envOr("AGENT_MANAGER_TENANT_NETWORK_POLICY_FLAVOR", ""), "Kind of the network policies copied into a tenant: kubernetes (NetworkPolicy) or cilium (CiliumNetworkPolicy); empty copies none (AGENT_MANAGER_TENANT_NETWORK_POLICY_FLAVOR)")
	f.StringVar(&o.tenantNetworkPolicies, "tenant-network-policies", envOr("AGENT_MANAGER_TENANT_NETWORK_POLICIES", ""), "Comma-separated names of the network policies copied from --kagent-namespace into every tenant (AGENT_MANAGER_TENANT_NETWORK_POLICIES)")
	f.DurationVar(&o.tenantInterval, "tenant-interval", envDuration("AGENT_MANAGER_TENANT_INTERVAL", 10*time.Minute), "How often every tenant is reconciled without a change; a change to a tenant, its agents, its copies or their platform sources reconciles it at once (AGENT_MANAGER_TENANT_INTERVAL)")
	f.StringVar(&o.tenantLeaderElectionNamespace, "tenant-leader-election-namespace", envOr("AGENT_MANAGER_TENANT_LEADER_ELECTION_NAMESPACE", ""), "Namespace of the Lease "+tenants.LeaderElectionID+" the replicas elect the tenant reconciler's leader with; empty is the pod's namespace, so a run outside a cluster with --multi-tenant must set it (AGENT_MANAGER_TENANT_LEADER_ELECTION_NAMESPACE)")
	f.BoolVar(&o.mcpEnabled, "mcp-enabled", envBool("AGENT_MANAGER_MCP_ENABLED", true), "Serve the MCP streamable-HTTP endpoint (AGENT_MANAGER_MCP_ENABLED)")
	f.StringVar(&o.mcpPath, "mcp-path", envOr("AGENT_MANAGER_MCP_PATH", "/mcp"), "MCP endpoint path (AGENT_MANAGER_MCP_PATH)")
	f.BoolVar(&o.oauthEnabled, "enable-oauth", envBool("AGENT_MANAGER_OAUTH_ENABLED", false), "Require an OAuth 2.1 bearer token on the MCP endpoint and the REST API, validated against the platform IdP (mcp-oauth); the caller's identity travels with every request (AGENT_MANAGER_OAUTH_ENABLED)")
	f.StringVar(&o.oauthBaseURL, "oauth-base-url", envOr("AGENT_MANAGER_OAUTH_BASE_URL", ""), "Public base URL of this server: the issuer of its OAuth metadata, https or loopback http (AGENT_MANAGER_OAUTH_BASE_URL)")
	f.StringVar(&o.oauthProvider, "oauth-provider", envOr("AGENT_MANAGER_OAUTH_PROVIDER", server.ProviderDex), "Identity provider: dex or google (AGENT_MANAGER_OAUTH_PROVIDER)")
	f.StringVar(&o.dexIssuerURL, "dex-issuer-url", envOr("DEX_ISSUER_URL", ""), "Dex issuer URL (DEX_ISSUER_URL)")
	f.StringVar(&o.dexClientID, "dex-client-id", envOr("DEX_CLIENT_ID", ""), "Dex client ID (DEX_CLIENT_ID)")
	f.StringVar(&o.dexClientSecret, "dex-client-secret", envOr("DEX_CLIENT_SECRET", ""), "Dex client secret; prefer the environment (DEX_CLIENT_SECRET)")
	f.StringVar(&o.dexCAFile, "dex-ca-file", envOr("DEX_CA_FILE", ""), "PEM CA bundle of a Dex with a private certificate; verifies discovery, token and JWKS calls (DEX_CA_FILE)")
	f.BoolVar(&o.dexAllowPrivateIP, "allow-private-oauth-urls", envBool("AGENT_MANAGER_OAUTH_ALLOW_PRIVATE_URLS", false), "Let the Dex issuer resolve to a private or loopback address, an in-cluster Dex (AGENT_MANAGER_OAUTH_ALLOW_PRIVATE_URLS)")
	f.StringVar(&o.googleClientID, "google-client-id", envOr("GOOGLE_CLIENT_ID", ""), "Google OAuth client ID (GOOGLE_CLIENT_ID)")
	f.StringVar(&o.googleClientSecret, "google-client-secret", envOr("GOOGLE_CLIENT_SECRET", ""), "Google OAuth client secret; prefer the environment (GOOGLE_CLIENT_SECRET)")
	f.StringVar(&o.oauthTrustedAudiences, "oauth-trusted-audiences", envOr("OAUTH_TRUSTED_AUDIENCES", ""), "Comma-separated OAuth client IDs whose IdP id_tokens are accepted as bearer tokens — the platform client and the audiences the MCPServer requires, which every forwarded token carries (OAUTH_TRUSTED_AUDIENCES)")
	f.BoolVar(&o.ssoAllowPrivateIPs, "sso-allow-private-ips", envBool("SSO_ALLOW_PRIVATE_IPS", false), "Let the IdP's JWKS endpoint resolve to a private address when validating forwarded tokens (SSO_ALLOW_PRIVATE_IPS)")
	f.BoolVar(&o.allowPublicClientRegistration, "allow-public-client-registration", envBool("AGENT_MANAGER_OAUTH_ALLOW_PUBLIC_REGISTRATION", false), "Accept unauthenticated dynamic client registration; labs only (AGENT_MANAGER_OAUTH_ALLOW_PUBLIC_REGISTRATION)")
	f.BoolVar(&o.downstreamOAuth, "downstream-oauth", envBool("AGENT_MANAGER_DOWNSTREAM_OAUTH", false), "Call the Kubernetes API as the caller, with the caller's IdP token, for everything a request does — the ServiceAccount holds no permissions (the chart renders none) and nothing runs without a caller. Needs --enable-oauth and an apiserver that trusts the IdP (AGENT_MANAGER_DOWNSTREAM_OAUTH)")
	f.StringVar(&o.githubAuthorizationServer, "github-authorization-server", envOr("AGENT_MANAGER_GITHUB_AUTHORIZATION_SERVER", ""), "Issuer identity of the GitHub App muster pins this server's MCP registration to (https://github.com/apps/giantswarm-agent-manager): the bearer of every MCP call is then the person's App user token, verified with GET /user and used for commit mode's pull request, and the person's IdP ID token arrives in X-Muster-Id-Token (MCPServer auth.forwardIdentity). Empty: commit mode is not offered. Needs --enable-oauth (AGENT_MANAGER_GITHUB_AUTHORIZATION_SERVER)")
	f.StringVar(&o.githubAPIURL, "github-api-url", envOr("AGENT_MANAGER_GITHUB_API_URL", server.DefaultGitHubAPIURL), "GitHub REST API base URL for GET /user and commit mode (AGENT_MANAGER_GITHUB_API_URL)")
	return cmd
}

func runServe(ctx context.Context, o *serveOptions) error {
	log := slog.Default()
	skillsTokens, err := skillsTokenSource(o)
	if err != nil {
		return err
	}
	app, _ := skillsTokens.(*skills.App)
	if o.skillsGitAuthMint && (app == nil || o.skillsGitAuth == "") {
		return fmt.Errorf("--skills-git-auth-mint needs the skills GitHub App (--skills-github-app-id) and --skills-git-auth-secret-name: the App's installation token is written into that Secret")
	}
	if strings.HasPrefix(o.skillsGitAuth, agents.TenantCredentialPrefix) {
		log.Warn("the installation's skills credential is named "+agents.TenantCredentialPrefix+"*, the prefix of a tenant's own credentials: where it is present in an Organization's namespace, admission lets a tenant name it; rename the Secret and list the old name in --skills-git-auth-previous-secret-names", "secret", o.skillsGitAuth)
	}
	if o.githubAuthorizationServer != "" && !o.oauthEnabled {
		return fmt.Errorf("--github-authorization-server needs --enable-oauth: the forwarded IdP ID token is validated by the OAuth resource server")
	}
	if o.kagentTarget != "" && !o.oauthEnabled {
		return fmt.Errorf("--kagent-target needs --enable-oauth: the session tools call kagent as the caller, with the caller's IdP token, and never as the ServiceAccount")
	}
	if o.musterActorBearer && o.musterBindingsURL == "" {
		return fmt.Errorf("--muster-actor-bearer needs --muster-bindings-url: a session's toolset is bound with its muster binding")
	}
	if o.musterBindingsURL != "" && o.kagentTarget == "" {
		return fmt.Errorf("--muster-bindings-url needs --kagent-target: only a session started through kagent is bound in muster")
	}
	if (o.workspacesPrefix != "" || o.toolchainsPrefix != "") && o.musterMCPURL == "" {
		return fmt.Errorf("--workspaces-tools-prefix and --toolchains-tools-prefix need --muster-mcp-url: the workspace-manager and the toolchain-manager are called through muster under their prefixes")
	}
	if o.musterMCPURL != "" && o.workspacesPrefix == "" && o.toolchainsPrefix == "" {
		return fmt.Errorf("--muster-mcp-url needs --workspaces-tools-prefix or --toolchains-tools-prefix: it names no server to call")
	}
	if o.workspacesPrefix != "" && o.musterBindingsURL == "" {
		return fmt.Errorf("--workspaces-tools-prefix needs --muster-bindings-url: a Session in a default workspace is bound in muster as any workspace Session")
	}
	if o.toolchainsPrefix != "" && o.toolchainRepo == "" {
		return fmt.Errorf("--toolchains-tools-prefix needs --toolchain-repository: it holds the toolchains a Session mounts to the toolchain-manager's list")
	}
	if o.downstreamOAuth && !o.oauthEnabled {
		return fmt.Errorf("--downstream-oauth needs --enable-oauth: without OAuth there is no caller token to present to the Kubernetes API")
	}

	// Exports only when OTEL_EXPORTER_OTLP_ENDPOINT (or OTEL_TRACES_EXPORTER)
	// is set; the W3C propagator is installed either way.
	shutdownTracing, err := tracing.Init(ctx, tracing.WithServiceName("agent-manager"), tracing.WithServiceVersion(build.Version))
	if err != nil {
		return fmt.Errorf("tracing: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := shutdownTracing(shutdownCtx); err != nil {
			log.Warn("flushing traces failed", "error", err)
		}
	}()
	shutdownMetrics, err := metrics.Init(ctx, metrics.WithServiceName("agent-manager"), metrics.WithServiceVersion(build.Version))
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := shutdownMetrics(shutdownCtx); err != nil {
			log.Warn("flushing metrics failed", "error", err)
		}
	}()

	// The pod's own client: the in-cluster address and CA, and API discovery
	// at startup (what every authenticated principal may read). With
	// downstream OAuth that is all the ServiceAccount ever does — every
	// request runs with the caller's token through the caller provider.
	clients, err := kube.New(kube.Config{Kubeconfig: o.kubeconfig, Context: o.kubeContext, InCluster: o.inCluster})
	if err != nil {
		return fmt.Errorf("agent-manager needs Kubernetes access: %w", err)
	}
	var provider kube.Provider = kube.NewServiceAccountProvider(clients)
	if o.downstreamOAuth {
		provider = kube.NewCallerProvider(clients, log)
	}

	kagentVersion := strings.TrimPrefix(o.kagentAPIVersion, agents.KagentAPIGroup+"/")
	if kagentVersion == "" || kagentVersion == "auto" {
		kagentVersion, err = kube.DiscoverVersion(clients.Discovery(), agents.KagentAPIGroup, "agents")
		if err != nil {
			log.Warn("kagent API discovery failed, using default", "default", agents.DefaultKagentAPIVersion, "error", err)
			kagentVersion = agents.DefaultKagentAPIVersion
		}
	}
	helmReleaseAPI := discoverGroupVersion(o.helmReleaseAPI, clients, "helm.toolkit.fluxcd.io", "helmreleases", agents.DefaultHelmReleaseAPIVersion, log)
	ociRepositoryAPI := discoverGroupVersion(o.ociRepositoryAPI, clients, "source.toolkit.fluxcd.io", "ocirepositories", agents.DefaultOCIRepositoryAPIVersion, log)

	resolver, err := chart.NewResolver(o.chartOCIURL, o.chartSemver, o.chartRefresh, nil, log, chart.WithSemverFilter(o.chartSemverFilter))
	if err != nil {
		return err
	}

	var discoverer *skills.Discoverer
	if repos := splitList(o.skillsRepositories); len(repos) > 0 {
		discoverer = skills.New(skills.Config{Repositories: repos, APIURL: o.skillsGitHubAPI, Tokens: skillsTokens, CacheTTL: o.skillsCacheTTL}, log)
	}
	// Skills are pinned before they are written: a branch or tag to its head
	// commit through the GitHub API, an image tag to its digest.
	pinner := skills.NewResolver(o.skillsGitHubAPI, skillsTokens, nil, nil)

	// The one thing agent-manager does without a caller: the boot Secret of
	// private git skills, written with the ServiceAccount's permission to
	// update that one Secret.
	var bootSecret *skills.BootSecret
	if o.skillsGitAuthMint {
		bootSecret = skills.NewBootSecret(app, clients.Typed(), o.skillsGitAuth, splitList(o.skillsGitAuthPrev), agents.ManagedNamespaces(o.kagentNamespace, splitList(o.managedNamespaces)), log)
	}

	harnessEgress, err := parseHarnessEgress(o.harnessEgress)
	if err != nil {
		return err
	}
	toolchains, err := toolchainSource(o.toolchainRepo, o.toolchainRegCfg)
	if err != nil {
		return err
	}
	if toolchains != nil && o.kagentTarget == "" {
		return fmt.Errorf("--toolchain-repository needs --kagent-target: toolchains are mounted into sessions started through kagent")
	}

	// The agent chart's source as agent-manager creates it; an existing one
	// follows a changed range on start.
	sourceCompose := agents.ComposeConfig{ChartOCIURL: o.chartOCIURL, ChartName: resolver.Name(), ChartSemver: o.chartSemver, ChartSemverFilter: o.chartSemverFilter,
		OCIRepositoryInterval: o.ociRepositoryInterval, OCIRepositoryAPIVersion: ociRepositoryAPI}
	migrate.ConvergeSources(ctx, clients.Dynamic(), agents.ManagedNamespaces(o.kagentNamespace, splitList(o.managedNamespaces)), sourceCompose, log)

	var tenantSet *tenants.Set
	if o.multiTenant {
		policyGVK, err := networkPolicyGVK(o.tenantNetworkPolicy)
		if err != nil {
			return err
		}
		poolVersion, err := kube.DiscoverVersion(clients.Discovery(), "ate.dev", "workerpools")
		if err != nil {
			log.Warn("WorkerPool API discovery failed, using default", "default", "v1alpha1", "error", err)
			poolVersion = "v1alpha1"
		}
		kagentGVK := func(kind string) schema.GroupVersionKind {
			return schema.GroupVersionKind{Group: agents.KagentAPIGroup, Version: kagentVersion, Kind: kind}
		}
		tenantSet, err = tenants.NewSet(clients.RESTConfig(), tenants.Config{
			PlatformNamespace:  o.kagentNamespace,
			HarnessName:        o.harnessName,
			ModelConfigName:    o.tenantModelConfig,
			ModelSecretName:    o.tenantModelSecret,
			WorkerPoolReplicas: o.tenantWorkerPoolReplicas,
			FluxServiceAccount: o.helmReleaseSA,
			FluxClusterRole:    o.tenantFluxClusterRole,
			NetworkPolicies:    splitList(o.tenantNetworkPolicies),
			NetworkPolicyGVK:   policyGVK,
			OCIRepository:      func(ns string) *unstructured.Unstructured { return agents.BuildOCIRepository(ns, sourceCompose) },
			ChartName:          resolver.Name(),
			HelmReleaseGVK:     schema.FromAPIVersionAndKind(helmReleaseAPI, "HelmRelease"),
			OCIRepositoryGVK:   schema.FromAPIVersionAndKind(ociRepositoryAPI, "OCIRepository"),
			AgentGVK:           kagentGVK("Agent"),
			HarnessGVK:         kagentGVK("Harness"),
			ModelConfigGVK:     kagentGVK("ModelConfig"),
			WorkerPoolGVK:      schema.GroupVersionKind{Group: "ate.dev", Version: poolVersion, Kind: "WorkerPool"},
			SyncPeriod:         o.tenantInterval,
			// Empty: the pod's namespace.
			LeaderElectionNamespace: o.tenantLeaderElectionNamespace,
		}, log)
		if err != nil {
			return fmt.Errorf("--multi-tenant: %w", err)
		}
	}

	var sessions agents.SessionClient
	if o.kagentTarget != "" {
		client, err := kagent.Dial(o.kagentTarget)
		if err != nil {
			return fmt.Errorf("--kagent-target: %w", err)
		}
		defer func() { _ = client.Close() }()
		sessions = client
	}
	var bindings agents.SessionBinder
	if o.musterBindingsURL != "" {
		client, err := muster.New(o.musterBindingsURL, nil)
		if err != nil {
			return fmt.Errorf("--muster-bindings-url: %w", err)
		}
		bindings = client
	}
	var workspaceManager agents.WorkspaceAuthorizer
	if o.workspacesPrefix != "" {
		client, err := workspaces.New(o.musterMCPURL, o.workspacesPrefix, build.Version)
		if err != nil {
			return fmt.Errorf("--muster-mcp-url: %w", err)
		}
		workspaceManager = client
	}
	var toolchainCatalog agents.ToolchainCatalog
	if o.toolchainsPrefix != "" {
		client, err := toolchainmanager.New(o.musterMCPURL, o.toolchainsPrefix, build.Version)
		if err != nil {
			return fmt.Errorf("--toolchains-tools-prefix: %w", err)
		}
		toolchainCatalog = client
	}

	svc := agents.New(provider, resolver, discoverer, pinner, agents.Config{
		DefaultNamespace:  o.kagentNamespace,
		ManagedNamespaces: splitList(o.managedNamespaces),
		Compose: agents.ComposeConfig{
			ChartOCIURL:                      o.chartOCIURL,
			ChartName:                        resolver.Name(),
			ChartSemver:                      o.chartSemver,
			ChartSemverFilter:                o.chartSemverFilter,
			HelmReleaseInterval:              o.helmReleaseInterval,
			OCIRepositoryInterval:            o.ociRepositoryInterval,
			ServiceAccountName:               o.helmReleaseSA,
			HelmReleaseAPIVersion:            helmReleaseAPI,
			OCIRepositoryAPIVersion:          ociRepositoryAPI,
			MusterURL:                        o.musterURL,
			TargetMusterURL:                  o.targetMusterURL,
			HarnessName:                      o.harnessName,
			SkillsGitAuthSecretName:          o.skillsGitAuth,
			SkillsGitAuthPreviousSecretNames: splitList(o.skillsGitAuthPrev),
		},
		SkillsBootSecret: bootSecret,
		KagentAPIVersion: kagentVersion,
		Version:          build.Version,
		GitHub:           gitHubRemote(o.githubAuthorizationServer, o.githubAPIURL),
		Sessions:         sessions,
		Bindings:         bindings,
		Workspaces:       workspaceManager,
		ToolchainCatalog: toolchainCatalog,
		ToolsetBound:     o.musterActorBearer,
		HarnessEgress:    harnessEgress,
		Toolchains:       toolchains,
		Tenants:          tenantSetOrNil(tenantSet),
	}, log)

	srvCfg := server.Config{Addr: o.listen, MCPEnabled: o.mcpEnabled, MCPPath: o.mcpPath}
	if o.oauthEnabled {
		srvCfg.OAuth = &server.OAuthConfig{
			BaseURL:                       o.oauthBaseURL,
			Provider:                      o.oauthProvider,
			DexIssuerURL:                  o.dexIssuerURL,
			DexClientID:                   o.dexClientID,
			DexClientSecret:               o.dexClientSecret,
			DexCAFile:                     o.dexCAFile,
			DexAllowPrivateIP:             o.dexAllowPrivateIP,
			GoogleClientID:                o.googleClientID,
			GoogleClientSecret:            o.googleClientSecret,
			TrustedAudiences:              splitList(o.oauthTrustedAudiences),
			SSOAllowPrivateIPs:            o.ssoAllowPrivateIPs,
			AllowPublicClientRegistration: o.allowPublicClientRegistration,
			DownstreamOAuth:               o.downstreamOAuth,
		}
		if o.githubAuthorizationServer != "" {
			srvCfg.OAuth.GitHub = &server.GitHubPin{AuthorizationServer: o.githubAuthorizationServer, APIURL: o.githubAPIURL}
		}
	}
	srv, err := server.New(srvCfg, svc, api.NewMCPServer(svc, build.Version), log)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The tenant reconciler runs beside the server and stops with it. A
	// failure at run time (a lost leader Lease, say) does not stop the
	// server: the reconciler is replaced and get_info reports the failure.
	closed := make(chan struct{})
	close(closed)
	var tenantsDone <-chan struct{} = closed
	stopTenants := func() {}
	if tenantSet != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		stopTenants = cancel
		// The tenants are managed namespaces from the first request on.
		if tenantsDone, err = tenantSet.Run(ctx); err != nil {
			return fmt.Errorf("tenant manager: %w", err)
		}
	}
	info := svc.Info(ctx)
	log.Info("agent-manager starting", "version", build.Version, "commit", build.Commit, "listen", o.listen, "rest", api.Prefix, "mcp", o.mcpPath, "mcpEnabled", o.mcpEnabled,
		"oauth", o.oauthEnabled, "downstreamOAuth", o.downstreamOAuth, "commit", info.Capabilities["commit"], "identity", info.Identity,
		"namespaces", svc.Managed(), "multiTenant", o.multiTenant, "chart", o.chartOCIURL, "chartSemver", o.chartSemver, "chartVersion", info.Chart.LatestVersion, "schemaSource", info.Chart.SchemaSource,
		"kagentAPI", kagentVersion, "harness", info.Harness.Name, "musterURL", info.Muster.URL, "helmReleaseAPI", helmReleaseAPI, "ociRepositoryAPI", ociRepositoryAPI, "skillsRepositories", info.SkillsRepositories, "skillsGitHubApp", o.skillsAppID, "skillsGitAuthSecretName", o.skillsGitAuth, "kagentTarget", o.kagentTarget)

	if bootSecret != nil {
		go bootSecret.Run(ctx)
	}
	err = srv.Run(ctx)
	// The Lease is released before the process ends.
	stopTenants()
	<-tenantsDone
	return err
}

// discoverGroupVersion resolves "auto" against the API server, else validates
// the given group/version string.
func discoverGroupVersion(flag string, clients kube.Client, group, resource, def string, log *slog.Logger) string {
	if flag != "" && flag != "auto" {
		if !strings.HasPrefix(flag, group+"/") {
			return group + "/" + flag
		}
		return flag
	}
	v, err := kube.DiscoverVersion(clients.Discovery(), group, resource)
	if err != nil {
		log.Warn("Flux API discovery failed, using default", "group", group, "default", def, "error", err)
		return def
	}
	return group + "/" + v
}

// parseHarnessEgress reads --harness-egress: origins by Harness name. The
// lists reach the agents on those Harnesses, whose dry-run judges them.
func parseHarnessEgress(raw string) (map[string][]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var out map[string][]string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("--harness-egress: a JSON object of origin lists by Harness name: %w", err)
	}
	return out, nil
}

// toolchainSource reads --toolchain-repository, <registry>/<path>, and the
// registry credentials of --toolchain-registry-config; nil for none.
func toolchainSource(raw, registryConfig string) (*agents.ToolchainSource, error) {
	if strings.TrimSpace(raw) == "" {
		if registryConfig != "" {
			return nil, fmt.Errorf("--toolchain-registry-config needs --toolchain-repository: it holds the credentials toolchains are read with")
		}
		return nil, nil
	}
	ref, err := oci.ParseImageReference(raw)
	if err != nil {
		return nil, fmt.Errorf("--toolchain-repository: %w", err)
	}
	if ref.Tag != "" || ref.Digest != "" {
		return nil, fmt.Errorf("--toolchain-repository %q: a repository, without a tag or digest: the version is each session's", raw)
	}
	creds, err := toolchainRegistryCredentials(registryConfig)
	if err != nil {
		return nil, err
	}
	return &agents.ToolchainSource{Repository: ref.Reference, Registry: oci.NewRegistry(nil).WithCredentials(creds)}, nil
}

// toolchainRegistryCredentials reads --toolchain-registry-config; none when
// it is empty or names no file, an optional Secret not created.
func toolchainRegistryCredentials(path string) (map[string]oci.Basic, error) {
	if path == "" {
		return nil, nil
	}
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	creds, err := oci.LoadDockerConfig(path)
	if err != nil {
		return nil, fmt.Errorf("--toolchain-registry-config: %w", err)
	}
	return creds, nil
}

// tenantSetOrNil keeps a nil reconciler a nil interface.
func tenantSetOrNil(r *tenants.Set) agents.TenantSet {
	if r == nil {
		return nil
	}
	return r
}

// networkPolicyGVK is the kind of network policy a flavor copies.
func networkPolicyGVK(flavor string) (schema.GroupVersionKind, error) {
	switch flavor {
	case "":
		return schema.GroupVersionKind{}, nil
	case "kubernetes":
		return schema.GroupVersionKind{Group: "networking.k8s.io", Version: "v1", Kind: "NetworkPolicy"}, nil
	case "cilium":
		return schema.GroupVersionKind{Group: "cilium.io", Version: "v2", Kind: "CiliumNetworkPolicy"}, nil
	}
	return schema.GroupVersionKind{}, fmt.Errorf("--tenant-network-policy-flavor %q: kubernetes, cilium or empty", flavor)
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envInt64(key string, def int64) int64 {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}

func envDuration(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

// gitHubRemote offers commit mode on an App-pinned registration: the pull
// request is opened as the person, with the App user token muster puts on the
// call. Nil without the pin: commit mode is unsupported.
func gitHubRemote(authorizationServer, apiURL string) agents.RemoteFor {
	if authorizationServer == "" {
		return nil
	}
	return func(token string) (agents.GitHubRemote, error) {
		if apiURL == server.DefaultGitHubAPIURL {
			return commit.NewGitHub(token)
		}
		return commit.NewGitHub(token, commit.WithBaseURL(apiURL))
	}
}

// skillsTokenSource is what skills are read with: the skills GitHub App's
// installation tokens, a static token, or nothing (anonymous).
func skillsTokenSource(o *serveOptions) (skills.TokenSource, error) {
	if o.skillsAppID == "" && o.skillsAppInstall == "" && o.skillsAppKeyFile == "" {
		return skills.StaticToken(o.skillsToken), nil
	}
	if o.skillsToken != "" {
		return nil, fmt.Errorf("--skills-github-app-id and --skills-github-token (GITHUB_TOKEN) are mutually exclusive: skills are read with one credential")
	}
	key, err := os.ReadFile(o.skillsAppKeyFile)
	if err != nil {
		return nil, fmt.Errorf("--skills-github-app-private-key-file: %w", err)
	}
	return skills.NewApp(o.skillsGitHubAPI, o.skillsAppID, o.skillsAppInstall, key, nil)
}
