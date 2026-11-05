// Package tenants finds the legacy tenant namespaces: a Giant Swarm
// Organization's org-<name>, labelled giantswarm.io/organization, where agents
// lived before the Agent Platform's organizations (epic
// giantswarm/giantswarm#38891). agent-manager no longer keeps anything in
// them: the tenant reconciler is gone, and agent-platform-operator keeps the
// organization namespaces. agent-manager still manages the agents a legacy
// tenant holds, and `agent-manager migrate --multi-tenant` still moves their
// chart source (giantswarm/agent-manager#170), until the move job of the
// migration slice (giantswarm/giantswarm#38990) moves them into an
// organization namespace and giantswarm/agent-manager#166 retires the
// setting.
package tenants

import (
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// OrganizationLabel marks a namespace as a legacy tenant.
	OrganizationLabel = "giantswarm.io/organization"
	// ObjectLabel marked what the retired tenant reconciler wrote; its copies
	// stay in the legacy tenants until the move job, and an object with it is
	// still never deleted with an agent.
	ObjectLabel = "agent-manager.giantswarm.io/tenant-object"
)

// IsTenant reports whether ns is a tenant beside platformNamespace: labelled,
// not the platform namespace, not going away.
func IsTenant(ns *corev1.Namespace, platformNamespace string) bool {
	if _, ok := ns.Labels[OrganizationLabel]; !ok {
		return false
	}
	return ns.Name != platformNamespace && ns.DeletionTimestamp == nil && ns.Status.Phase != corev1.NamespaceTerminating
}

// List are the tenant namespaces beside platformNamespace, sorted.
func List(ctx context.Context, typed kubernetes.Interface, platformNamespace string) ([]string, error) {
	list, err := typed.CoreV1().Namespaces().List(ctx, metav1.ListOptions{LabelSelector: OrganizationLabel})
	if err != nil {
		return nil, fmt.Errorf("list the namespaces labelled %s: %w", OrganizationLabel, err)
	}
	var found []string
	for i := range list.Items {
		if IsTenant(&list.Items[i], platformNamespace) {
			found = append(found, list.Items[i].Name)
		}
	}
	slices.Sort(found)
	return found, nil
}
