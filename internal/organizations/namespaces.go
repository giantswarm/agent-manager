package organizations

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	apov1alpha1 "github.com/giantswarm/agent-platform-operator/api/v1alpha1"
	orgv1alpha1 "github.com/giantswarm/organization-operator/api/v1alpha1"
	"golang.org/x/sync/singleflight"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// listTTL is how long a list of the organization namespaces answers again: a
// burst of tool calls lists them once. A namespace deleted since stays in it
// that long at most; a read by name (Get, Holds, Find's miss) never waits for
// it.
const listTTL = 10 * time.Second

// listBudget bounds the one list that concurrent callers share, so that one
// caller that goes away does not fail the others.
const listBudget = 30 * time.Second

// Namespace identifies an organization or installation model target.
type Namespace struct {
	// Scope is platform for installation models and empty for organizations.
	Scope string `json:"scope,omitempty"`
	// Name is the namespace.
	Name string `json:"namespace"`
	// Organization names its Organization (security.giantswarm.io), and
	// OrganizationUID is that Organization's UID: an organization deleted and
	// created again under the same name keeps its namespace's name, never its
	// UID.
	Organization    string `json:"organization"`
	OrganizationUID string `json:"organizationUID,omitempty"`
	// prepared marks the target a credential operation recorded: its
	// Organization must still exist with OrganizationUID when it is saved.
	prepared bool
}

// Namespaces finds the organization namespaces and the Organizations with
// agent-manager's own ServiceAccount, which
// may list both read-only: RBAC cannot narrow a list by label or by the
// caller, so it reads them all. Nothing it answers grants anything: what a
// caller may see of them, and do with them, is asked as the caller.
type Namespaces struct {
	typed    kubernetes.Interface
	dyn      dynamic.Interface
	platform string

	// now is the clock of the list's cache.
	now    func() time.Time
	group  singleflight.Group
	mu     sync.Mutex
	listed []Namespace
	at     time.Time
}

// NewNamespaces finds organization namespaces with typed and Organizations
// with dyn, the ServiceAccount's clients.
// platform (kagent.namespace) is never an organization namespace.
func NewNamespaces(typed kubernetes.Interface, dyn dynamic.Interface, platform string) *Namespaces {
	return &Namespaces{typed: typed, dyn: dyn, platform: platform, now: time.Now}
}

// Organizations is every Organization that has the Agent Platform enabled.
func (n *Namespaces) Organizations(ctx context.Context) ([]orgv1alpha1.Organization, error) {
	list, err := n.dyn.Resource(GVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list the Organizations: %w", err)
	}
	out := make([]orgv1alpha1.Organization, 0, len(list.Items))
	for i := range list.Items {
		org, err := organizationOf(&list.Items[i])
		if err != nil {
			return nil, err
		}
		if agentPlatformEnabled(org) {
			out = append(out, *org)
		}
	}
	return out, nil
}

// Organization reads the Organization name: ok is false when it does not
// exist.
func (n *Namespaces) Organization(ctx context.Context, name string) (*orgv1alpha1.Organization, bool, error) {
	u, err := n.dyn.Resource(GVR).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read Organization %s: %w", name, err)
	}
	org, err := organizationOf(u)
	if err != nil {
		return nil, false, err
	}
	return org, true, nil
}

// List is every organization namespace, labelled
// agent-platform.giantswarm.io/organization, that is not terminating, sorted
// by name. A Giant Swarm Organization's org-<name>, labelled
// giantswarm.io/organization, is not one. The answer is at most listTTL old:
// concurrent callers share one list, and a failed list is not kept.
func (n *Namespaces) List(ctx context.Context) ([]Namespace, error) {
	if out, ok := n.cached(); ok {
		return out, nil
	}
	ch := n.group.DoChan("list", func() (any, error) {
		// A list that ended between the read above and this flight filled
		// the cache: it answers.
		if out, ok := n.cached(); ok {
			return out, nil
		}
		shared, cancel := context.WithTimeout(context.WithoutCancel(ctx), listBudget)
		defer cancel()
		started := n.now()
		out, err := n.list(shared, apov1alpha1.OrganizationLabel)
		if err != nil {
			return nil, err
		}
		n.mu.Lock()
		n.listed, n.at = out, started
		n.mu.Unlock()
		return out, nil
	})
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("list the namespaces labelled %s: %w", apov1alpha1.OrganizationLabel, ctx.Err())
	case r := <-ch:
		if r.Err != nil {
			return nil, r.Err
		}
		return slices.Clone(r.Val.([]Namespace)), nil
	}
}

// cached is the kept list while it is younger than listTTL.
func (n *Namespaces) cached() ([]Namespace, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.at.IsZero() || n.now().Sub(n.at) >= listTTL {
		return nil, false
	}
	return slices.Clone(n.listed), true
}

// list reads the organization namespaces that selector selects from the API
// server, past the cache.
func (n *Namespaces) list(ctx context.Context, selector string) ([]Namespace, error) {
	list, err := n.typed.CoreV1().Namespaces().List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list the namespaces labelled %s: %w", selector, err)
	}
	var out []Namespace
	for i := range list.Items {
		if ns, ok := n.of(&list.Items[i]); ok {
			out = append(out, ns)
		}
	}
	slices.SortFunc(out, func(a, b Namespace) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// Get reads one namespace: ok is false when it is not an organization
// namespace, or is terminating.
func (n *Namespaces) Get(ctx context.Context, name string) (Namespace, bool, error) {
	obj, err := n.typed.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Namespace{}, false, nil
	}
	if err != nil {
		return Namespace{}, false, fmt.Errorf("read namespace %s: %w", name, err)
	}
	ns, ok := n.of(obj)
	return ns, ok, nil
}

// of reads an organization namespace from its labels.
func (n *Namespaces) of(obj *corev1.Namespace) (Namespace, bool) {
	org, ok := obj.Labels[apov1alpha1.OrganizationLabel]
	if !ok || obj.Name == n.platform || obj.DeletionTimestamp != nil || obj.Status.Phase == corev1.NamespaceTerminating {
		return Namespace{}, false
	}
	return Namespace{Name: obj.Name, Organization: org, OrganizationUID: obj.Labels[apov1alpha1.OrganizationUIDLabel]}, true
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

// Find is the organization namespace of the Organization organization: ok
// is false when there is none, or more than one namespace carries its label.
// The cached list answers when it holds exactly one; otherwise the label is
// listed from the API server, so a namespace created after the cache filled
// is found at once.
func (n *Namespaces) Find(ctx context.Context, organization string) (Namespace, bool, error) {
	list, err := n.List(ctx)
	if err != nil {
		return Namespace{}, false, err
	}
	found := withOrganization(list, organization)
	if len(found) != 1 && len(validation.IsValidLabelValue(organization)) == 0 {
		fresh, err := n.list(ctx, labels.Set{apov1alpha1.OrganizationLabel: organization}.String())
		if err != nil {
			return Namespace{}, false, err
		}
		found = withOrganization(fresh, organization)
	}
	if len(found) != 1 {
		return Namespace{}, false, nil
	}
	return found[0], true, nil
}

func withOrganization(list []Namespace, organization string) []Namespace {
	var found []Namespace
	for _, ns := range list {
		if ns.Organization == organization {
			found = append(found, ns)
		}
	}
	return found
}
