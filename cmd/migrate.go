package cmd

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
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

	chartOCIURL       string
	chartSemver       string
	chartSemverFilter string
	// chartSemverFilterSet is true when the flag or its variable is given,
	// even empty: only then does migrate change the sources' filters.
	chartSemverFilterSet bool

	skillsGitHubAPI string
	skillsToken     string

	reportConfigMap string
	dryRun          bool
}

func newMigrateCmd() *cobra.Command {
	o := &migrateOptions{}
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Migrate the managed namespaces' agents to the next Generic chart major: 1.x to 2.x (api.kagent.dev Agent), or 0.x to 1.x (kagent.dev AgentTemplate)",
		Long: `Expand–contract migration of an installation's agents, run once per
installation as a Job of the connectivity chart or by hand with a kubeconfig.
Every phase is idempotent and gated on the previous one; re-running is safe.

The target range (--agent-chart-semver) picks the path by its major, because a
cluster serves one kagent line at a time and a namespace's releases share one
chart source:

  2.x       1.x to 2.x: kagent.dev/v1alpha3 AgentTemplate to api.kagent.dev
            Agent. Sub-agent bindings in extraTools[] and extraAgentSpec.tools[]
            move from agent to subAgent (a Shared isolation is dropped, a
            Dedicated one is refused), the 1.x Harness admission label in labels
            becomes agent.harness. Needs api.kagent.dev agents served, except
            with --dry-run, which validates and diffs before the kagent upgrade.
  1.x       0.x to 1.x: kagent.dev/v1alpha2 Agent to kagent.dev/v1alpha3
            AgentTemplate. The removed keys are dropped, muster.toolNames is
            renamed to muster.tools, every skill is pinned to a commit or a
            digest through the GitHub API and the registry. Needs kagent.dev
            agenttemplates served.

A release deployed from a chart older than the path's source major is reported
as failed and never rewritten.

  expand    every Generic-chart HelmRelease rendering into a managed namespace
            gets the target major's values, validated against the target
            chart's values.schema.json; the releases of a chart source are
            written only when every one of them validates. A GitOps-owned or
            external release is never written — its rewrite is emitted as a
            diff. The namespace's agent-chart OCIRepository moves to the target
            range last, once every release it serves is on target values and
            the registry has a version in that range.
  wait      the contract runs only when every release is deployed from a chart
            in the target range, every object the target chart renders (the
            Agent on 2.x, the AgentTemplate on 1.x) is Ready, and no object of
            the old line is still rendered by a release Flux has yet to
            upgrade; until then the report names what is pending.
  contract  the leftover agent objects of the old line in the managed
            namespaces are deleted (kagent.dev/v1alpha3 AgentTemplates on 2.x;
            kagent.dev/v1alpha2 Agents on 1.x, then the CRDs agents,
            sandboxagents, agentharnesses, memories and toolservers of
            kagent.dev). On 2.x the kagent.dev CRDs stay: the platform removes
            them.

Every run writes a report ConfigMap per managed namespace (--report-configmap;
keys phase, summary, report.yaml) and prints it; --dry-run prints it and
writes nothing. The exit code is 0 whenever the run did what the cluster's
state admits — pending releases included — and non-zero only for what needs an
operator: no cluster access, a target range with no migration path, the
target line's kagent API not served, a read or write the API server refused,
the report not writable. Every flag can also be set through the environment
variable named next to it; flags win.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, envSet := os.LookupEnv("AGENT_CHART_SEMVER_FILTER")
			o.chartSemverFilterSet = envSet || cmd.Flags().Changed("agent-chart-semver-filter")
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
	f.StringVar(&o.kagentAPIVersion, "kagent-api-version", envOr("KAGENT_API_VERSION", "auto"), "Version of the kagent API the target chart renders into: api.kagent.dev (Agents) for 2.x, kagent.dev (AgentTemplates) for 1.x; auto discovers the version serving that resource, default "+agents.DefaultKagentAPIVersion+" (KAGENT_API_VERSION)")
	f.StringVar(&o.harnessName, "harness-name", envOr("AGENT_HARNESS_NAME", agents.DefaultHarnessName), "Name of the platform Harness every agent runs on: composed as the chart value agent.harness when it is not the chart default; on 1.x its AgentTemplate.status.harnesses[] entry decides the wait phase (AGENT_HARNESS_NAME)")
	f.StringVar(&o.helmReleaseAPI, "flux-helmrelease-api-version", envOr("FLUX_HELMRELEASE_API_VERSION", "auto"), "helm.toolkit.fluxcd.io API version composed into HelmReleases; auto discovers it (FLUX_HELMRELEASE_API_VERSION)")
	f.StringVar(&o.ociRepositoryAPI, "flux-ocirepository-api-version", envOr("FLUX_OCIREPOSITORY_API_VERSION", "auto"), "source.toolkit.fluxcd.io API version composed into OCIRepositories; auto discovers it (FLUX_OCIREPOSITORY_API_VERSION)")
	f.StringVar(&o.chartOCIURL, "agent-chart-oci-url", envOr("AGENT_CHART_OCI_URL", agents.DefaultChartOCIURL), "OCI URL of the agent chart every agent renders from (AGENT_CHART_OCI_URL)")
	f.StringVar(&o.chartSemver, "agent-chart-semver", envOr("AGENT_CHART_SEMVER", agents.DefaultChartSemver), "Semver range the agent-chart OCIRepositories move to; its major picks the path: 2.x migrates 1.x releases (kagent.dev AgentTemplate) to 2.x (api.kagent.dev Agent), 1.x migrates 0.x releases to 1.x. 2.x follows every 2.x release of the Generic chart and never a pre-release (AGENT_CHART_SEMVER)")
	f.StringVar(&o.chartSemverFilter, "agent-chart-semver-filter", envOr("AGENT_CHART_SEMVER_FILTER", ""), "Regular expression the agent chart's tags must match before the range is evaluated, as the OCIRepository's ref.semverFilter; empty filters nothing; unset, each source keeps its own, set empty removes it (AGENT_CHART_SEMVER_FILTER)")
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
	major, err := migrate.TargetMajor(o.chartSemver)
	if err != nil {
		return err
	}
	kagentVersion, err := migrateKagentVersion(clients, o, major, log)
	if err != nil {
		return err
	}
	helmReleaseAPI := discoverGroupVersion(o.helmReleaseAPI, clients, "helm.toolkit.fluxcd.io", "helmreleases", agents.DefaultHelmReleaseAPIVersion, log)
	ociRepositoryAPI := discoverGroupVersion(o.ociRepositoryAPI, clients, "source.toolkit.fluxcd.io", "ocirepositories", agents.DefaultOCIRepositoryAPIVersion, log)

	resolver, err := chart.NewResolver(o.chartOCIURL, o.chartSemver, 10*time.Minute, nil, log, chart.WithSemverFilter(o.chartSemverFilter))
	if err != nil {
		return err
	}
	pinner := skills.NewResolver(o.skillsGitHubAPI, skills.StaticToken(o.skillsToken), nil, nil)
	compose := agents.ComposeConfig{
		ChartOCIURL: o.chartOCIURL, ChartName: resolver.Name(), ChartSemver: o.chartSemver, ChartSemverFilter: o.chartSemverFilter,
		HelmReleaseAPIVersion: helmReleaseAPI, OCIRepositoryAPIVersion: ociRepositoryAPI, HarnessName: o.harnessName,
	}
	svc := agents.New(kube.NewServiceAccountProvider(clients), resolver, nil, pinner, agents.Config{
		DefaultNamespace: o.kagentNamespace, ManagedNamespaces: splitList(o.managedNamespaces), Compose: compose, KagentAPIVersion: kagentVersion, Version: build.Version,
	}, log)
	// On 2.x the service is the status reader, so the wait phase and
	// get_agent_status agree on what a Ready Agent is; the service reads no
	// kagent.dev AgentTemplate, which 1.x renders.
	var status migrate.StatusReader = svc
	if major == 1 {
		status = migrate.TemplateStatus{Client: clients.Dynamic(), APIVersion: kagentVersion, Harness: o.harnessName}
	}
	var targetFilter *string
	if o.chartSemverFilterSet {
		targetFilter = &o.chartSemverFilter
	}
	namespaces := svc.Info(ctx).Namespaces.Managed
	runner, err := migrate.New(clients, resolver, pinner, status, migrate.Options{
		Namespaces:              namespaces,
		GitOpsNamespaces:        splitList(o.gitopsNamespaces),
		HarnessName:             o.harnessName,
		ChartOCIURL:             o.chartOCIURL,
		TargetSemver:            o.chartSemver,
		TargetSemverFilter:      targetFilter,
		ReportConfigMap:         o.reportConfigMap,
		DryRun:                  o.dryRun,
		KagentAPIVersion:        kagentVersion,
		HelmReleaseAPIVersion:   helmReleaseAPI,
		OCIRepositoryAPIVersion: ociRepositoryAPI,
		Version:                 build.Version,
	}, log)
	if err != nil {
		return err
	}
	log.Info("agent-manager migrate starting", "version", build.Version, "commit", build.Commit, "namespaces", namespaces, "gitopsNamespaces", splitList(o.gitopsNamespaces),
		"chart", o.chartOCIURL, "targetSemver", o.chartSemver, "targetMajor", major, "targetSemverFilter", o.chartSemverFilter, "targetSemverFilterSet", o.chartSemverFilterSet, "harness", o.harnessName, "kagentAPI", kagentVersion, "dryRun", o.dryRun, "report", o.reportConfigMap, "githubToken", o.skillsToken != "")

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

// migrateKagentVersion is the served version of the kagent API the target
// chart renders into; the one precondition that is never guessed. 1.x values
// under a 0.x chart, or 2.x values before the api.kagent.dev line runs, break
// every agent, so without the resource nothing is written. A dry run of the
// 1.x -> 2.x path goes ahead before the kagent upgrade: it validates and
// diffs, and its wait phase says the Agent resource is not served yet.
func migrateKagentVersion(clients kube.Client, o *migrateOptions, major uint64, log *slog.Logger) (string, error) {
	group, resource := "kagent.dev", "agenttemplates"
	if major == 2 {
		group, resource = agents.KagentAPIGroup, "agents"
	}
	version := strings.TrimPrefix(o.kagentAPIVersion, group+"/")
	if version != "" && version != "auto" {
		return version, nil
	}
	version, err := kube.DiscoverVersion(clients.Discovery(), group, resource)
	if err == nil {
		return version, nil
	}
	if major == 2 && o.dryRun {
		log.Warn("api.kagent.dev agents not served; the dry run validates and diffs only", "error", err, "assumedVersion", agents.DefaultKagentAPIVersion)
		return agents.DefaultKagentAPIVersion, nil
	}
	return "", fmt.Errorf("the target chart %d.x renders %s of group %s, which the cluster does not serve (%v); run migrate after the kagent upgrade (--dry-run works before it on 2.x) — nothing was changed", major, resource, group, err)
}
