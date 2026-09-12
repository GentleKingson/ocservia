package enrollment

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/mysql"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	enrollmentstore "github.com/GentleKingson/ocservia/control-plane/internal/enrollment/store"
	localstore "github.com/GentleKingson/ocservia/control-plane/internal/localslice/store"
	"github.com/google/uuid"
)

type lifecycleFixture struct {
	b         database.Backend
	workspace uuid.UUID
}

func newLifecycleFixture(t *testing.T) lifecycleFixture {
	t.Helper()
	f := lifecycleFixture{enrollmentBackend(t), uuid.Must(uuid.NewV7())}
	f.within(t, func(tx database.Tx) error {
		s, err := localstore.Local(tx)
		if err != nil {
			return err
		}
		at, err := database.WallTime(context.Background(), tx)
		if err != nil {
			return err
		}
		_, err = s.EnsureWorkspace(context.Background(), f.workspace, f.workspace.String(), at)
		return err
	})
	return f
}

func (f lifecycleFixture) within(t *testing.T, fn func(database.Tx) error) {
	t.Helper()
	if err := database.Within(context.Background(), f.b, database.ReadCommitted, fn); err != nil {
		t.Fatal(err)
	}
}

func (f lifecycleFixture) query(pg, my string, args ...any) (string, []any) {
	if _, ok := f.b.(*mysql.Backend); ok {
		pg = my
		for i, arg := range args {
			if id, ok := arg.(uuid.UUID); ok {
				args[i] = mysql.UUIDBytes(id)
			}
		}
	}
	return pg, args
}

func (f lifecycleFixture) exec(t *testing.T, pg, my string, args ...any) {
	t.Helper()
	q, args := f.query(pg, my, args...)
	if _, err := f.b.Exec(context.Background(), q, args...); err != nil {
		t.Fatal(err)
	}
}

func (f lifecycleFixture) trust(t *testing.T, fn func(enrollmentstore.TrustStore) error) {
	t.Helper()
	f.within(t, func(tx database.Tx) error {
		s, err := enrollmentstore.Trust(tx)
		if err != nil {
			return err
		}
		return fn(s)
	})
}

func (f lifecycleFixture) job(t *testing.T) enrollmentstore.TrustJob {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	v := enrollmentstore.TrustJob{NodeID: id, EndpointID: append(bytes.Clone(id[:]), id[:]...), DesiredState: "revoked", Revision: 2, Reason: "PR-05"}
	f.within(t, func(tx database.Tx) error {
		s, err := enrollmentstore.Enrollment(tx)
		if err != nil {
			return err
		}
		at, err := database.WallTime(context.Background(), tx)
		if err != nil {
			return err
		}
		if err := s.InsertNode(context.Background(), id, f.workspace, id.String(), at); err != nil {
			return err
		}
		return s.InsertEndpoint(context.Background(), id, v.EndpointID, at)
	})
	f.trust(t, func(s enrollmentstore.TrustStore) error {
		return s.Enqueue(context.Background(), v, value.Timestamp{Valid: true, Micros: value.NegativeInfinity})
	})
	return v
}

type trustSnapshot struct {
	Revision                  uint64
	Attempts                  int
	Update, Required, Close   bool
	Until, Available, Updated value.Timestamp
	Worker                    *uuid.UUID
	Error                     *string
}

func (f lifecycleFixture) snapshot(t *testing.T, id uuid.UUID) trustSnapshot {
	t.Helper()
	q, args := f.query(`SELECT revision,attempts,update_applied,close_required,close_applied,locked_until,available_at,updated_at,locked_by,last_error FROM node_trust_convergence WHERE node_id=$1`, `SELECT revision,attempts,update_applied,close_required,close_applied,locked_until,available_at,updated_at,locked_by,last_error FROM node_trust_convergence WHERE node_id=?`, id)
	var v trustSnapshot
	if err := f.b.QueryRow(context.Background(), q, args...).Scan(&v.Revision, &v.Attempts, &v.Update, &v.Required, &v.Close, &v.Until, &v.Available, &v.Updated, &v.Worker, &v.Error); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestTrustLeaseBackendIntegration(t *testing.T) {
	f, ctx := newLifecycleFixture(t), context.Background()
	worker := uuid.New()
	v := f.job(t)
	claim := func(t *testing.T) enrollmentstore.TrustJob {
		t.Helper()
		var got enrollmentstore.TrustJob
		f.trust(t, func(s enrollmentstore.TrustStore) error { var err error; got, err = s.Claim(ctx, worker); return err })
		if got.NodeID != v.NodeID {
			t.Fatalf("claimed wrong node: %v", got.NodeID)
		}
		return got
	}
	v = claim(t)
	stale := func(t *testing.T, old enrollmentstore.TrustJob) {
		t.Helper()
		before := f.snapshot(t, v.NodeID)
		f.trust(t, func(s enrollmentstore.TrustStore) error {
			for _, action := range []func(context.Context, enrollmentstore.TrustJob, uuid.UUID) (bool, error){s.Renew, s.MarkUpdateApplied, s.MarkCloseApplied} {
				if changed, err := action(ctx, old, worker); err != nil || changed {
					t.Fatalf("stale mutation: %v %v", changed, err)
				}
			}
			if err := s.Release(ctx, old, worker, time.Hour, "stale"); err != nil {
				return err
			}
			return s.UnlockComplete(ctx, old, worker)
		})
		if !reflect.DeepEqual(before, f.snapshot(t, v.NodeID)) {
			t.Fatal("stale claim changed durable state")
		}
	}
	t.Run("expired-unclaimed", func(t *testing.T) {
		f.exec(t, `UPDATE node_trust_convergence SET locked_until=$1 WHERE node_id=$2`, `UPDATE node_trust_convergence SET locked_until=? WHERE node_id=?`, value.Timestamp{Valid: true, Micros: value.NegativeInfinity}, v.NodeID)
		stale(t, v)
	})
	t.Run("same-worker-new-attempt", func(t *testing.T) {
		old := v
		v = claim(t)
		if v.Attempts != old.Attempts+1 {
			t.Fatal("attempt not advanced")
		}
		stale(t, old)
	})
	t.Run("expiry-during-lock-wait", func(t *testing.T) {
		lock, err := f.b.Begin(ctx, database.ReadCommitted)
		if err != nil {
			t.Fatal(err)
		}
		defer rollback(lock)
		at, err := database.WallTime(ctx, lock)
		if err != nil {
			t.Fatal(err)
		}
		until, _ := at.Add(300 * time.Millisecond)
		q, args := f.query(`UPDATE node_trust_convergence SET locked_until=$1 WHERE node_id=$2`, `UPDATE node_trust_convergence SET locked_until=? WHERE node_id=?`, until, v.NodeID)
		if _, err := lock.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
		waiting, err := f.b.Begin(ctx, database.ReadCommitted)
		if err != nil {
			t.Fatal(err)
		}
		defer rollback(waiting)
		s, err := enrollmentstore.Trust(waiting)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			changed, err := s.Renew(ctx, v, worker)
			if changed {
				err = errors.New("renewed expired claim")
			}
			done <- err
		}()
		time.Sleep(600 * time.Millisecond)
		if err := lock.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if err := waiting.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		stale(t, v)
		v = claim(t)
	})
	t.Run("new-revision", func(t *testing.T) {
		old := v
		v.Revision++
		f.trust(t, func(s enrollmentstore.TrustStore) error {
			return s.Enqueue(ctx, v, value.Timestamp{Valid: true, Micros: value.NegativeInfinity})
		})
		v = claim(t)
		stale(t, old)
		f.trust(t, func(s enrollmentstore.TrustStore) error {
			if changed, err := s.MarkCloseApplied(ctx, v, worker); err != nil || changed {
				t.Fatal("close before update", changed, err)
			}
			for range 2 {
				if changed, err := s.Renew(ctx, v, worker); err != nil || !changed {
					t.Fatal("renew", changed, err)
				}
			}
			if changed, err := s.MarkUpdateApplied(ctx, v, worker); err != nil || !changed {
				t.Fatal(changed, err)
			}
			return s.Release(ctx, v, worker, 0, "close retry")
		})
		f.exec(t, `UPDATE node_trust_convergence SET available_at=$1 WHERE node_id=$2`, `UPDATE node_trust_convergence SET available_at=? WHERE node_id=?`, value.Timestamp{Valid: true, Micros: value.NegativeInfinity}, v.NodeID)
		v = claim(t)
		if !v.UpdateApplied || !v.CloseRequired || v.CloseApplied {
			t.Fatal("lost retry progress", v)
		}
		f.trust(t, func(s enrollmentstore.TrustStore) error {
			changed, err := s.MarkCloseApplied(ctx, v, worker)
			if !changed && err == nil {
				return errors.New("close not marked")
			}
			return err
		})
		if got := f.snapshot(t, v.NodeID); !got.Close || got.Worker != nil || got.Error != nil {
			t.Fatal(got)
		}
	})
}

type leasedTransport struct {
	update func(context.Context) error
	closes int
}

func (s *leasedTransport) UpdateNodeTrust(ctx context.Context, _, _ []byte, _ transportv1.NodeTrustState, _ string, _ uint64, _ []byte, _ *agentv1.FenceBindingV2) error {
	return s.update(ctx)
}
func (s *leasedTransport) CloseNode(context.Context, []byte, string, *agentv1.FenceBindingV2) error {
	s.closes++
	return nil
}

func TestTrustWorkerRenewalBackendIntegration(t *testing.T) {
	f, ctx := newLifecycleFixture(t), context.Background()
	v := f.job(t)
	transport := &leasedTransport{}
	worker, err := NewTrustConvergenceWorkerBackend(f.b, transport, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	transport.update = func(ctx context.Context) error {
		before := f.snapshot(t, v.NodeID)
		timer := time.NewTimer(enrollmentstore.TrustLeaseTTL + time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
		after := f.snapshot(t, v.NodeID)
		if after.Until.Micros <= before.Until.Micros || after.Attempts != before.Attempts {
			return errors.New("in-flight lease was not renewed")
		}
		return nil
	}
	if worked, err := worker.RunOnce(ctx); err != nil || !worked {
		t.Fatal(worked, err)
	}
	if got := f.snapshot(t, v.NodeID); !got.Update || !got.Close || got.Worker != nil || transport.closes != 1 {
		t.Fatal(got, transport.closes)
	}
	// Supersede intent during transport. The renewal loop must cancel the old
	// request and must not record its late success or close the node afterward.
	v = f.job(t)
	transport.update = func(ctx context.Context) error {
		v.Revision++
		f.trust(t, func(s enrollmentstore.TrustStore) error {
			return s.Enqueue(ctx, v, value.Timestamp{Valid: true, Micros: value.NegativeInfinity})
		})
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(enrollmentstore.TrustLeaseTTL):
			return errors.New("lost lease did not cancel transport")
		}
	}
	if worked, err := worker.RunOnce(ctx); !worked || !errors.Is(err, enrollmentstore.ErrTrustLeaseLost) {
		t.Fatal(worked, err)
	}
	if got := f.snapshot(t, v.NodeID); got.Update || got.Close || got.Worker != nil || transport.closes != 1 {
		t.Fatal("stale transport result escaped", got)
	}
	transport.update = func(context.Context) error { return nil }
	if worked, err := worker.RunOnce(ctx); !worked || err != nil {
		t.Fatal(worked, err)
	}
}

func TestEnrollmentSafetyBackendIntegration(t *testing.T) {
	f, ctx := newLifecycleFixture(t), context.Background()
	signer, err := commandauth.NewRandomSigner()
	if err != nil {
		t.Fatal(err)
	}
	s := NewBackend(f.b, "", "test", signer)
	t.Run("expiry-before-consumption-rolls-back", func(t *testing.T) {
		for i, bootstrap := range []bool{false, true} {
			endpoint := endpointFixture(byte(211 + i))
			var token Token
			if bootstrap {
				token, err = s.CreateBootstrapToken(ctx, BootstrapTokenSpec{WorkspaceID: f.workspace, Environment: "test", ActorID: "operator", Reason: "expiry", RequestID: uuid.NewString()})
				if err != nil {
					t.Fatal(err)
				}
			} else {
				token = createToken(t, s, f.workspace, endpoint)
			}
			request := enrollmentRequest(token.Value, endpoint)
			calls := 0
			s.now = func() time.Time {
				calls++
				if calls == 1 {
					return token.ExpiresAt.Add(-time.Second)
				}
				return token.ExpiresAt.Add(time.Second)
			}
			if _, err := s.Enroll(ctx, request); !errors.Is(err, ErrInvalidToken) {
				t.Fatal("expired token consumed", err)
			}
			s.now = time.Now
			f.within(t, func(tx database.Tx) error {
				store, err := enrollmentstore.Enrollment(tx)
				if err != nil {
					return err
				}
				if exists, err := store.EndpointExists(ctx, endpoint); err != nil || exists {
					t.Fatal("partial endpoint", exists, err)
				}
				return nil
			})
			if _, err := s.Enroll(ctx, request); err != nil {
				t.Fatal("rollback consumed credential", err)
			}
		}
	})
	t.Run("bootstrap-concurrent-endpoint-and-revoked-replay", func(t *testing.T) {
		token, err := s.CreateBootstrapToken(ctx, BootstrapTokenSpec{WorkspaceID: f.workspace, Environment: "test", ActorID: "operator", Reason: "binding", RequestID: uuid.NewString()})
		if err != nil {
			t.Fatal(err)
		}
		requests := []*agentv1.EnrollRequest{enrollmentRequest(token.Value, endpointFixture(213)), enrollmentRequest(token.Value, endpointFixture(214))}
		var wg sync.WaitGroup
		responses := make(chan *agentv1.EnrollResponse, 2)
		errs := make(chan error, 2)
		for _, request := range requests {
			wg.Add(1)
			go func() { defer wg.Done(); v, err := s.Enroll(ctx, request); responses <- v; errs <- err }()
		}
		wg.Wait()
		var node uuid.UUID
		for range 2 {
			if v := <-responses; v != nil {
				if node != uuid.Nil {
					t.Fatal("two endpoints consumed bootstrap")
				}
				node, _ = uuid.FromBytes(v.NodeId)
			}
			if err := <-errs; err != nil && !errors.Is(err, ErrEndpointMismatch) {
				t.Fatal(err)
			}
		}
		if node == uuid.Nil {
			t.Fatal("no endpoint bound")
		}
		f.within(t, func(tx database.Tx) error {
			store, err := enrollmentstore.Enrollment(tx)
			if err != nil {
				return err
			}
			at, err := database.WallTime(ctx, tx)
			if err != nil {
				return err
			}
			_, err = store.Revoke(ctx, node, at)
			return err
		})
		for _, request := range requests {
			if err := s.ValidateEnrollment(ctx, request); !errors.Is(err, ErrInvalidToken) {
				t.Fatal("revoked bootstrap preflight", err)
			}
			if _, err := s.Enroll(ctx, request); err == nil {
				t.Fatal("revoked bootstrap replay")
			}
		}
	})
	t.Run("cross-workspace-endpoint-conflict", func(t *testing.T) {
		other := uuid.Must(uuid.NewV7())
		// Use the same database, creating another workspace through its store.
		f.within(t, func(tx database.Tx) error {
			store, err := localstore.Local(tx)
			if err != nil {
				return err
			}
			at, err := database.WallTime(ctx, tx)
			if err != nil {
				return err
			}
			_, err = store.EnsureWorkspace(ctx, other, other.String(), at)
			return err
		})
		endpoint := endpointFixture(215)
		first := createToken(t, s, f.workspace, endpoint)
		if _, err := s.Enroll(ctx, enrollmentRequest(first.Value, endpoint)); err != nil {
			t.Fatal(err)
		}
		second := createToken(t, s, other, endpoint)
		if _, err := s.Enroll(ctx, enrollmentRequest(second.Value, endpoint)); err == nil {
			t.Fatal("cross-workspace endpoint reused")
		}
		if err := s.ValidateEnrollment(ctx, enrollmentRequest(second.Value, endpoint)); err != nil {
			t.Fatal("failed enrollment consumed token", err)
		}
	})
	if result, err := audit.NewBackendManager(f.b, nil).Verify(ctx, f.workspace); err != nil || !result.Valid {
		t.Fatal("audit chain", result, err)
	}
}
