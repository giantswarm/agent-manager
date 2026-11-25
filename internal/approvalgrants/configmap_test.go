package approvalgrants

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestConfigMapStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientset()
	// The fake keeps no resourceVersion; the API server always does.
	version := 0
	client.PrependReactor("*", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if obj, ok := action.(k8stesting.CreateAction); ok {
			version++
			obj.GetObject().(*corev1.ConfigMap).ResourceVersion = strconv.Itoa(version)
		}
		return false, nil, nil
	})
	store := NewConfigMapStore(client.CoreV1().ConfigMaps("agent-platform"))
	agent := Agent{UID: "u1", Namespace: "kagent", Name: "alert-review"}

	empty, err := store.Get(ctx, agent)
	if err != nil || empty.Version != "" || len(empty.Grants) != 0 {
		t.Fatalf("empty record: %+v %v", empty, err)
	}
	g := Grant{Entry: "tool:x_github_merge", Server: "github", ConnectorNamespace: "muster", ConnectorUID: "c1", GrantedBy: "dana@example.com", GrantedAt: time.Unix(1, 0).UTC(), Generation: 3}
	empty.Grants = []Grant{g}
	if err := store.Replace(ctx, empty); err != nil {
		t.Fatalf("create: %v", err)
	}
	cm, err := client.CoreV1().ConfigMaps("agent-platform").Get(ctx, "approval-grants-u1", metavOpts())
	if err != nil {
		t.Fatalf("configmap: %v", err)
	}
	if cm.Labels[LabelGrants] != "v1" || cm.Labels[LabelAgentNamespace] != "kagent" || cm.Labels[LabelAgentName] != "alert-review" {
		t.Errorf("labels: %v", cm.Labels)
	}
	if err := store.Replace(ctx, empty); !errors.Is(err, ErrConflict) {
		t.Errorf("second create over a missing version: want conflict, got %v", err)
	}
	got, err := store.Get(ctx, agent)
	if err != nil || len(got.Grants) != 1 || got.Grants[0] != g || got.Version == "" {
		t.Fatalf("read back: %+v %v", got, err)
	}
	list, err := store.ListByNamespace(ctx, "kagent")
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %+v %v", list, err)
	}
	if other, _ := store.ListByNamespace(ctx, "other"); len(other) != 0 {
		t.Errorf("other namespace lists %v", other)
	}
	got.Grants = nil
	if err := store.Replace(ctx, got); err != nil {
		t.Fatalf("delete by empty replace: %v", err)
	}
	if after, _ := store.Get(ctx, agent); after.Version != "" {
		t.Errorf("record still there: %+v", after)
	}
	if err := store.Delete(ctx, "u1"); err != nil {
		t.Errorf("delete missing: %v", err)
	}
}

func TestMemoryStoreConflict(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryStore()
	a := Agent{UID: "u", Namespace: "n", Name: "a"}
	r, _ := m.Get(ctx, a)
	r.Grants = []Grant{{Entry: "default"}}
	if err := m.Replace(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := m.Replace(ctx, r); !errors.Is(err, ErrConflict) {
		t.Errorf("stale replace: %v", err)
	}
}

func metavOpts() metav1.GetOptions { return metav1.GetOptions{} }
