package organizations

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/agent-manager/internal/agents"
)

// Every create and update of an organization write, its dry run and the real
// write, asks the API server for strict field validation; the API server's
// refusal of an unknown field reaches the caller as an invalid request that
// names the field. TestEnvtestOrganizationWritesAreStrict shows the same
// against a real API server.
func TestOrganizationWritesAskForStrictFieldValidation(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	var validation []string
	dyn.PrependReactor("*", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		switch a := action.(type) {
		case k8stesting.CreateActionImpl:
			validation = append(validation, "create "+a.CreateOptions.FieldValidation)
		case k8stesting.UpdateActionImpl:
			validation = append(validation, "update "+a.UpdateOptions.FieldValidation)
		}
		return false, nil, nil
	})
	obj := func(name string) map[string]any {
		return map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": name, "namespace": "ns"}}
	}
	// The fake client writes a dry run too: each call has its own object.
	for _, dryRun := range [][]string{dryRunAll, nil} {
		name := "real"
		if dryRun != nil {
			name = "dry"
		}
		_, err := do(t.Context(), dyn, write{gvr: secretGVR, kind: kindSecret, op: opCreate, obj: obj(name), ns: "ns", name: name}, dryRun)
		require.NoError(t, err)
		_, err = do(t.Context(), dyn, write{gvr: secretGVR, kind: kindSecret, op: opUpdate, obj: obj(name), ns: "ns", name: name}, dryRun)
		require.NoError(t, err)
	}
	assert.Equal(t, []string{"create Strict", "update Strict", "create Strict", "update Strict"}, validation)

	dyn.PrependReactor("create", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewBadRequest(`Secret in version "v1" cannot be handled as a Secret: strict decoding error: unknown field "spec"`)
	})
	err := apply(t.Context(), dyn, []write{{gvr: secretGVR, kind: kindSecret, op: opCreate, obj: obj("b"), ns: "ns", name: "b", what: "Secret ns/b"}}, false)
	require.ErrorIs(t, err, agents.ErrInvalid)
	assert.ErrorContains(t, err, `dry run of create Secret ns/b: `)
	assert.ErrorContains(t, err, `unknown field "spec"`)
	_, err = dyn.Resource(secretGVR).Namespace("ns").Get(t.Context(), "b", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err), "nothing is written: %v", err)
}
