package skills

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

var agentGVR = schema.GroupVersionResource{Group: "api.kagent.dev", Version: "v1alpha3", Resource: "agents"}

type fakeScopedTokens struct {
	account string
	asked   [][]string
}

func (f *fakeScopedTokens) Account(context.Context) (string, error) { return f.account, nil }

func (f *fakeScopedTokens) ScopedTokenValidFor(_ context.Context, repos []string, d time.Duration) (string, time.Time, error) {
	f.asked = append(f.asked, repos)
	token := "ghs"
	for _, r := range repos {
		token += "_" + r
	}
	return token, time.Now().Add(time.Hour), nil
}

func gitEntry(url, secret string) map[string]any {
	git := map[string]any{"url": url, "commit": "0123456789012345678901234567890123456789"}
	if secret != "" {
		git["credentialRef"] = map[string]any{"name": secret, "key": "token"}
	}
	return map[string]any{"name": "x", "source": map[string]any{"git": git}}
}

func agentObject(name string, skills []any, plugins []any) *unstructured.Unstructured {
	template := map[string]any{}
	if skills != nil {
		template["skills"] = skills
	}
	if plugins != nil {
		template["plugins"] = plugins
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "api.kagent.dev/v1alpha3",
		"kind":       "Agent",
		"metadata":   map[string]any{"name": name, "namespace": "kagent"},
		"spec":       map[string]any{"template": template},
	}}
}

func ownSecret(name, agent string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "kagent",
		Labels:      map[string]string{AgentSecretLabel: AgentSecretLabelValue},
		Annotations: map[string]string{AgentSecretAnnotation: agent},
	}}
}

func secretData(t *testing.T, kube *fake.Clientset, name string) map[string][]byte {
	t.Helper()
	s, err := kube.CoreV1().Secrets("kagent").Get(t.Context(), name, metav1.GetOptions{})
	require.NoError(t, err)
	return s.Data
}

func TestAgentSecretsScopesTheTokenToTheAgentsRepositories(t *testing.T) {
	kube := fake.NewClientset(ownSecret("sre-agent-skills-token", "sre-agent"))
	tokens := &fakeScopedTokens{account: "giantswarm"}
	a := NewAgentSecrets(tokens, kube, nil, agentGVR, []string{"kagent"}, nil)
	agent := agentObject("sre-agent",
		[]any{
			gitEntry("https://github.com/giantswarm/claude-code", "sre-agent-skills-token"),
			gitEntry("https://github.com/giantswarm/claude-code.git", "sre-agent-skills-token"),
			gitEntry("https://github.com/anthropics/skills", "sre-agent-skills-token"),
			gitEntry("https://github.com/giantswarm/public-skills", ""),
		},
		[]any{gitEntry("https://github.com/GiantSwarm/agent-skills", "sre-agent-skills-token")},
	)

	require.NoError(t, a.Refresh(t.Context(), agent))
	assert.Equal(t, [][]string{{"agent-skills", "claude-code"}}, tokens.asked, "plugins count, other owners and uncredentialed sources do not")
	assert.Equal(t, basic("ghs_agent-skills_claude-code"), secretData(t, kube, "sre-agent-skills-token")[BootSecretKey])

	st, ok := a.StatusOf("kagent", "sre-agent")
	require.True(t, ok)
	assert.Equal(t, []string{"giantswarm/agent-skills", "giantswarm/claude-code"}, st.Repositories)
	assert.Equal(t, []string{"https://github.com/anthropics/skills"}, st.Skipped)
	assert.Empty(t, st.Error)
	require.NotNil(t, st.ExpiresAt)

	kube.ClearActions()
	require.NoError(t, a.Refresh(t.Context(), agent))
	assert.Empty(t, updates(kube.Actions()), "an unchanged token is not written again")
}

func TestAgentSecretsLeavesSecretsThatAreNotTheAgentsOwn(t *testing.T) {
	provisioned := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "team-token", Namespace: "kagent"}, Data: map[string][]byte{"token": []byte("pat")}}
	kube := fake.NewClientset(provisioned)
	tokens := &fakeScopedTokens{account: "giantswarm"}
	a := NewAgentSecrets(tokens, kube, nil, agentGVR, []string{"kagent"}, nil)

	for _, agent := range []*unstructured.Unstructured{
		agentObject("provisioned", []any{gitEntry("https://github.com/giantswarm/claude-code", "team-token")}, nil),
		agentObject("missing", []any{gitEntry("https://github.com/giantswarm/claude-code", "absent")}, nil),
		agentObject("anonymous", []any{gitEntry("https://github.com/giantswarm/claude-code", "")}, nil),
	} {
		require.NoError(t, a.Refresh(t.Context(), agent), agent.GetName())
	}
	assert.Empty(t, tokens.asked)
	assert.Empty(t, updates(kube.Actions()))
	assert.Empty(t, a.Status())
}

func TestAgentSecretsRefusesASecretMarkedForAnotherAgent(t *testing.T) {
	kube := fake.NewClientset(ownSecret("helpdesk-skills-token", "giantswarm-helpdesk"))
	tokens := &fakeScopedTokens{account: "giantswarm"}
	a := NewAgentSecrets(tokens, kube, nil, agentGVR, []string{"kagent"}, nil)

	err := a.Refresh(t.Context(), agentObject("borrower", []any{gitEntry("https://github.com/giantswarm/claude-code", "helpdesk-skills-token")}, nil))
	require.ErrorContains(t, err, `Agent "giantswarm-helpdesk", not of "borrower"`)
	assert.Empty(t, tokens.asked)
	assert.Empty(t, updates(kube.Actions()))
	st, ok := a.StatusOf("kagent", "borrower")
	require.True(t, ok)
	assert.NotEmpty(t, st.Error)
}

func TestAgentSecretsNeverMintsForAnEmptyRepositorySet(t *testing.T) {
	secret := ownSecret("outsider-skills-token", "outsider")
	secret.Data = map[string][]byte{BootSecretKey: basic("ghs_old")}
	kube := fake.NewClientset(secret)
	tokens := &fakeScopedTokens{account: "giantswarm"}
	a := NewAgentSecrets(tokens, kube, nil, agentGVR, []string{"kagent"}, nil)

	err := a.Refresh(t.Context(), agentObject("outsider", []any{gitEntry("https://github.com/anthropics/skills", "outsider-skills-token")}, nil))
	require.ErrorContains(t, err, "no git source")
	assert.Empty(t, tokens.asked, "an empty list would be the whole installation")
	assert.NotContains(t, secretData(t, kube, "outsider-skills-token"), BootSecretKey, "the token of the previous set is removed")
}

func TestAgentSecretsRunFillsTheSecretOfAWatchedAgent(t *testing.T) {
	kube := fake.NewClientset(ownSecret("oee-analyst-skills-token", "oee-analyst"))
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{agentGVR: "AgentList"},
		agentObject("oee-analyst", []any{gitEntry("https://github.com/giantswarm/agent-skills", "oee-analyst-skills-token")}, nil))
	a := NewAgentSecrets(&fakeScopedTokens{account: "giantswarm"}, kube, dyn, agentGVR, []string{"kagent"}, nil)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		a.Run(ctx)
		close(done)
	}()
	require.Eventually(t, func() bool {
		s, err := kube.CoreV1().Secrets("kagent").Get(ctx, "oee-analyst-skills-token", metav1.GetOptions{})
		return err == nil && string(s.Data[BootSecretKey]) == string(basic("ghs_agent-skills"))
	}, 5*time.Second, 20*time.Millisecond)

	require.NoError(t, dyn.Resource(agentGVR).Namespace("kagent").Delete(ctx, "oee-analyst", metav1.DeleteOptions{}))
	require.Eventually(t, func() bool { return len(a.Status()) == 0 }, 5*time.Second, 20*time.Millisecond, "a deleted Agent is forgotten")
	cancel()
	<-done
}
