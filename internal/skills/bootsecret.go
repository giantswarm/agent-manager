package skills

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// BootSecretKey is the key of the boot Secret the agent chart's
// skillsGitAuthSecretRef names.
const BootSecretKey = "token"

// Timing of the boot Secret. The written token is replaced once it has less
// than bootTokenValidity left, and the Secret is checked every
// bootSecretInterval, so an agent that boots at any moment reads a token
// with at least bootTokenValidity - bootSecretInterval (35 minutes) left.
// A failed refresh is retried after bootSecretRetry.
const (
	bootTokenValidity  = 45 * time.Minute
	bootSecretInterval = 10 * time.Minute
	bootSecretRetry    = time.Minute
)

// InstallationTokens mints installation tokens of a GitHub App.
type InstallationTokens interface {
	TokenValidFor(ctx context.Context, d time.Duration) (string, time.Time, error)
}

// BootSecretStatus is the state of the boot Secret, reported by get_info.
type BootSecretStatus struct {
	// SecretName is the Secret (key token) in every namespace.
	SecretName string `json:"secretName"`
	// Namespaces are the namespaces the Secret is kept in.
	Namespaces []string `json:"namespaces"`
	// RefreshedAt is when the last refresh succeeded.
	RefreshedAt *time.Time `json:"refreshedAt,omitempty"`
	// ExpiresAt is when the written token expires.
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	// Error is the last refresh's failure; empty after a success.
	Error string `json:"error,omitempty"`
}

// BootSecret keeps the Secret agents fetch their git skills with at boot
// filled with an installation token of the skills GitHub App, in the form
// the platform's egress gateway sends as Basic authentication (the base64
// of "x-access-token:<token>"). The Secret exists already (the chart
// renders it); only its token is written, with the ServiceAccount's one
// permission to update that Secret by name.
type BootSecret struct {
	tokens     InstallationTokens
	kube       kubernetes.Interface
	name       string
	namespaces []string
	log        *slog.Logger

	mu     sync.Mutex
	status BootSecretStatus
}

// NewBootSecret keeps the Secret name in namespaces.
func NewBootSecret(tokens InstallationTokens, kube kubernetes.Interface, name string, namespaces []string, log *slog.Logger) *BootSecret {
	if log == nil {
		log = slog.Default()
	}
	ns := append([]string(nil), namespaces...)
	return &BootSecret{tokens: tokens, kube: kube, name: name, namespaces: ns, log: log,
		status: BootSecretStatus{SecretName: name, Namespaces: ns}}
}

// Run refreshes the Secret at once and then on every interval until ctx
// ends.
func (b *BootSecret) Run(ctx context.Context) {
	for {
		wait := bootSecretInterval
		if err := b.Refresh(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			b.log.Error("skills boot Secret refresh failed", "secret", b.name, "namespaces", b.namespaces, "error", err)
			wait = bootSecretRetry
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// Refresh writes a token valid for at least bootTokenValidity into the
// Secret of every namespace whose token differs.
func (b *BootSecret) Refresh(ctx context.Context) error {
	err := b.refresh(ctx)
	b.mu.Lock()
	defer b.mu.Unlock()
	if err != nil {
		b.status.Error = err.Error()
		return err
	}
	b.status.Error = ""
	return nil
}

func (b *BootSecret) refresh(ctx context.Context) error {
	token, expires, err := b.tokens.TokenValidFor(ctx, bootTokenValidity)
	if err != nil {
		return err
	}
	value := []byte(base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token)))
	for _, ns := range b.namespaces {
		secret, err := b.kube.CoreV1().Secrets(ns).Get(ctx, b.name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("read Secret %s/%s: %w", ns, b.name, err)
		}
		if bytes.Equal(secret.Data[BootSecretKey], value) {
			continue
		}
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data[BootSecretKey] = value
		if _, err := b.kube.CoreV1().Secrets(ns).Update(ctx, secret, metav1.UpdateOptions{FieldManager: "agent-manager"}); err != nil {
			return fmt.Errorf("write Secret %s/%s: %w", ns, b.name, err)
		}
		b.log.Info("skills boot Secret refreshed", "secret", b.name, "namespace", ns, "expiresAt", expires)
	}
	now := time.Now()
	b.mu.Lock()
	b.status.RefreshedAt, b.status.ExpiresAt = &now, &expires
	b.mu.Unlock()
	return nil
}

// Status reports the Secret's state.
func (b *BootSecret) Status() BootSecretStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.status
	out.Namespaces = append([]string(nil), b.status.Namespaces...)
	return out
}
