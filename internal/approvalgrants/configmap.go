package approvalgrants

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

// Labels and the data key of a grants ConfigMap.
const (
	LabelManagedBy      = "app.kubernetes.io/managed-by"
	LabelGrants         = "approval.agent-manager.giantswarm.io/grants"
	LabelAgentNamespace = "approval.agent-manager.giantswarm.io/agent-namespace"
	LabelAgentName      = "approval.agent-manager.giantswarm.io/agent-name"
	DataKey             = "grants.json"
	namePrefix          = "approval-grants-"
)

// document is the stored form.
type document struct {
	APIVersion         string  `json:"apiVersion"`
	Kind               string  `json:"kind"`
	Agent              Agent   `json:"agent"`
	Grants             []Grant `json:"grants"`
	PendingSessionSync bool    `json:"pendingSessionSync,omitempty"`
}

// ConfigMapStore keeps each agent's grants in a ConfigMap of the grants
// namespace, written with agent-manager's ServiceAccount.
type ConfigMapStore struct {
	maps typedcorev1.ConfigMapInterface
	log  *slog.Logger
}

// NewConfigMapStore is the store over the ConfigMaps of one namespace.
func NewConfigMapStore(maps typedcorev1.ConfigMapInterface) *ConfigMapStore {
	return &ConfigMapStore{maps: maps, log: slog.Default()}
}

// WithLogger logs skipped records to log.
func (s *ConfigMapStore) WithLogger(log *slog.Logger) *ConfigMapStore {
	if log != nil {
		s.log = log
	}
	return s
}

// Name is the ConfigMap holding an agent's grants.
func Name(agentUID string) string { return namePrefix + agentUID }

// labelValue keeps a label value within 63 characters: longer ones are
// replaced by a hash.
func labelValue(v string) string {
	if len(v) <= 63 {
		return v
	}
	sum := sha256.Sum256([]byte(v))
	return "h-" + hex.EncodeToString(sum[:])[:40]
}

// Get implements Store.
func (s *ConfigMapStore) Get(ctx context.Context, agent Agent) (Record, error) {
	cm, err := s.maps.Get(ctx, Name(agent.UID), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Record{Agent: agent}, nil
	}
	if err != nil {
		return Record{}, fmt.Errorf("read approval grants of %s: %w", agent.Key(), err)
	}
	r, err := decode(cm)
	if err != nil || r.Agent != agent {
		// A record that fails its checks, or names another agent, holds no
		// grant: it is replaced on the next write.
		reason := "it names another agent"
		if err != nil {
			reason = err.Error()
		}
		s.log.WarnContext(ctx, "approval grants record skipped", "configmap", cm.Name, "agent", agent.Key(), "reason", reason)
		return Record{Agent: agent, Version: cm.ResourceVersion}, nil
	}
	return r, nil
}

// decode reads a record and checks it: its document's version and kind, and
// that its object name and labels name the agent the document names.
func decode(cm *corev1.ConfigMap) (Record, error) {
	var doc document
	if err := json.Unmarshal([]byte(cm.Data[DataKey]), &doc); err != nil {
		return Record{}, fmt.Errorf("approval grants %s are unreadable: %w", cm.Name, err)
	}
	if doc.APIVersion != APIVersion || doc.Kind != Kind {
		return Record{}, fmt.Errorf("approval grants %s are %s %s, not %s %s", cm.Name, doc.APIVersion, doc.Kind, APIVersion, Kind)
	}
	switch {
	case doc.Agent.UID == "" || cm.Name != Name(doc.Agent.UID):
		return Record{}, fmt.Errorf("approval grants %s name agent uid %q, not the one in the object's name", cm.Name, doc.Agent.UID)
	case cm.Labels[LabelGrants] != "v1" || cm.Labels[LabelAgentNamespace] != labelValue(doc.Agent.Namespace) || cm.Labels[LabelAgentName] != labelValue(doc.Agent.Name):
		return Record{}, fmt.Errorf("approval grants %s carry labels that do not name agent %s", cm.Name, doc.Agent.Key())
	}
	return Record{Agent: doc.Agent, Grants: doc.Grants, PendingSessionSync: doc.PendingSessionSync, Version: cm.ResourceVersion}, nil
}

// ListByNamespace implements Store.
func (s *ConfigMapStore) ListByNamespace(ctx context.Context, namespace string) ([]Record, error) {
	sel := labels.Set{LabelGrants: "v1", LabelAgentNamespace: labelValue(namespace)}.AsSelector().String()
	list, err := s.maps.List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return nil, fmt.Errorf("list approval grants of namespace %s: %w", namespace, err)
	}
	out := make([]Record, 0, len(list.Items))
	for i := range list.Items {
		r, err := decode(&list.Items[i])
		if err != nil {
			s.log.WarnContext(ctx, "approval grants record skipped", "configmap", list.Items[i].Name, "reason", err.Error())
			continue
		}
		if r.Agent.Namespace == namespace {
			out = append(out, r)
		}
	}
	return out, nil
}

// Replace implements Store.
func (s *ConfigMapStore) Replace(ctx context.Context, r Record) error {
	name := Name(r.Agent.UID)
	if len(r.Grants) == 0 && !r.PendingSessionSync {
		if r.Version == "" {
			return nil
		}
		err := s.maps.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{ResourceVersion: &r.Version}})
		switch {
		case apierrors.IsNotFound(err):
			return nil
		case apierrors.IsConflict(err):
			return ErrConflict
		case err != nil:
			return fmt.Errorf("delete approval grants of %s: %w", r.Agent.Key(), err)
		}
		return nil
	}
	data, err := json.Marshal(document{APIVersion: APIVersion, Kind: Kind, Agent: r.Agent, Grants: r.Grants, PendingSessionSync: r.PendingSessionSync})
	if err != nil {
		return fmt.Errorf("encode approval grants: %w", err)
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, ResourceVersion: r.Version, Labels: map[string]string{
			LabelManagedBy:      "agent-manager",
			LabelGrants:         "v1",
			LabelAgentNamespace: labelValue(r.Agent.Namespace),
			LabelAgentName:      labelValue(r.Agent.Name),
		}},
		Data: map[string]string{DataKey: string(data)},
	}
	if r.Version == "" {
		_, err = s.maps.Create(ctx, cm, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			return ErrConflict
		}
	} else {
		_, err = s.maps.Update(ctx, cm, metav1.UpdateOptions{})
		if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
			return ErrConflict
		}
	}
	if err != nil {
		return fmt.Errorf("write approval grants of %s: %w", r.Agent.Key(), err)
	}
	return nil
}

// Delete implements Store.
func (s *ConfigMapStore) Delete(ctx context.Context, agentUID string) error {
	err := s.maps.Delete(ctx, Name(agentUID), metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete approval grants: %w", err)
	}
	return nil
}
