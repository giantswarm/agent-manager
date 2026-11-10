package agents

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// DefaultModelFieldManager names the follower as the manager of the
// modelConfig it writes.
const DefaultModelFieldManager = "agent-manager-default-model"

// defaultModelInterval is how often the follower looks again.
const defaultModelInterval = 30 * time.Second

// DefaultModelFollower keeps one Agent on its namespace's default model while
// the Agent carries FollowsDefaultModelLabel=true: the platform's default
// agent, rendered without a model so a default marked later still reaches it.
// None or several marked: the Agent is left as it is. It runs without a
// caller, as the ServiceAccount (get and patch on that Agent, list on the
// namespace's ModelConfigs).
type DefaultModelFollower struct {
	dyn       dynamic.Interface
	agents    schema.GroupVersionResource
	models    schema.GroupVersionResource
	namespace string
	agent     string
	log       *slog.Logger
}

// NewDefaultModelFollower follows Agent namespace/agent; kagentAPIVersion is
// the served api.kagent.dev version.
func NewDefaultModelFollower(dyn dynamic.Interface, kagentAPIVersion, namespace, agent string, log *slog.Logger) *DefaultModelFollower {
	if log == nil {
		log = slog.Default()
	}
	gv := schema.GroupVersion{Group: KagentAPIGroup, Version: orDefault(kagentAPIVersion, DefaultKagentAPIVersion)}
	return &DefaultModelFollower{dyn: dyn, agents: gv.WithResource("agents"), models: gv.WithResource("modelconfigs"), namespace: namespace, agent: agent, log: log}
}

// Run follows at once, then every defaultModelInterval until ctx ends.
func (f *DefaultModelFollower) Run(ctx context.Context) {
	ticker := time.NewTicker(defaultModelInterval)
	defer ticker.Stop()
	for {
		if err := f.Follow(ctx); err != nil && ctx.Err() == nil {
			f.log.Error("default model: follow failed", "agent", f.namespace+"/"+f.agent, "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Follow patches the Agent's spec.template.modelConfig.name to the default
// model when the Agent follows it and the name differs.
func (f *DefaultModelFollower) Follow(ctx context.Context) error {
	obj, err := f.dyn.Resource(f.agents).Namespace(f.namespace).Get(ctx, f.agent, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if obj.GetLabels()[FollowsDefaultModelLabel] != "true" {
		return nil
	}
	list, err := f.dyn.Resource(f.models).Namespace(f.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	models := namespaceModels{namespace: f.namespace, marked: markedModelConfigs(list.Items)}
	for i := range list.Items {
		models.configs = append(models.configs, ModelConfig{Name: list.Items[i].GetName()})
	}
	def, ok := models.defaultModel()
	if !ok {
		f.log.Debug("default model: none or several marked; leaving the agent", "agent", f.namespace+"/"+f.agent, "marked", models.marked)
		return nil
	}
	if current, _, _ := unstructured.NestedString(obj.Object, "spec", "template", "modelConfig", "name"); current == def.Name {
		return nil
	}
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{"modelConfig": map[string]any{"name": def.Name}}}})
	if err != nil {
		return err
	}
	if _, err := f.dyn.Resource(f.agents).Namespace(f.namespace).Patch(ctx, f.agent, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: DefaultModelFieldManager}); err != nil {
		return err
	}
	f.log.Info("default model: agent follows", "agent", f.namespace+"/"+f.agent, "modelConfig", def.Name)
	return nil
}
