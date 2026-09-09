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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/giantswarm/agent-manager/internal/identity"
	"github.com/giantswarm/agent-manager/internal/kube"
	"github.com/giantswarm/agent-manager/internal/skills"
)

// Config configures the service.
type Config struct {
	// DefaultNamespace receives agents when a request names none.
	DefaultNamespace string
	// ManagedNamespaces are the namespaces the service may read and write
	// (RBAC exists there); DefaultNamespace is always included.
	ManagedNamespaces []string
	// Compose is the platform side of the composition.
	Compose ComposeConfig
	// Version is the service version reported by GET /info.
	Version string
}

// Service is the agent lifecycle.
type Service struct {
	kube   kube.Provider
	skills *skills.Discoverer
	cfg    Config
	log    *slog.Logger
}

// New builds the service. skills may be nil (list_skills then reports
// unsupported).
func New(k kube.Provider, s *skills.Discoverer, cfg Config, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	if cfg.DefaultNamespace == "" {
		cfg.DefaultNamespace = "kagent"
	}
	cfg.Compose.APIVersion = orDefault(cfg.Compose.APIVersion, DefaultAPIVersion)
	cfg.Compose.MusterServer = orDefault(cfg.Compose.MusterServer, DefaultMusterServer)
	cfg.Compose.DefaultHarness = orDefault(cfg.Compose.DefaultHarness, DefaultHarness)
	managed := []string{cfg.DefaultNamespace}
	for _, ns := range cfg.ManagedNamespaces {
		if ns != "" && ns != cfg.DefaultNamespace {
			managed = append(managed, ns)
		}
	}
	cfg.ManagedNamespaces = managed
	return &Service{kube: k, skills: s, cfg: cfg, log: log}
}

// InfoResponse is GET /info: what this installation can do, so the portal and
// agents feature-detect instead of guessing.
type InfoResponse struct {
	Version string `json:"version"`
	// Namespaces the service manages agents in.
	Namespaces struct {
		Default string   `json:"default"`
		Managed []string `json:"managed"`
	} `json:"namespaces"`
	// Capabilities are explicit flags; a false flag means the matching
	// operation or argument is not available on this installation.
	Capabilities map[string]bool `json:"capabilities"`
	// Identity says how calls reach the API server: `caller` (every call
	// presents the signed-in user's IdP token; the user's RBAC governs) or
	// `serviceAccount` (the service's own identity behind a trusted proxy).
	Identity string `json:"identity"`
	// APIVersions are the served kagent.dev versions the service composes and
	// reads.
	APIVersions struct {
		AgentTemplate   string `json:"agentTemplate"`
		RemoteMCPServer string `json:"remoteMcpServer"`
		Harness         string `json:"harness"`
		ModelConfig     string `json:"modelConfig"`
	} `json:"apiVersions"`
	// Kagent is the platform side of the composition.
	Kagent struct {
		// MusterServer is the platform RemoteMCPServer every toolset carrier
		// is copied from and every agent without a carrier binds.
		MusterServer string `json:"musterServer"`
		// DefaultHarness runs agents that name no harness.
		DefaultHarness string `json:"defaultHarness"`
		// ToolsetHeader is the header the toolset carrier sends.
		ToolsetHeader string `json:"toolsetHeader"`
	} `json:"kagent"`
	// SkillsRepositories are the configured skill repositories.
	SkillsRepositories []string `json:"skillsRepositories"`
}

// Info reports the installation's capabilities.
func (s *Service) Info(context.Context) InfoResponse {
	var out InfoResponse
	out.Version = s.cfg.Version
	out.Namespaces.Default = s.cfg.DefaultNamespace
	out.Namespaces.Managed = append([]string(nil), s.cfg.ManagedNamespaces...)
	out.Capabilities = map[string]bool{
		"list": true, "get": true, "create": true, "update": true, "delete": true,
		"status": true, "validate": true, "modelConfigs": true,
		"skills":         s.skills != nil,
		"writesAsCaller": s.kube.Identity() == kube.IdentityCaller,
		// kagent main: the Harness is the runtime, chosen per agent and
		// validated against the Harnesses that admit the template.
		"harnesses": true,
		// The toolset travels as a per-agent RemoteMCPServer (the carrier).
		"toolsetCarrier": true,
		// What an AgentTemplate cannot express.
		"perToolSelection":   false, // tools[] needs controller-side discovery, off for muster
		"runtime":            false, // the Harness is the runtime
		"iconUrl":            false, // no icon field on the template
		"skillGitAuthSecret": false, // skill sources are read anonymously
		"mutableSkillRefs":   false, // git skills pin a commit, OCI skills a digest
	}
	out.Identity = s.kube.Identity()
	out.APIVersions.AgentTemplate = s.cfg.Compose.APIVersion
	out.APIVersions.RemoteMCPServer = s.cfg.Compose.APIVersion
	out.APIVersions.Harness = s.cfg.Compose.APIVersion
	out.APIVersions.ModelConfig = s.cfg.Compose.APIVersion
	out.Kagent.MusterServer = s.cfg.Compose.MusterServer
	out.Kagent.DefaultHarness = s.cfg.Compose.DefaultHarness
	out.Kagent.ToolsetHeader = ToolsetHeader
	if s.skills != nil {
		out.SkillsRepositories = s.skills.Repositories()
	} else {
		out.SkillsRepositories = []string{}
	}
	return out
}

// ---- GVRs -----------------------------------------------------------------

func (s *Service) kagentGVR(resource string) schema.GroupVersionResource {
	gv, err := schema.ParseGroupVersion(s.cfg.Compose.APIVersion)
	if err != nil {
		gv = schema.GroupVersion{Group: "kagent.dev", Version: s.cfg.Compose.APIVersion}
	}
	return gv.WithResource(resource)
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

func (s *Service) dyn(ctx context.Context) (dynamic.Interface, error) {
	c, err := s.kube.Client(ctx)
	if err != nil {
		if errorsIs(err, kube.ErrNoCallerToken) {
			return nil, fmt.Errorf("%w: the request carries no identity token to act with: %v", ErrUnauthenticated, err)
		}
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}
	return c.Dynamic(), nil
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

func createOptions() metav1.CreateOptions { return metav1.CreateOptions{FieldManager: FieldManager} }
func updateOptions() metav1.UpdateOptions { return metav1.UpdateOptions{FieldManager: FieldManager} }

// ---- reads ------------------------------------------------------------------

// List returns the agents of a namespace: every AgentTemplate, with its
// toolset carrier when it has one.
func (s *Service) List(ctx context.Context, ns string) ([]Agent, error) {
	ns, err := s.Namespace(ns)
	if err != nil {
		return nil, err
	}
	dyn, err := s.dyn(ctx)
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
	byName := make(map[string]*unstructured.Unstructured, len(servers.Items))
	for i := range servers.Items {
		byName[servers.Items[i].GetName()] = &servers.Items[i]
	}
	out := make([]Agent, 0, len(templates.Items))
	for i := range templates.Items {
		tpl := &templates.Items[i]
		out = append(out, s.agentView(tpl, byName[CarrierName(s.cfg.Compose.MusterServer, tpl.GetName())]))
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
	dyn, err := s.dyn(ctx)
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
	if tpl == nil {
		return nil, notFoundf("agent %s/%s: no AgentTemplate of that name", ns, name)
	}
	carrier, err := s.getCarrier(ctx, dyn, ns, name)
	if err != nil {
		return nil, err
	}
	a := s.agentView(tpl, carrier)
	return &a, nil
}

// getTemplate returns nil, nil when the AgentTemplate does not exist.
func (s *Service) getTemplate(ctx context.Context, dyn dynamic.Interface, ns, name string) (*unstructured.Unstructured, error) {
	return s.getObject(ctx, dyn, s.templateGVR(), ns, name, "agenttemplate")
}

// getCarrier returns the agent's toolset carrier, nil, nil when absent.
func (s *Service) getCarrier(ctx context.Context, dyn dynamic.Interface, ns, agent string) (*unstructured.Unstructured, error) {
	return s.getObject(ctx, dyn, s.mcpServerGVR(), ns, CarrierName(s.cfg.Compose.MusterServer, agent), "remotemcpserver")
}

// getPlatformServer returns the platform's muster RemoteMCPServer of ns, which
// every carrier is copied from; a missing one is an installation problem.
func (s *Service) getPlatformServer(ctx context.Context, dyn dynamic.Interface, ns string) (*unstructured.Unstructured, error) {
	obj, err := s.getObject(ctx, dyn, s.mcpServerGVR(), ns, s.cfg.Compose.MusterServer, "remotemcpserver")
	if err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, fmt.Errorf("platform RemoteMCPServer %s/%s does not exist: every agent binds the platform's muster gateway, which the connectivity chart renders into the kagent namespace when kagent and muster are enabled", ns, s.cfg.Compose.MusterServer)
	}
	return obj, nil
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

// agentView reads a template and its carrier into the read model.
func (s *Service) agentView(tpl, carrier *unstructured.Unstructured) Agent {
	cfg := s.cfg.Compose
	d, unmanaged := declarationOf(tpl, carrier, cfg)
	spec, _, _ := unstructured.NestedMap(tpl.Object, "spec")
	a := Agent{
		Name: tpl.GetName(), Namespace: tpl.GetNamespace(),
		DisplayName: d.DisplayName, Description: d.Description, ModelConfig: d.ModelConfig, SystemMessage: d.SystemMessage,
		Harness: d.Harness, Skills: d.Skills, Toolset: d.Toolset, ImplicitFullAccess: d.bindsPlatformServer,
		Managed: managedOf(tpl), Generation: tpl.GetGeneration(), UnmanagedFields: unmanaged, Spec: spec,
	}
	ts := templateStatusOf(tpl)
	a.Harnesses = ts.Harnesses
	a.ObservedGeneration = ts.ObservedGeneration
	if len(ts.Harnesses) > 0 {
		a.Ready = boolPtr(len(anyReady(ts)) > 0)
	}
	if d.Toolset != nil || bindsCarrier(tpl, cfg) {
		a.ToolsetCarrier = carrierView(tpl.GetName(), carrier, cfg)
	}
	return a
}

// managedOf says who owns a template: applied from git, created here, or
// written by someone else.
func managedOf(tpl *unstructured.Unstructured) string {
	labels := tpl.GetLabels()
	switch {
	case labels[KustomizationNameLabel] != "":
		return ManagedGitOps
	case labels[ManagedByLabel] == ManagedByValue:
		return ManagedAgentManager
	}
	return ManagedExternal
}

// ---- model configs ----------------------------------------------------------

// ListModelConfigs lists the kagent ModelConfigs of a namespace.
func (s *Service) ListModelConfigs(ctx context.Context, ns string) ([]ModelConfig, error) {
	ns, err := s.Namespace(ns)
	if err != nil {
		return nil, err
	}
	dyn, err := s.dyn(ctx)
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
		raw, _, _ := unstructured.NestedSlice(item.Object, "status", "conditions")
		conds := conditionsOf(raw)
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

// requireAdmission checks that a Harness of ns admits the declaration's labels.
func (s *Service) requireAdmission(ctx context.Context, dyn dynamic.Interface, ns string, d declaration) error {
	lbls, err := d.templateLabels(s.cfg.Compose)
	if err != nil {
		return err
	}
	harnesses, err := s.listHarnesses(ctx, dyn, ns)
	if err != nil {
		return err
	}
	return requireHarnessAdmission(harnesses, lbls, ns)
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
	if spec.Harness != "" {
		if err := ValidateHarness(spec.Harness); err != nil {
			errs = append(errs, err)
		}
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
	dyn, err := s.dyn(ctx)
	if err != nil {
		return nil, err
	}
	res := &ValidateResult{Mode: "create"}
	for _, e := range checkSpec(spec) {
		res.addError(e)
	}
	d := declaration{Spec: spec}
	for _, check := range []func() error{
		func() error { return s.requireModelConfig(ctx, dyn, ns, spec.ModelConfig) },
		func() error { return s.requireAdmission(ctx, dyn, ns, d) },
		func() error { return s.requireFreeName(ctx, dyn, ns, spec.Name) },
	} {
		if err := check(); err != nil {
			if !isDomainError(err) {
				return nil, err
			}
			res.addError(err)
		}
	}
	tpl, err := BuildAgentTemplate(d, s.cfg.Compose, identity.Caller(ctx))
	if err != nil {
		res.addError(err)
	}
	var carrier *unstructured.Unstructured
	if d.Toolset != nil {
		platform, err := s.getPlatformServer(ctx, dyn, ns)
		switch {
		case err != nil && !isDomainError(err):
			res.addError(err)
		case err != nil:
			return nil, err
		default:
			carrier, _ = BuildToolsetCarrier(spec.Name, d.Toolset, platform, s.cfg.Compose, identity.Caller(ctx))
		}
	}
	res.Manifests = ComposeManifests(tpl, carrier)
	res.Valid = len(res.Errors) == 0
	return res, nil
}

// requireFreeName fails when an AgentTemplate or a carrier of that name exists.
func (s *Service) requireFreeName(ctx context.Context, dyn dynamic.Interface, ns, name string) error {
	if name == "" {
		return nil
	}
	if tpl, err := s.getTemplate(ctx, dyn, ns, name); err != nil {
		return err
	} else if tpl != nil {
		return conflictf("AgentTemplate %s/%s already exists; use update_agent to change it", ns, name)
	}
	if carrier, err := s.getCarrier(ctx, dyn, ns, name); err != nil {
		return err
	} else if carrier != nil {
		return conflictf("RemoteMCPServer %s/%s already exists (the toolset carrier the agent would get); remove it or pick another name", ns, carrier.GetName())
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
	dyn, err := s.dyn(ctx)
	if err != nil {
		return nil, err
	}
	tpl, carrier, err := s.writableTemplate(ctx, dyn, ns, upd.Name, upd.Force)
	if err != nil {
		return nil, err
	}
	res := &ValidateResult{Mode: "update"}
	current, _ := declarationOf(tpl, carrier, s.cfg.Compose)
	next, err := mergeUpdate(current, upd)
	if err != nil {
		res.addError(err)
	}
	if err := s.checkChanges(ctx, dyn, ns, current, next); err != nil {
		if !isDomainError(err) {
			return nil, err
		}
		res.addError(err)
	}
	nextTpl, err := BuildAgentTemplate(next, s.cfg.Compose, identity.Caller(ctx))
	if err != nil {
		res.addError(err)
	}
	var nextCarrier *unstructured.Unstructured
	if next.Toolset != nil {
		platform, err := s.getPlatformServer(ctx, dyn, ns)
		if err != nil {
			return nil, err
		}
		nextCarrier, _ = BuildToolsetCarrier(upd.Name, next.Toolset, platform, s.cfg.Compose, identity.Caller(ctx))
	}
	res.Manifests = ComposeManifests(nextTpl, nextCarrier)
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

// Create composes and applies the toolset carrier and the AgentTemplate of a
// new agent as the caller. Nothing is written when a check fails; a template
// that fails to apply takes its carrier with it.
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
	dyn, err := s.dyn(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.requireFreeName(ctx, dyn, ns, spec.Name); err != nil {
		return nil, err
	}
	if err := s.requireModelConfig(ctx, dyn, ns, spec.ModelConfig); err != nil {
		return nil, err
	}
	d := declaration{Spec: spec}
	if err := s.requireAdmission(ctx, dyn, ns, d); err != nil {
		return nil, err
	}
	caller := identity.Caller(ctx)
	tpl, err := BuildAgentTemplate(d, s.cfg.Compose, caller)
	if err != nil {
		return nil, err
	}
	platform, err := s.getPlatformServer(ctx, dyn, ns)
	if err != nil {
		return nil, err
	}
	carrier, dropped := BuildToolsetCarrier(spec.Name, d.Toolset, platform, s.cfg.Compose, caller)
	if len(dropped) > 0 {
		s.log.Warn("platform RemoteMCPServer carries an Authorization header; not copied onto the toolset carrier (it would override the caller's bearer)", "namespace", ns, "server", platform.GetName(), "dropped", dropped)
	}

	res := &CreateResult{RequestedBy: caller, Manifests: ComposeManifests(tpl, carrier)}
	// The carrier first, so the template's binding resolves at its first
	// reconcile; the template's failure removes the carrier again.
	createdCarrier, err := dyn.Resource(s.mcpServerGVR()).Namespace(ns).Create(ctx, carrier, createOptions())
	if err != nil {
		return nil, wrapKube(err, fmt.Sprintf("create RemoteMCPServer %s/%s", ns, carrier.GetName()))
	}
	res.Created.ToolsetCarrier = true
	createdTpl, err := dyn.Resource(s.templateGVR()).Namespace(ns).Create(ctx, tpl, createOptions())
	if err != nil {
		if delErr := dyn.Resource(s.mcpServerGVR()).Namespace(ns).Delete(ctx, carrier.GetName(), metav1.DeleteOptions{}); delErr != nil && !apierrors.IsNotFound(delErr) {
			s.log.Warn("toolset carrier left behind after the AgentTemplate failed", identity.LogAttr(ctx), "namespace", ns, "name", carrier.GetName(), "error", delErr)
		}
		return nil, wrapKube(err, fmt.Sprintf("create AgentTemplate %s/%s", ns, spec.Name))
	}
	res.Created.AgentTemplate = true
	res.Agent = s.agentView(createdTpl, createdCarrier)
	res.Status = statusOf(createdTpl, createdCarrier, s.cfg.Compose)
	s.log.Info("agent created", identity.LogAttr(ctx), "namespace", ns, "name", spec.Name, "modelConfig", spec.ModelConfig, "harness", d.harness(s.cfg.Compose), "toolset", ToolsetHeaderValue(d.Toolset))
	return res, nil
}

// ---- update --------------------------------------------------------------------

// writableTemplate fetches the template (and carrier) an update or delete
// targets and applies the ownership rules: a GitOps-owned or externally
// written template, or one carrying fields agent-manager does not compose, is
// refused unless force.
func (s *Service) writableTemplate(ctx context.Context, dyn dynamic.Interface, ns, name string, force bool) (*unstructured.Unstructured, *unstructured.Unstructured, error) {
	tpl, err := s.getTemplate(ctx, dyn, ns, name)
	if err != nil {
		return nil, nil, err
	}
	if tpl == nil {
		return nil, nil, notFoundf("agent %s/%s", ns, name)
	}
	carrier, err := s.getCarrier(ctx, dyn, ns, name)
	if err != nil {
		return nil, nil, err
	}
	if err := s.ownershipGuard(tpl, force); err != nil {
		return nil, nil, err
	}
	if !force {
		if _, unmanaged := declarationOf(tpl, carrier, s.cfg.Compose); len(unmanaged) > 0 {
			return nil, nil, conflictf("AgentTemplate %s/%s carries fields agent-manager does not compose (%s): a write here would drop them. Edit the template directly, or pass force to overwrite it", ns, name, strings.Join(unmanaged, ", "))
		}
	}
	return tpl, carrier, nil
}

// ownershipGuard refuses writes to templates agent-manager does not own.
func (s *Service) ownershipGuard(tpl *unstructured.Unstructured, force bool) error {
	if force {
		return nil
	}
	switch managedOf(tpl) {
	case ManagedGitOps:
		return conflictf("AgentTemplate %s/%s is applied by Flux Kustomization %q: its desired state lives in git, a live write would be undone. Change it in the GitOps repository, or pass force to write anyway", tpl.GetNamespace(), tpl.GetName(), tpl.GetLabels()[KustomizationNameLabel])
	case ManagedExternal:
		return conflictf("AgentTemplate %s/%s was not created by agent-manager (no %s=%s label): pass force to take it over", tpl.GetNamespace(), tpl.GetName(), ManagedByLabel, ManagedByValue)
	}
	return nil
}

// mergeUpdate applies an Update to the current declaration.
func mergeUpdate(current declaration, upd Update) (declaration, error) {
	next := current
	setOrClear := func(dst *string, v *string) {
		if v != nil {
			*dst = strings.TrimSpace(*v)
		}
	}
	setOrClear(&next.DisplayName, upd.DisplayName)
	setOrClear(&next.Description, upd.Description)
	setOrClear(&next.SystemMessage, upd.SystemMessage)
	setOrClear(&next.ModelConfig, upd.ModelConfig)
	setOrClear(&next.Harness, upd.Harness)
	if upd.Skills != nil {
		next.Skills = upd.Skills
		if next.Skills.IsEmpty() {
			next.Skills = nil
		}
	}
	if upd.Toolset != nil {
		// Replaces the whole list; an empty list is refused (preset:none is
		// the way to say "no tools"), so a toolset can never be cleared back
		// to implicit full access.
		if err := ValidateToolset(*upd.Toolset, false); err != nil {
			return next, err
		}
		next.Toolset = append([]string(nil), (*upd.Toolset)...)
		next.bindsPlatformServer = false
	}
	if upd.Labels != nil {
		next.Labels = *upd.Labels
	}
	if upd.Annotations != nil {
		next.Annotations = *upd.Annotations
	}
	if next.Harness != "" {
		if err := ValidateHarness(next.Harness); err != nil {
			return next, err
		}
	}
	return next, nil
}

// checkChanges validates what an update changes against the cluster: a new
// model config must exist, a new harness or label set must still be admitted.
func (s *Service) checkChanges(ctx context.Context, dyn dynamic.Interface, ns string, current, next declaration) error {
	if next.ModelConfig != current.ModelConfig {
		if err := s.requireModelConfig(ctx, dyn, ns, next.ModelConfig); err != nil {
			return err
		}
	}
	curLabels, _ := current.templateLabels(s.cfg.Compose)
	nextLabels, err := next.templateLabels(s.cfg.Compose)
	if err != nil {
		return err
	}
	if fmt.Sprint(curLabels) != fmt.Sprint(nextLabels) {
		return s.requireAdmission(ctx, dyn, ns, next)
	}
	return nil
}

// revisionNote tells the user what a template change means on kagent main.
const revisionNote = "the AgentTemplate changed: kagent compiles a new revision for every admitting Harness; running AgentInstances keep the revision they were created with — start a new instance to pick up the change"

// Update merges the change into the agent's declaration, re-composes both
// objects and writes what differs: the carrier for a toolset change (no new
// template revision), the template for everything else.
func (s *Service) Update(ctx context.Context, upd Update) (*UpdateResult, error) {
	ns, err := s.Namespace(upd.Namespace)
	if err != nil {
		return nil, err
	}
	if err := ValidateName(upd.Name); err != nil {
		return nil, err
	}
	if err := rejectRemoved(upd.removed()...); err != nil {
		return nil, err
	}
	dyn, err := s.dyn(ctx)
	if err != nil {
		return nil, err
	}
	tpl, carrier, err := s.writableTemplate(ctx, dyn, ns, upd.Name, upd.Force)
	if err != nil {
		return nil, err
	}
	current, _ := declarationOf(tpl, carrier, s.cfg.Compose)
	next, err := mergeUpdate(current, upd)
	if err != nil {
		return nil, err
	}
	if err := s.checkChanges(ctx, dyn, ns, current, next); err != nil {
		return nil, err
	}
	caller := identity.Caller(ctx)
	before, after := declarationMap(current), declarationMap(next)
	changed := changedPaths("", before, after)
	res := &UpdateResult{Before: before, After: after, Changed: changed, RequestedBy: caller}
	nextTpl, err := BuildAgentTemplate(next, s.cfg.Compose, caller)
	if err != nil {
		return nil, err
	}
	var nextCarrier *unstructured.Unstructured
	if next.Toolset != nil {
		platform, err := s.getPlatformServer(ctx, dyn, ns)
		if err != nil {
			return nil, err
		}
		nextCarrier, _ = BuildToolsetCarrier(upd.Name, next.Toolset, platform, s.cfg.Compose, caller)
	}
	res.Manifests = ComposeManifests(nextTpl, nextCarrier)
	if len(changed) == 0 {
		res.Agent = s.agentView(tpl, carrier)
		return res, nil
	}

	// The carrier first (a new one must exist before the template binds it).
	toolsetChanged := contains(changed, "toolset")
	if toolsetChanged && nextCarrier != nil {
		if carrier == nil {
			carrier, err = dyn.Resource(s.mcpServerGVR()).Namespace(ns).Create(ctx, nextCarrier, createOptions())
			if err != nil {
				return nil, wrapKube(err, fmt.Sprintf("create RemoteMCPServer %s/%s", ns, nextCarrier.GetName()))
			}
		} else {
			overlay(carrier, nextCarrier)
			carrier, err = dyn.Resource(s.mcpServerGVR()).Namespace(ns).Update(ctx, carrier, updateOptions())
			if err != nil {
				return nil, wrapKube(err, fmt.Sprintf("update RemoteMCPServer %s/%s", ns, carrier.GetName()))
			}
		}
	}
	// The template changes when anything but the toolset did, or when its
	// binding moves (from the platform server, or from no tools, onto the
	// carrier); a toolset change alone stays on the carrier.
	templateChanged := len(changed) > 1 || !toolsetChanged || current.serverBinding(s.cfg.Compose) != next.serverBinding(s.cfg.Compose)
	if templateChanged {
		overlay(tpl, nextTpl)
		tpl, err = dyn.Resource(s.templateGVR()).Namespace(ns).Update(ctx, tpl, updateOptions())
		if err != nil {
			return nil, wrapKube(err, fmt.Sprintf("update AgentTemplate %s/%s", ns, upd.Name))
		}
		res.Note = revisionNote
	}
	res.Agent = s.agentView(tpl, carrier)
	s.log.Info("agent updated", identity.LogAttr(ctx), "namespace", ns, "name", upd.Name, "changed", changed, "templateChanged", templateChanged)
	return res, nil
}

// overlay puts the composed object's labels, annotations and spec onto the
// served one, keeping its identity (resourceVersion, uid, finalizers) and the
// labels and annotations other tooling stamps.
func overlay(existing, composed *unstructured.Unstructured) {
	labels := map[string]string{}
	for k, v := range existing.GetLabels() {
		if toolingLabel(k) {
			labels[k] = v
		}
	}
	for k, v := range composed.GetLabels() {
		labels[k] = v
	}
	existing.SetLabels(labels)
	annotations := map[string]string{}
	for k, v := range existing.GetAnnotations() {
		if toolingAnnotation(k) && k != RequestedByAnnotation {
			annotations[k] = v
		}
	}
	for k, v := range composed.GetAnnotations() {
		annotations[k] = v
	}
	existing.SetAnnotations(annotations)
	existing.Object["spec"] = composed.Object["spec"]
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// ---- delete --------------------------------------------------------------------

// Delete removes the agent's AgentTemplate and its toolset carrier. A
// GitOps-owned or externally written template needs force.
func (s *Service) Delete(ctx context.Context, ns, name string, force bool) (*DeleteResult, error) {
	ns, err := s.Namespace(ns)
	if err != nil {
		return nil, err
	}
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	dyn, err := s.dyn(ctx)
	if err != nil {
		return nil, err
	}
	tpl, err := s.getTemplate(ctx, dyn, ns, name)
	if err != nil {
		return nil, err
	}
	if tpl == nil {
		return nil, notFoundf("agent %s/%s", ns, name)
	}
	if err := s.ownershipGuard(tpl, force); err != nil {
		return nil, err
	}
	res := &DeleteResult{Name: name, Namespace: ns, RequestedBy: identity.Caller(ctx)}
	if err := dyn.Resource(s.templateGVR()).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return nil, wrapKube(err, fmt.Sprintf("delete AgentTemplate %s/%s", ns, name))
	}
	res.AgentTemplateDeleted = true

	carrier, err := s.getCarrier(ctx, dyn, ns, name)
	switch {
	case err != nil:
		res.ToolsetCarrierKept = "could not read it: " + err.Error()
	case carrier == nil:
		// No carrier: the template bound the platform server or nothing.
	case carrier.GetLabels()[ManagedByLabel] != ManagedByValue:
		res.ToolsetCarrierKept = fmt.Sprintf("RemoteMCPServer %s was not created by agent-manager", carrier.GetName())
	default:
		if err := dyn.Resource(s.mcpServerGVR()).Namespace(ns).Delete(ctx, carrier.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			res.ToolsetCarrierKept = "delete refused: " + err.Error()
		} else {
			res.ToolsetCarrierDeleted = true
		}
	}
	s.log.Info("agent deleted", identity.LogAttr(ctx), "namespace", ns, "name", name, "toolsetCarrierDeleted", res.ToolsetCarrierDeleted)
	return res, nil
}
