package agents

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/agent-manager/internal/kube/kubetest"
)

// dryRuns is the fake API server's answer to dry-runs, with views for the
// assertions here.
type dryRuns struct {
	*kubetest.DryRuns
	t *testing.T
}

func answerDryRuns(t *testing.T, dyn *dynamicfake.FakeDynamicClient) *dryRuns {
	return &dryRuns{DryRuns: kubetest.AnswerDryRuns(dyn), t: t}
}

// kinds lists the kinds dry-run so far, in order, each by agent-manager.
func (d *dryRuns) kinds() []string {
	var out []string
	for _, r := range d.Received() {
		assert.Equal(d.t, FieldManager, r.FieldManager)
		out = append(out, r.Object.GetKind()+"/"+r.Object.GetName())
	}
	return out
}

// last is the last object of kind dry-run.
func (d *dryRuns) last(kind string) *unstructured.Unstructured {
	received := d.Received()
	for i := len(received) - 1; i >= 0; i-- {
		if received[i].Object.GetKind() == kind {
			return received[i].Object
		}
	}
	return nil
}

// longOrigin is an origin the chart's schema takes and the Agent CRD refuses:
// 271 characters, one over the CRD's maxLength.
var longOrigin = "https://" + strings.Repeat(strings.Repeat("a", 63)+".", 4) + "example"

// tooLong is the API server's refusal of an Agent with longOrigin, as a
// StatusError of reason Invalid carries it.
func tooLong(name string) error {
	return apierrors.NewInvalid(schema.GroupKind{Group: "api.kagent.dev", Kind: "Agent"}, name, field.ErrorList{
		field.TooLong(field.NewPath("spec", "egress").Index(0), longOrigin, 270),
	})
}

// The rendered Agent and RemoteMCPServer are dry-run as they would be
// installed, a tls:// origin among the Agent's egress, and the create goes on.
func TestCreateDryRunsTheRenderedObjects(t *testing.T) {
	f := seeded(t)
	spec := Spec{Name: "dba", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"}, Egress: []string{"tls://db.example.com:5432"}}

	_, err := f.svc.Create(t.Context(), spec)
	require.NoError(t, err)
	assert.Equal(t, []string{"Agent/dba", "RemoteMCPServer/dba"}, f.dryRuns.kinds())
	agent := f.dryRuns.last("Agent")
	assert.Equal(t, "kagent", agent.GetNamespace())
	egress, _, _ := unstructured.NestedStringSlice(agent.Object, "spec", "egress")
	assert.Equal(t, []string{"tls://db.example.com:5432"}, egress)
	_, err = f.dyn.Resource(hrGVR).Namespace("kagent").Get(t.Context(), "dba", metav1.GetOptions{})
	require.NoError(t, err, "the HelmRelease is written once the dry-run passes")
}

// The API server's field error is the answer, verbatim, of create, update and
// both validations; nothing is written.
func TestTheAPIServersRefusalIsTheAnswer(t *testing.T) {
	require.Len(t, longOrigin, 271)
	f := seeded(t)
	ctx := t.Context()
	f.dryRuns.Refuse = func(obj *unstructured.Unstructured) error {
		egress, _, _ := unstructured.NestedStringSlice(obj.Object, "spec", "egress")
		if obj.GetKind() == "Agent" && slices.Contains(egress, longOrigin) {
			return tooLong(obj.GetName())
		}
		return nil
	}
	refusal := func(name string) string {
		return fmt.Sprintf(`Agent kagent/%s is invalid: spec.egress[0]: Too long: may not be more than 270 bytes`, name)
	}
	spec := Spec{Name: "dba", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"}, Egress: []string{longOrigin}}

	dry, err := f.svc.ValidateCreate(ctx, spec)
	require.NoError(t, err)
	assert.False(t, dry.Valid)
	assert.Equal(t, []string{refusal("dba")}, dry.Errors)

	_, err = f.svc.Create(ctx, spec)
	require.ErrorIs(t, err, ErrInvalid)
	assert.ErrorContains(t, err, refusal("dba"))
	_, err = f.dyn.Resource(hrGVR).Namespace("kagent").Get(ctx, "dba", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err), "nothing is written: %v", err)

	upd := Update{Name: "verifier", Egress: &[]string{longOrigin}}
	vu, err := f.svc.ValidateUpdate(ctx, upd)
	require.NoError(t, err)
	assert.False(t, vu.Valid)
	assert.Equal(t, []string{refusal("verifier")}, vu.Errors)
	_, err = f.svc.Update(ctx, upd)
	require.ErrorIs(t, err, ErrInvalid)
	assert.ErrorContains(t, err, refusal("verifier"))
}

// A Forbidden the caller's RBAC explains leaves the object to Flux and says
// so; a Forbidden from an admission webhook, with RBAC allowing the write, is
// a refusal.
func TestForbiddenDryRuns(t *testing.T) {
	forbidden := func(obj *unstructured.Unstructured) error {
		if obj.GetKind() != "Agent" {
			return nil
		}
		return apierrors.NewForbidden(schema.GroupResource{Group: "api.kagent.dev", Resource: "agents"}, obj.GetName(), errSentinel("denied"))
	}
	spec := Spec{Name: "dba", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"}}

	t.Run("by RBAC", func(t *testing.T) {
		f := seeded(t)
		f.dryRuns.Refuse = forbidden
		f.typed.PrependReactor("create", "selfsubjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
			review := a.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
			review.Status.Allowed = review.Spec.ResourceAttributes.Resource != "agents"
			return true, review, nil
		})
		dry, err := f.svc.ValidateCreate(t.Context(), spec)
		require.NoError(t, err)
		assert.True(t, dry.Valid, "%v", dry.Errors)
		assert.Contains(t, dry.Notes, "Agent kagent/dba was not dry-run: the caller may not create agents.api.kagent.dev there; Flux applies it")
		_, err = f.svc.Create(t.Context(), spec)
		require.NoError(t, err)
	})
	t.Run("by admission", func(t *testing.T) {
		f := seeded(t)
		f.dryRuns.Refuse = forbidden
		dry, err := f.svc.ValidateCreate(t.Context(), spec)
		require.NoError(t, err)
		assert.False(t, dry.Valid)
		require.Len(t, dry.Errors, 1)
		assert.Contains(t, dry.Errors[0], `agents.api.kagent.dev "dba" is forbidden: denied`)
		_, err = f.svc.Create(t.Context(), spec)
		require.ErrorIs(t, err, ErrInvalid)
	})
}

// Values the schema refuses are not rendered, and their answer is the
// schema's.
func TestSchemaViolationsAreNotRendered(t *testing.T) {
	f := seeded(t)
	st, err := f.svc.site(t.Context(), Location{Namespace: "kagent"})
	require.NoError(t, err)
	check, err := f.svc.checkValues(t.Context(), st, "dba", map[string]any{"unknown": true})
	require.NoError(t, err)
	assert.False(t, check.rendered)
	assert.NotEmpty(t, check.violations)
	assert.ErrorContains(t, check.err(), "values do not satisfy the agent chart schema")
	assert.Empty(t, f.dryRuns.kinds())
}

type errSentinel string

func (e errSentinel) Error() string { return string(e) }
