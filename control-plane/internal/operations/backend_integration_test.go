package operations_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	certificatestore "github.com/GentleKingson/ocservia/control-plane/internal/certificates/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/connectionowner"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/localslice"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetrywrite"
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
	// Global capacity is 2. A persistent Unknown without a dispatch or probe
	// lease keeps its state and fence but is not an executing command; a
	// recovery attempt that takes a lease is, and must use the same budget.
	t.Run("unknown-capacity", func(t *testing.T) {
		newFixture := func(t *testing.T) (*outboxFixture, *operations.Service, func(node uuid.UUID, state string, published bool) uuid.UUID) {
			f := newOutboxFixture(t)
			service := operations.NewBackend(f.backend, 2, f.signer)
			// seed creates one operation and moves it to a durable state, either
			// quiescent (outbox published) or with its recovery outbox pending.
			seed := func(node uuid.UUID, state string, published bool) uuid.UUID {
				created, _, err := service.CreateSynthetic(context.Background(), outboxRequest(node))
				if err != nil {
					t.Fatal(err)
				}
				operation := uuid.MustParse(created.ID)
				if state != "queued" {
					f.exec(t, `UPDATE operations SET state=$1 WHERE id=$2`, `UPDATE operations SET state=? WHERE id=?`, state, operation)
					f.exec(t, `UPDATE commands SET state=$1 WHERE operation_id=$2`, `UPDATE commands SET state=? WHERE operation_id=?`, state, operation)
				}
				if published {
					at := value.Timestamp{Valid: true, Micros: value.NegativeInfinity}
					f.exec(t, `UPDATE outbox_events SET published_at=$1 WHERE command_id IN (SELECT id FROM commands WHERE operation_id=$2)`,
						`UPDATE outbox_events SET published_at=? WHERE command_id IN (SELECT id FROM commands WHERE operation_id=?)`, at, operation)
				}
				return operation
			}
			return f, service, seed
		}
		t.Run("quiescent-unknown-does-not-block-other-workspace", func(t *testing.T) {
			f, service, seed := newFixture(t)
			otherWorkspace := uuid.Must(uuid.NewV7())
			at, err := value.FromTime(time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			f.exec(t, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'PR04 other',$2,$3,$4)`,
				`INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES(?,'PR04 other',?,?,?)`, otherWorkspace, otherWorkspace.String(), at, at)
			node := uuid.Must(uuid.NewV7())
			f.exec(t, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES($1,$2,$3,'active',$4,$5)`,
				`INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES(?,?,?,'active',?,?)`, node, otherWorkspace, node.String(), at, at)
			endpoint := sha256.Sum256(node[:])
			f.exec(t, `INSERT INTO node_endpoint_keys(node_id,endpoint_id,state,bound_at) VALUES($1,$2,'active',$3)`,
				`INSERT INTO node_endpoint_keys(node_id,endpoint_id,state,bound_at) VALUES(?,?,'active',?)`, node, endpoint[:], at)
			seed(f.node(t), "unknown", true)
			seed(f.node(t), "unknown", true)
			want := seed(node, "queued", false)
			jobs, err := service.Claim(context.Background(), uuid.Must(uuid.NewV7()), 10, time.Minute)
			if err != nil || len(jobs) != 1 || jobs[0].OperationID != want {
				t.Fatal("quiescent Unknown blocked unrelated dispatch", jobs, err)
			}
		})
		t.Run("recovery-leases-share-global-capacity", func(t *testing.T) {
			f, service, seed := newFixture(t)
			for range 4 {
				seed(f.node(t), "unknown", false)
			}
			var claimed atomic.Int32
			var wg sync.WaitGroup
			failures := make(chan error, 4)
			for range 4 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					jobs, err := service.Claim(context.Background(), uuid.Must(uuid.NewV7()), 10, time.Minute)
					claimed.Add(int32(len(jobs)))
					failures <- err
				}()
			}
			wg.Wait()
			close(failures)
			for err := range failures {
				if err != nil {
					t.Fatal(err)
				}
			}
			leases := f.count(t, `SELECT count(*) FROM node_command_leases`, `SELECT count(*) FROM node_command_leases`)
			if claimed.Load() != 2 || leases != 2 {
				t.Fatal("concurrent recovery exceeded the global limit", claimed.Load(), leases)
			}
		})
		t.Run("recovery-waits-for-executing-commands", func(t *testing.T) {
			f, service, seed := newFixture(t)
			seed(f.node(t), "running", true)
			seed(f.node(t), "running", true)
			seed(f.node(t), "unknown", false)
			if jobs, err := service.Claim(context.Background(), uuid.Must(uuid.NewV7()), 10, time.Minute); err != nil || len(jobs) != 0 {
				t.Fatal("recovery lease bypassed a full execution budget", jobs, err)
			}
		})
		t.Run("same-resource-fence-survives-uncounted-unknown", func(t *testing.T) {
			f, service, seed := newFixture(t)
			node := f.node(t)
			older := seed(node, "unknown", true)
			newer := seed(node, "queued", false)
			for index, operation := range []uuid.UUID{older, newer} {
				f.exec(t, `UPDATE commands SET resource_type='user',resource_key='alice',expected_version=$1 WHERE operation_id=$2`,
					`UPDATE commands SET resource_type='user',resource_key='alice',expected_version=? WHERE operation_id=?`, index+1, operation)
			}
			if jobs, err := service.Claim(context.Background(), uuid.Must(uuid.NewV7()), 10, time.Minute); err != nil || len(jobs) != 0 {
				t.Fatal("same-resource write passed an Unknown predecessor", jobs, err)
			}
		})
	})
}

func TestOutboxReleaseBackendIntegration(t *testing.T) {
	for _, source := range []string{"certificate-release", "node-activation"} {
		t.Run(source, func(t *testing.T) {
			f := newOutboxFixture(t)
			ctx := t.Context()
			service := operations.NewBackend(f.backend, 1, f.signer)
			node := f.node(t)
			created, _, err := service.CreateSynthetic(ctx, outboxRequest(node))
			if err != nil {
				t.Fatal(err)
			}
			id := uuid.MustParse(created.ID)
			if source == "certificate-release" {
				f.exec(t, `UPDATE outbox_events SET available_at=$1`, `UPDATE outbox_events SET available_at=?`, value.Timestamp{Valid: true, Micros: value.PositiveInfinity})
			} else {
				f.exec(t, `UPDATE nodes SET status='offline' WHERE id=$1`, `UPDATE nodes SET status='offline' WHERE id=?`, node)
			}
			assertUnclaimed := func() {
				t.Helper()
				if jobs, err := service.Claim(ctx, uuid.Must(uuid.NewV7()), 1, time.Minute); err != nil || len(jobs) != 0 {
					t.Fatal("unexpected dispatch", jobs, err)
				}
			}
			change := func(tx database.Tx) error {
				if source == "certificate-release" {
					store, err := certificatestore.FromTransaction(tx)
					if err != nil {
						return err
					}
					// Missing operations retain the zero-row no-op behavior.
					if err := store.ReleaseOperation(ctx, uuid.Must(uuid.NewV7())); err != nil {
						return err
					}
					return store.ReleaseOperation(ctx, id)
				}
				store, err := telemetrywrite.From(tx)
				if err != nil {
					return err
				}
				return store.Activate(ctx, node, time.Now().UTC())
			}
			assertUnclaimed()
			rejected := errors.New("rollback release")
			if err := database.Within(ctx, f.backend, database.ReadCommitted, func(tx database.Tx) error {
				if err := change(tx); err != nil {
					return err
				}
				return rejected
			}); !errors.Is(err, rejected) {
				t.Fatal(err)
			}
			assertUnclaimed()
			if err := database.Within(ctx, f.backend, database.ReadCommitted, change); err != nil {
				t.Fatal(err)
			}
			d := claimOutbox(t, service)
			if d.OperationID != id {
				t.Fatal("released wrong operation", d)
			}
			if err := service.MarkSent(ctx, d); err != nil {
				t.Fatal(err)
			}
			// Repeated release/activation must not revive a published command.
			if err := database.Within(ctx, f.backend, database.ReadCommitted, change); err != nil {
				t.Fatal(err)
			}
			assertUnclaimed()
			// Store errors propagate from a closed transaction.
			tx, err := f.backend.Begin(ctx, database.ReadCommitted)
			if err != nil {
				t.Fatal(err)
			}
			if err := tx.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			if err := change(tx); err == nil {
				t.Fatal("closed transaction accepted")
			}
		})
	}
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
