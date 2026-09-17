package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/coordination"
	"github.com/GentleKingson/ocservia/control-plane/internal/useroperations"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestMaintenanceOrderAndErrors(t *testing.T) {
	for _, fail := range []string{"", "users", "rollouts", "telemetry", "certificates", "audit", "evidence"} {
		t.Run("fail="+fail, func(t *testing.T) {
			var called []string
			failure := errors.New("maintenance failed")
			step := func(name string) func(context.Context) error {
				return func(context.Context) error {
					called = append(called, name)
					if name == fail {
						return failure
					}
					return nil
				}
			}
			work := maintenanceWork{users: step("users"), rollouts: step("rollouts"), telemetry: step("telemetry"), certificates: step("certificates"), audit: step("audit")}
			work.evidence = func(ctx context.Context, _ *coordination.Session) error { return step("evidence")(ctx) }
			err := work.run(context.Background(), nil, time.Now(), 50, quietLogger())
			want := []string{"users", "rollouts", "telemetry", "certificates", "audit", "evidence"}
			if fail != "" && fail != "rollouts" {
				if !errors.Is(err, failure) {
					t.Fatalf("lost step failure: %v", err)
				}
				for i, name := range want {
					if name == fail {
						want = want[:i+1]
						break
					}
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(called, want) {
				t.Fatalf("steps = %v, want %v", called, want)
			}
		})
	}
}

type schedulerLeaderStub struct {
	session func(context.Context, func(context.Context, *coordination.Session) error) error
	stopped bool
}

func (s *schedulerLeaderStub) WithSession(ctx context.Context, body func(context.Context, *coordination.Session) error) error {
	return s.session(ctx, body)
}
func (s *schedulerLeaderStub) Stop() { s.stopped = true }

func TestSchedulerLeadershipRetry(t *testing.T) {
	for _, lost := range []error{coordination.ErrLeadershipLost, coordination.ErrNotLeader, context.Canceled, &useroperations.EnforcementCleanupError{Err: coordination.ErrNotLeader}, &useroperations.EnforcementCleanupError{Err: context.Canceled}} {
		t.Run(lost.Error(), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ticks := make(chan time.Time)
			attempts := make(chan struct{}, 2)
			calls := 0
			leader := &schedulerLeaderStub{session: func(context.Context, func(context.Context, *coordination.Session) error) error {
				calls++
				attempts <- struct{}{}
				if calls == 1 {
					return lost
				}
				cancel()
				return nil
			}}
			done := make(chan error, 1)
			go func() { done <- runScheduler(ctx, leader, ticks, maintenanceWork{}, 50, quietLogger()) }()
			select {
			case <-attempts:
			case <-time.After(2 * time.Second):
				t.Fatal("first run was not immediate")
			}
			select {
			case ticks <- time.Now():
			case err := <-done:
				t.Fatalf("leadership loss was fatal: %v", err)
			case <-time.After(2 * time.Second):
				t.Fatal("not waiting for next tick")
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) || calls != 2 || !leader.stopped {
					t.Fatalf("retry/stop: calls=%d stopped=%v err=%v", calls, leader.stopped, err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("scheduler did not stop")
			}
		})
	}
}

type schedulerLog struct {
	slog.Handler
	records chan slog.Record
}

func (h schedulerLog) Handle(ctx context.Context, record slog.Record) error {
	h.records <- record.Clone()
	return h.Handler.Handle(ctx, record)
}

func TestSchedulerCleanupFailureWaitsForTick(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ticks := make(chan time.Time)
	log := schedulerLog{quietLogger().Handler(), make(chan slog.Record, 8)}
	var steps []string
	failed := true
	work := maintenanceWork{users: func(context.Context) error {
		steps = append(steps, "users")
		if failed {
			return &useroperations.EnforcementCleanupError{Err: errors.New("delete denied")}
		}
		return nil
	}}
	step := func(name string) func(context.Context) error {
		return func(context.Context) error { steps = append(steps, name); return nil }
	}
	work.rollouts, work.telemetry, work.certificates, work.audit = step("rollouts"), step("telemetry"), step("certificates"), step("audit")
	work.evidence = func(context.Context, *coordination.Session) error {
		steps = append(steps, "evidence")
		cancel()
		return nil
	}
	leader := &schedulerLeaderStub{session: func(ctx context.Context, body func(context.Context, *coordination.Session) error) error {
		return body(ctx, nil)
	}}
	done := make(chan error, 1)
	go func() { done <- runScheduler(ctx, leader, ticks, work, 1, slog.New(log)) }()
	select {
	case record := <-log.records:
		if record.Level != slog.LevelError || record.Message != "policy enforcement cleanup failed; remaining maintenance skipped; retry on next tick" || !reflect.DeepEqual(steps, []string{"users"}) {
			t.Fatal("failed pass produced success or continued maintenance", record, steps)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cleanup failure was not logged")
	}
	failed = false
	select {
	case ticks <- time.Now():
	case err := <-done:
		t.Fatalf("cleanup failure exited scheduler: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not wait for tick")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || !leader.stopped || !reflect.DeepEqual(steps, []string{"users", "users", "rollouts", "telemetry", "certificates", "audit", "evidence"}) {
			t.Fatal("retry order/stop", steps, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not stop")
	}
}

func TestSchedulerCanceledCleanupDoesNotRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	leader := &schedulerLeaderStub{session: func(context.Context, func(context.Context, *coordination.Session) error) error {
		cancel()
		return &useroperations.EnforcementCleanupError{Err: context.Canceled}
	}}
	if err := runScheduler(ctx, leader, nil, maintenanceWork{}, 1, quietLogger()); !errors.Is(err, context.Canceled) || !leader.stopped {
		t.Fatal("canceled cleanup did not stop", err)
	}
}

func TestSchedulerFailureIsNotRetried(t *testing.T) {
	failure := errors.New("database maintenance failed")
	leader := &schedulerLeaderStub{session: func(context.Context, func(context.Context, *coordination.Session) error) error { return failure }}
	if err := runScheduler(context.Background(), leader, nil, maintenanceWork{}, 50, quietLogger()); !errors.Is(err, failure) || !leader.stopped {
		t.Fatalf("failure/stop: %v", err)
	}
}

func TestSchedulerCanceledParentPreservesOtherFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	failure := errors.New("database maintenance failed")
	leader := &schedulerLeaderStub{session: func(context.Context, func(context.Context, *coordination.Session) error) error {
		cancel()
		return failure
	}}
	if err := runScheduler(ctx, leader, nil, maintenanceWork{}, 1, quietLogger()); !errors.Is(err, failure) || !leader.stopped {
		t.Fatal("cancellation masked an unrelated fatal maintenance error", err)
	}
}
