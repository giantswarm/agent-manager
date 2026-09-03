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
	// KagentAPIVersion is the served kagent.dev version (v1alpha2).
	KagentAPIVersion string
	// Version is the service version reported by GET /info.
	Version string
}

// Service is the agent lifecycle.
type Service struct {
	kube   kube.Provider
	chart  ChartSource
	skills *skills.Discoverer
	cfg    Config
	log    *slog.Logger
}

// New builds the service. skills may be nil (list_skills then reports
// unsupported).
func New(k kube.Provider, c ChartSource, s *skills.Discoverer, cfg Config, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	if cfg.DefaultNamespace == "" {
		cfg.DefaultNamespace = "kagent"
	}
	if cfg.KagentAPIVersion == "" {
		cfg.KagentAPIVersion = "v1alpha2"
	}
	if cfg.Compose.ChartName == "" {
		cfg.Compose.ChartName = c.Name()
	}
	if cfg.Compose.ChartOCIURL == "" {
		cfg.Compose.ChartOCIURL = c.OCIURL()
	}
	if cfg.Compose.ChartSemver == "" {
		cfg.Compose.ChartSemver = c.SemverRange()
	}
	if cfg.Compose.HelmReleaseAPIVersion == "" {
		cfg.Compose.HelmReleaseAPIVersion = DefaultHelmReleaseAPIVersion
	}
	if cfg.Compose.OCIRepositoryAPIVersion == "" {
		cfg.Compose.OCIRepositoryAPIVersion = DefaultOCIRepositoryAPIVersion
	}
	managed := []string{cfg.DefaultNamespace}
	for _, ns := range cfg.ManagedNamespaces {
		if ns != "" && ns != cfg.DefaultNamespace {
			managed = append(managed, ns)
		}
	}
	cfg.ManagedNamespaces = managed
	return &Service{kube: k, chart: c, skills: s, cfg: cfg, log: log}
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
	// operation answers 501 unsupported.
	Capabilities map[string]bool `json:"capabilities"`
	// Identity says how writes reach the API server: serviceAccount (the
	// service's own identity behind the gateway's JWT policy).
	Identity string `json:"identity"`
	// APIVersions are the served CRD versions the service composes and reads.
	APIVersions struct {
		Agent         string `json:"agent"`
		ModelConfig   string `json:"modelConfig"`
		HelmRelease   string `json:"helmRelease"`
		OCIRepository string `json:"ociRepository"`
	} `json:"apiVersions"`
	// Flux settings the composed HelmReleases carry.
	Flux struct {
		HelmReleaseInterval   string `json:"helmReleaseInterval"`
		OCIRepositoryInterval string `json:"ociRepositoryInterval"`
		ServiceAccountName    string `json:"serviceAccountName,omitempty"`
	} `json:"flux"`
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
		"writesAsCaller": false,
	}
	out.Identity = s.kube.Identity()
	out.APIVersions.Agent = "kagent.dev/" + s.cfg.KagentAPIVersion
	out.APIVersions.ModelConfig = "kagent.dev/" + s.cfg.KagentAPIVersion
	out.APIVersions.HelmRelease = s.cfg.Compose.HelmReleaseAPIVersion
	out.APIVersions.OCIRepository = s.cfg.Compose.OCIRepositoryAPIVersion
	out.Flux.HelmReleaseInterval = orDefault(s.cfg.Compose.HelmReleaseInterval, DefaultHelmReleaseInterval)
	out.Flux.OCIRepositoryInterval = orDefault(s.cfg.Compose.OCIRepositoryInterval, DefaultOCIRepositoryInterval)
	out.Flux.ServiceAccountName = s.cfg.Compose.ServiceAccountName
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

func (s *Service) agentGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "kagent.dev", Version: s.cfg.KagentAPIVersion, Resource: "agents"}
}

func (s *Service) modelConfigGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "kagent.dev", Version: s.cfg.KagentAPIVersion, Resource: "modelconfigs"}
}

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

// List returns the agents of a namespace: every Agent CR (with its owning
// HelmRelease when Flux labels name one) plus every HelmRelease of the agent
// chart that has not rendered an Agent yet.
func (s *Service) List(ctx context.Context, ns string) ([]Agent, error) {
	ns, err := s.Namespace(ns)
	if err != nil {
		return nil, err
	}
	dyn, _, err := s.dyn(ctx)
	if err != nil {
		return nil, err
	}
	agentList, err := dyn.Resource(s.agentGVR()).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, wrapKube(err, "list agents in "+ns)
	}
	hrs, err := s.agentHelmReleases(ctx, dyn, ns)
	if err != nil {
		return nil, err
	}
	byName := map[string]*Agent{}
	for i := range agentList.Items {
		cr := &agentList.Items[i]
		a := agentFromCR(cr)
		if hrName, hrNs := ownerOf(cr); hrName != "" {
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
			// A HelmRelease whose Agent carries no provenance labels (an older
			// helm-controller): attach by name.
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
	cr, err := s.getAgentCR(ctx, dyn, ns, name)
	if err != nil {
		return nil, err
	}
	var a Agent
	hrName, hrNs := name, ns
	if cr != nil {
		a = agentFromCR(cr)
		if n, nsFromLabel := ownerOf(cr); n != "" {
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
	if cr == nil && hr == nil {
		return nil, notFoundf("agent %s/%s: no Agent and no HelmRelease of that name", ns, name)
	}
	return &a, nil
}

// getAgentCR returns nil, nil when the Agent does not exist.
func (s *Service) getAgentCR(ctx context.Context, dyn dynamic.Interface, ns, name string) (*unstructured.Unstructured, error) {
	cr, err := dyn.Resource(s.agentGVR()).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, wrapKube(err, fmt.Sprintf("get agent %s/%s", ns, name))
	}
	return cr, nil
}

// getHelmRelease returns nil, nil when the HelmRelease does not exist.
func (s *Service) getHelmRelease(ctx context.Context, dyn dynamic.Interface, ns, name string) (*unstructured.Unstructured, error) {
	hr, err := dyn.Resource(s.helmReleaseGVR()).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, wrapKube(err, fmt.Sprintf("get helmrelease %s/%s", ns, name))
	}
	return hr, nil
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
		conds := conditionsOf(&item)
		mc.Accepted = conditionStatus(conds, "Accepted")
		if c := findCondition(conds, "Accepted"); c != nil {
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

// ValidateCreate is create_agent without the write.
func (s *Service) ValidateCreate(ctx context.Context, spec Spec) (*ValidateResult, error) {
	ns, err := s.Namespace(spec.Namespace)
	if err != nil {
		return nil, err
	}
	spec.Namespace = ns
	dyn, _, err := s.dyn(ctx)
	if err != nil {
		return nil, err
	}
	res := &ValidateResult{Mode: "create"}
	if err := ValidateName(spec.Name); err != nil {
		res.Errors = append(res.Errors, strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": "))
	}
	if err := s.requireModelConfig(ctx, dyn, ns, spec.ModelConfig); err != nil {
		if !isDomainError(err) {
			return nil, err
		}
		res.Errors = append(res.Errors, strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": "))
	}
	if spec.Name != "" {
		if hr, _ := s.getHelmRelease(ctx, dyn, ns, spec.Name); hr != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("HelmRelease %s/%s already exists (use update_agent)", ns, spec.Name))
		}
		if cr, _ := s.getAgentCR(ctx, dyn, ns, spec.Name); cr != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("Agent %s/%s already exists", ns, spec.Name))
		}
	}
	values := BuildValues(spec)
	sch, violations := ValidateValues(ctx, s.chart, values)
	res.Errors = append(res.Errors, violations...)
	res.SchemaVersion, res.SchemaSource = sch.Version, sch.Source
	res.Manifests = ComposeManifests(spec.Name, ns, values, s.cfg.Compose)
	res.Valid = len(res.Errors) == 0
	return res, nil
}

// ValidateUpdate is update_agent without the write.
func (s *Service) ValidateUpdate(ctx context.Context, upd Update) (*ValidateResult, error) {
	ns, err := s.Namespace(upd.Namespace)
	if err != nil {
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
		res.Errors = append(res.Errors, strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": "))
	}
	sch, violations := ValidateValues(ctx, s.chart, values)
	res.Errors = append(res.Errors, violations...)
	res.SchemaVersion, res.SchemaSource = sch.Version, sch.Source
	res.Manifests = ComposeManifests(upd.Name, ns, values, s.cfg.Compose)
	res.Valid = len(res.Errors) == 0
	return res, nil
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
// HelmRelease of a new agent after validating the values against the chart
// schema. Nothing is written when validation fails.
func (s *Service) Create(ctx context.Context, spec Spec) (*CreateResult, error) {
	ns, err := s.Namespace(spec.Namespace)
	if err != nil {
		return nil, err
	}
	spec.Namespace = ns
	if err := ValidateName(spec.Name); err != nil {
		return nil, err
	}
	dyn, _, err := s.dyn(ctx)
	if err != nil {
		return nil, err
	}
	if hr, err := s.getHelmRelease(ctx, dyn, ns, spec.Name); err != nil {
		return nil, err
	} else if hr != nil {
		return nil, conflictf("HelmRelease %s/%s already exists; use update_agent to change it", ns, spec.Name)
	}
	if cr, err := s.getAgentCR(ctx, dyn, ns, spec.Name); err != nil {
		return nil, err
	} else if cr != nil {
		return nil, conflictf("Agent %s/%s already exists without a HelmRelease (a bare CR); delete it first (delete_agent with force) or pick another name", ns, spec.Name)
	}
	if err := s.requireModelConfig(ctx, dyn, ns, spec.ModelConfig); err != nil {
		return nil, err
	}
	values := BuildValues(spec)
	sch, violations := ValidateValues(ctx, s.chart, values)
	if len(violations) > 0 {
		return nil, invalidf("values do not satisfy the agent chart schema %s (%s): %s", sch.Version, sch.Source, strings.Join(violations, "; "))
	}

	res := &CreateResult{}
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
	s.log.Info("agent created", "namespace", ns, "name", spec.Name, "modelConfig", spec.ModelConfig, "ociRepositoryCreated", res.Created.OCIRepository)
	return res, nil
}

// ---- update --------------------------------------------------------------------

// writableHelmRelease fetches the HelmRelease an update or delete targets and
// applies the ownership rules: a bare Agent CR has nothing to write to; a
// GitOps-owned or suspended release is refused unless force.
func (s *Service) writableHelmRelease(ctx context.Context, dyn dynamic.Interface, ns, name string, force bool) (*unstructured.Unstructured, *unstructured.Unstructured, error) {
	cr, err := s.getAgentCR(ctx, dyn, ns, name)
	if err != nil {
		return nil, nil, err
	}
	hrName, hrNs := name, ns
	if cr != nil {
		if n, nsFromLabel := ownerOf(cr); n != "" {
			hrName, hrNs = n, orDefault(nsFromLabel, ns)
		}
	}
	hr, err := s.getHelmRelease(ctx, dyn, hrNs, hrName)
	if err != nil {
		return nil, nil, err
	}
	if hr == nil {
		if cr == nil {
			return nil, nil, notFoundf("agent %s/%s", ns, name)
		}
		return nil, cr, conflictf("Agent %s/%s is a bare CR with no HelmRelease behind it; agent-manager only writes HelmRelease values (recreate it with create_agent, or delete it with force)", ns, name)
	}
	if !force {
		if gitOpsOwnedHR(hr) {
			return nil, cr, conflictf("HelmRelease %s/%s is applied by Flux Kustomization %q: its desired state lives in git, a live write would be undone. Change it in the GitOps repository, or pass force to write anyway", hrNs, hrName, hr.GetLabels()[KustomizationNameLabel])
		}
		if suspended, _, _ := unstructured.NestedBool(hr.Object, "spec", "suspend"); suspended {
			return nil, cr, conflictf("HelmRelease %s/%s is suspended: Flux will not act on a change. Resume it first, or pass force", hrNs, hrName)
		}
	}
	return hr, cr, nil
}

// mergedValues applies an Update to the release's current values and returns
// (after, before).
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
	setOrDelete(agentBlock, "runtime", upd.Runtime)
	after["agent"] = agentBlock

	var modelErr error
	if upd.ModelConfig != nil {
		if err := s.requireModelConfig(ctx, dyn, ns, *upd.ModelConfig); err != nil {
			modelErr = err
		}
		after["modelConfig"] = map[string]any{"name": *upd.ModelConfig}
	}
	if upd.Skills != nil {
		if sk := skillsValues(upd.Skills); sk != nil {
			after["skills"] = sk
		} else {
			delete(after, "skills")
		}
	}
	if upd.ToolNames != nil {
		muster, _ := after["muster"].(map[string]any)
		if muster == nil {
			muster = map[string]any{}
		}
		if len(*upd.ToolNames) > 0 {
			muster["toolNames"] = toAnySlice(*upd.ToolNames)
		} else {
			delete(muster, "toolNames")
		}
		if len(muster) > 0 {
			after["muster"] = muster
		} else {
			delete(after, "muster")
		}
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
	return after, before, modelErr
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
// against the chart schema and writes it. The Agent CR itself is never
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
	res := &UpdateResult{Before: before, After: after, Changed: changed}
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
	s.log.Info("agent updated", "namespace", ns, "name", upd.Name, "changed", changed)
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

// Delete removes the agent's HelmRelease (helm-controller uninstalls the Agent)
// and the shared OCIRepository when no other release references it. A bare
// Agent CR is only deleted with force.
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
	res := &DeleteResult{Name: name, Namespace: ns}
	hr, cr, err := s.writableHelmRelease(ctx, dyn, ns, name, force)
	if err != nil {
		if hr == nil && cr != nil && force && errorsIs(err, ErrConflict) {
			// A bare Agent CR, forced: delete the CR itself.
			if err := dyn.Resource(s.agentGVR()).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				return nil, wrapKube(err, fmt.Sprintf("delete Agent %s/%s", ns, name))
			}
			res.AgentDeleted = true
			s.log.Info("bare agent deleted", "namespace", ns, "name", name)
			return res, nil
		}
		return nil, err
	}
	suspended, _, _ := unstructured.NestedBool(hr.Object, "spec", "suspend")
	if err := dyn.Resource(s.helmReleaseGVR()).Namespace(hr.GetNamespace()).Delete(ctx, hr.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return nil, wrapKube(err, fmt.Sprintf("delete HelmRelease %s/%s", hr.GetNamespace(), hr.GetName()))
	}
	res.HelmReleaseDeleted = true
	if suspended && force && cr != nil {
		// Flux drops the finalizer of a suspended release without uninstalling:
		// the Agent would stay behind, so remove it directly.
		if err := dyn.Resource(s.agentGVR()).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return nil, wrapKube(err, fmt.Sprintf("delete Agent %s/%s", ns, name))
		}
		res.AgentDeleted = true
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
	s.log.Info("agent deleted", "namespace", ns, "name", name, "ociRepositoryDeleted", true)
	return res, nil
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

// conditionsOf flattens status.conditions.
func conditionsOf(obj *unstructured.Unstructured) []Condition {
	raw, found, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if !found {
		return nil
	}
	out := make([]Condition, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		c := Condition{}
		c.Type, _ = m["type"].(string)
		c.Status, _ = m["status"].(string)
		c.Reason, _ = m["reason"].(string)
		c.Message, _ = m["message"].(string)
		c.LastTransitionTime, _ = m["lastTransitionTime"].(string)
		out = append(out, c)
	}
	return out
}

func findCondition(conds []Condition, typ string) *Condition {
	for i := range conds {
		if conds[i].Type == typ {
			return &conds[i]
		}
	}
	return nil
}

// conditionStatus maps a condition to true/false, nil when absent or Unknown.
func conditionStatus(conds []Condition, typ string) *bool {
	c := findCondition(conds, typ)
	if c == nil {
		return nil
	}
	switch c.Status {
	case "True":
		return boolPtr(true)
	case "False":
		return boolPtr(false)
	}
	return nil
}

func boolPtr(b bool) *bool { return &b }

// agentFromCR reads the Agent CR into the view.
func agentFromCR(cr *unstructured.Unstructured) Agent {
	a := Agent{Name: cr.GetName(), Namespace: cr.GetNamespace(), Exists: true, Managed: ManagedNone}
	a.DisplayName = cr.GetAnnotations()[DisplayNameAnnotation]
	a.Description, _, _ = unstructured.NestedString(cr.Object, "spec", "description")
	a.IconURL, _, _ = unstructured.NestedString(cr.Object, "spec", "iconUrl")
	a.Runtime, _, _ = unstructured.NestedString(cr.Object, "spec", "declarative", "runtime")
	a.ModelConfig, _, _ = unstructured.NestedString(cr.Object, "spec", "declarative", "modelConfig")
	a.SystemMessage, _, _ = unstructured.NestedString(cr.Object, "spec", "declarative", "systemMessage")
	a.Skills = skillsFromCR(cr)
	a.ToolNames = toolNamesFromCR(cr)
	a.Conditions = conditionsOf(cr)
	a.Ready = conditionStatus(a.Conditions, "Ready")
	a.Accepted = conditionStatus(a.Conditions, "Accepted")
	return a
}

func skillsFromCR(cr *unstructured.Unstructured) *Skills {
	raw, found, _ := unstructured.NestedMap(cr.Object, "spec", "skills")
	if !found {
		return nil
	}
	s := &Skills{}
	if refs, ok := raw["refs"].([]any); ok {
		for _, r := range refs {
			if str, ok := r.(string); ok {
				s.Refs = append(s.Refs, str)
			}
		}
	}
	if gitRefs, ok := raw["gitRefs"].([]any); ok {
		for _, g := range gitRefs {
			m, ok := g.(map[string]any)
			if !ok {
				continue
			}
			ref := SkillGitRef{}
			ref.URL, _ = m["url"].(string)
			ref.Path, _ = m["path"].(string)
			ref.Ref, _ = m["ref"].(string)
			ref.Name, _ = m["name"].(string)
			s.GitRefs = append(s.GitRefs, ref)
		}
	}
	if auth, ok := raw["gitAuthSecretRef"].(map[string]any); ok {
		s.GitAuthSecretName, _ = auth["name"].(string)
	}
	if s.IsEmpty() && s.GitAuthSecretName == "" {
		return nil
	}
	return s
}

// toolNamesFromCR returns the toolNames of the first MCP server tool (the
// muster gateway the chart wires).
func toolNamesFromCR(cr *unstructured.Unstructured) []string {
	tools, found, _ := unstructured.NestedSlice(cr.Object, "spec", "declarative", "tools")
	if !found {
		return nil
	}
	for _, t := range tools {
		m, ok := t.(map[string]any)
		if !ok {
			continue
		}
		server, ok := m["mcpServer"].(map[string]any)
		if !ok {
			continue
		}
		names, ok := server["toolNames"].([]any)
		if !ok {
			return nil
		}
		out := make([]string, 0, len(names))
		for _, n := range names {
			if str, ok := n.(string); ok {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}

// applyHelmRelease folds the owning release into the view and, when the Agent
// CR is absent, reads the identity fields from the values.
func applyHelmRelease(a *Agent, hr *unstructured.Unstructured) {
	ref := &HelmReleaseRef{Name: hr.GetName(), Namespace: hr.GetNamespace()}
	conds := conditionsOf(hr)
	ref.Ready = conditionStatus(conds, "Ready")
	if c := findCondition(conds, "Ready"); c != nil {
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
	if a.Exists || values == nil {
		return
	}
	// No Agent CR yet: the values are what the agent will be.
	agentBlock, _ := values["agent"].(map[string]any)
	if agentBlock != nil {
		if name, _ := agentBlock["name"].(string); name != "" {
			a.Name = name
		}
		a.DisplayName, _ = agentBlock["displayName"].(string)
		a.Description, _ = agentBlock["description"].(string)
		a.IconURL, _ = agentBlock["iconUrl"].(string)
		a.Runtime, _ = agentBlock["runtime"].(string)
		a.SystemMessage, _ = agentBlock["systemMessage"].(string)
	}
	if a.Name == "" {
		a.Name = hr.GetName()
	}
	if mc, _ := values["modelConfig"].(map[string]any); mc != nil {
		a.ModelConfig, _ = mc["name"].(string)
	}
	if sk, _ := values["skills"].(map[string]any); sk != nil {
		a.Skills = skillsFromCR(&unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"skills": sk}}})
	}
	if muster, _ := values["muster"].(map[string]any); muster != nil {
		if names, ok := muster["toolNames"].([]any); ok {
			for _, n := range names {
				if str, ok := n.(string); ok {
					a.ToolNames = append(a.ToolNames, str)
				}
			}
		}
	}
}
