package agents

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/giantswarm/agent-manager/internal/identity"
)

// defaultAgentCache keeps each caller's default agent for
// Config.DefaultAgentTTL: every agent listing reports it.
type defaultAgentCache struct {
	mu      sync.Mutex
	entries map[string]defaultAgentEntry
}

type defaultAgentEntry struct {
	agent   string
	expires time.Time
}

// maxDefaultAgentEntries bounds the cache; it is emptied when full.
const maxDefaultAgentEntries = 1024

func (c *defaultAgentCache) get(caller string, now time.Time) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[caller]
	if !ok || now.After(e.expires) {
		return "", false
	}
	return e.agent, true
}

func (c *defaultAgentCache) put(caller, agent string, expires time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil || len(c.entries) >= maxDefaultAgentEntries {
		c.entries = map[string]defaultAgentEntry{}
	}
	c.entries[caller] = defaultAgentEntry{agent: agent, expires: expires}
}

// defaultAgentAt is defaultAgent on the installation's own cluster, empty
// on a target cluster.
func (s *Service) defaultAgentAt(ctx context.Context, st *site) string {
	if st.loc.IsSet() {
		return ""
	}
	return s.defaultAgent(ctx)
}

// defaultAgent is the installation's default agent as the caller reads it:
// the Agent's namespace/name, as a portal agent id names it. It is the
// Agent labelled DefaultAgentLabel in a managed namespace, or the one a
// labelled HelmRelease renders there (its targetNamespace and release
// name, wherever the release lives); of several, a platform-owned one, then
// the first by namespace/name, with a warning. Empty when none.
func (s *Service) defaultAgent(ctx context.Context) string {
	ttl := s.cfg.DefaultAgentTTL
	caller := identity.Caller(ctx)
	now := time.Now()
	if ttl > 0 {
		if agent, ok := s.defaults.get(caller, now); ok {
			return agent
		}
	}
	agent, ok := s.readDefaultAgent(ctx)
	if ttl > 0 && ok {
		s.defaults.put(caller, agent, now.Add(ttl))
	}
	return agent
}

// readDefaultAgent reads the labelled objects: one list across all
// namespaces per kind, else (refused) one per managed namespace. ok is
// false when the caller's client could not be built.
func (s *Service) readDefaultAgent(ctx context.Context) (string, bool) {
	dyn, _, err := s.dyn(ctx)
	if err != nil {
		return "", false
	}
	managed := s.Managed(ctx)
	selector := metav1.ListOptions{LabelSelector: DefaultAgentLabel + "=true"}
	platform := map[string]bool{}
	for _, gvr := range []schema.GroupVersionResource{s.helmReleaseGVR(), s.agentGVR()} {
		items := s.labelled(ctx, dyn, gvr, managed, selector)
		for i := range items {
			obj := &items[i]
			ns, name := obj.GetNamespace(), obj.GetName()
			if gvr == s.helmReleaseGVR() {
				var local bool
				if ns, name, local = renderedAgent(obj); !local {
					continue
				}
			}
			if !slices.Contains(managed, ns) {
				continue
			}
			key := ns + "/" + name
			platform[key] = platform[key] || PlatformOwned(obj)
		}
	}
	if len(platform) == 0 {
		return "", true
	}
	keys := slices.Sorted(maps.Keys(platform))
	slices.SortStableFunc(keys, func(a, b string) int {
		switch {
		case platform[a] == platform[b]:
			return 0
		case platform[a]:
			return -1
		}
		return 1
	})
	if len(keys) > 1 {
		s.log.Warn("several agents are labelled the default; reporting one", "label", DefaultAgentLabel, "default", keys[0], "labelled", keys)
	}
	return keys[0], true
}

// labelled lists the objects of gvr matching selector: across all
// namespaces when the caller may, else in each managed namespace (a
// namespace the caller may not list is left out).
func (s *Service) labelled(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, managed []string, selector metav1.ListOptions) []unstructured.Unstructured {
	list, err := dyn.Resource(gvr).List(ctx, selector)
	if err == nil {
		return list.Items
	}
	var out []unstructured.Unstructured
	for _, ns := range managed {
		list, err := dyn.Resource(gvr).Namespace(ns).List(ctx, selector)
		if err != nil {
			s.log.Debug("default agent: list failed; leaving the namespace out", identity.LogAttr(ctx), "namespace", ns, "resource", gvr.Resource, "error", err)
			continue
		}
		out = append(out, list.Items...)
	}
	return out
}

// renderedAgent is the Agent a HelmRelease renders: in its targetNamespace
// (else its own), named agent.name, else its release name (Flux's default:
// [<targetNamespace>-]<name>). local is false for a release onto another
// cluster (spec.kubeConfig).
func renderedAgent(hr *unstructured.Unstructured) (ns, name string, local bool) {
	if _, found, _ := unstructured.NestedMap(hr.Object, "spec", "kubeConfig"); found {
		return "", "", false
	}
	target, _, _ := unstructured.NestedString(hr.Object, "spec", "targetNamespace")
	ns = orDefault(target, hr.GetNamespace())
	if name, _, _ = unstructured.NestedString(hr.Object, "spec", "values", "agent", "name"); name != "" {
		return ns, name, true
	}
	if name, _, _ = unstructured.NestedString(hr.Object, "spec", "releaseName"); name != "" {
		return ns, name, true
	}
	if target != "" {
		return ns, target + "-" + hr.GetName(), true
	}
	return ns, hr.GetName(), true
}
