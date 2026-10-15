package migrate

import (
	"context"
	"fmt"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/agent-manager/internal/agents"
)

// renameSkillsGitAuth moves a release whose skillsGitAuthSecretRef names a
// previous name of the installation's skills credential to its current name,
// in place, on either hop: the Secret was renamed, and the agent is to read
// its git skills from the current one. A release naming its own Secret, or
// none, is left as it is. So is one whose namespace has no Secret of the
// current name yet (agent-manager's chart creates it, and its rollout may
// trail this run): the previous Secret keeps working, and a later run moves
// the release; the warning says so.
func (r *Runner) renameSkillsGitAuth(ctx context.Context, rel *release, values map[string]any, ch *ValueChanges) string {
	current, previous := r.opts.SkillsGitAuthSecretName, r.opts.SkillsGitAuthPreviousSecretNames
	if current == "" {
		return ""
	}
	ref, _ := values[agents.SkillsGitAuthValuesKey].(map[string]any)
	name, _ := ref["name"].(string)
	if name == "" || name == current || !slices.Contains(previous, name) {
		return ""
	}
	ns := rel.renderNamespace()
	_, err := r.client.Typed().CoreV1().Secrets(ns).Get(ctx, current, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return fmt.Sprintf("skillsGitAuthSecretRef stays %s: namespace %s has no Secret %s yet; a later run moves it", name, ns, current)
	case err != nil:
		return fmt.Sprintf("skillsGitAuthSecretRef stays %s: the Secret %s/%s cannot be checked (%v); a later run moves it", name, ns, current, err)
	}
	ref["name"] = current
	ch.Set = append(ch.Set, agents.SkillsGitAuthValuesKey+".name="+current)
	return ""
}
