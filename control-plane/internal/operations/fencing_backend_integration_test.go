package operations_test

import (
	"context"
	"errors"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/connectionowner"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

func TestFencingBackendIntegration(t *testing.T) {
	f := newOutboxFixture(t)
	ctx := context.Background()
	identity := connectionowner.Identity{InstanceID: uuid.Must(uuid.NewV7()), Incarnation: 1}
	for _, action := range []string{"renew", "assert", "release"} {
		t.Run("owner-expired-during-"+action+"-wait", func(t *testing.T) {
			node := f.node(t)
			term, err := connectionowner.AcquireBackend(ctx, f.backend, node, identity, uuid.Must(uuid.NewV7()), time.Second)
			testutil.NoError(t, err)
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
		testutil.NoError(t, err)
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
		if _, err := f.backend.Exec(ctx, "DELETE FROM connection_owner_fencing WHERE 1=0"); !errors.Is(err, database.ErrPermission) {
			t.Fatal("fencing deletion permitted", err)
		}
	})
	t.Run("owner-readback-and-stale-dispatch", func(t *testing.T) {
		node := f.node(t)
		fault := &commitFaultBackend{Backend: f.backend, commit: true}
		fault.armed.Store(true)
		term, err := connectionowner.AcquireBackend(ctx, fault, node, identity, uuid.Must(uuid.NewV7()), time.Minute)
		testutil.NoError(t, err)
		fault.armed.Store(true)
		if _, err := term.RenewBackend(ctx, fault); err != nil {
			t.Fatal(err)
		}
		service := operations.NewBackend(f.backend, 1, f.signer)
		if _, _, err := service.Create(ctx, outboxRequest(node)); err != nil {
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
		testutil.NoError(t, err)
		next, err := connectionowner.AcquireBackend(ctx, f.backend, node, identity, uuid.Must(uuid.NewV7()), time.Minute)
		testutil.NoError(t, err)
		if err := service.MarkSentWithEnvelope(ctx, d, encoded); !errors.Is(err, connectionowner.ErrNotOwner) {
			t.Fatal("old owner accounted send", err)
		}
		if f.count(t, `SELECT count(*) FROM command_attempts WHERE state='sent'`) != 0 {
			t.Fatal("stale owner mutated attempt")
		}
		fault.armed.Store(true)
		if err := next.ReleaseBackend(ctx, fault); err != nil {
			t.Fatal("release readback", err)
		}
	})
}

func lockOwner(t *testing.T, f *outboxFixture, node uuid.UUID) database.Tx {
	t.Helper()
	tx, err := f.owner.Begin(context.Background(), database.ReadCommitted)
	testutil.NoError(t, err)
	var epoch int64
	if err := tx.QueryRow(context.Background(), `SELECT owner_epoch FROM connection_owner_fencing WHERE node_id=$1 FOR UPDATE`, node[:]).Scan(&epoch); err != nil {
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
