package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/api"
	"github.com/giantswarm/agent-manager/internal/kube"
	"github.com/giantswarm/agent-manager/internal/server"
	"github.com/giantswarm/agent-manager/internal/skills"
)

type serveOptions struct {
	listen string

	kubeconfig  string
	kubeContext string
	inCluster   bool

	kagentNamespace      string
	managedNamespaces    string
	kagentAPIVersion     string
	kagentMusterServer   string
	kagentDefaultHarness string

	skillsRepositories string
	skillsGitHubAPI    string
	skillsToken        string
	skillsCacheTTL     time.Duration

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
}

// defaultKagentAPIVersion is composed when discovery cannot answer.
const defaultKagentAPIVersion = "v1alpha3"

// retiredFlags are the flags of the Flux/agent-chart composition agent-manager
// replaced with kagent main's AgentTemplate. A chart release that still passes
// them must keep starting the binary, so they parse and do nothing (a
// deprecation notice is printed once per flag).
var retiredFlags = []string{
	"flux-helmrelease-api-version", "flux-ocirepository-api-version",
	"agent-chart-oci-url", "agent-chart-semver", "agent-chart-refresh",
	"helmrelease-interval", "ocirepository-interval", "helmrelease-service-account",
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
	f.StringVar(&o.managedNamespaces, "managed-namespaces", envOr("AGENT_MANAGER_MANAGED_NAMESPACES", ""), "Comma-separated additional namespaces agents may live in; RBAC and the platform muster RemoteMCPServer must exist there (AGENT_MANAGER_MANAGED_NAMESPACES)")
	f.StringVar(&o.kagentAPIVersion, "kagent-api-version", envOr("KAGENT_API_VERSION", "auto"), "kagent.dev API version for AgentTemplates, RemoteMCPServers, Harnesses and ModelConfigs; auto discovers the version serving agenttemplates, default "+defaultKagentAPIVersion+" (KAGENT_API_VERSION)")
	f.StringVar(&o.kagentMusterServer, "kagent-muster-server", envOr("KAGENT_MUSTER_SERVER", agents.DefaultMusterServer), "Name of the platform's muster RemoteMCPServer in every managed namespace: the server every agent binds and every toolset carrier is copied from (KAGENT_MUSTER_SERVER)")
	f.StringVar(&o.kagentDefaultHarness, "kagent-default-harness", envOr("KAGENT_DEFAULT_HARNESS", agents.DefaultHarness), "Harness an agent runs on when the request names none; the template is labelled kagent.dev/harness=<name> (KAGENT_DEFAULT_HARNESS)")
	f.StringVar(&o.skillsRepositories, "skills-repositories", envOr("AGENT_MANAGER_SKILLS_REPOSITORIES", ""), "Comma-separated GitHub repository URLs whose SKILL.md files are offered by list_skills (AGENT_MANAGER_SKILLS_REPOSITORIES)")
	f.StringVar(&o.skillsGitHubAPI, "skills-github-api", envOr("AGENT_MANAGER_SKILLS_GITHUB_API", "https://api.github.com"), "GitHub API base URL for skill discovery (AGENT_MANAGER_SKILLS_GITHUB_API)")
	f.StringVar(&o.skillsToken, "skills-github-token", envOr("GITHUB_TOKEN", ""), "GitHub token for private skill repositories and a higher rate limit; prefer the environment (GITHUB_TOKEN)")
	f.DurationVar(&o.skillsCacheTTL, "skills-cache-ttl", envDuration("AGENT_MANAGER_SKILLS_CACHE_TTL", 5*time.Minute), "How long a repository's discovered skills are reused (AGENT_MANAGER_SKILLS_CACHE_TTL)")
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
	registerRetiredFlags(f)
	return cmd
}

// registerRetiredFlags accepts and ignores the flags of the retired
// Flux/agent-chart composition.
func registerRetiredFlags(f *pflag.FlagSet) {
	for _, name := range retiredFlags {
		f.String(name, "", "")
		_ = f.MarkDeprecated(name, "the Flux/agent-chart composition is gone: agents are kagent.dev/v1alpha3 AgentTemplates; the flag is ignored")
	}
}

func runServe(ctx context.Context, o *serveOptions) error {
	log := slog.Default()
	if o.downstreamOAuth && !o.oauthEnabled {
		return fmt.Errorf("--downstream-oauth needs --enable-oauth: without OAuth there is no caller token to present to the Kubernetes API")
	}

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

	kagentVersion := o.kagentAPIVersion
	if kagentVersion == "" || kagentVersion == "auto" {
		kagentVersion, err = kube.DiscoverVersion(clients.Discovery(), "kagent.dev", "agenttemplates")
		if err != nil {
			log.Warn("kagent API discovery failed, using default", "default", defaultKagentAPIVersion, "error", err)
			kagentVersion = defaultKagentAPIVersion
		}
	}
	kagentVersion = strings.TrimPrefix(kagentVersion, "kagent.dev/")

	var discoverer *skills.Discoverer
	if repos := splitList(o.skillsRepositories); len(repos) > 0 {
		discoverer = skills.New(skills.Config{Repositories: repos, APIURL: o.skillsGitHubAPI, Token: o.skillsToken, CacheTTL: o.skillsCacheTTL}, log)
	}

	svc := agents.New(provider, discoverer, agents.Config{
		DefaultNamespace:  o.kagentNamespace,
		ManagedNamespaces: splitList(o.managedNamespaces),
		Compose: agents.ComposeConfig{
			APIVersion:     "kagent.dev/" + kagentVersion,
			MusterServer:   o.kagentMusterServer,
			DefaultHarness: o.kagentDefaultHarness,
		},
		Version: version,
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
	}
	srv, err := server.New(srvCfg, svc, api.NewMCPServer(svc, version), log)
	if err != nil {
		return err
	}
	info := svc.Info(ctx)
	log.Info("agent-manager starting", "version", version, "listen", o.listen, "rest", api.Prefix, "mcp", o.mcpPath, "mcpEnabled", o.mcpEnabled,
		"oauth", o.oauthEnabled, "downstreamOAuth", o.downstreamOAuth, "identity", info.Identity,
		"namespaces", info.Namespaces.Managed, "kagentAPI", info.APIVersions.AgentTemplate, "musterServer", info.Kagent.MusterServer,
		"defaultHarness", info.Kagent.DefaultHarness, "skillsRepositories", info.SkillsRepositories)

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return srv.Run(ctx)
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
