package agents

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"
)

// Every HelmRelease and OCIRepository write names agent-manager as the field
// manager: metadata.managedFields records the tool that applied a field; the
// person is recorded in the requestedBy annotation and in the apiserver audit
// log (the write runs with the caller's token), never in the field manager.
func TestWritesNameAgentManagerAsTheFieldManager(t *testing.T) {
	// No OCIRepository yet, so the create composes the shared source too.
	f := newFixture(t, []runtime.Object{
		modelConfig("kagent", "default-model-config", "Anthropic", "claude-sonnet-4-6"),
		harness("kagent", "kagent", "kagent"),
	})
	ctx := context.Background()

	managers := map[string][]string{}
	record := func(action clienttesting.Action) (bool, runtime.Object, error) {
		key := action.GetVerb() + " " + action.GetResource().Resource
		switch a := action.(type) {
		case clienttesting.CreateActionImpl:
			managers[key] = append(managers[key], a.GetCreateOptions().FieldManager)
		case clienttesting.UpdateActionImpl:
			managers[key] = append(managers[key], a.GetUpdateOptions().FieldManager)
		}
		return false, nil, nil // not handled: the tracker applies the write
	}
	f.dyn.PrependReactor("create", "*", record)
	f.dyn.PrependReactor("update", "*", record)

	_, err := f.svc.Create(ctx, Spec{Name: "fm", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"}})
	require.NoError(t, err)
	// The chart's rendered objects, as the controller would leave them.
	require.NoError(t, f.dyn.Tracker().Add(agentTemplate("kagent", "fm", "fm", "kagent", true, true)))
	require.NoError(t, f.dyn.Tracker().Add(remoteMCPServer("kagent", "fm")))
	desc := "renamed"
	_, err = f.svc.Update(ctx, Update{Name: "fm", Description: &desc})
	require.NoError(t, err)

	assert.Equal(t, []string{FieldManager}, managers["create ocirepositories"])
	assert.Equal(t, []string{FieldManager}, managers["create helmreleases"])
	assert.Equal(t, []string{FieldManager}, managers["update helmreleases"])
	for key, seen := range managers {
		for _, m := range seen {
			assert.Equal(t, FieldManager, m, key)
		}
	}
}
