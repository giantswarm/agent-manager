package agents

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentSummaryFieldsMatchAgent(t *testing.T) {
	fromRelease := map[string]bool{"Suspended": true, "Deleting": true, "ReleaseReason": true, "ReleaseMessage": true}
	agent := reflect.TypeOf(Agent{})
	summary := reflect.TypeOf(AgentSummary{})
	for i := range summary.NumField() {
		f := summary.Field(i)
		if fromRelease[f.Name] {
			continue
		}
		af, ok := agent.FieldByName(f.Name)
		require.True(t, ok, "AgentSummary.%s has no counterpart on Agent", f.Name)
		assert.Equal(t, af.Type, f.Type, f.Name)
		assert.Equal(t, af.Tag, f.Tag, f.Name)
	}
}

func TestAgentSummaryReportsTheReleaseState(t *testing.T) {
	no, yes := false, true
	failing := Agent{Name: "a", HelmRelease: &HelmReleaseRef{Ready: &no, Reason: "InstallFailed", Message: "boom", Suspended: true, Deleting: true}}
	s := failing.Summary()
	assert.True(t, s.Suspended)
	assert.True(t, s.Deleting)
	assert.Equal(t, "InstallFailed", s.ReleaseReason)
	assert.Equal(t, "boom", s.ReleaseMessage)

	healthy := Agent{Name: "a", HelmRelease: &HelmReleaseRef{Ready: &yes, Reason: "UpgradeSucceeded", Message: "ok"}}
	assert.Equal(t, AgentSummary{Name: "a"}, healthy.Summary(), "a Ready release adds nothing")
	assert.Equal(t, AgentSummary{Name: "a"}, Agent{Name: "a"}.Summary())
}
