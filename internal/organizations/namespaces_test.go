package organizations

import (
	"testing"
	"time"

	apov1alpha1 "github.com/giantswarm/agent-platform-operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// The agent tools manage the operator's organization namespaces only: never
// a Giant Swarm Organization's org-<name> labelled giantswarm.io/organization
// alone, the platform namespace, a namespace going away or an unlabelled one.
func TestNamesAreTheOrganizationNamespacesOnly(t *testing.T) {
	ns := func(name string, labels map[string]string, mutate func(*corev1.Namespace)) *corev1.Namespace {
		n := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
		if mutate != nil {
			mutate(n)
		}
		return n
	}
	org := map[string]string{apov1alpha1.OrganizationLabel: "payments", apov1alpha1.ParentLabel: "org-acme"}
	gone := metav1.NewTime(time.Now())
	finder := NewNamespaces(fake.NewClientset(
		ns("agents-payments-x7k2q", org, nil),
		ns("org-acme", map[string]string{"giantswarm.io/organization": "acme"}, nil),
		ns("kagent", org, nil),
		ns("plain", nil, nil),
		ns("agents-gone-a1b2c", org, func(n *corev1.Namespace) { n.DeletionTimestamp = &gone }),
	), nil, "kagent")

	names, err := finder.Names(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"agents-payments-x7k2q"}, names)

	got, ok, err := finder.Get(t.Context(), "agents-payments-x7k2q")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, Namespace{Name: "agents-payments-x7k2q", Organization: "payments", Parent: "org-acme"}, got)
	for _, name := range []string{"org-acme", "kagent", "plain", "agents-gone-a1b2c", "missing"} {
		_, ok, err = finder.Get(t.Context(), name)
		require.NoError(t, err)
		assert.False(t, ok, name)
	}
	// Holds is how the agent tools ask the same question.
	holds, err := finder.Holds(t.Context(), "agents-payments-x7k2q")
	require.NoError(t, err)
	assert.True(t, holds)
	holds, err = finder.Holds(t.Context(), "org-acme")
	require.NoError(t, err)
	assert.False(t, holds)
}
