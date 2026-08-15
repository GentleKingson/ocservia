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

// ErrRunnerStopped reports that the runner was stopped before the session
// could be established. A stopped runner never reacquires leadership.
var ErrRunnerStopped = errors.New("coordination: leadership runner stopped")

// sessionSnapshot is one atomically consistent view of the active session:
// its immutable identity and its context, or neither. It is captured under
// the runner mutex so a body can never run with a session or context that
// leadership loss already dropped.
type sessionSnapshot struct {
	session *Session
	ctx     context.Context
}

// Runner drives one scheduler leadership session: it acquires the lease,
// renews it in the background, cancels the session context when renewal
// fails, and exposes the active session to fenced maintenance bodies.
//
// Every mutable field is guarded by mu and no code reads r.session outside
// the lock. The Session itself is immutable; the runner owns the local lease
// deadline and advances it only while the session it belongs to is still
// current.
type Runner struct {
	pool          *pgxpool.Pool
	identity      Identity
	leaseTTL      time.Duration
	renewInterval time.Duration
	retryInterval time.Duration
	renewTimeout  time.Duration
	logger        *slog.Logger
	mu            sync.Mutex
	stopped       bool
	session       *Session
	sessionCtx    context.Context
	cancelSession context.CancelFunc
	renewStop     chan struct{}
	// localExpiry is the local deadline of the current session, anchored
	// before each acquire or renew round trip so it can never be later than
	// the lease PostgreSQL actually granted. Once passed, the runner stops
	// starting new fenced work even before PostgreSQL confirms the loss.
	localExpiry time.Time
	now         func() time.Time
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
	snap, ok := r.currentSession()
	if !ok {
		return nil, nil, false
	}
	return snap.session, snap.ctx, true
}

// currentSession snapshots the active session under one lock acquisition,
// dropping the session first when its local conservative deadline passed so
// the next call acquires a fresh term instead of extending stale leadership.
func (r *Runner) currentSession() (sessionSnapshot, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.session == nil {
		return sessionSnapshot{}, false
	}
	if !r.now().Before(r.localExpiry) {
		r.dropSessionLocked()
		return sessionSnapshot{}, false
	}
	return sessionSnapshot{session: r.session, ctx: r.sessionCtx}, true
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

// lost drops leadership when the failing session is still the current one; a
// stale renewal loop for a superseded session must not disturb its successor.
func (r *Runner) lost(err error, session *Session) {
	r.mu.Lock()
	dropped := r.session != nil && r.session == session
	if dropped {
		r.dropSessionLocked()
	}
	r.mu.Unlock()
	if dropped && r.logger != nil {
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
			// Anchor the local deadline before the round trip so the local
			// view of the lease is never more optimistic than the one
			// PostgreSQL granted.
			started := r.now()
			ctx, cancel := context.WithTimeout(context.WithoutCancel(sessionCtx), r.renewTimeout)
			err := session.Renew(ctx, r.pool)
			cancel()
			if err != nil {
				r.lost(err, session)
				return
			}
			r.mu.Lock()
			if r.session == session {
				r.localExpiry = started.Add(r.leaseTTL)
			}
			r.mu.Unlock()
		}
	}
}

// acquire blocks until the lease is acquired or ctx ends. Real database
// errors abort the wait; a held lease is retried. The returned deadline is
// anchored before the successful round trip, so it can never be later than
// the lease the database granted.
func (r *Runner) acquire(ctx context.Context) (*Session, time.Time, error) {
	for {
		started := r.now()
		session, err := Acquire(ctx, r.pool, r.identity, r.leaseTTL)
		if err == nil {
			return session, started.Add(r.leaseTTL), nil
		}
		if !errors.Is(err, ErrLeaseHeld) {
			return nil, time.Time{}, err
		}
		select {
		case <-ctx.Done():
			return nil, time.Time{}, ctx.Err()
		case <-time.After(r.retryInterval):
		}
	}
}

// installSession makes the acquired session current and starts its renewal
// loop. It refuses after Stop so a stopped runner can never resurrect
// leadership.
func (r *Runner) installSession(ctx context.Context, session *Session, deadline time.Time) (sessionSnapshot, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return sessionSnapshot{}, false
	}
	r.dropSessionLocked()
	sessionCtx, cancel := context.WithCancel(WithFence(ctx, session))
	stop := make(chan struct{})
	r.session = session
	r.sessionCtx = sessionCtx
	r.cancelSession = cancel
	r.renewStop = stop
	r.localExpiry = deadline
	go r.renewLoop(session, sessionCtx, stop)
	return sessionSnapshot{session: session, ctx: sessionCtx}, true
}

// WithSession runs body under an active leadership session with its own
// context. The context is cancelled as soon as renewal fails, which aborts
// in-flight fenced transactions before they can commit. If the body cannot
// complete under the term, WithSession returns ErrLeadershipLost; a parent
// context error is propagated as-is.
func (r *Runner) WithSession(ctx context.Context, body func(sessionCtx context.Context, session *Session) error) error {
	snap, ok := r.currentSession()
	if !ok {
		// Acquire without holding the lock: the retry loop may block while
		// another instance holds an unexpired lease, and renewal-loss
		// handling must stay responsive meanwhile.
		session, deadline, err := r.acquire(ctx)
		if err != nil {
			return err
		}
		snap, ok = r.installSession(ctx, session, deadline)
		if !ok {
			return ErrRunnerStopped
		}
		if r.logger != nil {
			r.logger.Info("scheduler leadership acquired", "instance_id", r.identity.InstanceID.String(), "epoch", session.Epoch())
		}
	}

	err := body(snap.ctx, snap.session)
	if errors.Is(err, ErrNotLeader) {
		r.lost(err, snap.session)
		return ErrLeadershipLost
	}
	if err != nil {
		return err
	}
	if snap.ctx.Err() != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrLeadershipLost
	}
	return nil
}

// Stop releases the runner: it drops any active session, stops renewal, and
// rejects later session installs on this runner. The lease then expires by
// PostgreSQL time and another instance takes over with a higher epoch.
func (r *Runner) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopped = true
	r.dropSessionLocked()
}
