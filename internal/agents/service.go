package agents

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

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
	// HarnessName is the platform Harness whose status.harnesses[] entry
	// decides an agent's readiness.
	HarnessName string
	// Version is the service version reported by GET /info.
	Version string
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
	cfg.HarnessName = orDefault(cfg.HarnessName, DefaultHarnessName)
	cfg.Compose.ChartName = orDefault(cfg.Compose.ChartName, c.Name())
	cfg.Compose.ChartOCIURL = orDefault(cfg.Compose.ChartOCIURL, c.OCIURL())
	cfg.Compose.ChartSemver = orDefault(cfg.Compose.ChartSemver, c.SemverRange())
	cfg.Compose.HelmReleaseAPIVersion = orDefault(cfg.Compose.HelmReleaseAPIVersion, DefaultHelmReleaseAPIVersion)
	cfg.Compose.OCIRepositoryAPIVersion = orDefault(cfg.Compose.OCIRepositoryAPIVersion, DefaultOCIRepositoryAPIVersion)
	managed := []string{cfg.DefaultNamespace}
	for _, ns := range cfg.ManagedNamespaces {
		if ns != "" && ns != cfg.DefaultNamespace {
			managed = append(managed, ns)
		}
	}
	cfg.ManagedNamespaces = managed
	return &Service{kube: k, chart: c, skills: s, pinner: p, cfg: cfg, log: log}
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
	} `json:"muster"`
	// SkillsRepositories are the configured skill repositories.
	SkillsRepositories []string `json:"skillsRepositories"`
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
		// The write tools apply live; landing the manifests as a pull
		// request in the owning GitOps repository is not available.
		"commit": false,
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
	out.Harness.Name = s.cfg.HarnessName
	out.Muster.URL = s.cfg.Compose.MusterURL
	if s.skills != nil {
		out.SkillsRepositories = s.skills.Repositories()
	} else {
		out.SkillsRepositories = []string{}
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

// wrapKube maps API server errors onto the domain sentinels.
func wrapKube(err error, what string) error {
	switch {
	case err == nil:
		return nil
	case apierrors.IsNotFound(err):
		return fmt.Errorf("%w: %s", ErrNotFound, what)
	case apierrors.IsForbidden(err):
		return fmt.Errorf("%w: %s: %v", ErrForbidden, what, err)
	case apierrors.IsAlreadyExists(err), apierrors.IsConflict(err):
		return fmt.Errorf("%w: %s: %v", ErrConflict, what, err)
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return fmt.Errorf("%w: %s: %v", ErrInvalid, what, err)
	default:
		return fmt.Errorf("%s: %w", what, err)
	}
}

// ---- reads ------------------------------------------------------------------

// List returns the agents of a namespace: every AgentTemplate (with its
// RemoteMCPServer and its owning HelmRelease when Flux labels name one) plus
// every HelmRelease of the agent chart that has not rendered a template yet.
func (s *Service) List(ctx context.Context, ns string) ([]Agent, error) {
	ns, err := s.Namespace(ns)
	if err != nil {
		return nil, err
	}
	dyn, _, err := s.dyn(ctx)
	if err != nil {
		return nil, err
	}
	templates, err := dyn.Resource(s.templateGVR()).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, wrapKube(err, "list agenttemplates in "+ns)
	}
	servers, err := dyn.Resource(s.mcpServerGVR()).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, wrapKube(err, "list remotemcpservers in "+ns)
	}
	serverByName := make(map[string]*unstructured.Unstructured, len(servers.Items))
	for i := range servers.Items {
		serverByName[servers.Items[i].GetName()] = &servers.Items[i]
	}
	hrs, err := s.agentHelmReleases(ctx, dyn, ns)
	if err != nil {
		return nil, err
	}
	byName := map[string]*Agent{}
	for i := range templates.Items {
		tpl := &templates.Items[i]
		a := s.agentFromTemplate(tpl, serverByName[tpl.GetName()])
		if hrName, hrNs := ownerOf(tpl); hrName != "" {
			hr := hrs[hrName]
			if hr == nil || hrNs != ns {
				hr, _ = s.getHelmRelease(ctx, dyn, orDefault(hrNs, ns), hrName)
			}
			if hr != nil {
				applyHelmRelease(&a, hr)
				delete(hrs, hrName)
			}
		}
		byName[a.Name] = &a
	}
	for _, hr := range hrs {
		a := Agent{Namespace: ns, Managed: ManagedHelmRelease}
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
func (s *Service) Get(ctx context.Context, ns, name string) (*Agent, error) {
	ns, err := s.Namespace(ns)
	if err != nil {
		return nil, err
	}
	dyn, _, err := s.dyn(ctx)
	if err != nil {
		return nil, err
	}
	return s.get(ctx, dyn, ns, name)
}

func (s *Service) get(ctx context.Context, dyn dynamic.Interface, ns, name string) (*Agent, error) {
	tpl, err := s.getTemplate(ctx, dyn, ns, name)
	if err != nil {
		return nil, err
	}
	var a Agent
	hrName, hrNs := name, ns
	if tpl != nil {
		server, err := s.getMCPServer(ctx, dyn, ns, name)
		if err != nil {
			return nil, err
		}
		a = s.agentFromTemplate(tpl, server)
		if n, nsFromLabel := ownerOf(tpl); n != "" {
			hrName, hrNs = n, orDefault(nsFromLabel, ns)
		}
	} else {
		a = Agent{Name: name, Namespace: ns, Managed: ManagedNone}
	}
	hr, err := s.getHelmRelease(ctx, dyn, hrNs, hrName)
	if err != nil {
		return nil, err
	}
	if hr != nil {
		applyHelmRelease(&a, hr)
	}
	if tpl == nil && hr == nil {
		return nil, notFoundf("agent %s/%s: no AgentTemplate and no HelmRelease of that name", ns, name)
	}
	return &a, nil
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

// agentHelmReleases lists the HelmReleases of ns that render the agent chart,
// keyed by name.
func (s *Service) agentHelmReleases(ctx context.Context, dyn dynamic.Interface, ns string) (map[string]*unstructured.Unstructured, error) {
	sources, err := s.agentChartSources(ctx, dyn, ns)
	if err != nil {
		return nil, err
	}
	list, err := dyn.Resource(s.helmReleaseGVR()).Namespace(ns).List(ctx, metav1.ListOptions{})
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
		out[hr.GetName()] = hr
	}
	return out, nil
}

// ---- model configs ----------------------------------------------------------

// ListModelConfigs lists the kagent ModelConfigs of a namespace.
func (s *Service) ListModelConfigs(ctx context.Context, ns string) ([]ModelConfig, error) {
	ns, err := s.Namespace(ns)
	if err != nil {
		return nil, err
	}
	dyn, _, err := s.dyn(ctx)
	if err != nil {
		return nil, err
	}
	return s.listModelConfigs(ctx, dyn, ns)
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
	return res, nil
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
	return errs
}

// ValidateCreate is create_agent without the write.
func (s *Service) ValidateCreate(ctx context.Context, spec Spec) (*ValidateResult, error) {
	ns, err := s.Namespace(spec.Namespace)
	if err != nil {
		return nil, err
	}
	spec.Namespace = ns
	if err := rejectRemoved(spec.removed()...); err != nil {
		return nil, err
	}
	dyn, _, err := s.dyn(ctx)
	if err != nil {
		return nil, err
	}
	res := &ValidateResult{Mode: "create"}
	for _, e := range checkSpec(spec) {
		res.addError(e)
	}
	if err := s.requireModelConfig(ctx, dyn, ns, spec.ModelConfig); err != nil {
		if !isDomainError(err) {
			return nil, err
		}
		res.addError(err)
	}
	if err := s.requireFreeName(ctx, dyn, ns, spec.Name); err != nil {
		if !isDomainError(err) {
			return nil, err
		}
		res.addError(err)
	}
	if pinned, err := pinSkills(ctx, s.pinner, spec.Skills, false); err != nil {
		res.addError(err)
	} else {
		spec.Skills = pinned
	}
	values := BuildValues(spec, s.cfg.Compose)
	sch, violations := ValidateValues(ctx, s.chart, values)
	res.Errors = append(res.Errors, violations...)
	res.SchemaVersion, res.SchemaSource = sch.Version, sch.Source
	res.Manifests = ComposeManifests(spec.Name, ns, values, s.cfg.Compose)
	res.Valid = len(res.Errors) == 0
	return res, nil
}

// requireFreeName fails when a HelmRelease or an AgentTemplate of that name
// exists.
func (s *Service) requireFreeName(ctx context.Context, dyn dynamic.Interface, ns, name string) error {
	if name == "" {
		return nil
	}
	if hr, err := s.getHelmRelease(ctx, dyn, ns, name); err != nil {
		return err
	} else if hr != nil {
		return conflictf("HelmRelease %s/%s already exists; use update_agent to change it", ns, name)
	}
	if tpl, err := s.getTemplate(ctx, dyn, ns, name); err != nil {
		return err
	} else if tpl != nil {
		return conflictf("AgentTemplate %s/%s already exists without a HelmRelease (a bare template); delete it first (delete_agent with force) or pick another name", ns, name)
	}
	return nil
}

// ValidateUpdate is update_agent without the write.
func (s *Service) ValidateUpdate(ctx context.Context, upd Update) (*ValidateResult, error) {
	ns, err := s.Namespace(upd.Namespace)
	if err != nil {
		return nil, err
	}
	if err := rejectRemoved(upd.removed()...); err != nil {
		return nil, err
	}
	dyn, _, err := s.dyn(ctx)
	if err != nil {
		return nil, err
	}
	hr, _, err := s.writableHelmRelease(ctx, dyn, ns, upd.Name, upd.Force)
	if err != nil {
		return nil, err
	}
	res := &ValidateResult{Mode: "update"}
	values, _, err := s.mergedValues(ctx, dyn, ns, upd, hr)
	if err != nil {
		if !isDomainError(err) {
			return nil, err
		}
		res.addError(err)
	}
	sch, violations := ValidateValues(ctx, s.chart, values)
	res.Errors = append(res.Errors, violations...)
	res.SchemaVersion, res.SchemaSource = sch.Version, sch.Source
	res.Manifests = ComposeManifests(upd.Name, ns, values, s.cfg.Compose)
	res.Valid = len(res.Errors) == 0
	return res, nil
}

func (r *ValidateResult) addError(err error) {
	r.Errors = append(r.Errors, strings.TrimPrefix(strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": "), ErrConflict.Error()+": "))
}

func isDomainError(err error) bool {
	for _, sentinel := range []error{ErrInvalid, ErrNotFound, ErrConflict, ErrUnsupported} {
		if errorsIs(err, sentinel) {
			return true
		}
	}
	return false
}

// ---- create --------------------------------------------------------------------

// Create composes and applies the OCIRepository (when missing) and the
// HelmRelease of a new agent after pinning its skills and validating the
// values against the chart schema. Nothing is written when a check fails.
func (s *Service) Create(ctx context.Context, spec Spec) (*CreateResult, error) {
	ns, err := s.Namespace(spec.Namespace)
	if err != nil {
		return nil, err
	}
	spec.Namespace = ns
	if err := rejectRemoved(spec.removed()...); err != nil {
		return nil, err
	}
	if errs := checkSpec(spec); len(errs) > 0 {
		return nil, errs[0]
	}
	dyn, _, err := s.dyn(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.requireFreeName(ctx, dyn, ns, spec.Name); err != nil {
		return nil, err
	}
	if err := s.requireModelConfig(ctx, dyn, ns, spec.ModelConfig); err != nil {
		return nil, err
	}
	if spec.Skills, err = pinSkills(ctx, s.pinner, spec.Skills, false); err != nil {
		return nil, err
	}
	values := BuildValues(spec, s.cfg.Compose)
	sch, violations := ValidateValues(ctx, s.chart, values)
	if len(violations) > 0 {
		return nil, invalidf("values do not satisfy the agent chart schema %s (%s): %s", sch.Version, sch.Source, strings.Join(violations, "; "))
	}

	res := &CreateResult{RequestedBy: identity.Caller(ctx)}
	res.Manifests = ComposeManifests(spec.Name, ns, values, s.cfg.Compose)

	// The chart source is shared per namespace: create it once, reuse it after.
	ociRepo := BuildOCIRepository(ns, s.cfg.Compose)
	existing, err := dyn.Resource(s.ociRepositoryGVR()).Namespace(ns).Get(ctx, ociRepo.GetName(), metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		if _, err := dyn.Resource(s.ociRepositoryGVR()).Namespace(ns).Create(ctx, ociRepo, metav1.CreateOptions{}); err != nil {
			return nil, wrapKube(err, fmt.Sprintf("create OCIRepository %s/%s", ns, ociRepo.GetName()))
		}
		res.Created.OCIRepository = true
	case err != nil:
		return nil, wrapKube(err, fmt.Sprintf("get OCIRepository %s/%s", ns, ociRepo.GetName()))
	default:
		if url, _, _ := unstructured.NestedString(existing.Object, "spec", "url"); url != s.cfg.Compose.ChartOCIURL {
			s.log.Warn("reusing an OCIRepository that points elsewhere", "namespace", ns, "name", ociRepo.GetName(), "url", url, "expected", s.cfg.Compose.ChartOCIURL)
		}
		if semver, _, _ := unstructured.NestedString(existing.Object, "spec", "ref", "semver"); semver != s.cfg.Compose.ChartSemver {
			s.log.Warn("reusing an OCIRepository on another range; the migrate command moves it", "namespace", ns, "name", ociRepo.GetName(), "semver", semver, "expected", s.cfg.Compose.ChartSemver)
		}
	}

	hr := BuildHelmRelease(spec.Name, ns, values, s.cfg.Compose)
	created, err := dyn.Resource(s.helmReleaseGVR()).Namespace(ns).Create(ctx, hr, metav1.CreateOptions{})
	if err != nil {
		return nil, wrapKube(err, fmt.Sprintf("create HelmRelease %s/%s", ns, spec.Name))
	}
	res.Created.HelmRelease = true
	agent := Agent{Name: spec.Name, Namespace: ns, Managed: ManagedHelmRelease}
	applyHelmRelease(&agent, created)
	res.Agent = agent
	if st, err := s.Status(ctx, ns, spec.Name); err == nil {
		res.Status = st
	}
	s.log.Info("agent created", identity.LogAttr(ctx), "namespace", ns, "name", spec.Name, "modelConfig", spec.ModelConfig, "skills", len(spec.Skills), "ociRepositoryCreated", res.Created.OCIRepository)
	return res, nil
}

// ---- update --------------------------------------------------------------------

// writableHelmRelease fetches the HelmRelease an update or delete targets and
// applies the ownership rules: a bare AgentTemplate has nothing to write to; a
// GitOps-owned or suspended release is refused unless force.
func (s *Service) writableHelmRelease(ctx context.Context, dyn dynamic.Interface, ns, name string, force bool) (*unstructured.Unstructured, *unstructured.Unstructured, error) {
	tpl, err := s.getTemplate(ctx, dyn, ns, name)
	if err != nil {
		return nil, nil, err
	}
	hrName, hrNs := name, ns
	if tpl != nil {
		if n, nsFromLabel := ownerOf(tpl); n != "" {
			hrName, hrNs = n, orDefault(nsFromLabel, ns)
		}
	}
	hr, err := s.getHelmRelease(ctx, dyn, hrNs, hrName)
	if err != nil {
		return nil, nil, err
	}
	if hr == nil {
		if tpl == nil {
			return nil, nil, notFoundf("agent %s/%s", ns, name)
		}
		return nil, tpl, conflictf("AgentTemplate %s/%s is a bare template with no HelmRelease behind it; agent-manager only writes HelmRelease values (recreate it with create_agent, or delete it with force)", ns, name)
	}
	if !force {
		if gitOpsOwnedHR(hr) {
			return nil, tpl, conflictf("HelmRelease %s/%s is applied by Flux Kustomization %q: its desired state lives in git, a live write would be undone. Change it in the GitOps repository, or pass force to write anyway", hrNs, hrName, hr.GetLabels()[KustomizationNameLabel])
		}
		if suspended, _, _ := unstructured.NestedBool(hr.Object, "spec", "suspend"); suspended {
			return nil, tpl, conflictf("HelmRelease %s/%s is suspended: Flux will not act on a change. Resume it first, or pass force", hrNs, hrName)
		}
	}
	return hr, tpl, nil
}

// mergedValues applies an Update to the release's current values and returns
// (after, before). Skills given replace the list and are pinned; refreshSkills
// re-pins every git skill to the head of its ref (the default branch unless
// the request names one).
func (s *Service) mergedValues(ctx context.Context, dyn dynamic.Interface, ns string, upd Update, hr *unstructured.Unstructured) (map[string]any, map[string]any, error) {
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
		if err := s.requireModelConfig(ctx, dyn, ns, *upd.ModelConfig); err != nil {
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
func (s *Service) Update(ctx context.Context, upd Update) (*UpdateResult, error) {
	ns, err := s.Namespace(upd.Namespace)
	if err != nil {
		return nil, err
	}
	if err := ValidateName(upd.Name); err != nil {
		return nil, err
	}
	dyn, _, err := s.dyn(ctx)
	if err != nil {
		return nil, err
	}
	if err := rejectRemoved(upd.removed()...); err != nil {
		return nil, err
	}
	hr, _, err := s.writableHelmRelease(ctx, dyn, ns, upd.Name, upd.Force)
	if err != nil {
		return nil, err
	}
	after, before, err := s.mergedValues(ctx, dyn, ns, upd, hr)
	if err != nil {
		return nil, err
	}
	sch, violations := ValidateValues(ctx, s.chart, after)
	if len(violations) > 0 {
		return nil, invalidf("values do not satisfy the agent chart schema %s (%s): %s", sch.Version, sch.Source, strings.Join(violations, "; "))
	}
	changed := changedPaths("", before, after)
	res := &UpdateResult{Before: before, After: after, Changed: changed, RequestedBy: identity.Caller(ctx)}
	res.Manifests = ComposeManifests(upd.Name, ns, after, s.cfg.Compose)
	if len(changed) == 0 {
		agent, err := s.get(ctx, dyn, ns, upd.Name)
		if err != nil {
			return nil, err
		}
		res.Agent = *agent
		return res, nil
	}
	if err := unstructured.SetNestedMap(hr.Object, after, "spec", "values"); err != nil {
		return nil, fmt.Errorf("set values: %w", err)
	}
	updated, err := dyn.Resource(s.helmReleaseGVR()).Namespace(hr.GetNamespace()).Update(ctx, hr, metav1.UpdateOptions{})
	if err != nil {
		return nil, wrapKube(err, fmt.Sprintf("update HelmRelease %s/%s", hr.GetNamespace(), hr.GetName()))
	}
	agent, err := s.get(ctx, dyn, ns, upd.Name)
	if err != nil {
		agent = &Agent{Name: upd.Name, Namespace: ns, Managed: ManagedHelmRelease}
		applyHelmRelease(agent, updated)
	}
	res.Agent = *agent
	s.log.Info("agent updated", identity.LogAttr(ctx), "namespace", ns, "name", upd.Name, "changed", changed, "refreshSkills", upd.RefreshSkills)
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
// with force.
func (s *Service) Delete(ctx context.Context, ns, name string, force bool) (*DeleteResult, error) {
	ns, err := s.Namespace(ns)
	if err != nil {
		return nil, err
	}
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	dyn, _, err := s.dyn(ctx)
	if err != nil {
		return nil, err
	}
	res := &DeleteResult{Name: name, Namespace: ns, RequestedBy: identity.Caller(ctx)}
	hr, tpl, err := s.writableHelmRelease(ctx, dyn, ns, name, force)
	if err != nil {
		if hr == nil && tpl != nil && force && errorsIs(err, ErrConflict) {
			// A bare AgentTemplate, forced: delete the template itself.
			if err := s.deleteRendered(ctx, dyn, ns, name, res, false); err != nil {
				return nil, err
			}
			s.log.Info("bare agent template deleted", identity.LogAttr(ctx), "namespace", ns, "name", name)
			return res, nil
		}
		return nil, err
	}
	suspended, _, _ := unstructured.NestedBool(hr.Object, "spec", "suspend")
	if err := dyn.Resource(s.helmReleaseGVR()).Namespace(hr.GetNamespace()).Delete(ctx, hr.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return nil, wrapKube(err, fmt.Sprintf("delete HelmRelease %s/%s", hr.GetNamespace(), hr.GetName()))
	}
	res.HelmReleaseDeleted = true
	if suspended && force && tpl != nil {
		// Flux drops the finalizer of a suspended release without uninstalling:
		// the rendered objects would stay behind, so remove them directly.
		if err := s.deleteRendered(ctx, dyn, ns, name, res, true); err != nil {
			return nil, err
		}
	}

	// Best-effort cleanup of the shared chart source: every uncertainty keeps
	// it (an orphan is inert; a wrongly deleted one breaks every other agent).
	ref := chartRefOf(hr)
	if ref == nil || ref.Kind != kindOCIRepository {
		res.OCIRepositoryKept = "the HelmRelease does not render from an OCIRepository"
		return res, nil
	}
	sourceNs := orDefault(ref.Namespace, hr.GetNamespace())
	others, err := s.otherReferences(ctx, dyn, sourceNs, ref.Name, hr.GetName())
	if err != nil {
		res.OCIRepositoryKept = "could not list the other HelmReleases of the namespace: " + err.Error()
		return res, nil
	}
	if len(others) > 0 {
		res.OCIRepositoryKept = fmt.Sprintf("still referenced by %d other HelmRelease(s): %s", len(others), strings.Join(others, ", "))
		return res, nil
	}
	if err := dyn.Resource(s.ociRepositoryGVR()).Namespace(sourceNs).Delete(ctx, ref.Name, metav1.DeleteOptions{}); err != nil {
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

// deleteRendered deletes the AgentTemplate named after the agent and, when
// withServer, the RemoteMCPServer of the same name that the same release
// rendered (its provenance labels name the release).
func (s *Service) deleteRendered(ctx context.Context, dyn dynamic.Interface, ns, name string, res *DeleteResult, withServer bool) error {
	if err := dyn.Resource(s.templateGVR()).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return wrapKube(err, fmt.Sprintf("delete AgentTemplate %s/%s", ns, name))
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
	if err := dyn.Resource(s.mcpServerGVR()).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return wrapKube(err, fmt.Sprintf("delete RemoteMCPServer %s/%s", ns, name))
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
	a.Ready = harnessReady(ts.Harnesses, s.cfg.HarnessName)
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
