package agents

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// The composition mirrors the portal's composeManifests.ts: an agent is a Flux
// HelmRelease with inline values following the Generic agent chart's 2.x
// schema (agent, modelConfig, skills, plugins, toolset, muster as top-level
// keys) that renders from the shared per-namespace OCIRepository named after
// the chart, which tracks the chart by the 2.x range so every agent follows
// the latest 2.x release.

// ComposeConfig is the platform side of the composition.
type ComposeConfig struct {
	// ChartOCIURL is the agent chart, e.g. oci://gsoci.azurecr.io/charts/giantswarm/agent.
	ChartOCIURL string
	// ChartName is the OCIRepository's name (the chart name).
	ChartName string
	// ChartSemver is the OCIRepository ref.semver range (2.x).
	ChartSemver string
	// ChartSemverFilter is the OCIRepository ref.semverFilter: the regular
	// expression the tags must match before the range is evaluated. Empty
	// composes none.
	ChartSemverFilter string
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
	// MusterURL is the platform's muster MCP URL, composed as the chart value
	// muster.url; empty composes nothing and the chart default applies.
	MusterURL string
	// TargetMusterURL is the muster MCP URL composed into an agent on a
	// workload cluster: one those clusters reach (the installation's public
	// endpoint). Empty: agents are refused a target cluster.
	TargetMusterURL string
	// HarnessName is the platform Harness an agent runs on unless it names
	// another: composed as the chart value agent.harness, the Agent object's
	// spec.harnessRef.name.
	HarnessName string
	// SkillsGitAuthSecretName is the installation's skills credential: the
	// Secret (key `token`) in the agent's namespace that every agent with a
	// git skill or git plugin reads them with, unless the agent names its
	// own. Empty: such agents fetch anonymously.
	SkillsGitAuthSecretName string
}

// Defaults of the composition, the values composeManifests.ts uses.
// FieldManager names this service as the manager of the fields it writes on
// HelmRelease and OCIRepository objects (metadata.managedFields[].manager): a
// field manager names the tool that applied a field, never the person — the
// caller is recorded in the requestedBy annotation and in the apiserver audit
// log (every write runs with the caller's token).
const FieldManager = "agent-manager"

const (
	DefaultChartOCIURL = "oci://gsoci.azurecr.io/charts/giantswarm/agent"
	// DefaultChartSemver is the range the per-namespace OCIRepository tracks:
	// every 2.x release of the Generic chart, never a pre-release build and
	// never a next major.
	DefaultChartSemver             = "2.x"
	DefaultHelmReleaseInterval     = "10m"
	DefaultOCIRepositoryInterval   = "30m"
	DefaultHelmReleaseAPIVersion   = "helm.toolkit.fluxcd.io/v2"
	DefaultOCIRepositoryAPIVersion = "source.toolkit.fluxcd.io/v1"
	// DefaultHarnessName is the platform Harness every agent runs on.
	DefaultHarnessName = "kagent"
)

// The Generic chart values contract since 1.x, for anyone rewriting 0.x
// values: RemovedValuePaths are the 0.x keys the chart refuses
// (additionalProperties: false), RenamedValuePaths the ones that moved.
var (
	RemovedValuePaths = []string{
		"agent.runtime", "replicas", "resources", "nodeSelector", "tolerations",
		"muster.serverRef", "muster.allowedHeaders", "muster.stsWellKnownUri", "skills.gitAuthSecretRef",
	}
	RenamedValuePaths = map[string]string{"muster.toolNames": "muster.tools"}
)

// SkillsGitAuthValuesKey is the chart value that fans one read credential
// out to every git skill and git plugin: {name: <Secret>}.
const SkillsGitAuthValuesKey = "skillsGitAuthSecretRef"

// skillsGitAuth is the credential Secret an agent with these skills and
// plugins reads them with: its own, else the installation's; none without a
// git source.
func skillsGitAuth(own string, skills Skills, plugins Plugins, cfg ComposeConfig) string {
	hasGit := false
	for _, sk := range skills {
		hasGit = hasGit || sk.Git != nil
	}
	for _, pl := range plugins {
		hasGit = hasGit || pl.Git != nil
	}
	if !hasGit {
		return ""
	}
	return orDefault(own, cfg.SkillsGitAuthSecretName)
}

// MaxSystemMessageLength is the agent chart's cap on agent.systemMessage: the
// compiled agent config must fit Substrate's 32768-character env value limit.
const MaxSystemMessageLength = 20000

// ValidateSystemMessage checks the system prompt against the chart's cap,
// counted in characters (code points), as the chart schema counts it.
func ValidateSystemMessage(message string) error {
	if length := utf8.RuneCountInString(message); length > MaxSystemMessageLength {
		return invalidf("systemMessage is %d characters; the agent chart accepts at most %d. Move long reference material into a skill", length, MaxSystemMessageLength)
	}
	return nil
}

var dns1123 = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// ValidateName checks the technical name: a DNS-1123 label of at most 63
// characters, as the Agent object, Helm and the chart's schema require.
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

// BuildValues composes the chart values from a spec whose skills are pinned
// (see pinSkills). Only what the caller set is emitted so the chart's
// defaults apply to everything else — the same rule as the portal (an empty
// prompt means "the chart's default prompt").
func BuildValues(spec Spec, cfg ComposeConfig) map[string]any {
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
	// The Harness the caller named, else the platform's: the Agent object's
	// spec.harnessRef.name.
	agent["harness"] = orDefault(spec.Harness, orDefault(cfg.HarnessName, DefaultHarnessName))
	if len(spec.Egress) > 0 {
		agent["egress"] = toAnySlice(spec.Egress)
	}
	values := map[string]any{
		"agent":       agent,
		"modelConfig": map[string]any{"name": spec.ModelConfig},
	}
	if skills := skillsValues(spec.Skills); skills != nil {
		values["skills"] = skills
	}
	if plugins := pluginsValues(spec.Plugins); plugins != nil {
		values["plugins"] = plugins
	}
	if name := skillsGitAuth(spec.GitAuthSecretName, spec.Skills, spec.Plugins, cfg); name != "" {
		values[SkillsGitAuthValuesKey] = map[string]any{"name": name}
	}
	if len(spec.Toolset) > 0 {
		// Exactly the declared list, as the chart's top-level value. Never
		// muster.tools: that key narrows the binding, not the toolset.
		values[ToolsetValuesKey] = toAnySlice(spec.Toolset)
	}
	if cfg.MusterURL != "" {
		values["muster"] = map[string]any{"url": cfg.MusterURL}
	}
	if len(spec.Labels) > 0 {
		values["labels"] = toAnyMap(spec.Labels)
	}
	if len(spec.Annotations) > 0 {
		values["annotations"] = toAnyMap(spec.Annotations)
	}
	return values
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
		"kind":       kindOCIRepository,
		"metadata": map[string]any{
			"name":      orDefault(cfg.ChartName, "agent"),
			"namespace": namespace,
			"labels":    map[string]any{ManagedByLabel: ManagedByValue},
		},
		"spec": map[string]any{
			"interval": orDefault(cfg.OCIRepositoryInterval, DefaultOCIRepositoryInterval),
			"url":      orDefault(cfg.ChartOCIURL, DefaultChartOCIURL),
			"ref":      chartRef(cfg),
		},
	}}
}

// chartRef is the OCIRepository's ref: the range, and the tag filter where one
// is configured.
func chartRef(cfg ComposeConfig) map[string]any {
	ref := map[string]any{"semver": orDefault(cfg.ChartSemver, DefaultChartSemver)}
	if cfg.ChartSemverFilter != "" {
		ref["semverFilter"] = cfg.ChartSemverFilter
	}
	return ref
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
