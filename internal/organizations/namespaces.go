package organizations

import (
	"context"
	"fmt"
	"slices"

	apov1alpha1 "github.com/giantswarm/agent-platform-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/giantswarm/agent-manager/internal/tenants"
)

// Namespace is an organization namespace as its labels describe it.
type Namespace struct {
	// Name is the namespace.
	Name string `json:"namespace"`
	// Organization and Parent name its AgentPlatformOrganization
	// (Parent/Organization); empty for a legacy tenant.
	Organization string `json:"organization,omitempty"`
	Parent       string `json:"parent,omitempty"`
	// Legacy is a Giant Swarm Organization's org-<name> namespace labelled
	// giantswarm.io/organization, kept until the move job (slice 18 of the
	// agent-platform-organizations plan) moves its agents.
	Legacy bool `json:"legacy,omitempty"`
}

// Namespaces finds the organization namespaces with agent-manager's own
// ServiceAccount, which may list namespaces read-only: RBAC cannot narrow a
// list by label, so it reads every namespace and keeps the labelled ones.
// Nothing it answers grants anything: what a caller may do in a namespace is
// asked as the caller.
type Namespaces struct {
	typed    kubernetes.Interface
	platform string
}

// NewNamespaces finds organization namespaces with typed, the
// ServiceAccount's client. platform (kagent.namespace) is never one.
func NewNamespaces(typed kubernetes.Interface, platform string) *Namespaces {
	return &Namespaces{typed: typed, platform: platform}
}

// List is every organization namespace that is not terminating, sorted by
// name: those labelled agent-platform.giantswarm.io/organization and the
// legacy tenants labelled giantswarm.io/organization.
func (n *Namespaces) List(ctx context.Context) ([]Namespace, error) {
	list, err := n.typed.CoreV1().Namespaces().List(ctx, metav1.ListOptions{LabelSelector: apov1alpha1.OrganizationLabel})
	if err != nil {
		return nil, fmt.Errorf("list the namespaces labelled %s: %w", apov1alpha1.OrganizationLabel, err)
	}
	var out []Namespace
	for i := range list.Items {
		if ns, ok := n.of(&list.Items[i]); ok {
			out = append(out, ns)
		}
	}
	legacy, err := tenants.List(ctx, n.typed, n.platform)
	if err != nil {
		return nil, err
	}
	for _, name := range legacy {
		if !slices.ContainsFunc(out, func(ns Namespace) bool { return ns.Name == name }) {
			out = append(out, Namespace{Name: name, Legacy: true})
		}
	}
	slices.SortFunc(out, func(a, b Namespace) int {
		switch {
		case a.Name < b.Name:
			return -1
		case a.Name > b.Name:
			return 1
		}
		return 0
	})
	return out, nil
}

// Get reads one namespace: ok is false when it is not an organization
// namespace (or a legacy tenant), or is terminating.
func (n *Namespaces) Get(ctx context.Context, name string) (Namespace, bool, error) {
	obj, err := n.typed.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Namespace{}, false, nil
	}
	if err != nil {
		return Namespace{}, false, fmt.Errorf("read namespace %s: %w", name, err)
	}
	if ns, ok := n.of(obj); ok {
		return ns, true, nil
	}
	if tenants.IsTenant(obj, n.platform) {
		return Namespace{Name: name, Legacy: true}, true, nil
	}
	return Namespace{}, false, nil
}

// of reads an organization namespace from its labels.
func (n *Namespaces) of(obj *corev1.Namespace) (Namespace, bool) {
	org, ok := obj.Labels[apov1alpha1.OrganizationLabel]
	if !ok || obj.Name == n.platform || obj.DeletionTimestamp != nil || obj.Status.Phase == corev1.NamespaceTerminating {
		return Namespace{}, false
	}
	return Namespace{Name: obj.Name, Organization: org, Parent: obj.Labels[apov1alpha1.ParentLabel]}, true
}

// Names are the organization namespaces' names: what the agent tools manage
// beside the configured namespaces.
func (n *Namespaces) Names(ctx context.Context) ([]string, error) {
	list, err := n.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(list))
	for _, ns := range list {
		out = append(out, ns.Name)
	}
	return out, nil
}

// Holds reports whether name is an organization namespace.
func (n *Namespaces) Holds(ctx context.Context, name string) (bool, error) {
	_, ok, err := n.Get(ctx, name)
	return ok, err
}
