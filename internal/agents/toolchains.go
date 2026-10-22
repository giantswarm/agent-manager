package agents

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agent-manager/internal/kagent"
	apiv1alpha1 "github.com/giantswarm/agent-manager/internal/kagent/gen/kagent/api/v1alpha1"
	"github.com/giantswarm/agent-manager/internal/oci"
)

// A toolchain is a named set of tools published as its own OCI image,
// <repository>/<name>:<version> (`latest` the newest), which kagent mounts
// read-only at /opt/kagent/tools/<name> into one Session's actor beside its
// Harness's unchanged image. start_session names a Session's toolchains with
// their versions; one that names none gets the agent's default toolchains,
// kept as DefaultToolchainsAnnotation in the chart's annotations value, the
// way the default workspace is. Each version is resolved to its digest when
// the Session starts, and the Session keeps it. The image declares what it
// is in its labels: its name, the Harness runtimes it serves, and the egress
// origins its tools need, which the Session's actor reaches beside its
// agent's. A toolchain that does not declare the agent's runtime is refused
// before the Session is created.

// DefaultToolchainsAnnotation lists the agent's default toolchains,
// comma-separated name[@version] entries, on the Agent object through the
// chart's annotations value. Set only by defaultToolchains.
const DefaultToolchainsAnnotation = "agent-platform.giantswarm.io/default-toolchains"

// The labels a toolchain image declares itself with.
const (
	ToolchainNameLabel      = "io.giantswarm.toolchain.name"
	ToolchainHarnessesLabel = "io.giantswarm.toolchain.harnesses"
	ToolchainEgressLabel    = "io.giantswarm.toolchain.egress"
)

const (
	// latestToolchainVersion is the version of a toolchain named without one.
	latestToolchainVersion = "latest"
	// MaxToolchains bounds a Session's toolchains, as kagent bounds its tool
	// images.
	MaxToolchains = 16
	// maxToolchainEgress bounds a toolchain's origins, as kagent bounds a
	// tool image's.
	maxToolchainEgress = 32
)

var (
	// toolchainNamePattern is kagent's rule for a tool image's name, the
	// directory it is mounted at.
	toolchainNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,56}[a-z0-9])?$`)
	// toolchainVersionPattern is an OCI tag.
	toolchainVersionPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
)

// ToolchainRegistry reads toolchain images: the digest a tag resolves to and
// an image's labels (oci.Registry).
type ToolchainRegistry interface {
	ManifestDigest(ctx context.Context, ref oci.Reference, tag string) (string, error)
	ImageLabels(ctx context.Context, ref oci.Reference, digest string) (map[string]string, error)
}

// ToolchainSource is where the installation's toolchains are published: the
// repository <name> is appended to, and the registry it is read from.
type ToolchainSource struct {
	Repository oci.Reference
	Registry   ToolchainRegistry
}

// Toolchain is a toolchain resolved for a Session: the version it was named
// with, the digest-pinned image kagent mounts and the origins it declares.
type Toolchain struct {
	Name string `json:"name"`
	// Version is the version the toolchain was named with; empty for a
	// session a retried start answers, created with another resolution.
	Version string   `json:"version,omitempty"`
	Image   string   `json:"image"`
	Egress  []string `json:"egress,omitempty"`
}

// toolchainRef is a toolchain as a request names it: name[@version].
type toolchainRef struct {
	name, version string
}

func (r toolchainRef) String() string {
	if r.version == "" {
		return r.name
	}
	return r.name + "@" + r.version
}

// parseToolchains reads name[@version] entries: what start_session and an
// agent's defaults name. what prefixes every refusal.
func parseToolchains(what string, entries []string) ([]toolchainRef, error) {
	if len(entries) > MaxToolchains {
		return nil, invalidf("%s: at most %d toolchains, got %d", what, MaxToolchains, len(entries))
	}
	out := make([]toolchainRef, 0, len(entries))
	for _, entry := range entries {
		name, version, hasVersion := strings.Cut(strings.TrimSpace(entry), "@")
		if !toolchainNamePattern.MatchString(name) {
			return nil, invalidf("%s: %q: want name[@version], the name lower-case letters, digits and dashes (at most 58)", what, entry)
		}
		if hasVersion && !toolchainVersionPattern.MatchString(version) {
			return nil, invalidf("%s: %q: the version is no image tag", what, entry)
		}
		if slices.ContainsFunc(out, func(r toolchainRef) bool { return r.name == name }) {
			return nil, invalidf("%s: toolchain %s is named twice", what, name)
		}
		out = append(out, toolchainRef{name: name, version: version})
	}
	return out, nil
}

// ValidateDefaultToolchains checks an agent's default toolchains' form; the
// registry's answer is checkDefaultToolchains'.
func ValidateDefaultToolchains(entries []string, target Target) error {
	if len(entries) == 0 {
		return nil
	}
	if target.IsSet() {
		return invalidf("defaultToolchains: Sessions start only on agents of the installation's own cluster, so an agent on a workload cluster has no default toolchains")
	}
	_, err := parseToolchains("defaultToolchains", entries)
	return err
}

// refuseDefaultToolchainsAnnotation refuses the annotation passed through
// annotations with a value other than current.
func refuseDefaultToolchainsAnnotation(requested map[string]string, current string) error {
	if v, ok := requested[DefaultToolchainsAnnotation]; ok && v != current {
		return invalidf("annotation %s is set by defaultToolchains", DefaultToolchainsAnnotation)
	}
	return nil
}

// splitList reads a comma-separated list, empty entries dropped.
func splitList(v string) []string {
	var out []string
	for _, item := range strings.Split(v, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// defaultToolchainsOf reads the default toolchains from the chart's values.
func defaultToolchainsOf(values map[string]any) []string {
	annotations, _ := values["annotations"].(map[string]any)
	v, _ := annotations[DefaultToolchainsAnnotation].(string)
	return splitList(v)
}

// setDefaultToolchains records entries in the chart's annotations value;
// none removes it.
func setDefaultToolchains(values map[string]any, entries []string) {
	annotations, _ := values["annotations"].(map[string]any)
	if len(entries) == 0 {
		if annotations != nil {
			delete(annotations, DefaultToolchainsAnnotation)
			if len(annotations) == 0 {
				delete(values, "annotations")
			}
		}
		return
	}
	if annotations == nil {
		annotations = map[string]any{}
	}
	trimmed := make([]string, len(entries))
	for i, e := range entries {
		trimmed[i] = strings.TrimSpace(e)
	}
	annotations[DefaultToolchainsAnnotation] = strings.Join(trimmed, ",")
	values["annotations"] = annotations
}

// mergeDefaultToolchains carries the default toolchains of before into after,
// or the ones upd sets (an empty list removes them). annotations never change
// them.
func mergeDefaultToolchains(before, after map[string]any, upd Update) error {
	current := defaultToolchainsOf(before)
	if upd.Annotations != nil {
		if err := refuseDefaultToolchainsAnnotation(*upd.Annotations, strings.Join(current, ",")); err != nil {
			return err
		}
	}
	next := current
	if upd.DefaultToolchains != nil {
		next = *upd.DefaultToolchains
		if err := ValidateDefaultToolchains(next, upd.Target); err != nil {
			return err
		}
	}
	setDefaultToolchains(after, next)
	return nil
}

var errToolchainsUnsupported = fmt.Errorf("%w: this installation publishes no toolchains (--toolchain-repository)", ErrUnsupported)

// resolveToolchains resolves each toolchain to its digest-pinned image and
// checks that it declares runtime, the agent's Harness runtime. harness names
// the Harness in refusals.
func (s *Service) resolveToolchains(ctx context.Context, harness, runtime string, refs []toolchainRef) ([]Toolchain, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	src := s.cfg.Toolchains
	if src == nil {
		return nil, errToolchainsUnsupported
	}
	out := make([]Toolchain, 0, len(refs))
	for _, ref := range refs {
		version := orDefault(ref.version, latestToolchainVersion)
		repo := src.Repository
		repo.Repository += "/" + ref.name
		image := oci.ImageReference{Reference: repo, Tag: version}
		digest, err := src.Registry.ManifestDigest(ctx, repo, version)
		if errors.Is(err, oci.ErrNotFound) {
			return nil, invalidf("toolchain %s: version %s is not published (%s/%s:%s)", ref.name, version, repo.Host, repo.Repository, version)
		}
		if err != nil {
			return nil, fmt.Errorf("toolchain %s@%s could not be resolved to a digest: %w", ref.name, version, err)
		}
		image.Digest = digest
		labels, err := src.Registry.ImageLabels(ctx, repo, digest)
		if err != nil {
			return nil, fmt.Errorf("toolchain %s@%s: its labels could not be read: %w", ref.name, version, err)
		}
		if declared := labels[ToolchainNameLabel]; declared != ref.name {
			return nil, invalidf("toolchain %s@%s: %s is no toolchain image of that name (label %s is %q)", ref.name, version, image.Pinned(), ToolchainNameLabel, declared)
		}
		if serves := splitList(labels[ToolchainHarnessesLabel]); !slices.Contains(serves, runtime) {
			return nil, invalidf("toolchain %s@%s does not fit Harness %s: it serves %s, and the Harness runs %s (%s)", ref.name, version, harness, listOrNone(serves), runtime, runtimeLabel(runtime))
		}
		egress := splitList(labels[ToolchainEgressLabel])
		if len(egress) > maxToolchainEgress {
			return nil, invalidf("toolchain %s@%s declares %d egress origins, more than %d", ref.name, version, len(egress), maxToolchainEgress)
		}
		out = append(out, Toolchain{Name: ref.name, Version: version, Image: image.Pinned(), Egress: egress})
	}
	return out, nil
}

// checkDefaultToolchains refuses default toolchains of values that are not
// published or do not fit the Harness, at write time. Without a toolchain
// source the defaults are refused as unsupported.
func (s *Service) checkDefaultToolchains(ctx context.Context, harness, runtime string, values map[string]any) error {
	entries := defaultToolchainsOf(values)
	if len(entries) == 0 {
		return nil
	}
	refs, err := parseToolchains("defaultToolchains", entries)
	if err != nil {
		return err
	}
	if _, err := s.resolveToolchains(ctx, harness, runtime, refs); err != nil {
		if isDomainError(err) {
			return fmt.Errorf("defaultToolchains: %w", err)
		}
		return err
	}
	return nil
}

// sessionToolchains resolves the toolchains a start names, else the agent's
// defaults, unless noToolchains: the tool images CreateSession is given.
func (s *Service) sessionToolchains(ctx context.Context, ref kagent.Ref, named []string, noToolchains bool) ([]Toolchain, error) {
	if noToolchains && len(named) > 0 {
		return nil, invalidf("toolchains and noToolchains exclude each other")
	}
	if noToolchains {
		return nil, nil
	}
	what := "toolchains"
	if len(named) == 0 {
		defaults, err := s.defaultToolchainsOfAgent(ctx, ref.Namespace, ref.Name)
		if err != nil || len(defaults) == 0 {
			return nil, err
		}
		named, what = defaults, "the agent's defaultToolchains"
	}
	refs, err := parseToolchains(what, named)
	if err != nil {
		return nil, err
	}
	harness, runtime, err := s.agentHarness(ctx, ref)
	if err != nil {
		return nil, err
	}
	return s.resolveToolchains(ctx, harness, runtime, refs)
}

// defaultToolchainsOfAgent is the default toolchains of the agent as its
// Agent object carries them; none for an agent without them or without an
// object.
func (s *Service) defaultToolchainsOfAgent(ctx context.Context, namespace, name string) ([]string, error) {
	st, err := s.site(ctx, In(namespace))
	if err != nil {
		return nil, err
	}
	obj, err := s.getAgentObject(ctx, st.agent, namespace, name)
	if err != nil || obj == nil {
		return nil, err
	}
	return splitList(obj.GetAnnotations()[DefaultToolchainsAnnotation]), nil
}

// agentHarness is the Harness the agent's Agent object runs on, with its
// runtime.
func (s *Service) agentHarness(ctx context.Context, ref kagent.Ref) (string, string, error) {
	st, err := s.site(ctx, In(ref.Namespace))
	if err != nil {
		return "", "", err
	}
	obj, err := s.getAgentObject(ctx, st.agent, ref.Namespace, ref.Name)
	if err != nil {
		return "", "", err
	}
	if obj == nil {
		return "", "", notFoundf("agent %s has no Agent object, so its Harness is not known", ref.Tenant())
	}
	name, _, _ := unstructured.NestedString(obj.Object, "spec", "harnessRef", "name")
	harness, h, err := s.harnessFor(ctx, st.agent, ref.Namespace, name)
	return harness, harnessRuntime(h), err
}

// toolImages is the toolchains as kagent's CreateSession takes them.
func toolImages(toolchains []Toolchain) []*apiv1alpha1.SessionToolImage {
	if len(toolchains) == 0 {
		return nil
	}
	out := make([]*apiv1alpha1.SessionToolImage, 0, len(toolchains))
	for _, t := range toolchains {
		out = append(out, &apiv1alpha1.SessionToolImage{Name: t.Name, Oci: t.Image, EgressDestinations: t.Egress})
	}
	return out
}

// mountedToolchains is the toolchains a created session mounts, as the
// controller recorded them: a retried start answers the session the first
// attempt created, whose digests stand even when a version moved since.
// False when the controller recorded none of the requested ones, the answer
// of one without session tool images, which creates the session without.
func mountedToolchains(session *apiv1alpha1.Session, requested []Toolchain) ([]Toolchain, bool) {
	if len(requested) == 0 {
		return nil, true
	}
	got := session.GetToolImages()
	if len(got) == 0 {
		return nil, false
	}
	out := make([]Toolchain, 0, len(got))
	for _, image := range got {
		t := Toolchain{Name: image.GetName(), Image: image.GetOci(), Egress: image.GetEgressDestinations()}
		if i := slices.IndexFunc(requested, func(r Toolchain) bool { return r.Name == t.Name && r.Image == t.Image }); i >= 0 {
			t.Version = requested[i].Version
		}
		out = append(out, t)
	}
	return out, true
}
