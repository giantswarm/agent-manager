package skills

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

type fakeTokens struct {
	token   string
	expires time.Time
	err     error
	asked   []time.Duration
}

func (f *fakeTokens) TokenValidFor(_ context.Context, d time.Duration) (string, time.Time, error) {
	f.asked = append(f.asked, d)
	return f.token, f.expires, f.err
}

func bootSecret(ns string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "agent-manager-skills-token", Namespace: ns}}
}

func basic(token string) []byte {
	return []byte(base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token)))
}

func TestBootSecretWritesTheAppTokenInBasicForm(t *testing.T) {
	kube := fake.NewClientset(bootSecret("kagent"), bootSecret("team-a"))
	tokens := &fakeTokens{token: "ghs_1", expires: time.Now().Add(time.Hour)}
	b := NewBootSecret(tokens, kube, "agent-manager-skills-token", []string{"kagent", "team-a"}, nil)
	ctx := context.Background()

	require.NoError(t, b.Refresh(ctx))
	for _, ns := range []string{"kagent", "team-a"} {
		s, err := kube.CoreV1().Secrets(ns).Get(ctx, "agent-manager-skills-token", metav1.GetOptions{})
		require.NoError(t, err)
		assert.Equal(t, basic("ghs_1"), s.Data[BootSecretKey], ns)
	}
	assert.Equal(t, []time.Duration{bootTokenValidity}, tokens.asked, "the written token outlives the next refresh")
	st := b.Status()
	assert.Empty(t, st.Error)
	require.NotNil(t, st.ExpiresAt)
	assert.Equal(t, tokens.expires, *st.ExpiresAt)

	// The same token is not written again; a new one is.
	kube.ClearActions()
	require.NoError(t, b.Refresh(ctx))
	assert.Empty(t, updates(kube.Actions()))
	tokens.token = "ghs_2"
	require.NoError(t, b.Refresh(ctx))
	assert.Len(t, updates(kube.Actions()), 2)
}

func TestBootSecretFailureIsReportedAndClearedOnSuccess(t *testing.T) {
	kube := fake.NewClientset(bootSecret("kagent"))
	tokens := &fakeTokens{err: errors.New("github app: POST returned 401")}
	b := NewBootSecret(tokens, kube, "agent-manager-skills-token", []string{"kagent"}, nil)
	ctx := context.Background()

	require.Error(t, b.Refresh(ctx))
	assert.Contains(t, b.Status().Error, "401")
	assert.Nil(t, b.Status().RefreshedAt)

	tokens.err, tokens.token, tokens.expires = nil, "ghs_1", time.Now().Add(time.Hour)
	require.NoError(t, b.Refresh(ctx))
	assert.Empty(t, b.Status().Error)
	assert.NotNil(t, b.Status().RefreshedAt)
}

func TestBootSecretNeverCreatesTheSecret(t *testing.T) {
	kube := fake.NewClientset()
	tokens := &fakeTokens{token: "ghs_1", expires: time.Now().Add(time.Hour)}
	b := NewBootSecret(tokens, kube, "agent-manager-skills-token", []string{"kagent"}, nil)

	err := b.Refresh(context.Background())
	require.Error(t, err)
	assert.Contains(t, b.Status().Error, "kagent/agent-manager-skills-token")
	for _, a := range kube.Actions() {
		assert.NotEqual(t, "create", a.GetVerb())
	}
}

func updates(actions []k8stesting.Action) []runtime.Object {
	var out []runtime.Object
	for _, a := range actions {
		if u, ok := a.(k8stesting.UpdateAction); ok {
			out = append(out, u.GetObject())
		}
	}
	return out
}
