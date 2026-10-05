package agents

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/giantswarm/agent-manager/internal/kube"
)

// An agent can run on a workload cluster of the installation instead of the
// installation's own cluster. Its HelmRelease and the shared OCIRepository
// stay on the installation, in the organization namespace that holds the
// cluster, and the release reaches the workload cluster through the Cluster
// API kubeconfig Secret (spec.kubeConfig.secretRef); the Agent, its
// RemoteMCPServer, the ModelConfigs and the Harnesses are on the workload
// cluster, read through a client built from that Secret's server address and
// CA that presents the caller's token.

// Target names the workload cluster an agent runs on: both fields or none.
type Target struct {
	// Organization owns the cluster: its namespace org-<organization> on the
	// installation holds the Cluster and its kubeconfig Secret.
	Organization string `json:"organization,omitempty"`
	// Cluster is the workload cluster's name.
	Cluster string `json:"cluster,omitempty"`
}

// Location is where an agent is: its namespace (on the workload cluster when
// a target is set) and the target.
type Location struct {
	Namespace string `json:"namespace,omitempty"`
	Target
}

// In is the Location of namespace ns on the installation's own cluster.
func In(ns string) Location { return Location{Namespace: ns} }

// The Cluster API contract of a workload cluster's kubeconfig.
const (
	// KubeconfigSecretSuffix: the Secret <cluster>-kubeconfig in the
	// organization namespace.
	KubeconfigSecretSuffix = "-kubeconfig" // #nosec G101 -- a Secret name suffix, not a credential
	// KubeconfigSecretKey is the key holding the kubeconfig.
	KubeconfigSecretKey = "value"
	// OrganizationNamespacePrefix: an organization's namespace on the
	// installation is org-<organization>.
	OrganizationNamespacePrefix = "org-"
	// ClusterLabel marks the HelmRelease of an agent placed on a workload
	// cluster with the cluster's name.
	ClusterLabel = "agent-platform.giantswarm.io/cluster"
)

// IsSet reports whether the target names a cluster.
func (t Target) IsSet() bool { return t.Organization != "" || t.Cluster != "" }

// Validate checks that both fields are DNS-1123 labels, or both are empty.
func (t Target) Validate() error {
	if !t.IsSet() {
		return nil
	}
	if t.Organization == "" || t.Cluster == "" {
		return invalidf("a target cluster needs both organization and cluster (got organization %q, cluster %q)", t.Organization, t.Cluster)
	}
	for field, v := range map[string]string{"organization": t.Organization, "cluster": t.Cluster} {
		if len(v) > 63 || !dns1123.MatchString(v) {
			return invalidf("%s %q must be a DNS-1123 label", field, v)
		}
	}
	return nil
}

// FluxNamespace is the installation namespace the agent's HelmRelease lives
// in: the organization's.
func (t Target) FluxNamespace() string { return OrganizationNamespacePrefix + t.Organization }

// KubeconfigSecret is the name of the cluster's kubeconfig Secret.
func (t Target) KubeconfigSecret() string { return t.Cluster + KubeconfigSecretSuffix }

// ReleaseName is the HelmRelease name of an agent on the cluster: prefixed
// with the cluster, since one organization namespace holds the releases of
// all its clusters. Its length is checked by ValidateReleaseName.
func (t Target) ReleaseName(agent string) string { return t.Cluster + "-" + agent }

// ValidateReleaseName refuses a cluster and agent name pair whose release
// name is not a label value (helm-controller stamps it on every object).
func (t Target) ValidateReleaseName(agent string) error {
	if name := t.ReleaseName(agent); len(name) > 63 {
		return invalidf("the HelmRelease name %q (cluster-agent) is longer than 63 characters; pick a shorter agent name", name)
	}
	return nil
}

// PlaceOnCluster turns an agent's HelmRelease into one that installs on the
// target cluster: named after cluster and agent in the organization
// namespace, installing the release named after the agent into ns there
// through the kubeconfig Secret. The chart source stays beside it. Flux
// impersonates spec.serviceAccountName on the remote cluster, where no such
// account exists, so it is dropped: the kubeconfig is the identity.
func PlaceOnCluster(hr *unstructured.Unstructured, t Target, agent, ns string) {
	fluxNS := t.FluxNamespace()
	hr.SetName(t.ReleaseName(agent))
	hr.SetNamespace(fluxNS)
	labels := hr.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[ClusterLabel] = t.Cluster
	hr.SetLabels(labels)
	spec, _ := hr.Object["spec"].(map[string]any)
	delete(spec, "serviceAccountName")
	if ref, ok := spec["chartRef"].(map[string]any); ok {
		ref["namespace"] = fluxNS
	}
	spec["releaseName"] = agent
	spec["targetNamespace"] = ns
	spec["storageNamespace"] = ns
	spec["kubeConfig"] = map[string]any{"secretRef": map[string]any{"name": t.KubeconfigSecret(), "key": KubeconfigSecretKey}}
}

// placedOn reads the target cluster's kubeconfig Secret name and the
// namespace on it from a HelmRelease; empty for a local one.
func placedOn(hr *unstructured.Unstructured) (secret, targetNS string) {
	secret, _, _ = unstructured.NestedString(hr.Object, "spec", "kubeConfig", "secretRef", "name")
	targetNS, _, _ = unstructured.NestedString(hr.Object, "spec", "targetNamespace")
	return secret, targetNS
}

// ForTarget is the composition of an agent on a workload cluster: muster at
// the URL the cluster reaches, and no installation skills credential (that
// Secret exists on the installation only; an agent on a cluster names its
// own).
func (c ComposeConfig) ForTarget() ComposeConfig {
	c.MusterURL = c.TargetMusterURL
	c.SkillsGitAuthSecretName = ""
	return c
}

// site is where one request's objects are: the Flux objects on the
// installation (in the agent's namespace, or the organization's for a
// target), the rendered ones where the agent runs.
type site struct {
	loc Location
	// flux reads and writes HelmReleases and OCIRepositories in fluxNS.
	flux   dynamic.Interface
	fluxNS string
	// agent reads the Agents, RemoteMCPServers, ModelConfigs and
	// Harnesses of loc.Namespace; agentClient the events there.
	agent       dynamic.Interface
	agentClient kube.Client
	fluxClient  kube.Client
	// compose is the composition for this site.
	compose ComposeConfig
}

// ns is the agent's namespace.
func (st *site) ns() string { return st.loc.Namespace }

// describe names an agent in messages: namespace/name, and the cluster.
func (st *site) describe(agent string) string {
	if st.loc.IsSet() {
		return fmt.Sprintf("%s/%s on cluster %s/%s", st.ns(), agent, st.loc.Organization, st.loc.Cluster)
	}
	return st.ns() + "/" + agent
}

// releaseName is the HelmRelease name of agent at this site.
func (st *site) releaseName(agent string) string {
	if st.loc.IsSet() {
		return st.loc.ReleaseName(agent)
	}
	return agent
}

// helmRelease composes the agent's HelmRelease for this site.
func (st *site) helmRelease(agent string, values map[string]any) *unstructured.Unstructured {
	hr := BuildHelmRelease(agent, st.ns(), values, st.compose)
	if st.loc.IsSet() {
		PlaceOnCluster(hr, st.loc.Target, agent, st.ns())
	}
	return hr
}

// manifests renders the pair for this site.
func (st *site) manifests(agent string, values map[string]any) Manifests {
	return Manifests{
		OCIRepository: ToYAML(BuildOCIRepository(st.fluxNS, st.compose)),
		HelmRelease:   ToYAML(st.helmRelease(agent, values)),
		Values:        values,
	}
}

// ownsRelease reports whether a HelmRelease of fluxNS belongs to this site:
// a local site owns the releases without a kubeConfig, a target the ones
// that install into its namespace through its cluster's Secret.
func (st *site) ownsRelease(hr *unstructured.Unstructured) bool {
	secret, targetNS := placedOn(hr)
	if !st.loc.IsSet() {
		return secret == ""
	}
	return secret == st.loc.KubeconfigSecret() && orDefault(targetNS, st.fluxNS) == st.ns()
}

// site resolves a Location: the namespace against the managed set, the
// installation's client and, for a target, the workload cluster's client
// from its kubeconfig Secret, read as the caller.
func (s *Service) site(ctx context.Context, loc Location) (*site, error) {
	ns, err := s.Namespace(loc.Namespace)
	if err != nil {
		return nil, err
	}
	loc.Namespace = ns
	if err := loc.Validate(); err != nil {
		return nil, err
	}
	dyn, client, err := s.dyn(ctx)
	if err != nil {
		return nil, err
	}
	st := &site{loc: loc, flux: dyn, fluxClient: client, fluxNS: ns, agent: dyn, agentClient: client, compose: s.cfg.Compose}
	if !loc.IsSet() {
		return st, nil
	}
	st.fluxNS = loc.FluxNamespace()
	st.compose = s.cfg.Compose.ForTarget()
	secret, err := client.Typed().CoreV1().Secrets(st.fluxNS).Get(ctx, loc.KubeconfigSecret(), metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return nil, invalidf("cluster %s of organization %s has no kubeconfig Secret %s/%s (Cluster API writes it when the cluster is created)", loc.Cluster, loc.Organization, st.fluxNS, loc.KubeconfigSecret())
	case err != nil:
		return nil, wrapKube(err, fmt.Sprintf("get the kubeconfig Secret %s/%s of cluster %s", st.fluxNS, loc.KubeconfigSecret(), loc.Cluster))
	}
	kubeconfig := secret.Data[KubeconfigSecretKey]
	if len(kubeconfig) == 0 {
		return nil, invalidf("the kubeconfig Secret %s/%s of cluster %s has no key %q", st.fluxNS, loc.KubeconfigSecret(), loc.Cluster, KubeconfigSecretKey)
	}
	remote, err := s.kube.Remote(ctx, kubeconfig)
	if err != nil {
		if errorsIs(err, kube.ErrNoCallerToken) {
			return nil, fmt.Errorf("%w: the request carries no identity token to act with on cluster %s: %v", ErrUnauthenticated, loc.Cluster, err)
		}
		return nil, fmt.Errorf("client for cluster %s: %w", loc.Cluster, err)
	}
	st.agent, st.agentClient = remote.Dynamic(), remote
	return st, nil
}
