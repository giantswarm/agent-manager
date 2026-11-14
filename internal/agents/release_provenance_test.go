package agents

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestListMissingExplicitOwnerDoesNotUseSameNamedRelease(t *testing.T) {
	for _, namespace := range []string{"kagent", "flux-giantswarm"} {
		t.Run(namespace, func(t *testing.T) {
			f := seeded(t)
			_, err := f.dyn.Resource(agentGVR).Namespace("kagent").Update(t.Context(),
				agentObject("kagent", "verifier", "missing", namespace, true, true), metav1.UpdateOptions{})
			require.NoError(t, err)

			list, err := f.svc.List(t.Context(), In(""), nil)
			require.NoError(t, err)
			require.Len(t, list, 1)
			assert.Equal(t, ManagedNone, list[0].Managed)
			assert.Nil(t, list[0].HelmRelease)
			assert.Nil(t, list[0].Values)
			assert.Empty(t, list[0].ReleaseWithheld)

			agent, err := f.svc.Get(t.Context(), In(""), "verifier")
			require.NoError(t, err)
			assert.Equal(t, agent.Managed, list[0].Managed)
			assert.Equal(t, agent.HelmRelease, list[0].HelmRelease)
		})
	}
}

func TestListAgentWithoutReleaseLabelsStillMatchesByName(t *testing.T) {
	f := seeded(t)
	_, err := f.dyn.Resource(agentGVR).Namespace("kagent").Update(t.Context(),
		agentObject("kagent", "verifier", "", "", true, true), metav1.UpdateOptions{})
	require.NoError(t, err)

	list, err := f.svc.List(t.Context(), In(""), nil)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, ManagedHelmRelease, list[0].Managed)
	require.NotNil(t, list[0].HelmRelease)
	assert.Equal(t, "verifier", list[0].HelmRelease.Name)
	assert.Equal(t, "kagent", list[0].HelmRelease.Namespace)
}

func TestListNamespaceLabelWithoutReleaseNameUsesSiteNamespace(t *testing.T) {
	f := seeded(t)
	object := agentObject("kagent", "verifier", "", "", true, true)
	object.SetLabels(map[string]string{HelmReleaseNamespaceLabel: "flux-giantswarm"})
	_, err := f.dyn.Resource(agentGVR).Namespace("kagent").Update(t.Context(),
		object, metav1.UpdateOptions{})
	require.NoError(t, err)
	refuse(f, "list", ociGVR)

	list, err := f.svc.List(t.Context(), In(""), nil)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Contains(t, list[0].ReleaseWithheld, "kagent")
	assert.Contains(t, list[0].ReleaseWithheld, RoleEditor)
	assert.NotContains(t, list[0].ReleaseWithheld, "flux-giantswarm")
}
