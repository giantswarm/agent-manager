package migrate

import (
	"context"
	"fmt"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/giantswarm/agent-manager/internal/agents"
)

// renameSkillsGitAuth moves a release whose skillsGitAuthSecretRef names a
// previous name of the installation's skills credential to its current name,
// in place, on either hop: the Secret was renamed, and the agent is to read
// its git skills from the current one. A release naming its own Secret, or
// none, is left as it is. So is one whose namespace has no Secret of the
// current name before the run's wait ends (agent-manager's chart creates it,
// and its rollout may trail this run): the previous Secret keeps working, and
// a later run moves the release; the warning says so.
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
	err := r.awaitSecret(ctx, ns, current)
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

// credentialPoll is how often a missing skills credential is looked for.
var credentialPoll = 5 * time.Second

// awaitSecret reads ns/name until it exists or the run's credential deadline
// passes; the error is the last read's.
func (r *Runner) awaitSecret(ctx context.Context, ns, name string) error {
	get := func(ctx context.Context) error {
		_, err := r.client.Typed().CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
		return err
	}
	err := get(ctx)
	left := r.credentialDeadline.Sub(r.now())
	if !apierrors.IsNotFound(err) || left <= 0 {
		return err
	}
	r.log.Info("waiting for the skills credential agent-manager's chart creates", "namespace", ns, "secret", name, "for", left.Round(time.Second))
	_ = wait.PollUntilContextTimeout(ctx, credentialPoll, left, false, func(ctx context.Context) (bool, error) {
		err = get(ctx)
		return !apierrors.IsNotFound(err), nil
	})
	return err
}
