package cmd

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/chart"
	"github.com/giantswarm/agent-manager/internal/kube"
	"github.com/giantswarm/agent-manager/internal/migrate"
	"github.com/giantswarm/agent-manager/internal/skills"
)

// migrateOptions shares serve's flags and environment variables for
// everything both need (cluster access, namespaces, the platform Harness, the
// chart, the GitHub API) and adds the migration's own.
type migrateOptions struct {
	kubeconfig  string
	kubeContext string
	inCluster   bool

	kagentNamespace   string
	managedNamespaces string
	gitopsNamespaces  string
	kagentAPIVersion  string
	harnessName       string
	helmReleaseAPI    string
	ociRepositoryAPI  string

	chartOCIURL string
	chartSemver string

	skillsGitHubAPI string
	skillsToken     string

	reportConfigMap string
	dryRun          bool
}

func newMigrateCmd() *cobra.Command {
	o := &migrateOptions{}
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Migrate the managed namespaces' agents from Generic chart 0.x (kagent API v1alpha2) to chart 1.x (API v2), then remove the v1alpha2 objects and CRDs",
		Long: `Expand–contract migration of an installation's agents, run once per
installation as a Job of the connectivity chart or by hand with a kubeconfig.
Every phase is idempotent and gated on the previous one; re-running is safe.

  expand    every Generic-chart HelmRelease rendering into a managed namespace
            gets 1.x values (the removed keys dropped, muster.toolNames renamed
            to muster.tools, every skill pinned to a commit or a digest through
            the GitHub API and the registry, validated against the chart's
            values.schema.json before anything is written); a GitOps-owned or
            external release is never written — its rewrite is emitted as a
            diff; the namespace's agent-chart OCIRepository moves to the target
            range last, once every release it serves is on 1.x values and the
            registry has a version in that range.
  wait      the contract runs only when every release is deployed from a 1.x
            chart, every AgentTemplate of the namespace is Ready on the
            platform Harness, and no v1alpha2 Agent is still rendered by a
            release Flux has yet to upgrade; until then the report names what
            is pending.
  contract  the leftover kagent.dev/v1alpha2 Agent objects of the managed
            namespaces are deleted, then the CRDs agents, sandboxagents,
            agentharnesses, memories and toolservers of kagent.dev.

Every run writes a report ConfigMap per managed namespace (--report-configmap;
keys phase, summary, report.yaml) and prints it; --dry-run prints it and
writes nothing. The exit code is 0 whenever the run did what the cluster's
state admits — pending releases included — and non-zero only for what needs an
operator: no cluster access, kagent API v2 not served, a read or write the
API server refused, the report not writable. Every flag can also be set
through the environment variable named next to it; flags win.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMigrate(cmd.Context(), cmd.OutOrStdout(), o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.kubeconfig, "kubeconfig", envOr("KUBECONFIG", ""), "Kubeconfig path; empty uses the default loading rules or in-cluster auth (KUBECONFIG)")
	f.StringVar(&o.kubeContext, "kube-context", envOr("KUBE_CONTEXT", ""), "Kubeconfig context override (KUBE_CONTEXT)")
	f.BoolVar(&o.inCluster, "in-cluster", envBool("KUBERNETES_IN_CLUSTER", false), "Force in-cluster Kubernetes auth (KUBERNETES_IN_CLUSTER)")
	f.StringVar(&o.kagentNamespace, "kagent-namespace", envOr("KAGENT_NAMESPACE", "kagent"), "Default namespace agents are created in and listed from (KAGENT_NAMESPACE)")
	f.StringVar(&o.managedNamespaces, "managed-namespaces", envOr("AGENT_MANAGER_MANAGED_NAMESPACES", ""), "Comma-separated additional namespaces agents may live in; RBAC must exist there (AGENT_MANAGER_MANAGED_NAMESPACES)")
	f.StringVar(&o.gitopsNamespaces, "gitops-namespaces", envOr("AGENT_MANAGER_MIGRATE_GITOPS_NAMESPACES", ""), "Comma-separated namespaces searched for GitOps-owned Generic-chart HelmReleases with a targetNamespace in the managed set, next to the ones the rendered objects' Flux provenance labels name; read only (AGENT_MANAGER_MIGRATE_GITOPS_NAMESPACES)")
	f.StringVar(&o.kagentAPIVersion, "kagent-api-version", envOr("KAGENT_API_VERSION", "auto"), "kagent.dev API version for AgentTemplates, Harnesses, RemoteMCPServers and ModelConfigs; auto discovers the version serving agenttemplates, default "+agents.DefaultKagentAPIVersion+" (KAGENT_API_VERSION)")
	f.StringVar(&o.harnessName, "harness-name", envOr("AGENT_HARNESS_NAME", agents.DefaultHarnessName), "Name of the platform Harness every agent runs on: composed as the chart value agent.harness (the admission label's value); its AgentTemplate.status.harnesses[] entry decides get_agent_status (AGENT_HARNESS_NAME)")
	f.StringVar(&o.helmReleaseAPI, "flux-helmrelease-api-version", envOr("FLUX_HELMRELEASE_API_VERSION", "auto"), "helm.toolkit.fluxcd.io API version composed into HelmReleases; auto discovers it (FLUX_HELMRELEASE_API_VERSION)")
	f.StringVar(&o.ociRepositoryAPI, "flux-ocirepository-api-version", envOr("FLUX_OCIREPOSITORY_API_VERSION", "auto"), "source.toolkit.fluxcd.io API version composed into OCIRepositories; auto discovers it (FLUX_OCIREPOSITORY_API_VERSION)")
	f.StringVar(&o.chartOCIURL, "agent-chart-oci-url", envOr("AGENT_CHART_OCI_URL", agents.DefaultChartOCIURL), "OCI URL of the agent chart every agent renders from (AGENT_CHART_OCI_URL)")
	f.StringVar(&o.chartSemver, "agent-chart-semver", envOr("AGENT_CHART_SEMVER", agents.DefaultChartSemver), "Semver range the OCIRepository tracks; 1.x follows every 1.x release of the Generic chart and never a pre-release (AGENT_CHART_SEMVER)")
	f.StringVar(&o.skillsGitHubAPI, "skills-github-api", envOr("AGENT_MANAGER_SKILLS_GITHUB_API", "https://api.github.com"), "GitHub API base URL for skill discovery and for resolving a skill's branch or tag to its head commit (AGENT_MANAGER_SKILLS_GITHUB_API)")
	f.StringVar(&o.skillsToken, "skills-github-token", envOr("GITHUB_TOKEN", ""), "GitHub token for private skill repositories and a higher rate limit; prefer the environment (GITHUB_TOKEN)")
	f.StringVar(&o.reportConfigMap, "report-configmap", envOr("AGENT_MANAGER_MIGRATE_REPORT_CONFIGMAP", migrate.DefaultReportConfigMap), "Name of the report ConfigMap written in every managed namespace (AGENT_MANAGER_MIGRATE_REPORT_CONFIGMAP)")
	f.BoolVar(&o.dryRun, "dry-run", envBool("AGENT_MANAGER_MIGRATE_DRY_RUN", false), "Print the report and the diffs; write nothing, not even the report (AGENT_MANAGER_MIGRATE_DRY_RUN)")
	return cmd
}

func runMigrate(ctx context.Context, out io.Writer, o *migrateOptions) error {
	log := slog.Default()
	clients, err := kube.New(kube.Config{Kubeconfig: o.kubeconfig, Context: o.kubeContext, InCluster: o.inCluster})
	if err != nil {
		return fmt.Errorf("agent-manager migrate needs Kubernetes access: %w", err)
	}
	// The one precondition that is never guessed: kagent API v2 must be
	// served. Rewriting values while the 0.x chart still renders would break
	// every agent, so without agenttemplates nothing happens.
	kagentVersion := strings.TrimPrefix(o.kagentAPIVersion, "kagent.dev/")
	if kagentVersion == "" || kagentVersion == "auto" {
		kagentVersion, err = kube.DiscoverVersion(clients.Discovery(), "kagent.dev", "agenttemplates")
		if err != nil {
			return fmt.Errorf("kagent API v2 is not served (no agenttemplates resource in group kagent.dev: %v); run migrate after the kagent upgrade — nothing was changed", err)
		}
	}
	helmReleaseAPI := discoverGroupVersion(o.helmReleaseAPI, clients, "helm.toolkit.fluxcd.io", "helmreleases", agents.DefaultHelmReleaseAPIVersion, log)
	ociRepositoryAPI := discoverGroupVersion(o.ociRepositoryAPI, clients, "source.toolkit.fluxcd.io", "ocirepositories", agents.DefaultOCIRepositoryAPIVersion, log)

	resolver, err := chart.NewResolver(o.chartOCIURL, o.chartSemver, 10*time.Minute, nil, log)
	if err != nil {
		return err
	}
	pinner := skills.NewResolver(o.skillsGitHubAPI, o.skillsToken, nil, nil)
	compose := agents.ComposeConfig{
		ChartOCIURL: o.chartOCIURL, ChartName: resolver.Name(), ChartSemver: o.chartSemver,
		HelmReleaseAPIVersion: helmReleaseAPI, OCIRepositoryAPIVersion: ociRepositoryAPI, HarnessName: o.harnessName,
	}
	// The service is the status reader: the wait phase asks get_agent_status
	// about every template, so the two agree on what Ready means.
	svc := agents.New(kube.NewServiceAccountProvider(clients), resolver, nil, pinner, agents.Config{
		DefaultNamespace: o.kagentNamespace, ManagedNamespaces: splitList(o.managedNamespaces), Compose: compose, KagentAPIVersion: kagentVersion, Version: version,
	}, log)
	runner := migrate.New(clients, resolver, pinner, svc, migrate.Options{
		Namespaces:              svc.Info(ctx).Namespaces.Managed,
		GitOpsNamespaces:        splitList(o.gitopsNamespaces),
		HarnessName:             o.harnessName,
		ChartOCIURL:             o.chartOCIURL,
		TargetSemver:            o.chartSemver,
		ReportConfigMap:         o.reportConfigMap,
		DryRun:                  o.dryRun,
		KagentAPIVersion:        kagentVersion,
		HelmReleaseAPIVersion:   helmReleaseAPI,
		OCIRepositoryAPIVersion: ociRepositoryAPI,
		Version:                 version,
	}, log)
	log.Info("agent-manager migrate starting", "version", version, "namespaces", svc.Info(ctx).Namespaces.Managed, "gitopsNamespaces", splitList(o.gitopsNamespaces),
		"chart", o.chartOCIURL, "targetSemver", o.chartSemver, "harness", o.harnessName, "kagentAPI", kagentVersion, "dryRun", o.dryRun, "report", o.reportConfigMap, "githubToken", o.skillsToken != "")

	res, runErr := runner.Run(ctx)
	if res != nil {
		for i, rep := range res.Reports {
			if i > 0 {
				_, _ = fmt.Fprintln(out, "---")
			}
			_, _ = fmt.Fprint(out, rep.String())
		}
	}
	return runErr
}
