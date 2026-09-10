package operations_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/connectionowner"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/localslice"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestOutboxBackendIntegration(t *testing.T) {
	t.Run("atomic-intent-and-ambiguous-commit", func(t *testing.T) {
		f := newOutboxFixture(t)
		ctx := context.Background()
		fault := &commitFaultBackend{Backend: f.backend}
		service := operations.NewBackend(fault, 1, f.signer)
		request := outboxRequest(f.node(t))
		fault.armed.Store(true)
		if _, _, err := service.CreateSynthetic(ctx, request); !errors.Is(err, lostCommit) {
			t.Fatal(err)
		}
		for _, table := range []string{"operations", "commands", "outbox_events", "audit_events", "operation_events"} {
			if n := f.count(t, "SELECT count(*) FROM "+table, "SELECT count(*) FROM "+table); n != 0 {
				t.Fatal("partial intent", table, n)
			}
		}
		fault.commit = true
		fault.armed.Store(true)
		created, replay, err := service.CreateSynthetic(ctx, request)
		if err != nil || replay {
			t.Fatal(created, replay, err)
		}
		again, replay, err := service.CreateSynthetic(ctx, request)
		if err != nil || !replay || again.ID != created.ID {
			t.Fatal("idempotent readback", again, replay, err)
		}
		for _, table := range []string{"operations", "commands", "outbox_events", "audit_events", "operation_events"} {
			if n := f.count(t, "SELECT count(*) FROM "+table, "SELECT count(*) FROM "+table); n != 1 {
				t.Fatal("duplicate intent", table, n)
			}
		}
		request.Message = "different effect"
		if _, _, err := service.CreateSynthetic(ctx, request); !errors.Is(err, operations.ErrIdempotencyConflict) {
			t.Fatal(err)
		}
		// A claim whose commit did not happen cannot escape to transport.
		fault.commit = false
		fault.armed.Store(true)
		if jobs, err := service.Claim(ctx, uuid.Must(uuid.NewV7()), 1, time.Minute); err == nil || len(jobs) != 0 {
			t.Fatal(jobs, err)
		}
		if n := f.count(t, "SELECT count(*) FROM command_attempts", "SELECT count(*) FROM command_attempts"); n != 0 {
			t.Fatal("partial claim", n)
		}
		fault.commit = true
		fault.armed.Store(true)
		d := claimOutbox(t, service)
		fault.armed.Store(true)
		if err := service.MarkSent(ctx, d); err != nil {
			t.Fatal("bookkeeping readback", err)
		}
		if err := service.MarkSent(ctx, d); err != nil {
			t.Fatal("bookkeeping replay", err)
		}
		if f.count(t, `SELECT count(*) FROM operation_events WHERE state='dispatched'`, `SELECT count(*) FROM operation_events WHERE state='dispatched'`) != 1 {
			t.Fatal("duplicate dispatch projection")
		}
	})
	t.Run("two-workers-and-commit-readback", func(t *testing.T) {
		f := newOutboxFixture(t)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		fault := &commitFaultBackend{Backend: f.backend, commit: true}
		service := operations.NewBackend(fault, 1, f.signer)
		for range 2 {
			if _, _, err := service.CreateSynthetic(ctx, outboxRequest(f.node(t))); err != nil {
				t.Fatal(err)
			}
		}
		var sends atomic.Int32
		sent := make(chan error, 2)
		sender := outboxSender(func(ctx context.Context, node, encoded []byte) error {
			sends.Add(1)
			var envelope agentv1.CommandEnvelope
			if err := proto.Unmarshal(encoded, &envelope); err != nil {
				sent <- err
				return err
			}
			command := uuid.Must(uuid.FromBytes(envelope.CommandId))
			q, args := f.query(`SELECT count(*) FROM command_attempts WHERE command_id=$1 AND state='sending'`,
				`SELECT count(*) FROM command_attempts WHERE command_id=? AND state='sending'`, []any{command})
			var n int
			err := f.backend.QueryRow(ctx, q, args...).Scan(&n)
			if err == nil && n != 1 {
				err = errors.New("transport ran before durable claim")
			}
			sent <- err
			return err
		})
		fault.armed.Store(true)
		done := make(chan error, 2)
		for range 2 {
			worker, err := operations.NewWorker(service, sender, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			go func() { done <- worker.Run(ctx) }()
		}
		select {
		case err := <-sent:
			if err != nil {
				t.Error(err)
			}
		case <-ctx.Done():
			t.Error("worker did not send")
		}
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) && f.count(t, `SELECT count(*) FROM command_attempts WHERE state='sent'`, `SELECT count(*) FROM command_attempts WHERE state='sent'`) == 0 {
			time.Sleep(20 * time.Millisecond)
		}
		cancel()
		for range 2 {
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		}
		if sends.Load() != 1 || f.count(t, "SELECT count(*) FROM command_attempts", "SELECT count(*) FROM command_attempts") != 1 {
			t.Fatal("duplicate send/claim", sends.Load())
		}
		if f.count(t, `SELECT count(*) FROM command_attempts WHERE state='sent'`, `SELECT count(*) FROM command_attempts WHERE state='sent'`) != 1 {
			t.Fatal("success not recorded")
		}
	})
	for _, accepted := range []bool{false, true} {
		name := "crash-after-claim"
		if accepted {
			name = "crash-after-transport-acceptance"
		}
		t.Run(name, func(t *testing.T) {
			f := newOutboxFixture(t)
			ctx := context.Background()
			service := operations.NewBackend(f.backend, 1, f.signer)
			created, _, err := service.CreateSynthetic(ctx, outboxRequest(f.node(t)))
			if err != nil {
				t.Fatal(err)
			}
			original := claimOutbox(t, service)
			var transportAccepted bool
			if accepted {
				sender := outboxSender(func(context.Context, []byte, []byte) error { transportAccepted = true; return nil })
				if err := sender.SendCommand(ctx, original.NodeID[:], original.Envelope); err != nil {
					t.Fatal(err)
				}
			}
			// Abandon all worker memory without MarkSent/MarkFailed, then restart.
			service = operations.NewBackend(f.backend, 1, f.signer)
			dispatch := original
			for attempt := 1; attempt <= 3; attempt++ {
				past := value.Timestamp{Valid: true, Micros: value.NegativeInfinity}
				f.exec(t, `UPDATE node_command_leases SET leased_until=$1 WHERE lease_token=$2`, `UPDATE node_command_leases SET leased_until=? WHERE lease_token=?`, past, dispatch.LeaseToken)
				f.exec(t, `UPDATE outbox_events SET locked_until=$1 WHERE id=$2`, `UPDATE outbox_events SET locked_until=? WHERE id=?`, past, dispatch.OutboxID)
				if err := service.Reap(ctx, 3); err != nil {
					t.Fatal(err)
				}
				op, err := service.Get(ctx, uuid.MustParse(created.ID))
				if err != nil || op.State != "unknown" {
					t.Fatal(op, err)
				}
				if err := service.MarkSent(ctx, dispatch); err == nil {
					t.Fatal("expired claim accepted")
				}
				if attempt == 3 {
					break
				}
				dispatch = claimOutbox(t, service)
				var before, after agentv1.CommandEnvelope
				if err := proto.Unmarshal(original.Envelope, &before); err != nil {
					t.Fatal(err)
				}
				if err := proto.Unmarshal(dispatch.Envelope, &after); err != nil {
					t.Fatal(err)
				}
				if after.DeliveryMode != agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY || !bytes.Equal(before.IdempotencyKey, after.IdempotencyKey) || !bytes.Equal(before.SemanticPayloadSha256, after.SemanticPayloadSha256) {
					t.Fatal("recovery can replay an unconfirmed effect")
				}
			}
			if jobs, err := service.Claim(ctx, uuid.Must(uuid.NewV7()), 1, time.Minute); err != nil || len(jobs) != 0 {
				t.Fatal("attempt cap bypassed", jobs, err)
			}
			if transportAccepted != accepted {
				t.Fatal("unexpected transport invocation")
			}
		})
	}
	t.Run("early-and-duplicate-result", func(t *testing.T) {
		f := newOutboxFixture(t)
		ctx := context.Background()
		service := operations.NewBackend(f.backend, 1, f.signer)
		created, _, err := service.CreateSynthetic(ctx, outboxRequest(f.node(t)))
		if err != nil {
			t.Fatal(err)
		}
		d := claimOutbox(t, service)
		var envelope agentv1.CommandEnvelope
		if err := proto.Unmarshal(d.Envelope, &envelope); err != nil {
			t.Fatal(err)
		}
		at := timestamppb.Now()
		result := &agentv1.CommandResult{CommandId: envelope.CommandId, IdempotencyKey: envelope.IdempotencyKey,
			PayloadSha256: envelope.SemanticPayloadSha256, SemanticPayloadHashVersion: envelope.SemanticPayloadHashVersion,
			State: agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED, AcceptedAt: at, CompletedAt: at}
		payload, err := proto.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		id := uuid.Must(uuid.NewV7())
		endpoint := sha256.Sum256(d.NodeID[:])
		event := &transportv1.TransportEvent{EventId: id[:], NodeId: d.NodeID[:], EndpointId: endpoint[:],
			Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT, OccurredAt: at, Traceparent: d.Traceparent, Payload: payload}
		ingress := localslice.NewBackend(f.backend, f.signer)
		for range 2 {
			if err := ingress.Ingest(ctx, event); err != nil {
				t.Fatal(err)
			}
		}
		for range 2 {
			if err := service.MarkSent(ctx, d); err != nil {
				t.Fatal("late bookkeeping", err)
			}
		}
		op, err := service.Get(ctx, uuid.MustParse(created.ID))
		if err != nil || op.State != "succeeded" {
			t.Fatal(op, err)
		}
		if f.count(t, "SELECT count(*) FROM agent_command_results", "SELECT count(*) FROM agent_command_results") != 1 || f.count(t, "SELECT count(*) FROM node_command_leases", "SELECT count(*) FROM node_command_leases") != 0 {
			t.Fatal("duplicate result or leaked claim")
		}
		if f.count(t, `SELECT count(*) FROM operation_events WHERE state='succeeded'`, `SELECT count(*) FROM operation_events WHERE state='succeeded'`) != 1 {
			t.Fatal("duplicate projection")
		}
	})
}

type outboxSender func(context.Context, []byte, []byte) error

func (s outboxSender) SendCommand(ctx context.Context, node, envelope []byte) error {
	return s(ctx, node, envelope)
}

func claimOutbox(t *testing.T, service *operations.Service) operations.Dispatch {
	t.Helper()
	jobs, err := service.Claim(context.Background(), uuid.Must(uuid.NewV7()), 1, time.Minute)
	if err != nil || len(jobs) != 1 {
		t.Fatal("claim", jobs, err)
	}
	return jobs[0]
}

func TestCoordinationDeadlockBackendIntegration(t *testing.T) {
	f := newOutboxFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	nodes := []uuid.UUID{f.node(t), f.node(t)}
	for _, node := range nodes {
		_, err := connectionowner.AcquireBackend(ctx, f.backend, node, connectionowner.Identity{InstanceID: uuid.Must(uuid.NewV7()), Incarnation: 1}, uuid.Must(uuid.NewV7()), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
	}
	// Fence rows have independent locks; node UPDATE triggers also lock the
	// shared exact-key guard on MySQL, preventing a two-row rendezvous.
	ready, proceed := make(chan struct{}, 2), make(chan struct{})
	done := make(chan error, 2)
	var attempts atomic.Int32
	for worker := range 2 {
		go func() {
			local := 0
			done <- database.WithinRetry(ctx, f.backend, database.ReadCommitted, func(tx database.Tx) error {
				local++
				attempts.Add(1)
				q, args := f.query(`UPDATE connection_owner_fencing SET owner_epoch=owner_epoch+1 WHERE node_id=$1`, `UPDATE connection_owner_fencing SET owner_epoch=owner_epoch+1 WHERE node_id=?`, []any{nodes[worker][:]})
				if n, err := tx.Exec(ctx, q, args...); err != nil {
					return err
				} else if n != 1 {
					return errors.New("missing first deadlock row")
				}
				if local == 1 {
					ready <- struct{}{}
					select {
					case <-proceed:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				q, args = f.query(`UPDATE connection_owner_fencing SET owner_epoch=owner_epoch+1 WHERE node_id=$1`, `UPDATE connection_owner_fencing SET owner_epoch=owner_epoch+1 WHERE node_id=?`, []any{nodes[1-worker][:]})
				n, err := tx.Exec(ctx, q, args...)
				if err == nil && n != 1 {
					return errors.New("missing second deadlock row")
				}
				return err
			})
		}()
	}
	for range 2 {
		select {
		case <-ready:
		case err := <-done:
			t.Fatal("deadlock setup failed", err)
		case <-ctx.Done():
			t.Fatal("deadlock rendezvous failed")
		}
	}
	close(proceed)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if attempts.Load() != 3 {
		t.Fatal("did not exercise one real deadlock rollback", attempts.Load())
	}
	for _, node := range nodes {
		if got := f.count(t, `SELECT owner_epoch FROM connection_owner_fencing WHERE node_id=$1`, `SELECT owner_epoch FROM connection_owner_fencing WHERE node_id=?`, node[:]); got != 3 {
			t.Fatal("partial transaction leaked", got)
		}
	}
}
