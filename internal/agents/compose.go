package agents

import (
	"fmt"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// The composition mirrors the portal's composeManifests.ts: an agent is a Flux
// HelmRelease with inline values following the agent chart's schema (agent,
// modelConfig, skills, muster as top-level keys) that renders from the shared
// per-namespace OCIRepository named after the chart, which tracks the chart by
// semver range so every agent follows the latest published release.

// ComposeConfig is the platform side of the composition.
type ComposeConfig struct {
	// ChartOCIURL is the agent chart, e.g. oci://gsoci.azurecr.io/charts/giantswarm/agent.
	ChartOCIURL string
	// ChartName is the OCIRepository's name (the chart name).
	ChartName string
	// ChartSemver is the OCIRepository ref.semver range (x.x.x).
	ChartSemver string
	// HelmReleaseInterval / OCIRepositoryInterval are the Flux intervals.
	HelmReleaseInterval   string
	OCIRepositoryInterval string
	// ServiceAccountName is HelmRelease.spec.serviceAccountName, required by a
	// Flux multi-tenancy admission policy in tenant namespaces; empty omits it.
	ServiceAccountName string
	// HelmReleaseAPIVersion / OCIRepositoryAPIVersion are the served Flux API
	// versions (helm.toolkit.fluxcd.io/v2, source.toolkit.fluxcd.io/v1).
	HelmReleaseAPIVersion   string
	OCIRepositoryAPIVersion string
}

// Defaults of the composition, the values composeManifests.ts uses.
const (
	DefaultChartOCIURL             = "oci://gsoci.azurecr.io/charts/giantswarm/agent"
	DefaultChartSemver             = "x.x.x"
	DefaultHelmReleaseInterval     = "10m"
	DefaultOCIRepositoryInterval   = "30m"
	DefaultHelmReleaseAPIVersion   = "helm.toolkit.fluxcd.io/v2"
	DefaultOCIRepositoryAPIVersion = "source.toolkit.fluxcd.io/v1"
)

var dns1123 = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// ValidateName checks the technical name: a DNS-1123 label of at most 63
// characters, as the Agent CRD, Helm and the chart's schema require.
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

// BuildValues composes the chart values from a spec. Only what the caller
// set is emitted so the chart's defaults apply to everything else — the same
// rule as the portal (an empty prompt means "the chart's default prompt").
func BuildValues(spec Spec) map[string]any {
	agent := map[string]any{
		// Pin the technical name so it does not depend on Flux's release-name
		// derivation.
		"name": spec.Name,
	}
	if strings.TrimSpace(spec.DisplayName) != "" {
		agent["displayName"] = spec.DisplayName
	}
	if strings.TrimSpace(spec.Description) != "" {
		agent["description"] = spec.Description
	}
	if strings.TrimSpace(spec.IconURL) != "" {
		agent["iconUrl"] = spec.IconURL
	}
	if strings.TrimSpace(spec.SystemMessage) != "" {
		agent["systemMessage"] = spec.SystemMessage
	}
	if strings.TrimSpace(spec.Runtime) != "" {
		agent["runtime"] = spec.Runtime
	}
	values := map[string]any{
		"agent":       agent,
		"modelConfig": map[string]any{"name": spec.ModelConfig},
	}
	if skills := skillsValues(spec.Skills); skills != nil {
		values["skills"] = skills
	}
	if len(spec.ToolNames) > 0 {
		values["muster"] = map[string]any{"toolNames": toAnySlice(spec.ToolNames)}
	}
	if len(spec.Labels) > 0 {
		values["labels"] = toAnyMap(spec.Labels)
	}
	if len(spec.Annotations) > 0 {
		values["annotations"] = toAnyMap(spec.Annotations)
	}
	return values
}

// skillsValues renders the skills block; nil when nothing is referenced (the
// chart's gitRefs is a plain array, an empty block is noise).
func skillsValues(s *Skills) map[string]any {
	if s.IsEmpty() {
		return nil
	}
	out := map[string]any{}
	if len(s.Refs) > 0 {
		out["refs"] = toAnySlice(s.Refs)
	}
	if len(s.GitRefs) > 0 {
		refs := make([]any, 0, len(s.GitRefs))
		for _, g := range s.GitRefs {
			entry := map[string]any{"url": g.URL}
			if g.Path != "" {
				entry["path"] = g.Path
			}
			if g.Ref != "" {
				entry["ref"] = g.Ref
			}
			entry["name"] = SkillName(g)
			refs = append(refs, entry)
		}
		out["gitRefs"] = refs
	}
	if s.GitAuthSecretName != "" {
		out["gitAuthSecretRef"] = map[string]any{"name": s.GitAuthSecretName}
	}
	return out
}

// SkillName is the directory a git skill mounts under: the explicit name,
// else the last path segment, else the repository name.
func SkillName(g SkillGitRef) string {
	if g.Name != "" {
		return g.Name
	}
	if p := strings.Trim(g.Path, "/"); p != "" {
		parts := strings.Split(p, "/")
		return parts[len(parts)-1]
	}
	repo := strings.TrimSuffix(strings.TrimSuffix(strings.Trim(g.URL, "/"), ".git"), "/")
	parts := strings.Split(repo, "/")
	return parts[len(parts)-1]
}

// BuildHelmRelease composes the HelmRelease of an agent.
func BuildHelmRelease(name, namespace string, values map[string]any, cfg ComposeConfig) *unstructured.Unstructured {
	spec := map[string]any{
		"interval": orDefault(cfg.HelmReleaseInterval, DefaultHelmReleaseInterval),
	}
	if cfg.ServiceAccountName != "" {
		spec["serviceAccountName"] = cfg.ServiceAccountName
	}
	spec["chartRef"] = map[string]any{
		"kind":      kindOCIRepository,
		"name":      orDefault(cfg.ChartName, "agent"),
		"namespace": namespace,
	}
	spec["values"] = values
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": orDefault(cfg.HelmReleaseAPIVersion, DefaultHelmReleaseAPIVersion),
		"kind":       "HelmRelease",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
			"labels":    map[string]any{ManagedByLabel: ManagedByValue},
		},
		"spec": spec,
	}}
}

// BuildOCIRepository composes the shared chart source of a namespace.
func BuildOCIRepository(namespace string, cfg ComposeConfig) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": orDefault(cfg.OCIRepositoryAPIVersion, DefaultOCIRepositoryAPIVersion),
		"kind":       "OCIRepository",
		"metadata": map[string]any{
			"name":      orDefault(cfg.ChartName, "agent"),
			"namespace": namespace,
			"labels":    map[string]any{ManagedByLabel: ManagedByValue},
		},
		"spec": map[string]any{
			"interval": orDefault(cfg.OCIRepositoryInterval, DefaultOCIRepositoryInterval),
			"url":      orDefault(cfg.ChartOCIURL, DefaultChartOCIURL),
			"ref":      map[string]any{"semver": orDefault(cfg.ChartSemver, DefaultChartSemver)},
		},
	}}
}

// ToYAML renders an object as YAML (what the applied manifest looks like).
func ToYAML(obj *unstructured.Unstructured) string {
	out, err := yaml.Marshal(obj.Object)
	if err != nil {
		return fmt.Sprintf("# marshal error: %v\n", err)
	}
	return string(out)
}

// ComposeManifests renders the pair for a namespace and values.
func ComposeManifests(name, namespace string, values map[string]any, cfg ComposeConfig) Manifests {
	return Manifests{
		OCIRepository: ToYAML(BuildOCIRepository(namespace, cfg)),
		HelmRelease:   ToYAML(BuildHelmRelease(name, namespace, values, cfg)),
		Values:        values,
	}
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
