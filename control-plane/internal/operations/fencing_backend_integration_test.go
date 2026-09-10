package operations_test

import (
	"context"
	"errors"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/connectionowner"
	"github.com/GentleKingson/ocservia/control-plane/internal/coordination"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/schedulerlease"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

func TestFencingBackendIntegration(t *testing.T) {
	f := newOutboxFixture(t)
	ctx := context.Background()
	identity := connectionowner.Identity{InstanceID: uuid.Must(uuid.NewV7()), Incarnation: 1}
	t.Run("scheduler-noop-rows-affected", func(t *testing.T) {
		err := database.Within(ctx, f.backend, database.ReadCommitted, func(tx database.Tx) error {
			store, err := schedulerlease.FromTransaction(tx)
			if err != nil {
				return err
			}
			state, err := store.Lock(ctx)
			if err != nil {
				return err
			}
			at, err := database.WallTime(ctx, tx)
			if err != nil {
				return err
			}
			if err := store.Put(ctx, state, at); err != nil {
				return err
			}
			return store.Put(ctx, state, at)
		})
		if err != nil {
			t.Fatal("unchanged matched row treated as missing", err)
		}
	})
	for _, action := range []string{"renew", "assert", "release"} {
		t.Run("owner-expired-during-"+action+"-wait", func(t *testing.T) {
			node := f.node(t)
			term, err := connectionowner.AcquireBackend(ctx, f.backend, node, identity, uuid.Must(uuid.NewV7()), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			locked := lockOwner(t, f, node)
			defer locked.Rollback(ctx)
			waiting, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				switch action {
				case "renew":
					_, err := term.RenewBackend(waiting, f.backend)
					done <- err
				case "release":
					done <- term.ReleaseBackend(waiting, f.backend)
				default:
					done <- term.AssertCurrentBackend(waiting, f.backend)
				}
			}()
			assertBlocked(t, done)
			time.Sleep(time.Until(term.LeaseUntil()) + 50*time.Millisecond)
			if err := locked.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; !errors.Is(err, connectionowner.ErrNotOwner) {
				t.Fatal("expired term revived", err)
			}
			state, err := connectionowner.ReadStateBackend(ctx, f.backend, node)
			if err != nil || state.Epoch != term.Epoch() || state.LeaseUntilValid {
				t.Fatal(state, err)
			}
		})
	}
	t.Run("owner-acquire-clock-after-lock", func(t *testing.T) {
		node := f.node(t)
		term, err := connectionowner.AcquireBackend(ctx, f.backend, node, identity, uuid.Must(uuid.NewV7()), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		locked := lockOwner(t, f, node)
		defer locked.Rollback(ctx)
		waiting, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		done := make(chan error, 1)
		var next *connectionowner.Term
		go func() {
			var err error
			next, err = connectionowner.AcquireBackend(waiting, f.backend, node, connectionowner.Identity{InstanceID: uuid.Must(uuid.NewV7()), Incarnation: 2}, uuid.Must(uuid.NewV7()), time.Second)
			done <- err
		}()
		assertBlocked(t, done)
		time.Sleep(time.Until(term.LeaseUntil()) + 50*time.Millisecond)
		if err := locked.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal("acquire used pre-wait clock", err)
		}
		if next.Epoch() <= term.Epoch() || time.Until(next.LeaseUntil()) < 800*time.Millisecond {
			t.Fatal("stale acquisition deadline", next.LeaseUntil())
		}
	})
	t.Run("two-controllers-and-epoch-retention", func(t *testing.T) {
		node := f.node(t)
		start := make(chan struct{})
		type acquired struct {
			term *connectionowner.Term
			err  error
		}
		done := make(chan acquired, 2)
		for range 2 {
			go func() {
				<-start
				term, err := connectionowner.AcquireBackend(ctx, f.backend, node, connectionowner.Identity{InstanceID: uuid.Must(uuid.NewV7()), Incarnation: 1}, uuid.Must(uuid.NewV7()), time.Minute)
				done <- acquired{term, err}
			}()
		}
		close(start)
		var winner *connectionowner.Term
		for range 2 {
			v := <-done
			if v.err == nil {
				if winner != nil {
					t.Fatal("two live owners")
				}
				winner = v.term
			} else if !errors.Is(v.err, connectionowner.ErrLeaseHeld) {
				t.Fatal(v.err)
			}
		}
		if winner == nil {
			t.Fatal("no owner")
		}
		if err := winner.ReleaseBackend(ctx, f.backend); err != nil {
			t.Fatal(err)
		}
		for range 3 {
			next, err := connectionowner.AcquireBackend(ctx, f.backend, node, identity, uuid.Must(uuid.NewV7()), time.Minute)
			if err != nil || next.Epoch() <= winner.Epoch() {
				t.Fatal("epoch reused", next, err)
			}
			if err := winner.ReleaseBackend(ctx, f.backend); !errors.Is(err, connectionowner.ErrNotOwner) {
				t.Fatal("old release accepted", err)
			}
			if _, err := winner.RenewBackend(ctx, f.backend); !errors.Is(err, connectionowner.ErrNotOwner) {
				t.Fatal("old renew accepted", err)
			}
			winner = next
		}
		// Runtime cannot delete fencing authority to reset its epoch.
		for _, table := range []string{"connection_owner_fencing", "scheduler_leadership"} {
			if _, err := f.backend.Exec(ctx, "DELETE FROM "+table+" WHERE 1=0"); !errors.Is(err, database.ErrPermission) {
				t.Fatal("fencing deletion permitted", table, err)
			}
		}
	})
	t.Run("scheduler-renew-and-assert-after-wait", func(t *testing.T) {
		for _, assert := range []bool{false, true} {
			owner := schedulerlease.Owner{InstanceID: uuid.Must(uuid.NewV7()), Incarnation: 1}
			epoch, err := schedulerlease.Acquire(ctx, f.backend, owner, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			locked, err := f.owner.Begin(ctx, database.ReadCommitted)
			if err != nil {
				t.Fatal(err)
			}
			defer locked.Rollback(ctx)
			store, err := schedulerlease.FromTransaction(locked)
			if err != nil {
				t.Fatal(err)
			}
			state, err := store.Lock(ctx)
			if err != nil {
				t.Fatal(err)
			}
			until, err := state.Until.Time()
			if err != nil {
				t.Fatal(err)
			}
			waiting, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				if assert {
					done <- database.Within(waiting, f.backend, database.ReadCommitted, func(tx database.Tx) error { return schedulerlease.Assert(waiting, tx, owner, epoch) })
				} else {
					done <- schedulerlease.Renew(waiting, f.backend, owner, epoch, time.Second)
				}
			}()
			assertBlocked(t, done)
			time.Sleep(time.Until(until) + 50*time.Millisecond)
			if err := locked.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; !errors.Is(err, schedulerlease.ErrLost) {
				t.Fatal("scheduler lease revived", err)
			}
		}
	})
	t.Run("scheduler-contenders-and-ambiguous-commit", func(t *testing.T) {
		fault := &commitFaultBackend{Backend: f.backend, commit: true}
		fault.armed.Store(true)
		owner := schedulerlease.Owner{InstanceID: uuid.Must(uuid.NewV7()), Incarnation: 1}
		epoch, err := schedulerlease.Acquire(ctx, fault, owner, time.Minute)
		if err != nil {
			t.Fatal("acquire readback", err)
		}
		fault.armed.Store(true)
		if err := schedulerlease.Renew(ctx, fault, owner, epoch, time.Minute); err != nil {
			t.Fatal("renew readback", err)
		}
		past := value.Timestamp{Valid: true, Micros: value.NegativeInfinity}
		f.exec(t, `UPDATE scheduler_leadership SET lease_until=$1 WHERE id=1`, `UPDATE scheduler_leadership SET lease_until=? WHERE id=1`, past)
		start, done := make(chan struct{}), make(chan error, 2)
		for range 2 {
			go func() {
				<-start
				_, err := coordination.AcquireBackend(ctx, f.backend, coordination.Identity{InstanceID: uuid.Must(uuid.NewV7()), Incarnation: 2}, time.Minute)
				done <- err
			}()
		}
		close(start)
		won := 0
		for range 2 {
			err := <-done
			if err == nil {
				won++
			} else if !errors.Is(err, coordination.ErrLeaseHeld) {
				t.Fatal(err)
			}
		}
		if won != 1 {
			t.Fatal("scheduler contenders", won)
		}
		if err := schedulerlease.Renew(ctx, f.backend, owner, epoch, time.Minute); !errors.Is(err, schedulerlease.ErrLost) {
			t.Fatal("old scheduler renewed", err)
		}
		if n := f.count(t, "SELECT epoch FROM scheduler_leadership WHERE id=1", "SELECT epoch FROM scheduler_leadership WHERE id=1"); int64(n) <= epoch {
			t.Fatal("scheduler epoch reused", n)
		}
	})
	t.Run("owner-readback-and-stale-dispatch", func(t *testing.T) {
		node := f.node(t)
		fault := &commitFaultBackend{Backend: f.backend, commit: true}
		fault.armed.Store(true)
		term, err := connectionowner.AcquireBackend(ctx, fault, node, identity, uuid.Must(uuid.NewV7()), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		fault.armed.Store(true)
		if _, err := term.RenewBackend(ctx, fault); err != nil {
			t.Fatal(err)
		}
		service := operations.NewBackend(f.backend, 1, f.signer)
		if _, _, err := service.CreateSynthetic(ctx, outboxRequest(node)); err != nil {
			t.Fatal(err)
		}
		d := claimOutbox(t, service)
		var envelope agentv1.CommandEnvelope
		if err := proto.Unmarshal(d.Envelope, &envelope); err != nil {
			t.Fatal(err)
		}
		fenceID, connection := uuid.Must(uuid.NewV7()), term.ConnectionID()
		envelope.ConnectionFence = &agentv1.ConnectionFenceV2{FenceId: fenceID[:], NodeId: node[:], OwnerInstanceId: identity.InstanceID[:], OwnerIncarnation: 1, ConnectionId: connection[:], OwnerEpoch: uint64(term.Epoch())}
		envelope.FenceBinding = &agentv1.FenceBindingV2{FenceId: fenceID[:], NodeId: node[:], OwnerInstanceId: identity.InstanceID[:], OwnerIncarnation: 1, ConnectionId: connection[:], OwnerEpoch: uint64(term.Epoch()), OperationId: d.CommandID[:], OperationKind: agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND}
		encoded, err := proto.Marshal(&envelope)
		if err != nil {
			t.Fatal(err)
		}
		next, err := connectionowner.AcquireBackend(ctx, f.backend, node, identity, uuid.Must(uuid.NewV7()), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if err := service.MarkSentWithEnvelope(ctx, d, encoded); !errors.Is(err, connectionowner.ErrNotOwner) {
			t.Fatal("old owner accounted send", err)
		}
		if f.count(t, `SELECT count(*) FROM command_attempts WHERE state='sent'`, `SELECT count(*) FROM command_attempts WHERE state='sent'`) != 0 {
			t.Fatal("stale owner mutated attempt")
		}
		fault.armed.Store(true)
		if err := next.ReleaseBackend(ctx, fault); err != nil {
			t.Fatal("release readback", err)
		}
	})
	t.Run("disconnect-rejects-observer", func(t *testing.T) {
		node := f.node(t)
		term, err := connectionowner.AcquireBackend(ctx, f.backend, node, identity, uuid.Must(uuid.NewV7()), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if err := term.ReleaseBackend(ctx, f.backend); err != nil {
			t.Fatal(err)
		}
		connection := term.ConnectionID()
		if release, err := connectionowner.GuardObservedTermBackend(ctx, f.backend, node, identity.InstanceID, identity.Incarnation, connection, term.Epoch()); !errors.Is(err, connectionowner.ErrNotOwner) {
			if release != nil {
				release()
			}
			t.Fatal("disconnected term still authorizes external mutations", err)
		}
	})
}

func lockOwner(t *testing.T, f *outboxFixture, node uuid.UUID) database.Tx {
	t.Helper()
	tx, err := f.owner.Begin(context.Background(), database.ReadCommitted)
	if err != nil {
		t.Fatal(err)
	}
	q, args := f.query(`SELECT owner_epoch FROM connection_owner_fencing WHERE node_id=$1 FOR UPDATE`, `SELECT owner_epoch FROM connection_owner_fencing WHERE node_id=? FOR UPDATE`, []any{node[:]})
	var epoch int64
	if err := tx.QueryRow(context.Background(), q, args...).Scan(&epoch); err != nil {
		tx.Rollback(context.Background())
		t.Fatal(err)
	}
	return tx
}

func assertBlocked(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatal("authority lock bypassed", err)
	case <-time.After(150 * time.Millisecond):
	}
}
