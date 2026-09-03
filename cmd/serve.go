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

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/api"
	"github.com/giantswarm/agent-manager/internal/chart"
	"github.com/giantswarm/agent-manager/internal/kube"
	"github.com/giantswarm/agent-manager/internal/server"
	"github.com/giantswarm/agent-manager/internal/skills"
)

type serveOptions struct {
	listen string

	kubeconfig  string
	kubeContext string
	inCluster   bool

	kagentNamespace   string
	managedNamespaces string
	kagentAPIVersion  string
	helmReleaseAPI    string
	ociRepositoryAPI  string

	chartOCIURL           string
	chartSemver           string
	chartRefresh          time.Duration
	helmReleaseInterval   string
	ociRepositoryInterval string
	helmReleaseSA         string

	skillsRepositories string
	skillsGitHubAPI    string
	skillsToken        string
	skillsCacheTTL     time.Duration

	mcpEnabled bool
	mcpPath    string
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
	f.StringVar(&o.kagentAPIVersion, "kagent-api-version", envOr("KAGENT_API_VERSION", "auto"), "kagent.dev API version for Agents and ModelConfigs; auto discovers the server's preferred version (KAGENT_API_VERSION)")
	f.StringVar(&o.helmReleaseAPI, "flux-helmrelease-api-version", envOr("FLUX_HELMRELEASE_API_VERSION", "auto"), "helm.toolkit.fluxcd.io API version composed into HelmReleases; auto discovers it (FLUX_HELMRELEASE_API_VERSION)")
	f.StringVar(&o.ociRepositoryAPI, "flux-ocirepository-api-version", envOr("FLUX_OCIREPOSITORY_API_VERSION", "auto"), "source.toolkit.fluxcd.io API version composed into OCIRepositories; auto discovers it (FLUX_OCIREPOSITORY_API_VERSION)")
	f.StringVar(&o.chartOCIURL, "agent-chart-oci-url", envOr("AGENT_CHART_OCI_URL", agents.DefaultChartOCIURL), "OCI URL of the agent chart every agent renders from (AGENT_CHART_OCI_URL)")
	f.StringVar(&o.chartSemver, "agent-chart-semver", envOr("AGENT_CHART_SEMVER", agents.DefaultChartSemver), "Semver range the OCIRepository tracks; x.x.x follows every release (AGENT_CHART_SEMVER)")
	f.DurationVar(&o.chartRefresh, "agent-chart-refresh", envDuration("AGENT_CHART_REFRESH", 10*time.Minute), "How often the chart registry is re-read for the latest version and its values schema (AGENT_CHART_REFRESH)")
	f.StringVar(&o.helmReleaseInterval, "helmrelease-interval", envOr("HELMRELEASE_INTERVAL", agents.DefaultHelmReleaseInterval), "HelmRelease.spec.interval of composed agents (HELMRELEASE_INTERVAL)")
	f.StringVar(&o.ociRepositoryInterval, "ocirepository-interval", envOr("OCIREPOSITORY_INTERVAL", agents.DefaultOCIRepositoryInterval), "OCIRepository.spec.interval of the shared chart source (OCIREPOSITORY_INTERVAL)")
	f.StringVar(&o.helmReleaseSA, "helmrelease-service-account", envOr("HELMRELEASE_SERVICE_ACCOUNT", ""), "HelmRelease.spec.serviceAccountName, required by a Flux multi-tenancy admission policy in tenant namespaces; empty omits it (HELMRELEASE_SERVICE_ACCOUNT)")
	f.StringVar(&o.skillsRepositories, "skills-repositories", envOr("AGENT_MANAGER_SKILLS_REPOSITORIES", ""), "Comma-separated GitHub repository URLs whose SKILL.md files are offered by list_skills (AGENT_MANAGER_SKILLS_REPOSITORIES)")
	f.StringVar(&o.skillsGitHubAPI, "skills-github-api", envOr("AGENT_MANAGER_SKILLS_GITHUB_API", "https://api.github.com"), "GitHub API base URL for skill discovery (AGENT_MANAGER_SKILLS_GITHUB_API)")
	f.StringVar(&o.skillsToken, "skills-github-token", envOr("GITHUB_TOKEN", ""), "GitHub token for private skill repositories and a higher rate limit; prefer the environment (GITHUB_TOKEN)")
	f.DurationVar(&o.skillsCacheTTL, "skills-cache-ttl", envDuration("AGENT_MANAGER_SKILLS_CACHE_TTL", 5*time.Minute), "How long a repository's discovered skills are reused (AGENT_MANAGER_SKILLS_CACHE_TTL)")
	f.BoolVar(&o.mcpEnabled, "mcp-enabled", envBool("AGENT_MANAGER_MCP_ENABLED", true), "Serve the MCP streamable-HTTP endpoint (AGENT_MANAGER_MCP_ENABLED)")
	f.StringVar(&o.mcpPath, "mcp-path", envOr("AGENT_MANAGER_MCP_PATH", "/mcp"), "MCP endpoint path (AGENT_MANAGER_MCP_PATH)")
	return cmd
}

func runServe(ctx context.Context, o *serveOptions) error {
	log := slog.Default()

	clients, err := kube.New(kube.Config{Kubeconfig: o.kubeconfig, Context: o.kubeContext, InCluster: o.inCluster})
	if err != nil {
		return fmt.Errorf("agent-manager needs Kubernetes access: %w", err)
	}

	kagentVersion := o.kagentAPIVersion
	if kagentVersion == "" || kagentVersion == "auto" {
		kagentVersion, err = kube.DiscoverVersion(clients.Discovery(), "kagent.dev", "agents")
		if err != nil {
			log.Warn("kagent API discovery failed, using default", "default", "v1alpha2", "error", err)
			kagentVersion = "v1alpha2"
		}
	}
	helmReleaseAPI := discoverGroupVersion(o.helmReleaseAPI, clients, "helm.toolkit.fluxcd.io", "helmreleases", agents.DefaultHelmReleaseAPIVersion, log)
	ociRepositoryAPI := discoverGroupVersion(o.ociRepositoryAPI, clients, "source.toolkit.fluxcd.io", "ocirepositories", agents.DefaultOCIRepositoryAPIVersion, log)

	resolver, err := chart.NewResolver(o.chartOCIURL, o.chartSemver, o.chartRefresh, nil, log)
	if err != nil {
		return err
	}

	var discoverer *skills.Discoverer
	if repos := splitList(o.skillsRepositories); len(repos) > 0 {
		discoverer = skills.New(skills.Config{Repositories: repos, APIURL: o.skillsGitHubAPI, Token: o.skillsToken, CacheTTL: o.skillsCacheTTL}, log)
	}

	svc := agents.New(kube.NewServiceAccountProvider(clients), resolver, discoverer, agents.Config{
		DefaultNamespace:  o.kagentNamespace,
		ManagedNamespaces: splitList(o.managedNamespaces),
		Compose: agents.ComposeConfig{
			ChartOCIURL:             o.chartOCIURL,
			ChartName:               resolver.Name(),
			ChartSemver:             o.chartSemver,
			HelmReleaseInterval:     o.helmReleaseInterval,
			OCIRepositoryInterval:   o.ociRepositoryInterval,
			ServiceAccountName:      o.helmReleaseSA,
			HelmReleaseAPIVersion:   helmReleaseAPI,
			OCIRepositoryAPIVersion: ociRepositoryAPI,
		},
		KagentAPIVersion: kagentVersion,
		Version:          version,
	}, log)

	srv, err := server.New(server.Config{Addr: o.listen, MCPEnabled: o.mcpEnabled, MCPPath: o.mcpPath}, svc, api.NewMCPServer(svc, version), log)
	if err != nil {
		return err
	}
	info := svc.Info(ctx)
	log.Info("agent-manager starting", "version", version, "listen", o.listen, "rest", api.Prefix, "mcp", o.mcpPath, "mcpEnabled", o.mcpEnabled,
		"namespaces", info.Namespaces.Managed, "chart", o.chartOCIURL, "chartVersion", info.Chart.LatestVersion, "schemaSource", info.Chart.SchemaSource,
		"kagentAPI", kagentVersion, "helmReleaseAPI", helmReleaseAPI, "ociRepositoryAPI", ociRepositoryAPI, "skillsRepositories", info.SkillsRepositories)

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return srv.Run(ctx)
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
