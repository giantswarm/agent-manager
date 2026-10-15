package tenants

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"k8s.io/client-go/rest"
)

// restartDelay is how long a stopped reconciler waits before a new one
// starts.
const restartDelay = 10 * time.Second

// Status is the tenant reconciler's state, reported by get_info.
type Status struct {
	// Error is the last failure that stopped the reconciler on this replica,
	// FailedAt when it happened; a new reconciler was started after it.
	// Empty: none since the replica started.
	Error    string     `json:"error,omitempty"`
	FailedAt *time.Time `json:"failedAt,omitempty"`
}

// Set is the tenant reconciler as agent-manager runs it beside its API: a
// reconciler that stops with an error (a lost Lease, a kind no longer
// served) is replaced by a new one, and the API keeps serving meanwhile. A
// controller-runtime manager runs once, so each restart builds a new
// Reconciler, which again checks that its kinds are served and must win the
// Lease before it reconciles; the stopped one's reconciles end with its
// context. Until the new one's cache holds the tenant namespaces, the
// stopped one's last view of them answers.
type Set struct {
	restCfg *rest.Config
	cfg     Config
	log     *slog.Logger

	mu     sync.Mutex
	r      *Reconciler
	status Status
}

// NewSet builds the first reconciler: an error (a kind the API server does
// not serve, say) means agent-manager cannot keep the tenants.
func NewSet(restCfg *rest.Config, cfg Config, log *slog.Logger) (*Set, error) {
	if log == nil {
		log = slog.Default()
	}
	r, err := New(restCfg, cfg, log)
	if err != nil {
		return nil, err
	}
	return &Set{restCfg: restCfg, cfg: cfg, log: log, r: r}, nil
}

// Run starts the reconciler and returns once its cache holds the tenant
// namespaces, or with the error that stopped it before. It then keeps a
// reconciler running until ctx ends; done closes when the last one has
// stopped (and released the Lease).
func (s *Set) Run(ctx context.Context) (done <-chan struct{}, err error) {
	stopped, synced := start(ctx, s.reconciler())
	if !synced {
		if err := <-stopped; err != nil {
			return nil, err
		}
		return nil, ctx.Err()
	}
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		s.keep(ctx, stopped)
	}()
	return closed, nil
}

// keep replaces each reconciler that stops before ctx ends.
func (s *Set) keep(ctx context.Context, stopped <-chan error) {
	for {
		err := <-stopped
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("the manager stopped")
		}
		s.failed(err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(restartDelay):
		}
		r, err := New(s.restCfg, s.cfg, s.log)
		if err != nil {
			failed := make(chan error, 1)
			failed <- err
			stopped = failed
			continue
		}
		var synced bool
		if stopped, synced = start(ctx, r); synced {
			s.mu.Lock()
			s.r = r
			s.mu.Unlock()
		}
	}
}

// start runs r's manager until ctx ends and waits until its cache holds the
// tenant namespaces; synced is false when the manager stopped first. The
// manager's error arrives on stopped.
func start(ctx context.Context, r *Reconciler) (stopped <-chan error, synced bool) {
	errc := make(chan error, 1)
	syncCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		errc <- r.Start(ctx)
		cancel()
	}()
	return errc, r.WaitForCacheSync(syncCtx)
}

func (s *Set) failed(err error) {
	now := time.Now().UTC()
	s.mu.Lock()
	s.status = Status{Error: err.Error(), FailedAt: &now}
	s.mu.Unlock()
	s.log.Error("tenant reconciler stopped; the API keeps serving and a new reconciler starts", "error", err, "in", restartDelay)
}

func (s *Set) reconciler() *Reconciler {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.r
}

// Namespaces are the tenant namespaces, sorted.
func (s *Set) Namespaces() []string { return s.reconciler().Namespaces() }

// ModelConfig is the ModelConfig a tenant receives with its first agent.
func (s *Set) ModelConfig() string { return s.cfg.ModelConfigName }

// Status reports the reconciler's last failure on this replica.
func (s *Set) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}
