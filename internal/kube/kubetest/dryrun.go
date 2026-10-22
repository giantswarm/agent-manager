// Package kubetest helps tests that run against client-go's fake clients.
package kubetest

import (
	"slices"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

// DryRuns plays the API server's side of the dry-run creates and updates
// (dryRun=All) a fake dynamic client receives, which the fake would otherwise
// perform: it records every object and answers with Refuse's error, accepting
// when Refuse is nil or returns nil. A dry-run never reaches the fake's
// tracker; every other write does.
type DryRuns struct {
	// Refuse is the API server's judgement of an object.
	Refuse func(*unstructured.Unstructured) error

	mu   sync.Mutex
	objs []DryRun
}

// DryRun is one dry-run write the fake received.
type DryRun struct {
	Object       *unstructured.Unstructured
	FieldManager string
}

// AnswerDryRuns installs the reactors on dyn.
func AnswerDryRuns(dyn *dynamicfake.FakeDynamicClient) *DryRuns {
	d := &DryRuns{}
	dyn.PrependReactor("create", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		c := a.(k8stesting.CreateActionImpl)
		return d.answer(c.GetObject(), c.CreateOptions.DryRun, c.CreateOptions.FieldManager)
	})
	dyn.PrependReactor("update", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		u := a.(k8stesting.UpdateActionImpl)
		return d.answer(u.GetObject(), u.UpdateOptions.DryRun, u.UpdateOptions.FieldManager)
	})
	return d
}

func (d *DryRuns) answer(obj runtime.Object, dryRun []string, manager string) (bool, runtime.Object, error) {
	if !slices.Equal(dryRun, []string{metav1.DryRunAll}) {
		return false, nil, nil
	}
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return false, nil, nil
	}
	u = u.DeepCopy()
	d.mu.Lock()
	d.objs = append(d.objs, DryRun{Object: u, FieldManager: manager})
	refuse := d.Refuse
	d.mu.Unlock()
	if refuse != nil {
		if err := refuse(u); err != nil {
			return true, nil, err
		}
	}
	return true, u, nil
}

// Received lists the dry-runs so far, in order.
func (d *DryRuns) Received() []DryRun {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.objs)
}
