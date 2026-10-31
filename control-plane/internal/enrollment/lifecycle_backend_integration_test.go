package enrollment

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"slices"
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
	"github.com/GentleKingson/ocservia/control-plane/internal/ownersession"
	eventstore "github.com/GentleKingson/ocservia/control-plane/internal/transportevents/store"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type lifecycleFixture struct {
	b         database.Backend
	workspace uuid.UUID
}

func newLifecycleFixture(t *testing.T) lifecycleFixture {
	t.Helper()
	f := lifecycleFixture{enrollmentBackend(t), uuid.Must(uuid.NewV7())}
	f.within(t, func(tx database.Tx) error {
		s, err := eventstore.FromTransaction(tx)
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
	v := enrollmentstore.TrustJob{NodeID: id, EndpointID: append(bytes.Clone(id[:]), id[:]...), DesiredState: "revoked", Revision: 2, Reason: "test trust revocation"}
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
	close  func(context.Context) error
	closes int
}

func (s *leasedTransport) UpdateNodeTrust(ctx context.Context, _, _ []byte, _ transportv1.NodeTrustState, _ string, _ uint64, _ []byte, _ *agentv1.FenceBindingV2) error {
	return s.update(ctx)
}
func (s *leasedTransport) CloseNode(ctx context.Context, _ []byte, _ string, _ *agentv1.FenceBindingV2) error {
	s.closes++
	if s.close != nil {
		return s.close(ctx)
	}
	return nil
}

// The injector knows whether a side effect ran; the worker only sees the RPC
// result. Equal errors before and after execution must leave equal durable
// progress. This does not claim that a fake proves transport idempotency.
func TestTrustUncertainRPCBackendIntegration(t *testing.T) {
	for _, operation := range []string{"update", "close"} {
		for _, code := range []codes.Code{codes.OK, codes.DeadlineExceeded, codes.Unavailable, codes.Canceled} {
			for _, after := range []bool{false, true} {
				if code == codes.OK && !after {
					continue
				}
				name := fmt.Sprintf("%s/%s/after-execution=%t", operation, code, after)
				t.Run(name, func(t *testing.T) {
					f, ctx := newLifecycleFixture(t), context.Background()
					v := f.job(t)
					// Commit real revoked authority as well as the queue intent.
					f.within(t, func(tx database.Tx) error {
						s, err := enrollmentstore.Enrollment(tx)
						if err != nil {
							return err
						}
						at, err := database.WallTime(ctx, tx)
						if err != nil {
							return err
						}
						revision, err := s.Revoke(ctx, v.NodeID, at)
						if err == nil && revision != v.Revision {
							return errors.New("fixture revision mismatch")
						}
						return err
					})
					bindings, err := NewBackend(f.b, "", "", nil).ListNodeTrust(ctx)
					if err != nil || !slices.ContainsFunc(bindings, func(binding *transportv1.NodeTrustBinding) bool {
						return bytes.Equal(binding.NodeId, v.NodeID[:]) && binding.Revision == v.Revision && binding.State == transportv1.NodeTrustState_NODE_TRUST_STATE_REVOKED
					}) {
						t.Fatal("revoked authority not committed", bindings, err)
					}
					effects, updates := 0, 0
					inject := func(context.Context) error {
						if after {
							effects++
						}
						return status.Error(code, "injected RPC outcome")
					}
					transport := &leasedTransport{update: func(ctx context.Context) error {
						updates++
						if operation == "update" {
							return inject(ctx)
						}
						return nil
					}}
					if operation == "close" {
						transport.close = inject
					}
					worker, err := NewTrustConvergenceWorkerBackend(f.b, transport, slog.New(slog.NewTextHandler(io.Discard, nil)))
					if err != nil {
						t.Fatal(err)
					}
					worked, err := worker.RunOnce(ctx)
					if !worked || status.Code(err) != code {
						t.Fatal(worked, err)
					}
					got := f.snapshot(t, v.NodeID)
					wantUpdate := operation == "close" || code == codes.OK
					wantCloses := 0
					if wantUpdate {
						wantCloses = 1
					}
					if got.Update != wantUpdate || got.Close != (code == codes.OK) || !got.Required || got.Worker != nil || got.Attempts != 1 || (got.Error != nil) != (code != codes.OK) || transport.closes != wantCloses || effects != boolInt(after) {
						t.Fatalf("outcome=%+v effects=%d closes=%d", got, effects, transport.closes)
					}
					// Neither uncertain RPC results nor a subsequent lost owner
					// can manufacture the missing acknowledgement.
					if code != codes.OK {
						f.exec(t, `UPDATE node_trust_convergence SET available_at=$1 WHERE node_id=$2`, `UPDATE node_trust_convergence SET available_at=? WHERE node_id=?`, value.Timestamp{Valid: true, Micros: value.NegativeInfinity}, v.NodeID)
						transport.update = func(context.Context) error { updates++; return ownersession.ErrNotOwner }
						transport.close = func(context.Context) error { return ownersession.ErrNotOwner }
						if worked, err := worker.RunOnce(ctx); !worked || !errors.Is(err, ownersession.ErrNotOwner) {
							t.Fatal(worked, err)
						}
						got = f.snapshot(t, v.NodeID)
						if got.Update != wantUpdate || got.Close || got.Attempts != 2 || got.Error == nil {
							t.Fatal("owner loss acknowledged an unknown result", got)
						}
						if operation == "close" && updates != 1 {
							t.Fatal("replayed acknowledged update", updates)
						}
					}
					t.Logf("RPC=%s side_effect=%t DB update=%t close=%t attempts=%d", code, after, got.Update, got.Close, got.Attempts)
				})
			}
		}
	}
}

func TestTrustOwnerEndedBackendIntegration(t *testing.T) {
	for _, ending := range []string{"closed", "expired", "replaced"} {
		t.Run(ending, func(t *testing.T) {
			f, ctx := newLifecycleFixture(t), context.Background()
			v := f.job(t)
			signer, err := commandauth.NewRandomSigner()
			if err != nil {
				t.Fatal(err)
			}
			manager, err := ownersession.NewManagerBackend(f.b, signer, &recordingRegistrar{}, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			var endpoint [32]byte
			copy(endpoint[:], v.EndpointID)
			fence, err := manager.OpenSession(ctx, [16]byte(v.NodeID), endpoint, 1, []string{ownersession.FencingCapability})
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			action := func(context.Context, *agentv1.ConnectionFenceV2, *agentv1.FenceBindingV2) error { calls++; return nil }
			if err := manager.ExecuteFenced(ctx, [16]byte(v.NodeID), agentv1.FenceOperationKind_FENCE_OPERATION_KIND_CONNECTION_CLOSE, [16]byte(v.NodeID), ownersession.FencingCapability, action); err != nil || calls != 1 {
				t.Fatal("live owner refused", err, calls)
			}
			var connection [16]byte
			copy(connection[:], fence.ConnectionId)
			if ending == "closed" {
				if err := manager.CloseSession(ctx, [16]byte(v.NodeID), connection, int64(fence.OwnerEpoch)); err != nil {
					t.Fatal(err)
				}
			} else {
				// Wait for the actual signed lease deadline. Shortening only the
				// database deadline would not expire the already-issued fence.
				time.Sleep(time.Until(fence.LeaseUntil.AsTime()) + 50*time.Millisecond)
				if ending == "replaced" {
					successor, err := ownersession.NewManagerBackend(f.b, signer, &recordingRegistrar{}, 30*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
					if err != nil {
						t.Fatal(err)
					}
					next, err := successor.OpenSession(ctx, [16]byte(v.NodeID), endpoint, 1, []string{ownersession.FencingCapability})
					if err != nil || next.GetOwnerEpoch() <= fence.OwnerEpoch {
						t.Fatal(next, err)
					}
				}
			}
			if err := manager.ExecuteFenced(ctx, [16]byte(v.NodeID), agentv1.FenceOperationKind_FENCE_OPERATION_KIND_CONNECTION_CLOSE, [16]byte(v.NodeID), ownersession.FencingCapability, action); !errors.Is(err, ownersession.ErrNotOwner) || calls != 1 {
				t.Fatal("ended owner issued binding", err, calls)
			}
		})
	}
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func TestTrustSynchronousWorkerRaceBackendIntegration(t *testing.T) {
	for _, success := range []bool{false, true} {
		t.Run(fmt.Sprintf("late-worker-success=%t", success), func(t *testing.T) {
			f, ctx := newLifecycleFixture(t), context.Background()
			v := f.job(t)
			service := NewBackend(f.b, "", "", nil)
			entered, resume := make(chan struct{}), make(chan struct{})
			transport := &leasedTransport{update: func(context.Context) error {
				close(entered)
				<-resume
				if !success {
					return status.Error(codes.Unavailable, "late failure")
				}
				return nil
			}}
			worker, err := NewTrustConvergenceWorkerBackend(f.b, transport, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { _, err := worker.RunOnce(ctx); done <- err }()
			<-entered
			// The API records its own acknowledgements while the worker holds
			// a claim, then the worker delivers a late result for that intent.
			recorded, recordErr := service.RecordRevocationSynchronized(ctx, NodeTrust{v.NodeID, v.EndpointID, v.Revision}, v.Reason, true)
			close(resume)
			err = <-done
			if recordErr != nil || !recorded || (err == nil) != success {
				t.Fatal(recorded, recordErr, err)
			}
			got := f.snapshot(t, v.NodeID)
			if !got.Update || !got.Close || got.Worker != nil || got.Attempts != 1 {
				t.Fatal("late worker lost synchronous evidence", got)
			}
			if worked, err := worker.RunOnce(ctx); worked || err != nil {
				t.Fatal("settled intent reclaimed", worked, err)
			}
		})
	}
}

func TestTrustSupersededResultBackendIntegration(t *testing.T) {
	f, ctx := newLifecycleFixture(t), context.Background()
	v := f.job(t)
	old := v
	transport := &leasedTransport{update: func(context.Context) error {
		v.Revision++
		v.EndpointID = bytes.Repeat([]byte{9}, 32)
		v.Reason = "superseding intent"
		f.trust(t, func(s enrollmentstore.TrustStore) error {
			return s.Enqueue(ctx, v, value.Timestamp{Valid: true, Micros: value.NegativeInfinity})
		})
		return nil
	}}
	worker, err := NewTrustConvergenceWorkerBackend(f.b, transport, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := worker.RunOnce(ctx); !worked || err == nil {
		t.Fatal(worked, err)
	}
	before := f.snapshot(t, v.NodeID)
	if before.Revision != v.Revision || before.Update || before.Close || transport.closes != 0 {
		t.Fatal(before)
	}
	service := NewBackend(f.b, "", "", nil)
	if changed, err := service.RecordRevocationSynchronized(ctx, NodeTrust{old.NodeID, old.EndpointID, old.Revision}, old.Reason, true); changed || err != nil {
		t.Fatal(changed, err)
	}
	if !reflect.DeepEqual(before, f.snapshot(t, v.NodeID)) {
		t.Fatal("old synchronous result changed new intent")
	}
	transport.update = func(context.Context) error { return nil }
	if worked, err := worker.RunOnce(ctx); !worked || err != nil {
		t.Fatal(worked, err)
	}
	if got := f.snapshot(t, v.NodeID); !got.Update || !got.Close {
		t.Fatal(got)
	}
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

type ownerLostTransport struct{ calls int }

func (s *ownerLostTransport) UpdateNodeTrust(context.Context, []byte, []byte, transportv1.NodeTrustState, string, uint64, []byte, *agentv1.FenceBindingV2) error {
	s.calls++
	return ownersession.ErrNotOwner
}
func (s *ownerLostTransport) CloseNode(context.Context, []byte, string, *agentv1.FenceBindingV2) error {
	s.calls++
	return ownersession.ErrNotOwner
}

// A revocation request that closed the owner's connection leaves the worker
// without an owner. Lost ownership must never count as delivery; only the
// request's own fenced successes for the exact queued intent settle it.
func TestTrustSynchronousRevocationBackendIntegration(t *testing.T) {
	f, ctx := newLifecycleFixture(t), context.Background()
	service := NewBackend(f.b, "", "", nil)
	transport := &ownerLostTransport{}
	worker, err := NewTrustConvergenceWorkerBackend(f.b, transport, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	v := f.job(t)
	if worked, err := worker.RunOnce(ctx); !worked || !errors.Is(err, ownersession.ErrNotOwner) {
		t.Fatal(worked, err)
	}
	if got := f.snapshot(t, v.NodeID); got.Update || got.Close || got.Error == nil {
		t.Fatal("lost ownership settled trust convergence", got)
	}
	before := f.snapshot(t, v.NodeID)
	for _, other := range []struct {
		trust  NodeTrust
		reason string
	}{
		{NodeTrust{v.NodeID, v.EndpointID, v.Revision + 1}, v.Reason},
		{NodeTrust{v.NodeID, v.EndpointID, v.Revision - 1}, v.Reason},
		{NodeTrust{v.NodeID, bytes.Repeat([]byte{7}, 32), v.Revision}, v.Reason},
		{NodeTrust{v.NodeID, v.EndpointID, v.Revision}, "other reason"},
	} {
		if recorded, err := service.RecordRevocationSynchronized(ctx, other.trust, other.reason, true); err != nil || recorded {
			t.Fatal("settled a different intent", other, recorded, err)
		}
	}
	if !reflect.DeepEqual(before, f.snapshot(t, v.NodeID)) {
		t.Fatal("evidence for a different intent changed durable state")
	}
	trust := NodeTrust{v.NodeID, v.EndpointID, v.Revision}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if recorded, err := service.RecordRevocationSynchronized(cancelled, trust, v.Reason, true); err == nil || recorded {
		t.Fatal("cancelled bookkeeping claimed success", recorded, err)
	}
	if !reflect.DeepEqual(before, f.snapshot(t, v.NodeID)) {
		t.Fatal("failed bookkeeping changed durable progress")
	}
	writeFailure := errors.New("injected bookkeeping transaction failure")
	err = database.Within(ctx, f.b, database.ReadCommitted, func(tx database.Tx) error {
		store, err := enrollmentstore.Trust(tx)
		if err != nil {
			return err
		}
		if changed, err := store.RecordSynchronized(ctx, v, true); err != nil || !changed {
			return errors.Join(err, errors.New("bookkeeping write not exercised"))
		}
		return writeFailure
	})
	if !errors.Is(err, writeFailure) || !reflect.DeepEqual(before, f.snapshot(t, v.NodeID)) {
		t.Fatal("rolled-back bookkeeping left completion flags", err)
	}
	if recorded, err := service.RecordRevocationSynchronized(ctx, trust, v.Reason, false); err != nil || !recorded {
		t.Fatal(recorded, err)
	}
	if got := f.snapshot(t, v.NodeID); !got.Update || got.Close || got.Error != nil {
		t.Fatal("failed close was settled", got)
	}
	if recorded, err := service.RecordRevocationSynchronized(ctx, trust, v.Reason, true); err != nil || !recorded {
		t.Fatal(recorded, err)
	}
	f.exec(t, `UPDATE node_trust_convergence SET available_at=$1 WHERE node_id=$2`, `UPDATE node_trust_convergence SET available_at=? WHERE node_id=?`, value.Timestamp{Valid: true, Micros: value.NegativeInfinity}, v.NodeID)
	settled := f.snapshot(t, v.NodeID)
	if !settled.Update || !settled.Close {
		t.Fatal("synchronous revocation not settled", settled)
	}
	// Other fixtures' rows may still be pending; this one must not be claimed.
	_, _ = worker.RunOnce(ctx)
	if !reflect.DeepEqual(settled, f.snapshot(t, v.NodeID)) {
		t.Fatal("settled revocation was claimed again")
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
			store, err := eventstore.FromTransaction(tx)
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
