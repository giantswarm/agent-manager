package approvalgrants

import (
	"context"
	"errors"
	"reflect"
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
	if err != nil || len(got.Grants) != 1 || !reflect.DeepEqual(got.Grants[0], g) || got.Version == "" {
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

// A record whose object name, labels or agent disagree is skipped, not
// trusted: someone else's ConfigMap in the grants namespace lifts nothing
// (M2-S3-S-05, C-18). An undecodable one does not hide the others.
func TestConfigMapStoreSkipsRecordsThatFailTheirChecks(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientset()
	store := NewConfigMapStore(client.CoreV1().ConfigMaps("grants"))
	agent := Agent{UID: "u1", Namespace: "kagent", Name: "alert-review"}
	good := Record{Agent: agent, Grants: []Grant{{Entry: "default", Server: "github"}}}
	if err := store.Replace(ctx, good); err != nil {
		t.Fatal(err)
	}
	other := Agent{UID: "u2", Namespace: "kagent", Name: "other"}
	if err := store.Replace(ctx, Record{Agent: other, Grants: []Grant{{Entry: "default", Server: "github"}}}); err != nil {
		t.Fatal(err)
	}
	forge := func(name string, mutate func(*corev1.ConfigMap)) {
		cm, err := client.CoreV1().ConfigMaps("grants").Get(ctx, Name("u2"), metavOpts())
		if err != nil {
			t.Fatal(err)
		}
		cm = cm.DeepCopy()
		cm.Name, cm.ResourceVersion = name, ""
		mutate(cm)
		if _, err := client.CoreV1().ConfigMaps("grants").Create(ctx, cm, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	// Another agent's document under this agent's name.
	if err := client.CoreV1().ConfigMaps("grants").Delete(ctx, Name("u1"), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	forge(Name("u1"), func(*corev1.ConfigMap) {})
	got, err := store.Get(ctx, agent)
	if err != nil || len(got.Grants) != 0 {
		t.Fatalf("a record naming another agent lifts nothing: %+v %v", got, err)
	}
	// Labels that do not name the agent; an undecodable record.
	third := Agent{UID: "u3", Namespace: "kagent", Name: "third"}
	if err := store.Replace(ctx, Record{Agent: third, Grants: []Grant{{Entry: "default", Server: "github"}}}); err != nil {
		t.Fatal(err)
	}
	cm, err := client.CoreV1().ConfigMaps("grants").Get(ctx, Name("u3"), metavOpts())
	if err != nil {
		t.Fatal(err)
	}
	cm.Labels[LabelAgentName] = "other"
	if _, err := client.CoreV1().ConfigMaps("grants").Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	forge("approval-grants-y", func(cm *corev1.ConfigMap) { cm.Data[DataKey] = "{" })
	list, err := store.ListByNamespace(ctx, "kagent")
	if err != nil {
		t.Fatalf("one bad record must not fail the listing: %v", err)
	}
	if len(list) != 1 || list[0].Agent != other {
		t.Fatalf("only the sound record of u2 is listed: %+v", list)
	}
}

// A record without grants but with a pending sync is kept, so the drop can
// be sent again; a sync done deletes it.
func TestPendingRecordIsKeptWithoutGrants(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientset()
	version := 0
	client.PrependReactor("*", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if obj, ok := action.(k8stesting.CreateAction); ok {
			version++
			obj.GetObject().(*corev1.ConfigMap).ResourceVersion = strconv.Itoa(version)
		}
		return false, nil, nil
	})
	for name, store := range map[string]Store{"memory": NewMemoryStore(), "configmap": NewConfigMapStore(client.CoreV1().ConfigMaps("grants"))} {
		agent := Agent{UID: "u1", Namespace: "kagent", Name: "a"}
		r, _ := store.Get(ctx, agent)
		r.PendingSessionSync = true
		if err := store.Replace(ctx, r); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, err := store.Get(ctx, agent)
		if err != nil || !got.PendingSessionSync || got.Version == "" {
			t.Fatalf("%s: pending record kept: %+v %v", name, got, err)
		}
		got.PendingSessionSync = false
		if err := store.Replace(ctx, got); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if after, _ := store.Get(ctx, agent); after.Version != "" {
			t.Fatalf("%s: synced empty record deleted: %+v", name, after)
		}
	}
}
