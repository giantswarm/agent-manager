package agents

import (
	"context"
	"fmt"
	"slices"
	"strings"

	apov1alpha1 "github.com/giantswarm/agent-platform-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// A namespace's default model is the one ModelConfig there annotated
// apov1alpha1.DefaultModelConfigAnnotation=true, as with the default
// IngressClass: none annotated, or more than one, is no default. A create
// that names no modelConfig writes the default's name into the agent, so an
// agent never follows a later change of the default.

// IsMarkedDefault reports whether a ModelConfig carries the default mark.
func IsMarkedDefault(mc *unstructured.Unstructured) bool {
	return mc.GetAnnotations()[apov1alpha1.DefaultModelConfigAnnotation] == "true"
}

// markedModelConfigs are the sorted names of the ModelConfigs marked as the
// namespace default.
func markedModelConfigs(items []unstructured.Unstructured) []string {
	var marked []string
	for i := range items {
		if IsMarkedDefault(&items[i]) {
			marked = append(marked, items[i].GetName())
		}
	}
	slices.Sort(marked)
	return marked
}

// namespaceModels are the ModelConfigs of a namespace and the ones marked as
// its default.
type namespaceModels struct {
	namespace string
	configs   []ModelConfig
	marked    []string
}

// defaultModel is the namespace default; false when none or several are
// marked.
func (m namespaceModels) defaultModel() (ModelConfig, bool) {
	if len(m.marked) != 1 {
		return ModelConfig{}, false
	}
	return m.find(m.marked[0])
}

func (m namespaceModels) find(name string) (ModelConfig, bool) {
	for _, mc := range m.configs {
		if mc.Name == name {
			return mc, true
		}
	}
	return ModelConfig{}, false
}

func (m namespaceModels) names() []string {
	out := make([]string, 0, len(m.configs))
	for _, mc := range m.configs {
		out = append(out, mc.Name)
	}
	return out
}

// resolveModelConfig is the ModelConfig a create writes: name, which must
// exist in the site's namespace, else the namespace default. fromDefault
// reports that the default was taken.
func (s *Service) resolveModelConfig(ctx context.Context, st *site, name string) (mc ModelConfig, fromDefault bool, err error) {
	models, err := s.readModelConfigs(ctx, st)
	if err != nil {
		return ModelConfig{}, false, err
	}
	if strings.TrimSpace(name) != "" {
		mc, err := s.requireIn(ctx, st, models, name)
		return mc, false, err
	}
	if mc, ok := models.defaultModel(); ok {
		return mc, true, nil
	}
	if len(models.configs) == 0 {
		who, err := s.whoAddsModels(ctx, st)
		if err != nil {
			return ModelConfig{}, false, err
		}
		return ModelConfig{}, false, invalidf("modelConfig: namespace %s has no ModelConfigs, so it has no default model; %s", models.namespace, who)
	}
	const ask = "pass modelConfig, or have an admin mark one as the default (annotation " + apov1alpha1.DefaultModelConfigAnnotation + "=true)"
	if len(models.marked) > 1 {
		return ModelConfig{}, false, invalidf("modelConfig: namespace %s has no default model, %s are all marked as the default; %s. ModelConfigs: %s",
			models.namespace, strings.Join(models.marked, ", "), ask, strings.Join(models.names(), ", "))
	}
	return ModelConfig{}, false, invalidf("modelConfig: namespace %s has no default model; %s. ModelConfigs: %s", models.namespace, ask, strings.Join(models.names(), ", "))
}

// requireModelConfig fails with the valid names when name is not a ModelConfig
// of the site's namespace.
func (s *Service) requireModelConfig(ctx context.Context, st *site, name string) error {
	if strings.TrimSpace(name) == "" {
		return invalidf("modelConfig is required")
	}
	models, err := s.readModelConfigs(ctx, st)
	if err != nil {
		return err
	}
	_, err = s.requireIn(ctx, st, models, name)
	return err
}

func (s *Service) requireIn(ctx context.Context, st *site, models namespaceModels, name string) (ModelConfig, error) {
	if mc, ok := models.find(name); ok {
		return mc, nil
	}
	if len(models.configs) == 0 {
		who, err := s.whoAddsModels(ctx, st)
		if err != nil {
			return ModelConfig{}, err
		}
		return ModelConfig{}, invalidf("modelConfig %q does not exist in namespace %s (no ModelConfigs there; %s)", name, models.namespace, who)
	}
	return ModelConfig{}, invalidf("modelConfig %q does not exist in namespace %s; valid: %s", name, models.namespace, strings.Join(models.names(), ", "))
}

// whoAddsModels says who gives a namespace without ModelConfigs its first
// one: an admin of the organization in an organization namespace.
func (s *Service) whoAddsModels(ctx context.Context, st *site) (string, error) {
	if st.loc.IsSet() {
		return "an admin adds one in the portal", nil
	}
	tenant, err := s.isTenant(ctx, st.ns())
	if err != nil {
		return "", err
	}
	if tenant {
		return "an admin of the organization adds one in the portal or with set_organization_model", nil
	}
	return "a platform admin adds one in the portal", nil
}

// modelConfigNote is the Notes entry of a dry run whose ModelConfig has not
// resolved its references (its key Secret, say); empty when it has.
func modelConfigNote(mc ModelConfig) string {
	if mc.ResolvedRefs != nil && *mc.ResolvedRefs {
		return ""
	}
	state := "not reported yet"
	if mc.ResolvedRefs != nil {
		state = "False"
	}
	return fmt.Sprintf("modelConfig %s is not ResolvedRefs=True (%s): the agent does not run until it is", mc.Name, state)
}
