package agents

import (
	"context"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agent-manager/internal/identity"
)

// maxDefaultAgentEntries bounds the callers whose default agent is kept.
const maxDefaultAgentEntries = 1024

// defaultAgentAt is defaultAgent on the installation's own cluster, empty
// on a target cluster.
func (s *Service) defaultAgentAt(ctx context.Context, st *site) string {
	if st.loc.IsSet() {
		return ""
	}
	return s.defaultAgent(ctx)
}

// defaultAgent is the installation's default agent as the caller reads it:
// the namespace/name of the Agent labelled DefaultAgentLabel in a managed
// namespace, as a portal agent id names it; of several, a platform-owned
// one, then the first by namespace/name, with a warning. Empty when none.
// Each caller's answer is kept for Config.DefaultAgentTTL.
func (s *Service) defaultAgent(ctx context.Context) string {
	ttl := s.cfg.DefaultAgentTTL
	caller := identity.Caller(ctx)
	if ttl > 0 {
		if agent, ok := s.defaults.Get(caller); ok {
			return agent.(string)
		}
	}
	agent, ok := s.readDefaultAgent(ctx)
	if ttl > 0 && ok {
		s.defaults.Add(caller, agent, ttl)
	}
	return agent
}

// readDefaultAgent lists the labelled Agents: once across all namespaces,
// else (refused) once per managed namespace. ok is false when the caller's
// client could not be built.
func (s *Service) readDefaultAgent(ctx context.Context) (string, bool) {
	dyn, _, err := s.dyn(ctx)
	if err != nil {
		return "", false
	}
	managed := s.Managed(ctx)
	selector := metav1.ListOptions{LabelSelector: DefaultAgentLabel + "=true"}
	var labelled []*unstructured.Unstructured
	items, err := dyn.Resource(s.agentGVR()).List(ctx, selector)
	if err == nil {
		for i := range items.Items {
			if slices.Contains(managed, items.Items[i].GetNamespace()) {
				labelled = append(labelled, &items.Items[i])
			}
		}
	} else {
		for _, ns := range managed {
			list, err := dyn.Resource(s.agentGVR()).Namespace(ns).List(ctx, selector)
			if err != nil {
				s.log.Debug("default agent: list failed; leaving the namespace out", identity.LogAttr(ctx), "namespace", ns, "error", err)
				continue
			}
			for i := range list.Items {
				labelled = append(labelled, &list.Items[i])
			}
		}
	}
	if len(labelled) == 0 {
		return "", true
	}
	slices.SortFunc(labelled, func(a, b *unstructured.Unstructured) int {
		if pa, pb := PlatformOwned(a), PlatformOwned(b); pa != pb {
			if pa {
				return -1
			}
			return 1
		}
		return strings.Compare(a.GetNamespace()+"/"+a.GetName(), b.GetNamespace()+"/"+b.GetName())
	})
	def := labelled[0].GetNamespace() + "/" + labelled[0].GetName()
	if len(labelled) > 1 {
		keys := make([]string, len(labelled))
		for i, obj := range labelled {
			keys[i] = obj.GetNamespace() + "/" + obj.GetName()
		}
		s.log.Warn("several agents are labelled the default; reporting one", "label", DefaultAgentLabel, "default", def, "labelled", keys)
	}
	return def, true
}
