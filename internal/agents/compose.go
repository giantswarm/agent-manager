package agents

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"
)

// The composition, per the platform contract for kagent main: one
// kagent.dev/v1alpha3 AgentTemplate per agent in the kagent namespace — the
// portable behaviour (description, system prompt, model config, immutable
// skills) plus one MCP tool binding to the whole muster server — and, for the
// toolset the agent declares, a toolset carrier: a per-agent copy of the
// platform's `muster` RemoteMCPServer (url, protocol, discovery opt-out) that
// adds the X-Muster-Toolset header. The template binds the carrier instead of
// the platform server, muster resolves the header per request, and a toolset
// change only touches the carrier — never the template, so it never compiles a
// new revision. A template that binds the platform server directly has no
// toolset: implicit full access.

// ComposeConfig is the platform side of the composition.
type ComposeConfig struct {
	// APIVersion is the served kagent.dev version (kagent.dev/v1alpha3).
	APIVersion string
	// MusterServer names the platform's RemoteMCPServer in every managed
	// namespace: the shared muster gateway every toolset carrier is copied from.
	MusterServer string
	// DefaultHarness names the Harness a request without one runs on.
	DefaultHarness string
}

// Defaults of the composition.
const (
	DefaultAPIVersion   = "kagent.dev/v1alpha3"
	DefaultMusterServer = "muster"
	DefaultHarness      = "kagent"

	kindAgentTemplate   = "AgentTemplate"
	kindRemoteMCPServer = "RemoteMCPServer"
)

var (
	dns1123 = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	// The AgentTemplate CRD's patterns for immutable skill sources.
	gitCommit    = regexp.MustCompile(`^([0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)
	httpURL      = regexp.MustCompile(`^https?://[^\s]+$`)
	ociDigestRef = regexp.MustCompile(`^[^\s@]+@sha256:[0-9a-f]{64}$`)
)

// ValidateName checks the technical name: a DNS-1123 label of at most 63
// characters.
func ValidateName(name string) error {
	if name == "" {
		return invalidf("name is required")
	}
	if len(name) > 63 {
		return invalidf("name %q is longer than 63 characters", name)
	}
	if !dns1123.MatchString(name) {
		return invalidf("name %q must be a DNS-1123 label: lowercase letters, digits and '-', starting and ending alphanumeric", name)
	}
	return nil
}

// ValidateHarness checks a Harness name (a DNS-1123 label; it is a label value too).
func ValidateHarness(name string) error {
	if len(name) > 63 || !dns1123.MatchString(name) {
		return invalidf("harness %q must be a DNS-1123 label of at most 63 characters (the name of a kagent.dev Harness)", name)
	}
	return nil
}

// CarrierName is the toolset carrier's name: <platform server>-<agent>.
func CarrierName(musterServer, agent string) string {
	return orDefault(musterServer, DefaultMusterServer) + "-" + agent
}

// declaration is what agent-manager composes an agent from: the request, plus
// whether the template binds the platform server without a carrier — only
// ever read back from a template written by someone else (a create always
// declares a toolset).
type declaration struct {
	Spec
	bindsPlatformServer bool
}

// harness resolves the Harness the declaration runs on.
func (d declaration) harness(cfg ComposeConfig) string {
	return orDefault(d.Harness, orDefault(cfg.DefaultHarness, DefaultHarness))
}

// templateLabels are the labels the composed template carries: the request's
// plus the platform's own, which win.
func (d declaration) templateLabels(cfg ComposeConfig) (map[string]string, error) {
	harness := d.harness(cfg)
	if v, ok := d.Labels[HarnessLabel]; ok && v != harness {
		return nil, invalidf("labels[%s]=%q conflicts with harness %q: the Harness is chosen with the harness argument, not a label", HarnessLabel, v, harness)
	}
	labels := make(map[string]string, len(d.Labels)+2)
	for k, v := range d.Labels {
		labels[k] = v
	}
	labels[ManagedByLabel] = ManagedByValue
	labels[HarnessLabel] = harness
	return labels, nil
}

// BuildAgentTemplate composes the AgentTemplate of an agent. caller is
// recorded as the requesting identity; empty without OAuth.
func BuildAgentTemplate(d declaration, cfg ComposeConfig, caller string) (*unstructured.Unstructured, error) {
	labels, err := d.templateLabels(cfg)
	if err != nil {
		return nil, err
	}
	annotations := map[string]string{}
	for k, v := range d.Annotations {
		annotations[k] = v
	}
	if strings.TrimSpace(d.DisplayName) != "" {
		annotations[DisplayNameAnnotation] = d.DisplayName
	}
	if caller != "" {
		annotations[RequestedByAnnotation] = caller
	}
	spec := map[string]any{}
	if strings.TrimSpace(d.Description) != "" {
		spec["description"] = d.Description
	}
	if strings.TrimSpace(d.SystemMessage) != "" {
		spec["systemPrompt"] = d.SystemMessage
	}
	if strings.TrimSpace(d.ModelConfig) != "" {
		spec["modelConfig"] = map[string]any{"name": d.ModelConfig}
	}
	skills, err := skillsSpec(d.Skills)
	if err != nil {
		return nil, err
	}
	if len(skills) > 0 {
		spec["skills"] = skills
	}
	if binding := d.serverBinding(cfg); binding != "" {
		spec["tools"] = []any{mcpBinding(binding)}
	}
	metadata := map[string]any{
		"name":      d.Name,
		"namespace": d.Namespace,
		"labels":    toAnyMap(labels),
	}
	if len(annotations) > 0 {
		metadata["annotations"] = toAnyMap(annotations)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": orDefault(cfg.APIVersion, DefaultAPIVersion),
		"kind":       kindAgentTemplate,
		"metadata":   metadata,
		"spec":       spec,
	}}, nil
}

// serverBinding is the RemoteMCPServer the template binds: the toolset
// carrier when a toolset is declared, the platform server for implicit full
// access, none for a template without tools.
func (d declaration) serverBinding(cfg ComposeConfig) string {
	switch {
	case d.Toolset != nil:
		return CarrierName(cfg.MusterServer, d.Name)
	case d.bindsPlatformServer:
		return orDefault(cfg.MusterServer, DefaultMusterServer)
	}
	return ""
}

// mcpBinding binds the whole server: a per-tool selection (tools[]) needs
// controller-side discovery, which the platform turns off for muster.
func mcpBinding(server string) map[string]any {
	return map[string]any{"mcp": map[string]any{"server": map[string]any{"kind": kindRemoteMCPServer, "name": server}}}
}

// BuildToolsetCarrier composes the per-agent copy of the platform server that
// carries the toolset header: the platform spec (url, protocol, timeouts, tls,
// description) plus headersFrom X-Muster-Toolset. Any Authorization header of
// the platform server is dropped — a static header is applied last by the
// runtime and would override the caller's bearer, so every agent would act as
// that static identity; the names dropped are returned so the caller can
// warn. Never fails: the platform server exists (checked by the service).
func BuildToolsetCarrier(agent string, toolset []string, platform *unstructured.Unstructured, cfg ComposeConfig, caller string) (*unstructured.Unstructured, []string) {
	spec, _, _ := unstructured.NestedMap(platform.Object, "spec")
	if spec == nil {
		spec = map[string]any{}
	}
	spec = runtime.DeepCopyJSON(spec)
	header := ToolsetHeaderValue(toolset)
	var headers []any
	var dropped []string
	if existing, _ := spec["headersFrom"].([]any); existing != nil {
		for _, h := range existing {
			hm, _ := h.(map[string]any)
			name, _ := hm["name"].(string)
			switch {
			case strings.EqualFold(name, "Authorization"):
				dropped = append(dropped, name)
			case strings.EqualFold(name, ToolsetHeader):
				// Ours; replaced below.
			default:
				headers = append(headers, h)
			}
		}
	}
	headers = append(headers, map[string]any{"name": ToolsetHeader, "value": header})
	spec["headersFrom"] = headers
	desc, _ := spec["description"].(string)
	spec["description"] = fmt.Sprintf("%s — toolset [%s] of agent %s", orDefault(desc, "muster MCP gateway"), header, agent)

	labels := map[string]any{ManagedByLabel: ManagedByValue, AgentLabel: agent}
	if v, ok := platform.GetLabels()[DiscoveryLabel]; ok {
		labels[DiscoveryLabel] = v
	}
	metadata := map[string]any{
		"name":      CarrierName(cfg.MusterServer, agent),
		"namespace": platform.GetNamespace(),
		"labels":    labels,
	}
	if caller != "" {
		metadata["annotations"] = map[string]any{RequestedByAnnotation: caller}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": orDefault(cfg.APIVersion, DefaultAPIVersion),
		"kind":       kindRemoteMCPServer,
		"metadata":   metadata,
		"spec":       spec,
	}}, dropped
}

// ---- skills ------------------------------------------------------------------

// skillsSpec renders AgentTemplate.spec.skills: every entry an immutable
// source (git url + full commit, or a digest-pinned OCI reference), as the CRD
// requires. Anything else is refused with the reason, never dropped silently.
func skillsSpec(s *Skills) ([]any, error) {
	if s.IsEmpty() {
		return nil, nil
	}
	out := make([]any, 0, len(s.GitRefs)+len(s.Refs))
	seen := map[string]bool{}
	add := func(name string, source map[string]any) error {
		if seen[name] {
			return invalidf("skills: two skills would be named %q; give one of them a name", name)
		}
		seen[name] = true
		out = append(out, map[string]any{"name": name, "source": source})
		return nil
	}
	for i, g := range s.GitRefs {
		if !httpURL.MatchString(g.URL) {
			return nil, invalidf("skills.gitRefs[%d].url %q must be an http(s) URL", i, g.URL)
		}
		if !gitCommit.MatchString(g.Ref) {
			return nil, invalidf("skills.gitRefs[%d] (%s): ref %q must be a full git commit id (40 or 64 hex characters) — kagent pins skills to immutable sources; list_skills reports each skill's commit", i, g.URL, g.Ref)
		}
		path := strings.Trim(g.Path, "/")
		for _, seg := range strings.Split(path, "/") {
			if seg == ".." {
				return nil, invalidf("skills.gitRefs[%d].path %q must be relative without '..' segments", i, g.Path)
			}
		}
		source := map[string]any{"git": map[string]any{"url": g.URL, "commit": g.Ref}}
		if path != "" {
			source["path"] = path
		}
		if err := add(SkillName(g), source); err != nil {
			return nil, err
		}
	}
	for i, ref := range s.Refs {
		if !ociDigestRef.MatchString(ref) {
			return nil, invalidf("skills.refs[%d] %q must be a digest-pinned OCI reference (<repository>@sha256:<64 hex digits>) — kagent pins skills to immutable sources", i, ref)
		}
		if err := add(ociSkillName(ref), map[string]any{"oci": ref}); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// SkillName is a git skill's name: the explicit name, else the last path
// segment, else the repository name.
func SkillName(g SkillGitRef) string {
	if g.Name != "" {
		return g.Name
	}
	if p := strings.Trim(g.Path, "/"); p != "" {
		parts := strings.Split(p, "/")
		return parts[len(parts)-1]
	}
	repo := strings.TrimSuffix(strings.Trim(g.URL, "/"), ".git")
	parts := strings.Split(repo, "/")
	return parts[len(parts)-1]
}

// ociSkillName is an OCI skill's name: the repository's last path segment.
func ociSkillName(ref string) string {
	repo := ref
	if i := strings.Index(repo, "@"); i >= 0 {
		repo = repo[:i]
	}
	parts := strings.Split(repo, "/")
	last := parts[len(parts)-1]
	if i := strings.Index(last, ":"); i >= 0 {
		last = last[:i]
	}
	return last
}

// skillsOf reads spec.skills back into the request shape; sources the shape
// cannot express (bucket, an OCI source with a path) are reported as
// unmanaged fields.
func skillsOf(spec map[string]any) (*Skills, []string) {
	raw, _ := spec["skills"].([]any)
	if len(raw) == 0 {
		return nil, nil
	}
	s := &Skills{}
	var unmanaged []string
	for _, item := range raw {
		m, _ := item.(map[string]any)
		name, _ := m["name"].(string)
		source, _ := m["source"].(map[string]any)
		path, _ := source["path"].(string)
		switch {
		case source["git"] != nil:
			git, _ := source["git"].(map[string]any)
			url, _ := git["url"].(string)
			commit, _ := git["commit"].(string)
			s.GitRefs = append(s.GitRefs, SkillGitRef{URL: url, Path: path, Ref: commit, Name: name})
		case source["oci"] != nil && path == "":
			ref, _ := source["oci"].(string)
			s.Refs = append(s.Refs, ref)
		default:
			unmanaged = append(unmanaged, fmt.Sprintf("skills[%s].source", name))
		}
	}
	if s.IsEmpty() {
		s = nil
	}
	return s, unmanaged
}

// ---- reading a template back -----------------------------------------------

// toolingLabel reports a label another controller stamps and agent-manager
// preserves without treating it as the caller's (Flux provenance).
func toolingLabel(key string) bool {
	return strings.HasPrefix(key, "kustomize.toolkit.fluxcd.io/") || strings.HasPrefix(key, "helm.toolkit.fluxcd.io/")
}

// toolingAnnotation reports an annotation preserved the same way.
func toolingAnnotation(key string) bool {
	return key == "kubectl.kubernetes.io/last-applied-configuration" || key == RequestedByAnnotation
}

// declarationOf reads a served template (and its toolset carrier, nil when
// absent) back into the declaration agent-manager would have composed it
// from, and lists the spec fields it does not compose (unmanaged).
func declarationOf(tpl, carrier *unstructured.Unstructured, cfg ComposeConfig) (declaration, []string) {
	d := declaration{Spec: Spec{Name: tpl.GetName(), Namespace: tpl.GetNamespace()}}
	for k, v := range tpl.GetLabels() {
		switch {
		case k == ManagedByLabel, toolingLabel(k):
		case k == HarnessLabel:
			d.Harness = v
		default:
			if d.Labels == nil {
				d.Labels = map[string]string{}
			}
			d.Labels[k] = v
		}
	}
	for k, v := range tpl.GetAnnotations() {
		switch {
		case k == DisplayNameAnnotation:
			d.DisplayName = v
		case toolingAnnotation(k):
		default:
			if d.Annotations == nil {
				d.Annotations = map[string]string{}
			}
			d.Annotations[k] = v
		}
	}
	spec, _, _ := unstructured.NestedMap(tpl.Object, "spec")
	d.Description, _ = spec["description"].(string)
	d.SystemMessage, _ = spec["systemPrompt"].(string)
	if mc, _ := spec["modelConfig"].(map[string]any); mc != nil {
		d.ModelConfig, _ = mc["name"].(string)
	}
	var unmanaged []string
	d.Skills, unmanaged = skillsOf(spec)
	for _, key := range []string{"plugins", "promptTemplate", "systemPromptFrom"} {
		if _, ok := spec[key]; ok {
			unmanaged = append(unmanaged, key)
		}
	}
	platform := orDefault(cfg.MusterServer, DefaultMusterServer)
	carrierName := CarrierName(cfg.MusterServer, tpl.GetName())
	tools, _ := spec["tools"].([]any)
	for i, t := range tools {
		m, _ := t.(map[string]any)
		mcp, _ := m["mcp"].(map[string]any)
		if mcp == nil {
			unmanaged = append(unmanaged, fmt.Sprintf("tools[%d] (agent binding)", i))
			continue
		}
		server, _ := mcp["server"].(map[string]any)
		name, _ := server["name"].(string)
		if selection, _ := mcp["tools"].([]any); len(selection) > 0 {
			unmanaged = append(unmanaged, fmt.Sprintf("tools[%d].mcp.tools (per-tool selection of %s)", i, name))
			continue
		}
		switch name {
		case carrierName:
			d.Toolset = carrierToolset(carrier)
			if d.Toolset == nil {
				// The carrier is gone: the binding cannot resolve; report it.
				unmanaged = append(unmanaged, fmt.Sprintf("tools[%d] binds the toolset carrier %s, which does not exist", i, carrierName))
			}
		case platform:
			d.bindsPlatformServer = true
		default:
			unmanaged = append(unmanaged, fmt.Sprintf("tools[%d].mcp.server %s", i, name))
		}
	}
	sort.Strings(unmanaged)
	return d, unmanaged
}

// carrierToolset reads the X-Muster-Toolset header of a carrier; nil without
// a carrier or a literal header value.
func carrierToolset(carrier *unstructured.Unstructured) []string {
	if carrier == nil {
		return nil
	}
	if value := carrierHeader(carrier); value != "" {
		return ParseToolsetHeader(value)
	}
	return nil
}

// carrierHeader is the literal X-Muster-Toolset value of a RemoteMCPServer.
func carrierHeader(obj *unstructured.Unstructured) string {
	headers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "headersFrom")
	for _, h := range headers {
		hm, _ := h.(map[string]any)
		if name, _ := hm["name"].(string); strings.EqualFold(name, ToolsetHeader) {
			value, _ := hm["value"].(string)
			return value
		}
	}
	return ""
}

// declarationMap is the declaration as reported in update results (before /
// after) and diffed for the changed paths: the request's own field names.
func declarationMap(d declaration) map[string]any {
	out := map[string]any{}
	set := func(k, v string) {
		if v != "" {
			out[k] = v
		}
	}
	set("displayName", d.DisplayName)
	set("description", d.Description)
	set("systemMessage", d.SystemMessage)
	set("modelConfig", d.ModelConfig)
	set("harness", d.Harness)
	if !d.Skills.IsEmpty() {
		skills := map[string]any{}
		if len(d.Skills.Refs) > 0 {
			skills["refs"] = toAnySlice(d.Skills.Refs)
		}
		if len(d.Skills.GitRefs) > 0 {
			refs := make([]any, 0, len(d.Skills.GitRefs))
			for _, g := range d.Skills.GitRefs {
				entry := map[string]any{"url": g.URL, "ref": g.Ref, "name": SkillName(g)}
				if g.Path != "" {
					entry["path"] = g.Path
				}
				refs = append(refs, entry)
			}
			skills["gitRefs"] = refs
		}
		out["skills"] = skills
	}
	if d.Toolset != nil {
		out["toolset"] = toAnySlice(d.Toolset)
	}
	if len(d.Labels) > 0 {
		out["labels"] = toAnyMap(d.Labels)
	}
	if len(d.Annotations) > 0 {
		out["annotations"] = toAnyMap(d.Annotations)
	}
	return out
}

// changedPaths lists the dotted paths whose leaves differ.
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

// ---- rendering ---------------------------------------------------------------

// ToYAML renders an object as YAML (what the applied manifest looks like).
func ToYAML(obj *unstructured.Unstructured) string {
	out, err := yaml.Marshal(obj.Object)
	if err != nil {
		return fmt.Sprintf("# marshal error: %v\n", err)
	}
	return string(out)
}

// ComposeManifests renders the template and, when present, its carrier.
func ComposeManifests(tpl, carrier *unstructured.Unstructured) Manifests {
	m := Manifests{}
	if tpl != nil {
		m.AgentTemplate = ToYAML(tpl)
	}
	if carrier != nil {
		m.ToolsetCarrier = ToYAML(carrier)
	}
	return m
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func toAnySlice(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

func toAnyMap(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
