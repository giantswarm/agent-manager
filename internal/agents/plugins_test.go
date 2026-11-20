package agents

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateCreateSkillNameCollisions(t *testing.T) {
	image := kubectlRef + "@" + kubectlSum
	for name, testCase := range map[string]struct {
		skills  Skills
		plugins Plugins
		error   string
	}{
		"within a plugin": {
			plugins: Plugins{{OCI: image, Skills: []string{"triage", "triage"}}},
			error:   `/plugins/0/skills: items at 0 and 1 are equal`,
		},
		"across plugins": {
			plugins: Plugins{{OCI: image, Skills: []string{"triage"}}, {OCI: image, Skills: []string{"triage"}}},
			error:   `plugins[1].skills[0] "triage" is already used by plugins[0].skills[0]`,
		},
		"between a skill and a plugin": {
			skills:  Skills{{Name: "triage", OCI: image}},
			plugins: Plugins{{OCI: image, Skills: []string{"triage"}}},
			error:   `plugins[0].skills[0] "triage" is already used by skills[0]`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := seeded(t)
			result, err := fixture.svc.ValidateCreate(t.Context(), Spec{
				Name:        "sre",
				ModelConfig: "default-model-config",
				Toolset:     []string{"preset:read-only"},
				Skills:      testCase.skills,
				Plugins:     testCase.plugins,
			})
			require.NoError(t, err)
			require.False(t, result.Valid)
			require.Len(t, result.Errors, 1)
			require.Contains(t, result.Errors[0], testCase.error)

			updated, err := fixture.svc.ValidateUpdate(t.Context(), Update{
				Name: "verifier", Skills: &testCase.skills, Plugins: &testCase.plugins,
			})
			require.NoError(t, err)
			require.False(t, updated.Valid)
			require.Len(t, updated.Errors, 1)
			require.Contains(t, updated.Errors[0], testCase.error)
		})
	}
}
