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

// DefaultModelFollower fills one Agent's empty modelConfig with its
// namespace's default model, as a default StorageClass fills a PVC: the
// platform's default agent, rendered without a model so a default marked later
// still reaches it. A modelConfig already set, by the follower or anyone else,
// is never changed. None marked: nothing; several: a warning, nothing. It runs
// without a caller, as the ServiceAccount (get and patch on that Agent, list
// on the namespace's ModelConfigs). It stands in for a namespace default
// ModelConfig in kagent itself, kagent-dev/kagent#3553.
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

// Run fills at once, then every defaultModelInterval until ctx ends.
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

// Follow sets the Agent's spec.template.modelConfig.name to the default
// model while it is empty.
func (f *DefaultModelFollower) Follow(ctx context.Context) error {
	obj, err := f.dyn.Resource(f.agents).Namespace(f.namespace).Get(ctx, f.agent, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if current, _, _ := unstructured.NestedString(obj.Object, "spec", "template", "modelConfig", "name"); current != "" {
		return nil
	}
	list, err := f.dyn.Resource(f.models).Namespace(f.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	marked := markedModelConfigs(list.Items)
	switch {
	case len(marked) == 0:
		return nil
	case len(marked) > 1:
		f.log.Warn("default model: several ModelConfigs marked as the default; the agent's modelConfig stays empty", "agent", f.namespace+"/"+f.agent, "marked", marked)
		return nil
	}
	def := marked[0]
	// The resourceVersion makes the write fail, not overwrite, when the Agent
	// changed since the read.
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"resourceVersion": obj.GetResourceVersion()}, "spec": map[string]any{"template": map[string]any{"modelConfig": map[string]any{"name": def}}}})
	if err != nil {
		return err
	}
	if _, err := f.dyn.Resource(f.agents).Namespace(f.namespace).Patch(ctx, f.agent, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: DefaultModelFieldManager}); err != nil {
		if apierrors.IsConflict(err) {
			return nil // changed since the read; the next look decides
		}
		return err
	}
	f.log.Info("default model: filled the agent's modelConfig", "agent", f.namespace+"/"+f.agent, "modelConfig", def)
	return nil
}
