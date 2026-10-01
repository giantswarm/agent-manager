package agents

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"

	"github.com/giantswarm/agent-manager/internal/chart"
	"github.com/giantswarm/agent-manager/internal/identity"
	"github.com/giantswarm/agent-manager/internal/kube"
	"github.com/giantswarm/agent-manager/internal/skills"
)

// ChartSource is what the service needs to know about the agent chart.
type ChartSource interface {
	SchemaSource
	Info(ctx context.Context) chart.Info
	Name() string
	OCIURL() string
	SemverRange() string
}

// Config configures the service.
type Config struct {
	// DefaultNamespace receives agents when a request names none.
	DefaultNamespace string
	// ManagedNamespaces are the namespaces the service may read and write
	// (RBAC exists there); DefaultNamespace is always included.
	ManagedNamespaces []string
	// Compose is the platform side of the manifests.
	Compose ComposeConfig
	// KagentAPIVersion is the served kagent.dev version (v1alpha3).
	KagentAPIVersion string
	// Version is the service version reported by GET /info.
	Version string
	// GitHub offers commit mode: the pull request is opened as the person
	// with the GitHub token the App-pinned registration carries. Nil: commit
	// mode is refused as unsupported.
	GitHub RemoteFor
	// SkillsBootSecret keeps the installation's skills credential Secret
	// filled with the skills GitHub App's token; nil: the Secret is
	// provisioned by someone else.
	SkillsBootSecret *skills.BootSecret
}

// Service is the agent lifecycle.
type Service struct {
	kube   kube.Provider
	chart  ChartSource
	skills *skills.Discoverer
	pinner SkillPinner
	cfg    Config
	log    *slog.Logger
}

// DefaultKagentAPIVersion is composed when discovery cannot answer.
const DefaultKagentAPIVersion = "v1alpha3"

// New builds the service. skills may be nil (list_skills then reports
// unsupported); pinner may be nil (skills must then come pinned).
func New(k kube.Provider, c ChartSource, s *skills.Discoverer, p SkillPinner, cfg Config, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	cfg.DefaultNamespace = orDefault(cfg.DefaultNamespace, "kagent")
	cfg.KagentAPIVersion = orDefault(cfg.KagentAPIVersion, DefaultKagentAPIVersion)
	cfg.Compose.HarnessName = orDefault(cfg.Compose.HarnessName, DefaultHarnessName)
	cfg.Compose.ChartName = orDefault(cfg.Compose.ChartName, c.Name())
	cfg.Compose.ChartOCIURL = orDefault(cfg.Compose.ChartOCIURL, c.OCIURL())
	cfg.Compose.ChartSemver = orDefault(cfg.Compose.ChartSemver, c.SemverRange())
	cfg.Compose.HelmReleaseAPIVersion = orDefault(cfg.Compose.HelmReleaseAPIVersion, DefaultHelmReleaseAPIVersion)
	cfg.Compose.OCIRepositoryAPIVersion = orDefault(cfg.Compose.OCIRepositoryAPIVersion, DefaultOCIRepositoryAPIVersion)
	cfg.ManagedNamespaces = ManagedNamespaces(cfg.DefaultNamespace, cfg.ManagedNamespaces)
	return &Service{kube: k, chart: c, skills: s, pinner: p, cfg: cfg, log: log}
}

// ManagedNamespaces are the default namespace followed by the additional
// ones, without empty entries or the default repeated.
func ManagedNamespaces(def string, additional []string) []string {
	managed := []string{def}
	for _, ns := range additional {
		if ns != "" && ns != def {
			managed = append(managed, ns)
		}
	}
	return managed
}

// InfoResponse is GET /info: what this installation can do, so the portal and
// agents feature-detect instead of guessing.
type InfoResponse struct {
	Version string     `json:"version"`
	Chart   chart.Info `json:"chart"`
	// Namespaces the service manages agents in.
	Namespaces struct {
		Default string   `json:"default"`
		Managed []string `json:"managed"`
	} `json:"namespaces"`
	// Capabilities are explicit flags; a false flag means the matching
	// operation answers 501 unsupported (or, for commit, that the mode does
	// not exist on this installation).
	Capabilities map[string]bool `json:"capabilities"`
	// Identity says how calls reach the API server: `caller` (every call
	// presents the signed-in user's IdP token; the user's RBAC governs) or
	// `serviceAccount` (the service's own identity behind a trusted proxy).
	Identity string `json:"identity"`
	// APIVersions are the served CRD versions the service composes and reads.
	APIVersions struct {
		AgentTemplate   string `json:"agentTemplate"`
		Harness         string `json:"harness"`
		RemoteMCPServer string `json:"remoteMcpServer"`
		ModelConfig     string `json:"modelConfig"`
		HelmRelease     string `json:"helmRelease"`
		OCIRepository   string `json:"ociRepository"`
	} `json:"apiVersions"`
	// Flux settings the composed HelmReleases carry.
	Flux struct {
		HelmReleaseInterval   string `json:"helmReleaseInterval"`
		OCIRepositoryInterval string `json:"ociRepositoryInterval"`
		ServiceAccountName    string `json:"serviceAccountName,omitempty"`
	} `json:"flux"`
	// Harness is the platform Harness every agent runs on.
	Harness struct {
		Name string `json:"name"`
	} `json:"harness"`
	// Muster is the platform's muster MCP gateway as composed into every
	// agent: URL empty means the chart default applies.
	Muster struct {
		URL string `json:"url"`
		// TargetURL is the muster URL composed into agents on workload
		// clusters; empty: target clusters are refused.
		TargetURL string `json:"targetUrl,omitempty"`
	} `json:"muster"`
	// SkillsRepositories are the configured skill repositories.
	SkillsRepositories []string `json:"skillsRepositories"`
	// SkillsGitAuthSecretName is the installation's skills credential Secret
	// (key token) an agent with a git skill fetches with unless it names its
	// own; empty: anonymous fetches.
	SkillsGitAuthSecretName string `json:"skillsGitAuthSecretName,omitempty"`
	// SkillsGitAuthMint is the state of that Secret when agent-manager keeps
	// it filled with the skills GitHub App's token: the last refresh, the
	// token's expiry and a failed refresh's error.
	SkillsGitAuthMint *skills.BootSecretStatus `json:"skillsGitAuthMint,omitempty"`
}

// Info reports the installation's capabilities.
func (s *Service) Info(ctx context.Context) InfoResponse {
	var out InfoResponse
	out.Version = s.cfg.Version
	out.Chart = s.chart.Info(ctx)
	out.Namespaces.Default = s.cfg.DefaultNamespace
	out.Namespaces.Managed = append([]string(nil), s.cfg.ManagedNamespaces...)
	out.Capabilities = map[string]bool{
		"list": true, "get": true, "create": true, "update": true, "delete": true,
		"status": true, "validate": true, "modelConfigs": true,
		"skills":         s.skills != nil,
		"writesAsCaller": s.kube.Identity() == kube.IdentityCaller,
		// mode commit: the manifests land as a pull request, opened as the
		// person, in the repository that owns the namespace.
		"commit": s.CommitAvailable(),
		// targetCluster: create_agent and the reads take organization and
		// cluster to place an agent on a workload cluster.
		"targetCluster": s.cfg.Compose.TargetMusterURL != "",
	}
	out.Identity = s.kube.Identity()
	kagentAPI := "kagent.dev/" + s.cfg.KagentAPIVersion
	out.APIVersions.AgentTemplate = kagentAPI
	out.APIVersions.Harness = kagentAPI
	out.APIVersions.RemoteMCPServer = kagentAPI
	out.APIVersions.ModelConfig = kagentAPI
	out.APIVersions.HelmRelease = s.cfg.Compose.HelmReleaseAPIVersion
	out.APIVersions.OCIRepository = s.cfg.Compose.OCIRepositoryAPIVersion
	out.Flux.HelmReleaseInterval = orDefault(s.cfg.Compose.HelmReleaseInterval, DefaultHelmReleaseInterval)
	out.Flux.OCIRepositoryInterval = orDefault(s.cfg.Compose.OCIRepositoryInterval, DefaultOCIRepositoryInterval)
	out.Flux.ServiceAccountName = s.cfg.Compose.ServiceAccountName
	out.Harness.Name = s.cfg.Compose.HarnessName
	out.Muster.URL = s.cfg.Compose.MusterURL
	out.Muster.TargetURL = s.cfg.Compose.TargetMusterURL
	if s.skills != nil {
		out.SkillsRepositories = s.skills.Repositories()
	} else {
		out.SkillsRepositories = []string{}
	}
	out.SkillsGitAuthSecretName = s.cfg.Compose.SkillsGitAuthSecretName
	if s.cfg.SkillsBootSecret != nil {
		st := s.cfg.SkillsBootSecret.Status()
		out.SkillsGitAuthMint = &st
	}
	return out
}

// ---- GVRs -----------------------------------------------------------------

func gvrFor(apiVersion, resource string) schema.GroupVersionResource {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		gv = schema.GroupVersion{Version: apiVersion}
	}
	return gv.WithResource(resource)
}

func (s *Service) helmReleaseGVR() schema.GroupVersionResource {
	return gvrFor(s.cfg.Compose.HelmReleaseAPIVersion, "helmreleases")
}

func (s *Service) ociRepositoryGVR() schema.GroupVersionResource {
	return gvrFor(s.cfg.Compose.OCIRepositoryAPIVersion, "ocirepositories")
}

func (s *Service) kagentGVR(resource string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "kagent.dev", Version: s.cfg.KagentAPIVersion, Resource: resource}
}

func (s *Service) templateGVR() schema.GroupVersionResource    { return s.kagentGVR("agenttemplates") }
func (s *Service) mcpServerGVR() schema.GroupVersionResource   { return s.kagentGVR("remotemcpservers") }
func (s *Service) harnessGVR() schema.GroupVersionResource     { return s.kagentGVR("harnesses") }
func (s *Service) modelConfigGVR() schema.GroupVersionResource { return s.kagentGVR("modelconfigs") }

// ---- namespaces -------------------------------------------------------------

// Namespace resolves an optional namespace argument against the managed set.
func (s *Service) Namespace(ns string) (string, error) {
	if ns == "" {
		return s.cfg.DefaultNamespace, nil
	}
	for _, m := range s.cfg.ManagedNamespaces {
		if m == ns {
			return ns, nil
		}
	}
	return "", invalidf("namespace %q is not managed by agent-manager (managed: %s)", ns, strings.Join(s.cfg.ManagedNamespaces, ", "))
}

func (s *Service) dyn(ctx context.Context) (dynamic.Interface, kube.Client, error) {
	c, err := s.kube.Client(ctx)
	if err != nil {
		if errorsIs(err, kube.ErrNoCallerToken) {
			return nil, nil, fmt.Errorf("%w: the request carries no identity token to act with: %v", ErrUnauthenticated, err)
		}
		return nil, nil, fmt.Errorf("kubernetes client: %w", err)
	}
	return c.Dynamic(), c, nil
}

// wrapKube maps API server errors onto the domain sentinels. The API server's
// error stays in the chain, so apierrors.IsConflict still answers on a wrapped
// write error — what the retry around a read-modify-write looks for.
func wrapKube(err error, what string) error {
	switch {
	case err == nil:
		return nil
	case apierrors.IsNotFound(err):
		return fmt.Errorf("%w: %s", ErrNotFound, what)
	case apierrors.IsUnauthorized(err):
		// On a workload cluster: its apiserver does not trust the
		// installation's identity provider.
		return fmt.Errorf("%w: %s: the API server refused the caller's token (401); a workload cluster's apiserver must trust the installation's identity provider (OIDC issuer and client id set when the cluster is created): %w", ErrUnauthenticated, what, err)
	case apierrors.IsForbidden(err):
		return fmt.Errorf("%w: %s: %w", ErrForbidden, what, err)
	case apierrors.IsAlreadyExists(err), apierrors.IsConflict(err):
		return fmt.Errorf("%w: %s: %w", ErrConflict, what, err)
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return fmt.Errorf("%w: %s: %w", ErrInvalid, what, err)
	default:
		return fmt.Errorf("%s: %w", what, err)
	}
}

// ---- reads ------------------------------------------------------------------

// List returns the agents of a namespace: every AgentTemplate (with its
// RemoteMCPServer and its owning HelmRelease when Flux labels name one) plus
// every HelmRelease of the agent chart that has not rendered a template yet.
func (s *Service) List(ctx context.Context, loc Location) ([]Agent, error) {
	st, err := s.site(ctx, loc)
	if err != nil {
		return nil, err
	}
	ns := st.ns()
	templates, err := st.agent.Resource(s.templateGVR()).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, wrapKube(err, "list agenttemplates in "+ns)
	}
	servers, err := st.agent.Resource(s.mcpServerGVR()).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, wrapKube(err, "list remotemcpservers in "+ns)
	}
	serverByName := make(map[string]*unstructured.Unstructured, len(servers.Items))
	for i := range servers.Items {
		serverByName[servers.Items[i].GetName()] = &servers.Items[i]
	}
	hrs, err := s.agentHelmReleases(ctx, st)
	if err != nil {
		return nil, err
	}
	byName := map[string]*Agent{}
	for i := range templates.Items {
		tpl := &templates.Items[i]
		a := s.agentFromTemplate(tpl, serverByName[tpl.GetName()])
		a.Target = st.loc.Target
		if hrName, hrNs := ownerOf(tpl); hrName != "" {
			hr := hrs[hrName]
			if hr == nil || hrNs != st.fluxNS {
				hr, _ = s.getHelmRelease(ctx, st.flux, orDefault(hrNs, st.fluxNS), hrName)
			}
			if hr != nil {
				applyHelmRelease(&a, hr)
				delete(hrs, hrName)
			}
		}
		byName[a.Name] = &a
	}
	for _, hr := range hrs {
		a := Agent{Namespace: ns, Target: st.loc.Target, Managed: ManagedHelmRelease}
		applyHelmRelease(&a, hr)
		if a.Name == "" {
			a.Name = hr.GetName()
		}
		if existing, ok := byName[a.Name]; ok {
			// A HelmRelease whose template carries no provenance labels (an
			// older helm-controller): attach by name.
			if existing.HelmRelease == nil {
				applyHelmRelease(existing, hr)
			}
			continue
		}
		byName[a.Name] = &a
	}
	out := make([]Agent, 0, len(byName))
	for _, a := range byName {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Get returns one agent by name.
func (s *Service) Get(ctx context.Context, loc Location, name string) (*Agent, error) {
	st, err := s.site(ctx, loc)
	if err != nil {
		return nil, err
	}
	return s.get(ctx, st, name)
}

func (s *Service) get(ctx context.Context, st *site, name string) (*Agent, error) {
	tpl, hr, err := s.agentObjects(ctx, st, name)
	if err != nil {
		return nil, err
	}
	var a Agent
	if tpl != nil {
		server, err := s.getMCPServer(ctx, st.agent, st.ns(), name)
		if err != nil {
			return nil, err
		}
		a = s.agentFromTemplate(tpl, server)
	} else {
		a = Agent{Name: name, Namespace: st.ns(), Managed: ManagedNone}
	}
	a.Target = st.loc.Target
	if hr != nil {
		applyHelmRelease(&a, hr)
	}
	return &a, nil
}

// agentObjects reads an agent's AgentTemplate and its owning HelmRelease
// (either may be nil, not both): the release its provenance labels name,
// else the one named after the agent at the site.
func (s *Service) agentObjects(ctx context.Context, st *site, name string) (tpl, hr *unstructured.Unstructured, err error) {
	if tpl, err = s.getTemplate(ctx, st.agent, st.ns(), name); err != nil {
		return nil, nil, err
	}
	hrName, hrNs := st.releaseName(name), st.fluxNS
	if tpl != nil {
		if n, nsFromLabel := ownerOf(tpl); n != "" {
			hrName, hrNs = n, orDefault(nsFromLabel, st.fluxNS)
		}
	}
	if hr, err = s.getHelmRelease(ctx, st.flux, hrNs, hrName); err != nil {
		return nil, nil, err
	}
	if tpl == nil && hr == nil {
		return nil, nil, notFoundf("agent %s: no AgentTemplate and no HelmRelease of that name", st.describe(name))
	}
	return tpl, hr, nil
}

// getTemplate returns nil, nil when the AgentTemplate does not exist.
func (s *Service) getTemplate(ctx context.Context, dyn dynamic.Interface, ns, name string) (*unstructured.Unstructured, error) {
	return s.getObject(ctx, dyn, s.templateGVR(), ns, name, "agenttemplate")
}

// getMCPServer returns the agent's own RemoteMCPServer, nil, nil when absent.
func (s *Service) getMCPServer(ctx context.Context, dyn dynamic.Interface, ns, name string) (*unstructured.Unstructured, error) {
	return s.getObject(ctx, dyn, s.mcpServerGVR(), ns, name, "remotemcpserver")
}

// getHelmRelease returns nil, nil when the HelmRelease does not exist.
func (s *Service) getHelmRelease(ctx context.Context, dyn dynamic.Interface, ns, name string) (*unstructured.Unstructured, error) {
	return s.getObject(ctx, dyn, s.helmReleaseGVR(), ns, name, "helmrelease")
}

func (s *Service) getObject(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, ns, name, what string) (*unstructured.Unstructured, error) {
	obj, err := dyn.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, wrapKube(err, fmt.Sprintf("get %s %s/%s", what, ns, name))
	}
	return obj, nil
}

// agentChartSources returns the names of the OCIRepositories in ns that point
// at the agent chart (the conventional one named after the chart, plus any
// other with the same URL).
func (s *Service) agentChartSources(ctx context.Context, dyn dynamic.Interface, ns string) (map[string]bool, error) {
	names := map[string]bool{s.cfg.Compose.ChartName: true}
	list, err := dyn.Resource(s.ociRepositoryGVR()).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, wrapKube(err, "list ocirepositories in "+ns)
	}
	for _, item := range list.Items {
		url, _, _ := unstructured.NestedString(item.Object, "spec", "url")
		if url == s.cfg.Compose.ChartOCIURL {
			names[item.GetName()] = true
		}
	}
	return names, nil
}

// agentHelmReleases lists the HelmReleases of the site that render the agent
// chart, keyed by name.
func (s *Service) agentHelmReleases(ctx context.Context, st *site) (map[string]*unstructured.Unstructured, error) {
	ns := st.fluxNS
	sources, err := s.agentChartSources(ctx, st.flux, ns)
	if err != nil {
		return nil, err
	}
	list, err := st.flux.Resource(s.helmReleaseGVR()).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, wrapKube(err, "list helmreleases in "+ns)
	}
	out := map[string]*unstructured.Unstructured{}
	for i := range list.Items {
		hr := &list.Items[i]
		ref := chartRefOf(hr)
		if ref == nil || ref.Kind != kindOCIRepository || !sources[ref.Name] {
			continue
		}
		if ref.Namespace != "" && ref.Namespace != ns {
			continue
		}
		if !st.ownsRelease(hr) {
			continue
		}
		out[hr.GetName()] = hr
	}
	return out, nil
}

// ---- model configs ----------------------------------------------------------

// ListModelConfigs lists the kagent ModelConfigs of a namespace, on the
// target cluster when one is named.
func (s *Service) ListModelConfigs(ctx context.Context, loc Location) ([]ModelConfig, error) {
	st, err := s.site(ctx, loc)
	if err != nil {
		return nil, err
	}
	return s.listModelConfigs(ctx, st.agent, st.ns())
}

func (s *Service) listModelConfigs(ctx context.Context, dyn dynamic.Interface, ns string) ([]ModelConfig, error) {
	list, err := dyn.Resource(s.modelConfigGVR()).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, wrapKube(err, "list modelconfigs in "+ns)
	}
	out := make([]ModelConfig, 0, len(list.Items))
	for _, item := range list.Items {
		mc := ModelConfig{Name: item.GetName(), Namespace: item.GetNamespace()}
		mc.Provider, _, _ = unstructured.NestedString(item.Object, "spec", "provider")
		mc.Model, _, _ = unstructured.NestedString(item.Object, "spec", "model")
		conds := conditionsOfObject(&item)
		mc.Accepted = conditionStatus(conds, conditionAccepted)
		if c := findCondition(conds, conditionAccepted); c != nil {
			mc.Message = c.Message
		}
		mc.ManagedBy = item.GetLabels()[ManagedByLabel]
		out = append(out, mc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// requireModelConfig fails with the valid names when name is not a ModelConfig
// of ns.
func (s *Service) requireModelConfig(ctx context.Context, dyn dynamic.Interface, ns, name string) error {
	if strings.TrimSpace(name) == "" {
		return invalidf("modelConfig is required")
	}
	configs, err := s.listModelConfigs(ctx, dyn, ns)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(configs))
	for _, mc := range configs {
		if mc.Name == name {
			return nil
		}
		names = append(names, mc.Name)
	}
	if len(names) == 0 {
		return invalidf("modelConfig %q does not exist in namespace %s (no ModelConfigs there; a platform admin provisions them)", name, ns)
	}
	return invalidf("modelConfig %q does not exist in namespace %s; valid: %s", name, ns, strings.Join(names, ", "))
}

// requireHarness fails, naming the Harnesses that admit agents by
// HarnessLabel, when name is not one of them. Empty is the platform Harness.
func (s *Service) requireHarness(ctx context.Context, dyn dynamic.Interface, ns, name string) error {
	if name == "" {
		return nil
	}
	list, err := dyn.Resource(s.harnessGVR()).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list Harnesses in %s: %w", ns, err)
	}
	var names []string
	for _, h := range list.Items {
		admits, _, _ := unstructured.NestedString(h.Object, "spec", "allowedAgentTemplates", "selector", "matchLabels", HarnessLabel)
		if admits == "" {
			continue
		}
		if admits == name {
			return nil
		}
		names = append(names, admits)
	}
	if len(names) == 0 {
		return invalidf("harness %q does not exist in namespace %s (no Harness there admits agents by %s)", name, ns, HarnessLabel)
	}
	slices.Sort(names)
	return invalidf("harness %q does not exist in namespace %s; valid: %s", name, ns, strings.Join(names, ", "))
}

// ---- skills -------------------------------------------------------------------

// ListSkills discovers skills in the configured (or the given) repository.
func (s *Service) ListSkills(ctx context.Context, repository, ref string, refresh bool) (*skills.Result, error) {
	if s.skills == nil {
		return nil, fmt.Errorf("%w: no skill repositories are configured", ErrUnsupported)
	}
	res, err := s.skills.List(ctx, repository, ref, refresh)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	// A private repository is listed only to a caller who can read it.
	out := &skills.Result{Repositories: []skills.Repository{}, Skills: []skills.Skill{}}
	for _, repo := range res.Repositories {
		if repo.Private {
			if err := s.pinner.RequireReadable(ctx, repo.RepoURL, gitHubLogin(ctx)); err != nil {
				repo = skills.Repository{RepoURL: repo.RepoURL, Private: true, Skills: []skills.Skill{}, Error: err.Error(), FetchedAt: repo.FetchedAt}
			}
		}
		out.Repositories = append(out.Repositories, repo)
		out.Skills = append(out.Skills, repo.Skills...)
	}
	return out, nil
}

// ---- validate ------------------------------------------------------------------

// checkSpec runs the request-level checks of a create that need no cluster.
func checkSpec(spec Spec) []error {
	var errs []error
	if err := ValidateName(spec.Name); err != nil {
		errs = append(errs, err)
	}
	if err := ValidateToolset(spec.Toolset, true); err != nil {
		errs = append(errs, err)
	}
	if err := ValidateSkills(spec.Skills); err != nil {
		errs = append(errs, err)
	}
	if err := ValidateSystemMessage(spec.SystemMessage); err != nil {
		errs = append(errs, err)
	}
	if err := spec.Validate(); err != nil {
		errs = append(errs, err)
	} else if spec.IsSet() {
		if err := spec.ValidateReleaseName(spec.Name); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// ValidateCreate is create_agent without the write.
func (s *Service) ValidateCreate(ctx context.Context, spec Spec) (*ValidateResult, error) {
	if err := rejectRemoved(spec.removed()...); err != nil {
		return nil, err
	}
	st, err := s.site(ctx, spec.Location)
	if err != nil {
		return nil, err
	}
	ns := st.ns()
	spec.Namespace = ns
	res := &ValidateResult{Mode: "create"}
	for _, e := range checkSpec(spec) {
		res.addError(e)
	}
	if err := s.requireTargetMuster(st); err != nil {
		res.addError(err)
	}
	if err := s.requireModelConfig(ctx, st.agent, ns, spec.ModelConfig); err != nil {
		if !isDomainError(err) {
			return nil, err
		}
		res.addError(err)
	}
	if err := s.requireHarness(ctx, st.agent, ns, spec.Harness); err != nil {
		if !isDomainError(err) {
			return nil, err
		}
		res.addError(err)
	}
	if err := s.requireFreeName(ctx, st, spec.Name); errorsIs(err, ErrConflict) {
		res.addError(err)
	} else if err != nil {
		// A read the caller may not make, or a transient one, leaves the
		// name unchecked; create_agent still refuses a taken name.
		res.Notes = append(res.Notes, fmt.Sprintf("the name %q could not be checked for a clash: %v", spec.Name, err))
	}
	if err := requireReadableSkills(ctx, s.pinner, spec.Skills); err != nil {
		res.addError(err)
	}
	if pinned, err := pinSkills(ctx, s.pinner, spec.Skills, false); err != nil {
		res.addError(err)
	} else {
		spec.Skills = pinned
	}
	values := BuildValues(spec, st.compose)
	sch, violations := ValidateValues(ctx, s.chart, values)
	res.Errors = append(res.Errors, violations...)
	res.SchemaVersion, res.SchemaSource = sch.Version, sch.Source
	res.Manifests = st.manifests(spec.Name, values)
	res.Valid = len(res.Errors) == 0
	return res, nil
}

// requireFreeName fails when a HelmRelease or an AgentTemplate of that name
// exists.
func (s *Service) requireFreeName(ctx context.Context, st *site, name string) error {
	if name == "" {
		return nil
	}
	if hr, err := s.getHelmRelease(ctx, st.flux, st.fluxNS, st.releaseName(name)); err != nil {
		return err
	} else if hr != nil {
		return conflictf("HelmRelease %s/%s already exists; use update_agent to change it", st.fluxNS, st.releaseName(name))
	}
	if tpl, err := s.getTemplate(ctx, st.agent, st.ns(), name); err != nil {
		return err
	} else if tpl != nil {
		return conflictf("AgentTemplate %s already exists without a HelmRelease (a bare template); delete it first (delete_agent with force) or pick another name", st.describe(name))
	}
	return nil
}

// requireTargetMuster refuses an agent on a workload cluster when the
// installation names no muster URL those clusters reach: the in-cluster one
// resolves on the installation only.
func (s *Service) requireTargetMuster(st *site) error {
	if st.loc.IsSet() && st.compose.MusterURL == "" {
		return invalidf("this installation has no muster URL for agents on workload clusters (agent-manager's targetMusterURL); an agent on cluster %s could not reach muster", st.loc.Cluster)
	}
	return nil
}

// ValidateUpdate is update_agent without the write.
func (s *Service) ValidateUpdate(ctx context.Context, upd Update) (*ValidateResult, error) {
	if err := rejectRemoved(upd.removed()...); err != nil {
		return nil, err
	}
	st, err := s.site(ctx, upd.Location)
	if err != nil {
		return nil, err
	}
	hr, _, err := s.writableHelmRelease(ctx, st, upd.Name, upd.Force, upd.Force)
	if err != nil {
		return nil, err
	}
	res := &ValidateResult{Mode: "update"}
	values, _, err := s.mergedValues(ctx, st, upd, hr)
	if err != nil {
		if !isDomainError(err) {
			return nil, err
		}
		res.addError(err)
	}
	sch, violations := ValidateValues(ctx, s.chart, values)
	res.Errors = append(res.Errors, violations...)
	res.SchemaVersion, res.SchemaSource = sch.Version, sch.Source
	res.Manifests = st.manifests(upd.Name, values)
	res.Valid = len(res.Errors) == 0
	return res, nil
}

func (r *ValidateResult) addError(err error) {
	r.Errors = append(r.Errors, strings.TrimPrefix(strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": "), ErrConflict.Error()+": "))
}

func isDomainError(err error) bool {
	for _, sentinel := range []error{ErrInvalid, ErrNotFound, ErrConflict, ErrGitOpsOwned, ErrUnsupported} {
		if errorsIs(err, sentinel) {
			return true
		}
	}
	return false
}

// ---- create --------------------------------------------------------------------

// Create composes and applies the OCIRepository (when missing) and the
// HelmRelease of a new agent after pinning its skills and validating the
// values against the chart schema. Nothing is written when a check fails, or
// on a dry run. In commit mode the two land as files of a pull request in the
// repository that owns the namespace instead.
func (s *Service) Create(ctx context.Context, spec Spec) (*CreateResult, error) {
	ns, err := s.Namespace(spec.Namespace)
	if err != nil {
		return nil, err
	}
	spec.Namespace = ns
	if err := rejectRemoved(spec.removed()...); err != nil {
		return nil, err
	}
	mode, err := s.checkMode(spec.WriteOptions)
	if err != nil {
		return nil, err
	}
	if errs := checkSpec(spec); len(errs) > 0 {
		return nil, errs[0]
	}
	st, err := s.site(ctx, spec.Location)
	if err != nil {
		return nil, err
	}
	if err := s.requireTargetMuster(st); err != nil {
		return nil, err
	}
	if err := s.requireFreeName(ctx, st, spec.Name); err != nil {
		return nil, err
	}
	if err := s.requireModelConfig(ctx, st.agent, ns, spec.ModelConfig); err != nil {
		return nil, err
	}
	if err := s.requireHarness(ctx, st.agent, ns, spec.Harness); err != nil {
		return nil, err
	}
	if err := requireReadableSkills(ctx, s.pinner, spec.Skills); err != nil {
		return nil, err
	}
	if spec.Skills, err = pinSkills(ctx, s.pinner, spec.Skills, false); err != nil {
		return nil, err
	}
	values := BuildValues(spec, st.compose)
	sch, violations := ValidateValues(ctx, s.chart, values)
	if len(violations) > 0 {
		return nil, invalidf("values do not satisfy the agent chart schema %s (%s): %s", sch.Version, sch.Source, strings.Join(violations, "; "))
	}

	res := &CreateResult{RequestedBy: identity.Caller(ctx), WriteOutcome: WriteOutcome{Mode: mode, DryRun: spec.DryRun}}
	res.Manifests = st.manifests(spec.Name, values)
	if mode == ModeCommit {
		return s.createCommit(ctx, st, spec, values, res)
	}

	// The chart source is shared per namespace: create it once, reuse it after.
	fluxNS := st.fluxNS
	ociRepo := BuildOCIRepository(fluxNS, st.compose)
	existing, err := st.flux.Resource(s.ociRepositoryGVR()).Namespace(fluxNS).Get(ctx, ociRepo.GetName(), metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		if !spec.DryRun {
			if _, err := st.flux.Resource(s.ociRepositoryGVR()).Namespace(fluxNS).Create(ctx, ociRepo, metav1.CreateOptions{FieldManager: FieldManager}); err != nil {
				return nil, wrapKube(err, fmt.Sprintf("create OCIRepository %s/%s", fluxNS, ociRepo.GetName()))
			}
		}
		res.Created.OCIRepository = true
	case err != nil:
		return nil, wrapKube(err, fmt.Sprintf("get OCIRepository %s/%s", fluxNS, ociRepo.GetName()))
	default:
		if url, _, _ := unstructured.NestedString(existing.Object, "spec", "url"); url != s.cfg.Compose.ChartOCIURL {
			s.log.Warn("reusing an OCIRepository that points elsewhere", "namespace", fluxNS, "name", ociRepo.GetName(), "url", url, "expected", s.cfg.Compose.ChartOCIURL)
		}
		semver, _, _ := unstructured.NestedString(existing.Object, "spec", "ref", "semver")
		filter, _, _ := unstructured.NestedString(existing.Object, "spec", "ref", "semverFilter")
		if semver != s.cfg.Compose.ChartSemver || filter != s.cfg.Compose.ChartSemverFilter {
			s.log.Warn("reusing an OCIRepository on another range; the migrate command moves it", "namespace", fluxNS, "name", ociRepo.GetName(),
				"semver", semver, "semverFilter", filter, "expected", s.cfg.Compose.ChartSemver, "expectedSemverFilter", s.cfg.Compose.ChartSemverFilter)
		}
	}

	hr := st.helmRelease(spec.Name, values)
	if spec.DryRun {
		res.Created.HelmRelease = true
		res.Agent = Agent{Name: spec.Name, Namespace: ns, Target: spec.Target, Managed: ManagedHelmRelease}
		applyHelmRelease(&res.Agent, hr)
		return res, nil
	}
	created, err := st.flux.Resource(s.helmReleaseGVR()).Namespace(fluxNS).Create(ctx, hr, metav1.CreateOptions{FieldManager: FieldManager})
	if err != nil {
		return nil, wrapKube(err, fmt.Sprintf("create HelmRelease %s/%s", fluxNS, hr.GetName()))
	}
	res.Created.HelmRelease = true
	agent := Agent{Name: spec.Name, Namespace: ns, Target: spec.Target, Managed: ManagedHelmRelease}
	applyHelmRelease(&agent, created)
	res.Agent = agent
	if status, err := s.status(ctx, st, spec.Name); err == nil {
		res.Status = status
	}
	s.log.Info("agent created", identity.LogAttr(ctx), "namespace", ns, "cluster", spec.Cluster, "name", spec.Name, "modelConfig", spec.ModelConfig, "skills", len(spec.Skills), "ociRepositoryCreated", res.Created.OCIRepository)
	return res, nil
}

// createCommit is Create in commit mode: the HelmRelease as a file of
// agent-manager's directory in the repository that owns the namespace, with
// the shared chart source beside it when the namespace has none yet (or the
// directory carries it already), in one pull request as the caller. Nothing
// is written live.
func (s *Service) createCommit(ctx context.Context, st *site, spec Spec, values map[string]any, res *CreateResult) (*CreateResult, error) {
	ns, dyn := st.fluxNS, st.flux
	if err := s.requireOwnFile(st.releaseName(spec.Name)); err != nil {
		return nil, err
	}
	loc, err := s.namespaceLocation(ctx, st, spec.WriteOptions)
	if err != nil {
		return nil, err
	}
	c, err := s.newCommit(ctx, loc)
	if err != nil {
		return nil, err
	}
	write := map[string][]byte{c.file("HelmRelease", st.releaseName(spec.Name)): []byte(res.Manifests.HelmRelease)}
	source, err := s.getObject(ctx, dyn, s.ociRepositoryGVR(), ns, s.cfg.Compose.ChartName, "ocirepository")
	if err != nil {
		return nil, err
	}
	sourceFile := c.file(kindOCIRepository, s.cfg.Compose.ChartName)
	inDirectory, err := c.exists(ctx, sourceFile)
	if err != nil {
		return nil, err
	}
	if source == nil || inDirectory {
		write[sourceFile] = []byte(res.Manifests.OCIRepository)
	}
	res.Created.OCIRepository = source == nil
	res.Created.HelmRelease = true
	title := fmt.Sprintf("feat(agents): add agent %s in %s", spec.Name, ns)
	body := fmt.Sprintf("Adds the agent `%s` to namespace `%s`: a HelmRelease of the agent chart (`%s`, range `%s`) with model config `%s` and toolset `%s`. Flux applies it after the merge, and the agent's Harness compiles it.",
		spec.Name, ns, s.cfg.Compose.ChartOCIURL, s.cfg.Compose.ChartSemver, spec.ModelConfig, strings.Join(spec.Toolset, ", "))
	if res.Commit, err = c.open(ctx, write, nil, commitBranch("create", ns, spec.Name), title, body, spec.DryRun); err != nil {
		return nil, err
	}
	res.Agent = Agent{Name: spec.Name, Namespace: st.ns(), Target: spec.Target}
	applyHelmRelease(&res.Agent, st.helmRelease(spec.Name, values))
	res.Agent.Managed = ManagedGitOps
	s.log.Info("agent create committed", identity.LogAttr(ctx), "namespace", ns, "name", spec.Name, "repository", res.Commit.Repository, "pullRequest", res.Commit.PullRequest, "dryRun", spec.DryRun)
	return res, nil
}

// ---- update --------------------------------------------------------------------

// writableHelmRelease fetches the HelmRelease an update or delete targets and
// applies the ownership rules: a bare AgentTemplate has nothing to write to; a
// suspended release is refused unless force; a GitOps-owned one is refused
// with gitops_owned, naming mode commit, unless allowGitOps (commit mode,
// which changes it in git).
func (s *Service) writableHelmRelease(ctx context.Context, st *site, name string, force, allowGitOps bool) (*unstructured.Unstructured, *unstructured.Unstructured, error) {
	tpl, hr, err := s.agentObjects(ctx, st, name)
	if errorsIs(err, ErrNotFound) {
		return nil, nil, notFoundf("agent %s", st.describe(name))
	}
	if err != nil {
		return nil, nil, err
	}
	if hr == nil {
		return nil, tpl, conflictf("AgentTemplate %s is a bare template with no HelmRelease behind it; agent-manager only writes HelmRelease values (recreate it with create_agent, or delete it with force)", st.describe(name))
	}
	if gitOpsOwnedHR(hr) && !allowGitOps {
		return nil, tpl, s.gitOpsRefusal(ctx, st.flux, hr)
	}
	if !force {
		if suspended, _, _ := unstructured.NestedBool(hr.Object, "spec", "suspend"); suspended {
			return nil, tpl, conflictf("HelmRelease %s/%s is suspended: Flux will not act on a change. Resume it first, or pass force", hr.GetNamespace(), hr.GetName())
		}
	}
	return hr, tpl, nil
}

// mergedValues applies an Update to the release's current values and returns
// (after, before). Skills given replace the list and are pinned; refreshSkills
// re-pins every git skill to the head of its ref (the default branch unless
// the request names one).
// mergeSkillsGitAuth keeps the skills credential in step with the merged
// skills: the caller's Secret when it names one (empty: back to the
// installation's), otherwise the release's own, otherwise the installation's —
// so an update of an agent written before the credential existed carries it
// from then on. Without a git skill the value is dropped.
func mergeSkillsGitAuth(values map[string]any, own *string, cfg ComposeConfig) {
	current, _, _ := unstructured.NestedString(values, SkillsGitAuthValuesKey, "name")
	if own != nil {
		current = *own
	}
	if name := skillsGitAuth(current, skillsFromValues(values), cfg); name != "" {
		values[SkillsGitAuthValuesKey] = map[string]any{"name": name}
	} else {
		delete(values, SkillsGitAuthValuesKey)
	}
}

func (s *Service) mergedValues(ctx context.Context, st *site, upd Update, hr *unstructured.Unstructured) (map[string]any, map[string]any, error) {
	if upd.SystemMessage != nil {
		if err := ValidateSystemMessage(*upd.SystemMessage); err != nil {
			return nil, nil, err
		}
	}
	current, _, _ := unstructured.NestedMap(hr.Object, "spec", "values")
	if current == nil {
		current = map[string]any{}
	}
	before := runtime.DeepCopyJSON(current)
	after := runtime.DeepCopyJSON(current)
	agentBlock, _ := after["agent"].(map[string]any)
	if agentBlock == nil {
		agentBlock = map[string]any{}
	}
	if _, ok := agentBlock["name"]; !ok {
		agentBlock["name"] = upd.Name
	}
	setOrDelete(agentBlock, "displayName", upd.DisplayName)
	setOrDelete(agentBlock, "description", upd.Description)
	setOrDelete(agentBlock, "systemMessage", upd.SystemMessage)
	setOrDelete(agentBlock, "iconUrl", upd.IconURL)
	after["agent"] = agentBlock

	var firstErr error
	if upd.ModelConfig != nil {
		if err := s.requireModelConfig(ctx, st.agent, st.ns(), *upd.ModelConfig); err != nil {
			firstErr = err
		}
		after["modelConfig"] = map[string]any{"name": *upd.ModelConfig}
	}
	if upd.Skills != nil || upd.RefreshSkills {
		list := skillsFromValues(after)
		if upd.Skills != nil {
			list = *upd.Skills
			if err := ValidateSkills(list); err != nil {
				return after, before, err
			}
			if err := requireReadableSkills(ctx, s.pinner, list); err != nil {
				return after, before, err
			}
		}
		pinned, err := pinSkills(ctx, s.pinner, list, upd.RefreshSkills)
		if err != nil {
			return after, before, err
		}
		if sk := skillsValues(pinned); sk != nil {
			after["skills"] = sk
		} else {
			delete(after, "skills")
		}
	}
	mergeSkillsGitAuth(after, upd.GitAuthSecretName, st.compose)
	if upd.Toolset != nil {
		// Replaces the whole list; an empty list is refused (preset:none is
		// the way to say "no tools"), so a toolset can never be cleared back
		// to implicit full access.
		if err := ValidateToolset(*upd.Toolset, false); err != nil {
			return after, before, err
		}
		after[ToolsetValuesKey] = toAnySlice(*upd.Toolset)
	}
	if upd.Labels != nil {
		if len(*upd.Labels) > 0 {
			after["labels"] = toAnyMap(*upd.Labels)
		} else {
			delete(after, "labels")
		}
	}
	if upd.Annotations != nil {
		if len(*upd.Annotations) > 0 {
			after["annotations"] = toAnyMap(*upd.Annotations)
		} else {
			delete(after, "annotations")
		}
	}
	return after, before, firstErr
}

func setOrDelete(m map[string]any, key string, v *string) {
	if v == nil {
		return
	}
	if strings.TrimSpace(*v) == "" {
		delete(m, key)
		return
	}
	m[key] = *v
}

// Update merges the change into the HelmRelease values, validates the result
// against the chart schema and writes it. The rendered objects are never
// touched: helm-controller renders the change.
//
// The write is a read-modify-write, retried when the API server answers
// Conflict: helm-controller writes the release's status while it reconciles
// the previous change, so an update that follows another closely reads a
// resourceVersion that is stale by the time it writes — nothing the caller
// asked for is in conflict. Every attempt reads the release again and merges
// into its latest values; a Conflict that outlasts the attempts is reported.
// The refusals of writableHelmRelease are conflicts of the domain, not of the
// API server, and end the attempts at once.
func (s *Service) Update(ctx context.Context, upd Update) (*UpdateResult, error) {
	if err := ValidateName(upd.Name); err != nil {
		return nil, err
	}
	if err := rejectRemoved(upd.removed()...); err != nil {
		return nil, err
	}
	var err error
	if upd.Mode, err = s.checkMode(upd.WriteOptions); err != nil {
		return nil, err
	}
	st, err := s.site(ctx, upd.Location)
	if err != nil {
		return nil, err
	}
	var res *UpdateResult
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var err error
		res, err = s.update(ctx, st, upd)
		return err
	}); err != nil {
		return nil, err
	}
	return res, nil
}

// update is one attempt of Update: read the release, merge, validate, write.
func (s *Service) update(ctx context.Context, st *site, upd Update) (*UpdateResult, error) {
	ns := st.ns()
	hr, _, err := s.writableHelmRelease(ctx, st, upd.Name, upd.Force, upd.Mode == ModeCommit)
	if err != nil {
		return nil, err
	}
	after, before, err := s.mergedValues(ctx, st, upd, hr)
	if err != nil {
		return nil, err
	}
	sch, violations := ValidateValues(ctx, s.chart, after)
	if len(violations) > 0 {
		return nil, invalidf("values do not satisfy the agent chart schema %s (%s): %s", sch.Version, sch.Source, strings.Join(violations, "; "))
	}
	changed := changedPaths("", before, after)
	if changed == nil {
		changed = []string{}
	}
	res := &UpdateResult{Before: before, After: after, Changed: changed, RequestedBy: identity.Caller(ctx), WriteOutcome: WriteOutcome{Mode: upd.Mode, DryRun: upd.DryRun}}
	res.Manifests = st.manifests(upd.Name, after)
	if upd.Mode == ModeCommit {
		return s.updateCommit(ctx, st, upd, hr, res)
	}
	if len(changed) == 0 || upd.DryRun {
		agent, err := s.get(ctx, st, upd.Name)
		if err != nil {
			return nil, err
		}
		res.Agent = *agent
		return res, nil
	}
	if err := unstructured.SetNestedMap(hr.Object, after, "spec", "values"); err != nil {
		return nil, fmt.Errorf("set values: %w", err)
	}
	updated, err := st.flux.Resource(s.helmReleaseGVR()).Namespace(hr.GetNamespace()).Update(ctx, hr, metav1.UpdateOptions{FieldManager: FieldManager})
	if err != nil {
		if apierrors.IsConflict(err) {
			s.log.Debug("HelmRelease moved between read and write", identity.LogAttr(ctx), "namespace", hr.GetNamespace(), "name", hr.GetName(), "resourceVersion", hr.GetResourceVersion())
		}
		return nil, wrapKube(err, fmt.Sprintf("update HelmRelease %s/%s", hr.GetNamespace(), hr.GetName()))
	}
	agent, err := s.get(ctx, st, upd.Name)
	if err != nil {
		agent = &Agent{Name: upd.Name, Namespace: ns, Target: st.loc.Target, Managed: ManagedHelmRelease}
		applyHelmRelease(agent, updated)
	}
	res.Agent = *agent
	s.log.Info("agent updated", identity.LogAttr(ctx), "namespace", ns, "name", upd.Name, "changed", changed, "refreshSkills", upd.RefreshSkills)
	return res, nil
}

// updateCommit is Update in commit mode: the release's file in
// agent-manager's directory rewritten with the merged values, in one pull
// request as the caller; a release defined elsewhere in the repository is
// refused. Nothing is written live.
func (s *Service) updateCommit(ctx context.Context, st *site, upd Update, hr *unstructured.Unstructured, res *UpdateResult) (*UpdateResult, error) {
	ns := st.ns()
	loc, err := s.releaseLocation(ctx, st.flux, hr, upd.WriteOptions)
	if err != nil {
		return nil, err
	}
	c, err := s.newCommit(ctx, loc)
	if err != nil {
		return nil, err
	}
	p, err := c.requireReleaseFile(ctx, hr)
	if err != nil {
		return nil, err
	}
	title := fmt.Sprintf("chore(agents): update agent %s in %s", upd.Name, ns)
	body := fmt.Sprintf("Updates the agent `%s` in namespace `%s`: %s.", upd.Name, ns, strings.Join(res.Changed, ", "))
	if res.Commit, err = c.open(ctx, map[string][]byte{p: []byte(res.Manifests.HelmRelease)}, nil, commitBranch("update", ns, upd.Name), title, body, upd.DryRun); err != nil {
		return nil, err
	}
	agent, err := s.get(ctx, st, upd.Name)
	if err != nil {
		return nil, err
	}
	res.Agent = *agent
	s.log.Info("agent update committed", identity.LogAttr(ctx), "namespace", ns, "name", upd.Name, "changed", res.Changed, "repository", res.Commit.Repository, "pullRequest", res.Commit.PullRequest, "dryRun", upd.DryRun)
	return res, nil
}

// changedPaths lists the dotted value paths whose leaves differ.
func changedPaths(prefix string, before, after map[string]any) []string {
	keys := map[string]bool{}
	for k := range before {
		keys[k] = true
	}
	for k := range after {
		keys[k] = true
	}
	var out []string
	for k := range keys {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		b, bOK := before[k]
		a, aOK := after[k]
		bm, bIsMap := b.(map[string]any)
		am, aIsMap := a.(map[string]any)
		switch {
		case bIsMap && aIsMap:
			out = append(out, changedPaths(path, bm, am)...)
		case aIsMap && !bOK:
			// A whole block appeared: report its leaves.
			out = append(out, changedPaths(path, map[string]any{}, am)...)
		case bIsMap && !aOK:
			out = append(out, changedPaths(path, bm, map[string]any{})...)
		case bOK != aOK || fmt.Sprint(b) != fmt.Sprint(a):
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

// ---- delete --------------------------------------------------------------------

// Delete removes the agent's HelmRelease (helm-controller uninstalls the
// rendered AgentTemplate and RemoteMCPServer) and the shared OCIRepository
// when no other release references it. A bare AgentTemplate is only deleted
// with force. A dry run reports what would go and deletes nothing; in commit
// mode the files are removed with a pull request instead.
func (s *Service) Delete(ctx context.Context, loc Location, name string, force bool, w WriteOptions) (*DeleteResult, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	mode, err := s.checkMode(w)
	if err != nil {
		return nil, err
	}
	st, err := s.site(ctx, loc)
	if err != nil {
		return nil, err
	}
	ns, dyn := st.ns(), st.flux
	res := &DeleteResult{Name: name, Namespace: ns, Target: st.loc.Target, RequestedBy: identity.Caller(ctx), WriteOutcome: WriteOutcome{Mode: mode, DryRun: w.DryRun}}
	hr, tpl, err := s.writableHelmRelease(ctx, st, name, force, mode == ModeCommit)
	if err != nil {
		if mode == ModeApply && hr == nil && tpl != nil && force && errorsIs(err, ErrConflict) {
			// A bare AgentTemplate, forced: delete the template itself.
			if err := s.deleteRendered(ctx, st.agent, ns, name, res, false, w.DryRun); err != nil {
				return nil, err
			}
			s.log.Info("bare agent template deleted", identity.LogAttr(ctx), "namespace", ns, "name", name, "dryRun", w.DryRun)
			return res, nil
		}
		return nil, err
	}
	if mode == ModeCommit {
		return s.deleteCommit(ctx, dyn, hr, w, res)
	}
	suspended, _, _ := unstructured.NestedBool(hr.Object, "spec", "suspend")
	if !w.DryRun {
		if err := dyn.Resource(s.helmReleaseGVR()).Namespace(hr.GetNamespace()).Delete(ctx, hr.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return nil, wrapKube(err, fmt.Sprintf("delete HelmRelease %s/%s", hr.GetNamespace(), hr.GetName()))
		}
	}
	res.HelmReleaseDeleted = true
	if suspended && force && tpl != nil {
		// Flux drops the finalizer of a suspended release without uninstalling:
		// the rendered objects would stay behind, so remove them directly.
		if err := s.deleteRendered(ctx, st.agent, ns, name, res, true, w.DryRun); err != nil {
			return nil, err
		}
	}

	// Best-effort cleanup of the shared chart source: every uncertainty keeps
	// it (an orphan is inert; a wrongly deleted one breaks every other agent).
	source, kept := s.sourceToRemove(ctx, dyn, hr)
	if source == "" {
		res.OCIRepositoryKept = kept
		return res, nil
	}
	if w.DryRun {
		res.OCIRepositoryDeleted = true
		return res, nil
	}
	sourceNs := orDefault(chartRefOf(hr).Namespace, hr.GetNamespace())
	if err := dyn.Resource(s.ociRepositoryGVR()).Namespace(sourceNs).Delete(ctx, source, metav1.DeleteOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			res.OCIRepositoryKept = "no OCIRepository to delete"
		} else {
			res.OCIRepositoryKept = "delete refused: " + err.Error()
		}
		return res, nil
	}
	res.OCIRepositoryDeleted = true
	s.log.Info("agent deleted", identity.LogAttr(ctx), "namespace", ns, "name", name, "ociRepositoryDeleted", true)
	return res, nil
}

// sourceToRemove is the shared chart source a delete of hr takes with it —
// the OCIRepository it renders from when no other release of its namespace
// references it — or, empty, why it stays.
func (s *Service) sourceToRemove(ctx context.Context, dyn dynamic.Interface, hr *unstructured.Unstructured) (name, kept string) {
	ref := chartRefOf(hr)
	if ref == nil || ref.Kind != kindOCIRepository {
		return "", "the HelmRelease does not render from an OCIRepository"
	}
	others, err := s.otherReferences(ctx, dyn, orDefault(ref.Namespace, hr.GetNamespace()), ref.Name, hr.GetName())
	if err != nil {
		return "", "could not list the other HelmReleases of the namespace: " + err.Error()
	}
	if len(others) > 0 {
		return "", fmt.Sprintf("still referenced by %d other HelmRelease(s): %s", len(others), strings.Join(others, ", "))
	}
	return ref.Name, ""
}

// deleteCommit is Delete in commit mode: the release's file removed from
// agent-manager's directory, with the chart source's file when no other
// release references it, in one pull request as the caller. Nothing is
// deleted live; a Kustomization that does not prune leaves the release after
// the merge, which the answer names.
func (s *Service) deleteCommit(ctx context.Context, dyn dynamic.Interface, hr *unstructured.Unstructured, w WriteOptions, res *DeleteResult) (*DeleteResult, error) {
	loc, err := s.releaseLocation(ctx, dyn, hr, w)
	if err != nil {
		return nil, err
	}
	c, err := s.newCommit(ctx, loc)
	if err != nil {
		return nil, err
	}
	p, err := c.requireReleaseFile(ctx, hr)
	if err != nil {
		return nil, err
	}
	remove := []string{p}
	source, kept := s.sourceToRemove(ctx, dyn, hr)
	if source != "" {
		remove = append(remove, c.file(kindOCIRepository, source))
	} else {
		res.OCIRepositoryKept = kept
	}
	ns, name := hr.GetNamespace(), hr.GetName()
	title := fmt.Sprintf("feat(agents): remove agent %s in %s", name, ns)
	body := fmt.Sprintf("Removes the agent `%s` from namespace `%s`: its HelmRelease, and with it the AgentTemplate and the RemoteMCPServer it renders.", name, ns)
	if res.Commit, err = c.open(ctx, nil, remove, commitBranch("delete", ns, name), title, body, w.DryRun); err != nil {
		return nil, err
	}
	if !loc.prune && loc.kustomization != "" {
		res.Commit.LiveSteps = append(res.Commit.LiveSteps, fmt.Sprintf("Kustomization %s does not prune: after the merge, delete HelmRelease %s/%s by hand (kubectl delete helmrelease -n %s %s)", loc.kustomization, ns, name, ns, name))
	}
	s.log.Info("agent delete committed", identity.LogAttr(ctx), "namespace", ns, "name", name, "repository", res.Commit.Repository, "pullRequest", res.Commit.PullRequest, "dryRun", w.DryRun)
	return res, nil
}

// deleteRendered deletes the AgentTemplate named after the agent and, when
// withServer, the RemoteMCPServer of the same name that the same release
// rendered (its provenance labels name the release).
func (s *Service) deleteRendered(ctx context.Context, dyn dynamic.Interface, ns, name string, res *DeleteResult, withServer, dryRun bool) error {
	if !dryRun {
		if err := dyn.Resource(s.templateGVR()).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return wrapKube(err, fmt.Sprintf("delete AgentTemplate %s/%s", ns, name))
		}
	}
	res.AgentTemplateDeleted = true
	if !withServer {
		return nil
	}
	server, err := s.getMCPServer(ctx, dyn, ns, name)
	if err != nil || server == nil {
		return err
	}
	if owner, _ := ownerOf(server); owner == "" {
		// Not rendered by a release: somebody else's server of that name.
		return nil
	}
	if !dryRun {
		if err := dyn.Resource(s.mcpServerGVR()).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return wrapKube(err, fmt.Sprintf("delete RemoteMCPServer %s/%s", ns, name))
		}
	}
	res.RemoteMCPServerDeleted = true
	return nil
}

// otherReferences lists the HelmReleases of ns, other than self, whose chartRef
// is the OCIRepository source.
func (s *Service) otherReferences(ctx context.Context, dyn dynamic.Interface, ns, source, self string) ([]string, error) {
	list, err := dyn.Resource(s.helmReleaseGVR()).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var out []string
	for i := range list.Items {
		hr := &list.Items[i]
		if hr.GetName() == self {
			continue
		}
		ref := chartRefOf(hr)
		if ref != nil && ref.Kind == kindOCIRepository && ref.Name == source && orDefault(ref.Namespace, ns) == ns {
			out = append(out, hr.GetName())
		}
	}
	sort.Strings(out)
	return out, nil
}

// ---- read helpers --------------------------------------------------------------

// ownerOf reads the Flux provenance labels of an object rendered by a release.
func ownerOf(obj *unstructured.Unstructured) (name, namespace string) {
	labels := obj.GetLabels()
	return labels[HelmReleaseNameLabel], labels[HelmReleaseNamespaceLabel]
}

func gitOpsOwnedHR(hr *unstructured.Unstructured) bool {
	_, ok := hr.GetLabels()[KustomizationNameLabel]
	return ok
}

func chartRefOf(hr *unstructured.Unstructured) *ChartRef {
	ref, found, _ := unstructured.NestedMap(hr.Object, "spec", "chartRef")
	if !found {
		return nil
	}
	out := &ChartRef{}
	out.Kind, _ = ref["kind"].(string)
	out.Name, _ = ref["name"].(string)
	out.Namespace, _ = ref["namespace"].(string)
	return out
}

// agentFromTemplate reads the AgentTemplate and the agent's RemoteMCPServer
// (nil when absent) into the view.
func (s *Service) agentFromTemplate(tpl, server *unstructured.Unstructured) Agent {
	a := Agent{Name: tpl.GetName(), Namespace: tpl.GetNamespace(), Exists: true, Managed: ManagedNone}
	a.DisplayName = tpl.GetAnnotations()[DisplayNameAnnotation]
	a.IconURL = tpl.GetAnnotations()[IconURLAnnotation]
	a.Description, _, _ = unstructured.NestedString(tpl.Object, "spec", "description")
	a.ModelConfig, _, _ = unstructured.NestedString(tpl.Object, "spec", "modelConfig", "name")
	a.SystemMessage, _, _ = unstructured.NestedString(tpl.Object, "spec", "systemPrompt")
	a.Skills = skillsFromTemplate(tpl)
	a.Tools = toolBindingsOf(tpl)
	a.Toolset, a.ImplicitFullAccess = toolsetOf(tpl, server)
	ts := templateStatusOf(tpl)
	a.Harnesses = ts.Harnesses
	a.Ready = harnessReady(ts.Harnesses, s.harnessOf(tpl))
	return a
}

// toolBindingsOf reads spec.tools[].mcp.
func toolBindingsOf(tpl *unstructured.Unstructured) []ToolBinding {
	tools, _, _ := unstructured.NestedSlice(tpl.Object, "spec", "tools")
	var out []ToolBinding
	for _, t := range tools {
		m, _ := t.(map[string]any)
		mcp, ok := m["mcp"].(map[string]any)
		if !ok {
			continue
		}
		server, _ := mcp["server"].(map[string]any)
		b := ToolBinding{Tools: stringSlice(mcp["tools"])}
		b.Server, _ = server["name"].(string)
		out = append(out, b)
	}
	return out
}

// toolsetOf reads the toolset a template carries through its own
// RemoteMCPServer: the X-Muster-Toolset header (spec.headersFrom[]). A bound
// server without the header means implicit full access; no binding to the
// agent's own server means neither (a preset:none agent binds nothing).
func toolsetOf(tpl, server *unstructured.Unstructured) (toolset []string, implicitFullAccess bool) {
	bound := false
	for _, b := range toolBindingsOf(tpl) {
		if b.Server == tpl.GetName() {
			bound = true
		}
	}
	if !bound {
		return nil, false
	}
	if server != nil {
		if value := headerOf(server, ToolsetHeader); value != "" {
			return ParseToolsetHeader(value), false
		}
	}
	return nil, true
}

// headerOf reads a static header value from a RemoteMCPServer's headersFrom.
func headerOf(server *unstructured.Unstructured, name string) string {
	headers, _, _ := unstructured.NestedSlice(server.Object, "spec", "headersFrom")
	for _, h := range headers {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if n, _ := hm["name"].(string); n == name {
			value, _ := hm["value"].(string)
			return value
		}
	}
	return ""
}

// applyHelmRelease folds the owning release into the view and, when the
// template is absent, reads the identity fields from the values.
func applyHelmRelease(a *Agent, hr *unstructured.Unstructured) {
	ref := &HelmReleaseRef{Name: hr.GetName(), Namespace: hr.GetNamespace()}
	conds := conditionsOfObject(hr)
	ref.Ready = conditionStatus(conds, conditionReady)
	if c := findCondition(conds, conditionReady); c != nil {
		ref.Reason, ref.Message = c.Reason, c.Message
	}
	ref.Suspended, _, _ = unstructured.NestedBool(hr.Object, "spec", "suspend")
	ref.GitOpsOwned = gitOpsOwnedHR(hr)
	ref.Deleting = hr.GetDeletionTimestamp() != nil
	ref.ChartRef = chartRefOf(hr)
	ref.LastAttemptedRevision, _, _ = unstructured.NestedString(hr.Object, "status", "lastAttemptedRevision")
	if history, found, _ := unstructured.NestedSlice(hr.Object, "status", "history"); found && len(history) > 0 {
		if entry, ok := history[0].(map[string]any); ok {
			ref.ChartVersion, _ = entry["chartVersion"].(string)
		}
	}
	if ref.ChartVersion == "" {
		ref.ChartVersion = ref.LastAttemptedRevision
	}
	a.HelmRelease = ref
	if ref.GitOpsOwned {
		a.Managed = ManagedGitOps
	} else {
		a.Managed = ManagedHelmRelease
	}
	values, _, _ := unstructured.NestedMap(hr.Object, "spec", "values")
	a.Values = values
	// The HelmRelease values are the declaration: a declared toolset is
	// reported as such, a release without one is implicit full access — even
	// while the template has not been rendered yet.
	if values != nil {
		if toolset := stringSlice(values[ToolsetValuesKey]); toolset != nil {
			a.Toolset, a.ImplicitFullAccess = toolset, false
		} else {
			a.Toolset, a.ImplicitFullAccess = nil, true
		}
	}
	if a.Exists || values == nil {
		return
	}
	// No template yet: the values are what the agent will be.
	agentBlock, _ := values["agent"].(map[string]any)
	if agentBlock != nil {
		if name, _ := agentBlock["name"].(string); name != "" {
			a.Name = name
		}
		a.DisplayName, _ = agentBlock["displayName"].(string)
		a.Description, _ = agentBlock["description"].(string)
		a.IconURL, _ = agentBlock["iconUrl"].(string)
		a.SystemMessage, _ = agentBlock["systemMessage"].(string)
	}
	if a.Name == "" {
		a.Name = hr.GetName()
	}
	if mc, _ := values["modelConfig"].(map[string]any); mc != nil {
		a.ModelConfig, _ = mc["name"].(string)
	}
	a.Skills = skillsFromValues(values)
}
