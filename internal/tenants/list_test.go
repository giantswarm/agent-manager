package tenants

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestListFindsTheTenantsAsTheReconcilerDoes(t *testing.T) {
	ns := func(name string, labelled bool, mutate func(*corev1.Namespace)) *corev1.Namespace {
		n := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if labelled {
			n.Labels = map[string]string{OrganizationLabel: name}
		}
		if mutate != nil {
			mutate(n)
		}
		return n
	}
	gone := metav1.NewTime(time.Now())
	typed := fake.NewClientset(
		ns("org-b", true, nil),
		ns("org-a", true, nil),
		ns("kagent", true, nil),
		ns("plain", false, nil),
		ns("org-deleting", true, func(n *corev1.Namespace) { n.DeletionTimestamp = &gone }),
		ns("org-terminating", true, func(n *corev1.Namespace) { n.Status.Phase = corev1.NamespaceTerminating }),
	)
	found, err := List(t.Context(), typed, "kagent")
	require.NoError(t, err)
	require.Equal(t, []string{"org-a", "org-b"}, found)
}
