package agents

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/giantswarm/agent-manager/internal/identity"
	apiv1alpha1 "github.com/giantswarm/agent-manager/internal/kagent/gen/kagent/api/v1alpha1"
)

// shortSessionCreate shortens createSession's bounds for the test.
func shortSessionCreate(t *testing.T, budget time.Duration) {
	t.Helper()
	b, h, d := sessionCreateBudget, sessionCreateHeartbeat, sessionCreateRetryDelay
	sessionCreateBudget, sessionCreateHeartbeat, sessionCreateRetryDelay = budget, 10*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { sessionCreateBudget, sessionCreateHeartbeat, sessionCreateRetryDelay = b, h, d })
}

// progressLog collects what a start reports as progress.
type progressLog struct {
	mu       sync.Mutex
	messages []string
}

func (p *progressLog) report(message string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.messages = append(p.messages, message)
}

func (p *progressLog) len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.messages)
}

func TestStartSessionWaitsForAColdToolset(t *testing.T) {
	shortSessionCreate(t, 5*time.Second)
	svc, ctrl := newSessionFixture(t)
	// kagent prepares the toolset: it answers Unavailable twice, each after
	// longer than a heartbeat, then creates the session.
	unprepared := 2
	ctrl.Prepare = func(_ context.Context, _ *apiv1alpha1.CreateSessionRequest) error {
		time.Sleep(30 * time.Millisecond)
		if unprepared > 0 {
			unprepared--
			return status.Error(codes.Unavailable, "the toolset's golden snapshot is not ready")
		}
		return nil
	}
	progress := &progressLog{}
	ctx := WithProgress(identity.ContextWithToken(t.Context(), callerToken), progress.report)

	started, err := svc.StartSession(ctx, StartSession{Name: "sre", RequestID: "req-1"})
	require.NoError(t, err)
	assert.NotEmpty(t, started.Session)

	creates := methodCalls(ctrl, "CreateSession")
	require.Len(t, creates, 3, "two Unavailable answers were retried")
	for _, c := range creates {
		assert.Equal(t, "req-1", c.(*apiv1alpha1.CreateSessionRequest).GetRequestId(), "every attempt carries the start's request id")
	}
	assert.Positive(t, progress.len(), "the caller heard progress while kagent prepared")
	assert.Contains(t, progress.messages[0], "preparing the session's toolchains")
}

func TestStartSessionGivesUpOnAColdToolsetAtItsBudget(t *testing.T) {
	shortSessionCreate(t, time.Second)
	svc, ctrl := newSessionFixture(t)
	// kagent answers Unavailable once, then holds the retry until the budget
	// cuts it short.
	var attempts int
	ctrl.Prepare = func(ctx context.Context, _ *apiv1alpha1.CreateSessionRequest) error {
		if attempts++; attempts > 1 {
			<-ctx.Done()
			return status.FromContextError(ctx.Err()).Err()
		}
		return status.Error(codes.Unavailable, "the toolset's golden snapshot is not ready")
	}

	_, err := svc.StartSession(identity.ContextWithToken(t.Context(), callerToken), StartSession{Name: "sre", RequestID: "req-1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `start again with requestId "req-1"`)
	assert.Contains(t, err.Error(), "golden snapshot is not ready", "kagent's last answer is kept over the attempt the budget cut short")
	assert.Len(t, methodCalls(ctrl, "CreateSession"), 2)
}

func TestStartSessionRetriesNoRefusal(t *testing.T) {
	shortSessionCreate(t, 5*time.Second)
	svc, ctrl := newSessionFixture(t)
	ctrl.NotReady["kagent/sre"] = true

	_, err := svc.StartSession(identity.ContextWithToken(t.Context(), callerToken), StartSession{Name: "sre"})
	require.ErrorIs(t, err, ErrConflict)
	assert.Len(t, methodCalls(ctrl, "CreateSession"), 1, "a refusal is answered at once")
}

func TestStartSessionStopsWithItsCaller(t *testing.T) {
	shortSessionCreate(t, 5*time.Second)
	svc, ctrl := newSessionFixture(t)
	ctx, cancel := context.WithCancel(identity.ContextWithToken(t.Context(), callerToken))
	ctrl.Prepare = func(context.Context, *apiv1alpha1.CreateSessionRequest) error {
		cancel()
		return status.Error(codes.Unavailable, "the toolset's golden snapshot is not ready")
	}

	_, err := svc.StartSession(ctx, StartSession{Name: "sre", RequestID: "req-1"})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "did not create the session within", "the caller left; the budget did not run out")
	assert.Len(t, methodCalls(ctrl, "CreateSession"), 1)
}
