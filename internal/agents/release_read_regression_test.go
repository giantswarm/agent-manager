package agents

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
)

func TestListDeniedExternalRelease(t *testing.T) {
	for _, denied := range []struct {
		name     string
		verb     string
		resource schema.GroupVersionResource
	}{
		{"source listing", "list", ociGVR},
		{"release listing", "list", hrGVR},
		{"owning release", "get", hrGVR},
	} {
		t.Run(denied.name, func(t *testing.T) {
			f := seeded(t)
			mustCreate(t, f, agentGVR, agentObject("kagent", "external", "verifier", "flux-giantswarm", true, true))
			f.dyn.PrependReactor(denied.verb, denied.resource.Resource, func(action k8stesting.Action) (bool, runtime.Object, error) {
				if denied.verb == "get" && action.GetNamespace() != "flux-giantswarm" {
					return false, nil, nil
				}
				return true, nil, apierrors.NewForbidden(denied.resource.GroupResource(), "", errors.New("denied"))
			})

			list, err := f.svc.List(t.Context(), In(""), nil)
			require.NoError(t, err)
			require.Len(t, list, 2)
			for _, agent := range list {
				if agent.Name == "external" {
					assert.Equal(t, ManagedUnknown, agent.Managed)
					assert.Contains(t, agent.ReleaseWithheld, "flux-giantswarm")
					assert.NotContains(t, agent.ReleaseWithheld, RoleEditor)
					assert.Nil(t, agent.HelmRelease)
					assert.Nil(t, agent.Values)
				} else if denied.verb == "get" {
					assert.Equal(t, ManagedHelmRelease, agent.Managed)
					assert.Empty(t, agent.ReleaseWithheld)
					require.NotNil(t, agent.HelmRelease)
					assert.Equal(t, "kagent", agent.HelmRelease.Namespace)
				}
			}
		})
	}
}

func TestListOwningReleaseReadErrors(t *testing.T) {
	for name, readError := range map[string]error{
		"unauthorized": apierrors.NewUnauthorized("expired caller token"),
		"unavailable":  apierrors.NewInternalError(errors.New("etcd unavailable")),
	} {
		t.Run(name, func(t *testing.T) {
			f := seeded(t)
			mustCreate(t, f, agentGVR, agentObject("kagent", "external", "external", "flux-giantswarm", true, true))
			f.dyn.PrependReactor("get", hrGVR.Resource, func(action k8stesting.Action) (bool, runtime.Object, error) {
				if action.GetNamespace() != "flux-giantswarm" {
					return false, nil, nil
				}
				return true, nil, readError
			})
			list, err := f.svc.List(t.Context(), In(""), nil)
			require.ErrorIs(t, err, readError)
			assert.Nil(t, list)
		})
	}
}

func TestListDeniedExternalOwnerDoesNotUseSameNamedLocalRelease(t *testing.T) {
	f := seeded(t)
	_, err := f.dyn.Resource(agentGVR).Namespace("kagent").Update(t.Context(),
		agentObject("kagent", "verifier", "external", "flux-giantswarm", true, true), metav1.UpdateOptions{})
	require.NoError(t, err)
	refuse(f, "get", hrGVR)

	list, err := f.svc.List(t.Context(), In(""), nil)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, ManagedUnknown, list[0].Managed)
	assert.Contains(t, list[0].ReleaseWithheld, "flux-giantswarm")
	assert.Nil(t, list[0].HelmRelease)
	assert.Nil(t, list[0].Values)
}

func TestListExternalReleaseDoesNotConsumeSameNamedLocalRelease(t *testing.T) {
	f := seeded(t)
	values := map[string]any{"agent": map[string]any{"name": "external"}}
	mustCreate(t, f, hrGVR, helmRelease("flux-giantswarm", "verifier", values, true, nil))
	mustCreate(t, f, agentGVR, agentObject("kagent", "external", "verifier", "flux-giantswarm", true, true))
	f.dyn.PrependReactor("get", hrGVR.Resource, func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetNamespace() != "kagent" {
			return false, nil, nil
		}
		return true, nil, apierrors.NewForbidden(hrGVR.GroupResource(), "", errors.New("list only"))
	})

	list, err := f.svc.List(t.Context(), In(""), nil)
	require.NoError(t, err)
	require.Len(t, list, 2)
	for _, agent := range list {
		assert.Equal(t, ManagedHelmRelease, agent.Managed)
		assert.Empty(t, agent.ReleaseWithheld)
		require.NotNil(t, agent.HelmRelease)
		if agent.Name == "external" {
			assert.Equal(t, "flux-giantswarm", agent.HelmRelease.Namespace)
		} else {
			assert.Equal(t, "kagent", agent.HelmRelease.Namespace)
		}
	}
}

func TestStatusDeniedReleaseQualifiesSummary(t *testing.T) {
	f := seeded(t)
	refuse(f, "get", hrGVR)
	status, err := f.svc.Status(t.Context(), In(""), "verifier")
	require.NoError(t, err)
	assert.Equal(t, VerdictReady, status.Verdict)
	assert.Contains(t, status.Summary, "Agent is Ready")
	assert.Contains(t, status.Summary, "HelmRelease state was not read")
	assert.NotEmpty(t, status.ReleaseWithheld)
}

func TestGetDeniedReleaseDoesNotClaimSourcesWereDenied(t *testing.T) {
	f := seeded(t)
	refuse(f, "get", hrGVR)
	agent, err := f.svc.Get(t.Context(), In(""), "verifier")
	require.NoError(t, err)
	assert.Contains(t, agent.ReleaseWithheld, "Flux read was forbidden")
	assert.NotContains(t, agent.ReleaseWithheld, "OCIRepositories")
}
