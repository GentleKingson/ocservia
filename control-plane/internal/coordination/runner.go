package coordination

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrLeadershipLost reports that the maintenance body did not complete under
// one continuous leadership term. The caller should log, stay alive, and
// retry on the next tick; it must not treat this as a process failure.
var ErrLeadershipLost = errors.New("coordination: maintenance session lost leadership")

// Runner drives one scheduler leadership session: it acquires the lease,
// renews it in the background, cancels the session context when renewal
// fails, and exposes the active session to fenced maintenance bodies.
type Runner struct {
	pool          *pgxpool.Pool
	identity      Identity
	leaseTTL      time.Duration
	renewInterval time.Duration
	retryInterval time.Duration
	renewTimeout  time.Duration
	logger        *slog.Logger
	mu            sync.Mutex
	session       *Session
	sessionCtx    context.Context
	cancelSession context.CancelFunc
	renewStop     chan struct{}
	now           func() time.Time
}

// NewRunner creates a leadership runner. The lease TTL must comfortably
// exceed the renew interval and the renewal round trip.
func NewRunner(pool *pgxpool.Pool, identity Identity, leaseTTL, renewInterval time.Duration, logger *slog.Logger) *Runner {
	return &Runner{
		pool:          pool,
		identity:      identity,
		leaseTTL:      leaseTTL,
		renewInterval: renewInterval,
		retryInterval: time.Second,
		renewTimeout:  5 * time.Second,
		logger:        logger,
		now:           time.Now,
	}
}

// Session returns the currently active session and its context, or nil when
// leadership is not held.
func (r *Runner) Session() (*Session, context.Context, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.session == nil {
		return nil, nil, false
	}
	return r.session, r.sessionCtx, true
}

func (r *Runner) dropSessionLocked() {
	if r.session == nil {
		return
	}
	r.cancelSession()
	r.session = nil
	r.sessionCtx = nil
	r.cancelSession = nil
	close(r.renewStop)
	r.renewStop = nil
}

func (r *Runner) lost(err error) {
	r.mu.Lock()
	stopped := r.session == nil
	if !stopped {
		r.dropSessionLocked()
	}
	r.mu.Unlock()
	if !stopped && r.logger != nil {
		r.logger.Warn("scheduler leadership lost", "alert_kind", "scheduler.leadership_lost", "error", err.Error())
	}
}

func (r *Runner) renewLoop(session *Session, sessionCtx context.Context, stop <-chan struct{}) {
	ticker := time.NewTicker(r.renewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.WithoutCancel(sessionCtx), r.renewTimeout)
			err := session.Renew(ctx, r.pool)
			cancel()
			if err != nil {
				r.lost(err)
				return
			}
		}
	}
}

// acquire blocks until the lease is acquired or ctx ends. Real database
// errors abort the wait; a held lease is retried.
func (r *Runner) acquire(ctx context.Context) (*Session, error) {
	for {
		session, err := Acquire(ctx, r.pool, r.identity, r.leaseTTL)
		switch {
		case err == nil:
			return session, nil
		case errors.Is(err, ErrLeaseHeld):
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(r.retryInterval):
			}
		default:
			return nil, err
		}
	}
}

// WithSession runs body under an active leadership session with its own
// context. The context is cancelled as soon as renewal fails, which aborts
// in-flight fenced transactions before they can commit. If the body cannot
// complete under the term, WithSession returns ErrLeadershipLost; a parent
// context error is propagated as-is.
func (r *Runner) WithSession(ctx context.Context, body func(sessionCtx context.Context, session *Session) error) error {
	r.mu.Lock()
	if r.session != nil && r.session.ExpiredLocally(r.now()) {
		r.dropSessionLocked()
	}
	r.mu.Unlock()
	if r.session == nil {
		// Acquire without holding the lock: the retry loop may block while
		// another instance holds an unexpired lease, and renewal-loss
		// handling must stay responsive meanwhile.
		session, err := r.acquire(ctx)
		if err != nil {
			return err
		}
		r.mu.Lock()
		sessionCtx, cancel := context.WithCancel(WithFence(ctx, session))
		stop := make(chan struct{})
		r.session = session
		r.sessionCtx = sessionCtx
		r.cancelSession = cancel
		r.renewStop = stop
		r.mu.Unlock()
		go r.renewLoop(session, sessionCtx, stop)
		if r.logger != nil {
			r.logger.Info("scheduler leadership acquired", "instance_id", r.identity.InstanceID.String(), "epoch", session.Epoch())
		}
	}
	r.mu.Lock()
	session := r.session
	sessionCtx := r.sessionCtx
	r.mu.Unlock()

	err := body(sessionCtx, session)
	if errors.Is(err, ErrNotLeader) {
		r.lost(err)
		return ErrLeadershipLost
	}
	if err != nil {
		return err
	}
	if sessionCtx.Err() != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrLeadershipLost
	}
	return nil
}

// Stop releases the runner: it drops the session and stops renewal. The
// lease then expires by PostgreSQL time and another instance takes over
// with a higher epoch.
func (r *Runner) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dropSessionLocked()
}
