package agents

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/giantswarm/agent-manager/internal/kagent"
	apiv1alpha1 "github.com/giantswarm/agent-manager/internal/kagent/gen/kagent/api/v1alpha1"
)

// Progress tells the caller of a long operation that it is still working.
// The MCP server sends it as notifications/progress on the caller's progress
// token; muster restarts its tool-call timeout on each one.
type Progress func(message string)

type progressKey struct{}

// WithProgress returns a context whose long operations report to p.
func WithProgress(ctx context.Context, p Progress) context.Context {
	return context.WithValue(ctx, progressKey{}, p)
}

// ProgressFrom is the context's Progress, a no-op when it has none.
func ProgressFrom(ctx context.Context) Progress {
	if p, ok := ctx.Value(progressKey{}).(Progress); ok && p != nil {
		return p
	}
	return func(string) {}
}

// The bounds of createSession, variables so tests can shorten them.
var (
	// sessionCreateBudget is how long a start waits for kagent to create its
	// Session, retries included: a toolset no Session has used yet is
	// prepared (its golden snapshot) before kagent answers.
	sessionCreateBudget = 3 * time.Minute
	// sessionCreateHeartbeat is how often the caller hears progress while
	// kagent prepares; well inside muster's 30s tool-call timeout.
	sessionCreateHeartbeat = 10 * time.Second
	// sessionCreateRetryDelay is the pause before an attempt is repeated.
	sessionCreateRetryDelay = 2 * time.Second
)

// createSession is CreateSession given the time kagent takes to prepare a
// toolset no Session has used yet. While it waits the caller hears progress
// every sessionCreateHeartbeat, which keeps a caller's timeout from ending
// the start; an attempt kagent answers Unavailable or DeadlineExceeded is
// repeated with the same request id, which kagent keys the Session on, until
// sessionCreateBudget has passed.
func (s *Service) createSession(ctx context.Context, token string, in kagent.SessionCreate) (*apiv1alpha1.Session, error) {
	ctx, cancel := context.WithTimeout(ctx, sessionCreateBudget)
	defer cancel()
	stop := heartbeat(ctx, ProgressFrom(ctx), sessionCreateHeartbeat)
	defer stop()
	// last is kagent's latest answer, kept over the error of an attempt the
	// budget cut short.
	var last error
	for attempt := 1; ; attempt++ {
		session, err := s.cfg.Sessions.CreateSession(ctx, token, in)
		if err == nil || !retryableCreate(err) {
			return session, err
		}
		if ctx.Err() == nil || last == nil {
			last = err
		}
		s.log.InfoContext(ctx, "kagent has not created the session yet; retrying with the same request id",
			"agent", in.Agent.Tenant(), "attempt", attempt, "error", err)
		select {
		case <-ctx.Done():
			return nil, sessionCreateTimeout(ctx, in.RequestID, last)
		case <-time.After(sessionCreateRetryDelay):
		}
	}
}

// retryableCreate tells whether kagent may still create the Session on a
// retry: it answers Unavailable or DeadlineExceeded while the toolset is
// being prepared.
func retryableCreate(err error) bool {
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded:
		return true
	}
	return false
}

// sessionCreateTimeout is the error of a start whose budget ran out: when
// the budget ended it, how to pick the start up; otherwise the caller's own
// cancellation as kagent reported it.
func sessionCreateTimeout(ctx context.Context, requestID string, err error) error {
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("kagent did not create the session within %s, it may still be preparing its toolchains (start again with requestId %q to pick it up): %w",
		sessionCreateBudget, requestID, err)
}

// heartbeat reports progress every interval until stop is called or ctx
// ends.
func heartbeat(ctx context.Context, report Progress, interval time.Duration) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		start := time.Now()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				report(fmt.Sprintf("kagent is preparing the session's toolchains (%s)", time.Since(start).Round(time.Second)))
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}
