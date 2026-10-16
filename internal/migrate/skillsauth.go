package migrate

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/skills"
)

// renameSkillsGitAuth moves a release whose skillsGitAuthSecretRef names a
// previous name of the installation's skills credential to its current name,
// in place, on either hop: the Secret was renamed, and the agent is to read
// its git skills from the current one. A release naming its own Secret, or
// none, is left as it is. So is one whose namespace has no Secret of the
// current name holding its key before the run's wait ends (agent-manager's
// chart creates it empty and agent-manager fills it, and that rollout may
// trail this run; kagent refuses a credentialRef to a Secret without the
// key): the previous Secret keeps working, and a later run moves the
// release; the warning says so.
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
	case errors.Is(err, errCredentialEmpty):
		return fmt.Sprintf("skillsGitAuthSecretRef stays %s: the Secret %s/%s has no key %s yet (agent-manager fills it); a later run moves it", name, ns, current, skills.BootSecretKey)
	case err != nil:
		return fmt.Sprintf("skillsGitAuthSecretRef stays %s: the Secret %s/%s cannot be checked (%v); a later run moves it", name, ns, current, err)
	}
	ref["name"] = current
	ch.Set = append(ch.Set, agents.SkillsGitAuthValuesKey+".name="+current)
	return ""
}

// credentialPoll is how often a missing skills credential is looked for.
var credentialPoll = 5 * time.Second

// errCredentialEmpty: the Secret exists without its key.
var errCredentialEmpty = errors.New("the skills credential Secret holds no token yet")

// awaitSecret reads ns/name until it exists holding its key or the run's
// credential deadline passes; the error is the last read's.
func (r *Runner) awaitSecret(ctx context.Context, ns, name string) error {
	get := func(ctx context.Context) error {
		secret, err := r.client.Typed().CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if len(secret.Data[skills.BootSecretKey]) == 0 {
			return errCredentialEmpty
		}
		return nil
	}
	pending := func(err error) bool { return apierrors.IsNotFound(err) || errors.Is(err, errCredentialEmpty) }
	err := get(ctx)
	left := r.credentialDeadline.Sub(r.now())
	if !pending(err) || left <= 0 {
		return err
	}
	r.log.Info("waiting for the skills credential agent-manager's chart creates and agent-manager fills", "namespace", ns, "secret", name, "for", left.Round(time.Second))
	_ = wait.PollUntilContextTimeout(ctx, credentialPoll, left, false, func(ctx context.Context) (bool, error) {
		err = get(ctx)
		return !pending(err), nil
	})
	return err
}
