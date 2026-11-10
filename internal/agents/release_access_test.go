package agents

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
)

func refuse(f *fixture, verb string, gvr schema.GroupVersionResource) {
	f.dyn.PrependReactor(verb, gvr.Resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(gvr.GroupResource(), "", errors.New("platform-user may not read Flux objects"))
	})
}

func requireWithheld(t *testing.T, a Agent) {
	t.Helper()
	assert.Equal(t, "verifier", a.Name)
	assert.True(t, a.Exists)
	assert.Equal(t, "Display verifier", a.DisplayName)
	assert.Equal(t, "default-model-config", a.ModelConfig)
	assert.Nil(t, a.HelmRelease)
	assert.Nil(t, a.Values)
	assert.Equal(t, ManagedUnknown, a.Managed)
	assert.Contains(t, a.ReleaseWithheld, "agent-platform:editor")
	assert.Contains(t, a.ReleaseWithheld, "kagent")
	assert.Equal(t, a.ReleaseWithheld, a.Summary().ReleaseWithheld)
}

func TestList_WithoutFluxReadAccessReturnsAgentsWithoutReleaseDetails(t *testing.T) {
	for name, refused := range map[string]struct {
		verb string
		gvr  schema.GroupVersionResource
	}{
		"ocirepositories": {"list", ociGVR},
		"helmreleases":    {"list", hrGVR},
	} {
		t.Run(name, func(t *testing.T) {
			f := seeded(t)
			refuse(f, refused.verb, refused.gvr)

			list, err := f.svc.List(t.Context(), In(""), nil)
			require.NoError(t, err)
			require.Len(t, list, 1)
			requireWithheld(t, list[0])
		})
	}
}

func TestList_FluxReadFailureOtherThanForbiddenStaysAnError(t *testing.T) {
	f := seeded(t)
	f.dyn.PrependReactor("list", ociGVR.Resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errors.New("etcd unavailable"))
	})

	_, err := f.svc.List(t.Context(), In(""), nil)
	require.ErrorContains(t, err, "etcd unavailable")
}

func TestGet_WithoutHelmReleaseReadAccessReturnsAgentWithoutReleaseDetails(t *testing.T) {
	f := seeded(t)
	refuse(f, "get", hrGVR)

	got, err := f.svc.Get(t.Context(), In(""), "verifier")
	require.NoError(t, err)
	requireWithheld(t, *got)
}

func TestGet_WithoutAgentOrHelmReleaseReadAccessIsForbidden(t *testing.T) {
	f := seeded(t)
	refuse(f, "get", hrGVR)
	refuse(f, "get", agentGVR)

	_, err := f.svc.Get(t.Context(), In(""), "verifier")
	require.ErrorIs(t, err, ErrForbidden)
}

func TestGet_AbsentAgentWithoutHelmReleaseReadAccessIsForbidden(t *testing.T) {
	f := seeded(t)
	refuse(f, "get", hrGVR)

	_, err := f.svc.Get(t.Context(), In(""), "pending")
	require.ErrorIs(t, err, ErrForbidden)
	assert.Contains(t, err.Error(), "agent-platform:editor")
}

func TestUpdate_WithoutHelmReleaseReadAccessIsStillForbidden(t *testing.T) {
	f := seeded(t)
	refuse(f, "get", hrGVR)

	desc := "x"
	_, err := f.svc.Update(t.Context(), Update{Name: "verifier", Description: &desc})
	require.ErrorIs(t, err, ErrForbidden)
}

func TestStatus_WithoutHelmReleaseReadAccessIsTheAgentVerdict(t *testing.T) {
	f := seeded(t)
	refuse(f, "get", hrGVR)

	st, err := f.svc.Status(t.Context(), In(""), "verifier")
	require.NoError(t, err)
	assert.Equal(t, VerdictReady, st.Verdict)
	assert.True(t, st.Agent.Exists)
	assert.Nil(t, st.HelmRelease)
	assert.Contains(t, st.ReleaseWithheld, "agent-platform:editor")
}

func TestStatus_AbsentAgentWithoutHelmReleaseReadAccessIsForbidden(t *testing.T) {
	f := seeded(t)
	refuse(f, "get", hrGVR)

	_, err := f.svc.Status(t.Context(), In(""), "pending")
	require.ErrorIs(t, err, ErrForbidden)
}
