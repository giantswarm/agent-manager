package migrate

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agent-manager/internal/agents"
)

const (
	skillsCredential     = "agent-manager-skills-token"
	skillsCredentialPrev = "tenant-credential-agent-manager-skills-token" // #nosec G101 -- a Secret name, not a credential
)

func withSkillsCredential(values map[string]any, name string) map[string]any {
	values[agents.SkillsGitAuthValuesKey] = map[string]any{"name": name}
	return values
}

// credentialSecret is the Secret of the current name agent-manager's chart
// renders in ns.
func (c *cluster) credentialSecret(t *testing.T, ns string) {
	t.Helper()
	_, err := c.typed.CoreV1().Secrets(ns).Create(t.Context(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: skillsCredential, Namespace: ns}, Data: map[string][]byte{"token": []byte("x")}}, metav1.CreateOptions{})
	require.NoError(t, err)
}

// emptyCredentialSecret is that Secret as the chart renders it, before
// agent-manager fills it.
func (c *cluster) emptyCredentialSecret(t *testing.T, ns string) {
	t.Helper()
	_, err := c.typed.CoreV1().Secrets(ns).Create(t.Context(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: skillsCredential, Namespace: ns}}, metav1.CreateOptions{})
	require.NoError(t, err)
}

func renameOptions(namespaces ...string) Options {
	return Options{Namespaces: namespaces, SkillsGitAuthSecretName: skillsCredential, SkillsGitAuthPreviousSecretNames: []string{skillsCredentialPrev}}
}

// A release on 2.x values that names the previous installation credential is
// moved to the current name; one naming its own Secret is left as it is.
func TestSkillsCredentialMovesToItsCurrentName(t *testing.T) {
	ctx := t.Context()
	agentValues := func(name string) map[string]any {
		return map[string]any{"agent": map[string]any{"name": name}, "modelConfig": map[string]any{"name": "default-model-config"}}
	}
	c := newCluster(
		ociRepository(kagentNS, v2Target),
		helmRelease(kagentNS, "moved", withSkillsCredential(agentValues("moved"), skillsCredentialPrev), "2.0.0"), agentObj(kagentNS, "moved", "moved", kagentNS, true),
		helmRelease(kagentNS, "own", withSkillsCredential(agentValues("own"), "team-token"), "2.0.0"), agentObj(kagentNS, "own", "own", kagentNS, true),
		helmRelease(kagentNS, "current", withSkillsCredential(agentValues("current"), skillsCredential), "2.0.0"), agentObj(kagentNS, "current", "current", kagentNS, true),
	)
	c.credentialSecret(t, kagentNS)

	res, err := c.runnerV2(t, renameOptions(kagentNS), v2Latest).Run(ctx)
	require.NoError(t, err)
	rels := byRelease(res.Reports[0])
	require.Equal(t, ActionRewritten, rels["kagent/moved"].Action)
	require.Equal(t, []string{agents.SkillsGitAuthValuesKey + ".name=" + skillsCredential}, rels["kagent/moved"].Changes.Set)
	require.Equal(t, map[string]any{"name": skillsCredential}, c.values(t, kagentNS, "moved")[agents.SkillsGitAuthValuesKey])
	require.Equal(t, ActionUnchanged, rels["kagent/own"].Action)
	require.Equal(t, map[string]any{"name": "team-token"}, c.values(t, kagentNS, "own")[agents.SkillsGitAuthValuesKey])
	require.Equal(t, ActionUnchanged, rels["kagent/current"].Action)

	res, err = c.runnerV2(t, renameOptions(kagentNS), v2Latest).Run(ctx)
	require.NoError(t, err)
	require.False(t, res.Reports[0].Changed, "a second run changes nothing")
}

// Without the installation's current name nothing is moved.
func TestSkillsCredentialWithoutCurrentNameIsLeft(t *testing.T) {
	values := withSkillsCredential(map[string]any{"agent": map[string]any{"name": "a"}, "modelConfig": map[string]any{"name": "default-model-config"}}, skillsCredentialPrev)
	c := newCluster(ociRepository(kagentNS, v2Target), helmRelease(kagentNS, "a", values, "2.0.0"), agentObj(kagentNS, "a", "a", kagentNS, true))

	opts := renameOptions(kagentNS)
	opts.SkillsGitAuthSecretName = ""
	res, err := c.runnerV2(t, opts, v2Latest).Run(t.Context())
	require.NoError(t, err)
	require.Equal(t, ActionUnchanged, byRelease(res.Reports[0])["kagent/a"].Action)
	require.Empty(t, c.writes(kagentNS))
}

// A GitOps-owned release naming the previous credential gets the diff and is
// never written.
func TestSkillsCredentialOfAGitOpsReleaseIsADiff(t *testing.T) {
	values := withSkillsCredential(map[string]any{"agent": map[string]any{"name": "sre-agent"}, "modelConfig": map[string]any{"name": "default-model-config"}}, skillsCredentialPrev)
	hr := gitOpsLabeled(helmRelease("flux-giantswarm", "sre-agent", values, "2.0.0"))
	require.NoError(t, unstructured.SetNestedField(hr.Object, kagentNS, "spec", "targetNamespace"))
	c := newCluster(gitOpsLabeled(ociRepository("flux-giantswarm", v2Target)), hr, agentObj(kagentNS, "sre-agent", "sre-agent", "flux-giantswarm", true))
	c.credentialSecret(t, kagentNS)

	res, err := c.runnerV2(t, renameOptions(kagentNS), v2Latest).Run(t.Context())
	require.NoError(t, err)
	rel := byRelease(res.Reports[0])["flux-giantswarm/sre-agent"]
	require.Equal(t, ActionDiff, rel.Action)
	require.Contains(t, rel.Diff, "-      name: "+skillsCredentialPrev+"\n")
	require.Contains(t, rel.Diff, "+      name: "+skillsCredential+"\n")
	require.Empty(t, c.writes("flux-giantswarm"))
}

// Before agent-manager's rollout created the Secret of the current name, a
// release keeps the previous one (still filled) and the report warns; the
// next run moves it.
func TestSkillsCredentialWaitsForTheCurrentSecret(t *testing.T) {
	ctx := t.Context()
	values := withSkillsCredential(map[string]any{"agent": map[string]any{"name": "a"}, "modelConfig": map[string]any{"name": "default-model-config"}}, skillsCredentialPrev)
	c := newCluster(ociRepository(kagentNS, v2Target), helmRelease(kagentNS, "a", values, "2.0.0"), agentObj(kagentNS, "a", "a", kagentNS, true))

	res, err := c.runnerV2(t, renameOptions(kagentNS), v2Latest).Run(ctx)
	require.NoError(t, err)
	rel := byRelease(res.Reports[0])["kagent/a"]
	require.Equal(t, ActionUnchanged, rel.Action)
	require.Contains(t, strings.Join(rel.Warnings, "\n"), "namespace kagent has no Secret "+skillsCredential+" yet")
	require.Empty(t, c.writes(kagentNS))

	c.credentialSecret(t, kagentNS)
	res, err = c.runnerV2(t, renameOptions(kagentNS), v2Latest).Run(ctx)
	require.NoError(t, err)
	require.Equal(t, ActionRewritten, byRelease(res.Reports[0])["kagent/a"].Action)
	require.Equal(t, map[string]any{"name": skillsCredential}, c.values(t, kagentNS, "a")[agents.SkillsGitAuthValuesKey])
}

// A run waits for the Secret agent-manager's rollout creates while it runs,
// and moves the release once it is there.
func TestSkillsCredentialWaitsWithinTheRun(t *testing.T) {
	poll := credentialPoll
	credentialPoll = 10 * time.Millisecond
	t.Cleanup(func() { credentialPoll = poll })
	values := withSkillsCredential(map[string]any{"agent": map[string]any{"name": "a"}, "modelConfig": map[string]any{"name": "default-model-config"}}, skillsCredentialPrev)
	c := newCluster(ociRepository(kagentNS, v2Target), helmRelease(kagentNS, "a", values, "2.0.0"), agentObj(kagentNS, "a", "a", kagentNS, true))
	go func() {
		time.Sleep(50 * time.Millisecond)
		c.credentialSecret(t, kagentNS)
	}()

	opts := renameOptions(kagentNS)
	opts.SkillsGitAuthWait = 5 * time.Second
	res, err := c.runnerV2(t, opts, v2Latest).Run(t.Context())
	require.NoError(t, err)
	require.Equal(t, ActionRewritten, byRelease(res.Reports[0])["kagent/a"].Action)
	require.Equal(t, map[string]any{"name": skillsCredential}, c.values(t, kagentNS, "a")[agents.SkillsGitAuthValuesKey])
}

// A Secret of the current name without its key (the chart's render, before
// agent-manager fills it) does not move a release: kagent would refuse it.
func TestSkillsCredentialWaitsForTheToken(t *testing.T) {
	ctx := t.Context()
	values := withSkillsCredential(map[string]any{"agent": map[string]any{"name": "a"}, "modelConfig": map[string]any{"name": "default-model-config"}}, skillsCredentialPrev)
	c := newCluster(ociRepository(kagentNS, v2Target), helmRelease(kagentNS, "a", values, "2.0.0"), agentObj(kagentNS, "a", "a", kagentNS, true))
	c.emptyCredentialSecret(t, kagentNS)

	res, err := c.runnerV2(t, renameOptions(kagentNS), v2Latest).Run(ctx)
	require.NoError(t, err)
	rel := byRelease(res.Reports[0])["kagent/a"]
	require.Equal(t, ActionUnchanged, rel.Action)
	require.Contains(t, strings.Join(rel.Warnings, "\n"), "has no key token yet")
	require.Empty(t, c.writes(kagentNS))
}
