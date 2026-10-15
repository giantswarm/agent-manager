package migrate

import (
	"slices"

	"github.com/giantswarm/agent-manager/internal/agents"
)

// renameSkillsGitAuth moves a release whose skillsGitAuthSecretRef names a
// previous name of the installation's skills credential to its current name,
// in place, on either hop: the Secret was renamed, and the agent is to read
// its git skills from the current one. A release naming its own Secret, or
// none, is left as it is.
func renameSkillsGitAuth(values map[string]any, current string, previous []string, ch *ValueChanges) {
	if current == "" {
		return
	}
	ref, _ := values[agents.SkillsGitAuthValuesKey].(map[string]any)
	name, _ := ref["name"].(string)
	if name == "" || name == current || !slices.Contains(previous, name) {
		return
	}
	ref["name"] = current
	ch.Set = append(ch.Set, agents.SkillsGitAuthValuesKey+".name="+current)
}
