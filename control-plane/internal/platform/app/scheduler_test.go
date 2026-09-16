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
	for _, lost := range []error{coordination.ErrLeadershipLost, coordination.ErrNotLeader, context.Canceled} {
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

func TestSchedulerFailureIsNotRetried(t *testing.T) {
	failure := errors.New("database maintenance failed")
	leader := &schedulerLeaderStub{session: func(context.Context, func(context.Context, *coordination.Session) error) error { return failure }}
	if err := runScheduler(context.Background(), leader, nil, maintenanceWork{}, 50, quietLogger()); !errors.Is(err, failure) || !leader.stopped {
		t.Fatalf("failure/stop: %v", err)
	}
}
