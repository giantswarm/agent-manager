package organizations

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apov1alpha1 "github.com/giantswarm/agent-platform-operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
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
	org := map[string]string{apov1alpha1.OrganizationLabel: "payments"}
	gone := metav1.NewTime(time.Now())
	finder := NewNamespaces(kubefake.NewClientset(
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
	assert.Equal(t, Namespace{Name: "agents-payments-x7k2q", Organization: "payments"}, got)
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

// cachedFinder is a Namespaces over a fake API server that counts its
// namespace lists, with a clock the test moves.
func cachedFinder(t *testing.T, objs ...runtime.Object) (*Namespaces, *kubefake.Clientset, *atomic.Int32, *time.Time) {
	t.Helper()
	typed := kubefake.NewClientset(objs...)
	lists := &atomic.Int32{}
	typed.PrependReactor("list", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		lists.Add(1)
		return false, nil, nil
	})
	finder := NewNamespaces(typed, nil, "kagent")
	now := time.Now()
	finder.now = func() time.Time { return now }
	return finder, typed, lists, &now
}

func orgNamespace(name, organization string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{apov1alpha1.OrganizationLabel: organization}}}
}

// A burst of concurrent organization and agent tool calls lists the
// namespaces once; the list is read again after listTTL.
func TestABurstListsTheNamespacesOnce(t *testing.T) {
	finder, _, lists, now := cachedFinder(t, orgNamespace("agents-payments-x7k2q", "payments"))
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			names, err := finder.Names(t.Context())
			assert.NoError(t, err)
			assert.Equal(t, []string{"agents-payments-x7k2q"}, names)
			got, ok, err := finder.Find(t.Context(), "payments")
			assert.NoError(t, err)
			assert.True(t, ok)
			assert.Equal(t, "agents-payments-x7k2q", got.Name)
		})
	}
	wg.Wait()
	assert.Equal(t, int32(1), lists.Load())

	*now = now.Add(listTTL)
	_, err := finder.Names(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int32(2), lists.Load())
}

// A namespace created after the list was read is found by its name and by
// its organization at once, and is in the list after listTTL.
func TestANewNamespaceIsFoundAtOnce(t *testing.T) {
	finder, typed, _, now := cachedFinder(t, orgNamespace("agents-payments-x7k2q", "payments"))
	_, err := finder.Names(t.Context())
	require.NoError(t, err)
	_, err = typed.CoreV1().Namespaces().Create(t.Context(), orgNamespace("agents-billing-a1b2c", "billing"), metav1.CreateOptions{})
	require.NoError(t, err)

	holds, err := finder.Holds(t.Context(), "agents-billing-a1b2c")
	require.NoError(t, err)
	assert.True(t, holds)
	got, ok, err := finder.Find(t.Context(), "billing")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, Namespace{Name: "agents-billing-a1b2c", Organization: "billing"}, got)
	// A name that can be no label value selects nothing, and is no error.
	_, ok, err = finder.Find(t.Context(), "not a label value")
	require.NoError(t, err)
	assert.False(t, ok)

	*now = now.Add(listTTL)
	names, err := finder.Names(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"agents-billing-a1b2c", "agents-payments-x7k2q"}, names)
}

// A deleted namespace stays in the list for listTTL at most; a read by its
// name says at once that it is no organization namespace.
func TestADeletedNamespaceLeavesTheListAfterTheCacheTime(t *testing.T) {
	finder, typed, _, now := cachedFinder(t, orgNamespace("agents-payments-x7k2q", "payments"), orgNamespace("agents-billing-a1b2c", "billing"))
	_, err := finder.Names(t.Context())
	require.NoError(t, err)
	require.NoError(t, typed.CoreV1().Namespaces().Delete(t.Context(), "agents-billing-a1b2c", metav1.DeleteOptions{}))

	holds, err := finder.Holds(t.Context(), "agents-billing-a1b2c")
	require.NoError(t, err)
	assert.False(t, holds)
	names, err := finder.Names(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"agents-billing-a1b2c", "agents-payments-x7k2q"}, names, "within the cache time")

	*now = now.Add(listTTL)
	names, err = finder.Names(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"agents-payments-x7k2q"}, names)
}

// A failed list is not kept: the next call lists again.
func TestAFailedListIsNotKept(t *testing.T) {
	finder, typed, lists, _ := cachedFinder(t, orgNamespace("agents-payments-x7k2q", "payments"))
	failed := false
	typed.PrependReactor("list", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		if failed {
			return false, nil, nil
		}
		failed = true
		return true, nil, apierrors.NewServiceUnavailable("etcd timeout")
	})
	_, err := finder.Names(t.Context())
	require.Error(t, err)
	names, err := finder.Names(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"agents-payments-x7k2q"}, names)
	assert.Equal(t, int32(1), lists.Load(), "the counting reactor sees the successful list only")
}
