package operations_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	certificatestore "github.com/GentleKingson/ocservia/control-plane/internal/certificates/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/connectionowner"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	telemetry "github.com/GentleKingson/ocservia/control-plane/internal/telemetry/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/transportevents"
	resultstore "github.com/GentleKingson/ocservia/control-plane/internal/transportevents/store"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestCreateRejectsRevokedBrowserSession(t *testing.T) {
	f := newOutboxFixture(t)
	service := operations.NewBackend(f.backend, 1, f.signer)
	request := outboxRequest(f.node(t))
	f.exec(t, `UPDATE auth_sessions SET revoked_at=now() WHERE id=$1`, outboxSessionID)
	if _, _, err := service.Create(t.Context(), request); !errors.Is(err, auth.ErrForbidden) {
		t.Fatal("revoked session operation", err)
	}
	for _, table := range []string{"operations", "commands", "outbox_events", "audit_events"} {
		if n := f.count(t, "SELECT count(*) FROM "+table); n != 0 {
			t.Fatal("revoked session persisted operation state", table, n)
		}
	}
}

func TestDashboardSummaryCache(t *testing.T) {
	f := newOutboxFixture(t)
	fault := &commitFaultBackend{Backend: f.backend}
	service := operations.NewBackend(fault, 1, f.signer)
	ctx := t.Context()
	if _, _, err := service.Create(ctx, outboxRequest(f.node(t))); err != nil {
		t.Fatal(err)
	}
	check := func(workspace uuid.UUID, want int64) {
		t.Helper()
		got, err := service.SummaryInWorkspace(ctx, workspace)
		if err != nil || got.Active != want {
			t.Fatalf("summary for %s: %+v, %v", workspace, got, err)
		}
	}
	check(f.workspace, 1)
	fault.armed.Store(true)
	check(f.workspace, 1) // A hit avoids the failing transaction.
	other := uuid.Must(uuid.NewV7())
	if _, err := service.SummaryInWorkspace(ctx, other); !errors.Is(err, lostCommit) {
		t.Fatalf("expected a fresh read for another workspace, got %v", err)
	}
	check(other, 0) // Retry after the failed query.
	fault.armed.Store(true)
	time.Sleep(time.Second)
	if _, err := service.SummaryInWorkspace(ctx, other); !errors.Is(err, lostCommit) {
		t.Fatalf("expected a fresh read after expiry, got %v", err)
	}
}

func TestOutboxBackendIntegration(t *testing.T) {
	t.Run("atomic-intent-and-ambiguous-commit", func(t *testing.T) {
		f := newOutboxFixture(t)
		ctx := context.Background()
		fault := &commitFaultBackend{Backend: f.backend}
		service := operations.NewBackend(fault, 1, f.signer)
		request := outboxRequest(f.node(t))
		fault.armed.Store(true)
		if _, _, err := service.Create(ctx, request); !errors.Is(err, lostCommit) {
			t.Fatal(err)
		}
		for _, table := range []string{"operations", "commands", "outbox_events", "audit_events", "operation_events"} {
			if n := f.count(t, "SELECT count(*) FROM "+table); n != 0 {
				t.Fatal("partial intent", table, n)
			}
		}
		fault.commit = true
		fault.armed.Store(true)
		created, replay, err := service.Create(ctx, request)
		if err != nil || replay {
			t.Fatal(created, replay, err)
		}
		again, replay, err := service.Create(ctx, request)
		if err != nil || !replay || again.ID != created.ID {
			t.Fatal("idempotent readback", again, replay, err)
		}
		for _, table := range []string{"operations", "commands", "outbox_events", "audit_events", "operation_events"} {
			if n := f.count(t, "SELECT count(*) FROM "+table); n != 1 {
				t.Fatal("duplicate intent", table, n)
			}
		}
		request.Payload = operations.SyntheticEchoPayload{Message: "different effect"}
		if _, _, err := service.Create(ctx, request); !errors.Is(err, operations.ErrIdempotencyConflict) {
			t.Fatal(err)
		}
		// A claim whose commit did not happen cannot escape to transport.
		fault.commit = false
		fault.armed.Store(true)
		if jobs, err := service.Claim(ctx, uuid.Must(uuid.NewV7()), 1, time.Minute); err == nil || len(jobs) != 0 {
			t.Fatal(jobs, err)
		}
		if n := f.count(t, "SELECT count(*) FROM command_attempts"); n != 0 {
			t.Fatal("partial claim", n)
		}
		fault.commit = true
		fault.armed.Store(true)
		d := claimOutbox(t, service)
		fault.armed.Store(true)
		if err := service.MarkSentWithEnvelope(ctx, d, d.Envelope); err != nil {
			t.Fatal("bookkeeping readback", err)
		}
		if err := service.MarkSentWithEnvelope(ctx, d, d.Envelope); err != nil {
			t.Fatal("bookkeeping replay", err)
		}
		if f.count(t, `SELECT count(*) FROM operation_events WHERE state='dispatched'`) != 1 {
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
			if _, _, err := service.Create(ctx, outboxRequest(f.node(t))); err != nil {
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
			var n int
			err := f.backend.QueryRow(ctx, `SELECT count(*) FROM command_attempts WHERE command_id=$1 AND state='sending'`, command).Scan(&n)
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
			testutil.NoError(t, err)
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
		for time.Now().Before(deadline) && f.count(t, `SELECT count(*) FROM command_attempts WHERE state='sent'`) == 0 {
			time.Sleep(20 * time.Millisecond)
		}
		cancel()
		for range 2 {
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		}
		if sends.Load() != 1 || f.count(t, "SELECT count(*) FROM command_attempts") != 1 {
			t.Fatal("duplicate send/claim", sends.Load())
		}
		if f.count(t, `SELECT count(*) FROM command_attempts WHERE state='sent'`) != 1 {
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
			created, _, err := service.Create(ctx, outboxRequest(f.node(t)))
			testutil.NoError(t, err)
			original := claimOutbox(t, service)
			var transportAccepted bool
			if accepted {
				sender := outboxSender(func(context.Context, []byte, []byte) error { transportAccepted = true; return nil })
				testutil.NoError(t, sender.SendCommand(ctx, original.NodeID[:], original.Envelope))
			}
			// Abandon all worker memory without MarkSent/MarkFailed, then restart.
			service = operations.NewBackend(f.backend, 1, f.signer)
			dispatch := original
			for attempt := 1; attempt <= 4; attempt++ {
				if attempt == 4 {
					// Reap(ctx, 3) still permits a fourth reconcile-only
					// delivery. Jump it to the shared reconciliation ceiling.
					f.exec(t, `UPDATE outbox_events SET attempts=$1 WHERE id=$2`, operationstore.ReconciliationAttemptLimit, dispatch.OutboxID)
					f.exec(t, `UPDATE command_attempts SET attempt_number=$1 WHERE id=$2`, operationstore.ReconciliationAttemptLimit, dispatch.AttemptID)
				}
				past := value.Timestamp{Valid: true, Micros: value.NegativeInfinity}
				f.exec(t, `UPDATE node_command_leases SET leased_until=$1 WHERE lease_token=$2`, past, dispatch.LeaseToken)
				f.exec(t, `UPDATE outbox_events SET locked_until=$1 WHERE id=$2`, past, dispatch.OutboxID)
				testutil.NoError(t, service.Reap(ctx, 3))
				op, err := service.Get(ctx, uuid.MustParse(created.ID))
				if err != nil || op.State != "unknown" {
					t.Fatal(op, err)
				}
				if err := service.MarkSentWithEnvelope(ctx, dispatch, dispatch.Envelope); err == nil {
					t.Fatal("expired claim accepted")
				}
				if attempt == 4 {
					break
				}
				dispatch = claimOutbox(t, service)
				var before, after agentv1.CommandEnvelope
				testutil.NoError(t, proto.Unmarshal(original.Envelope, &before))
				testutil.NoError(t, proto.Unmarshal(dispatch.Envelope, &after))
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
	t.Run("receipt-fuse-bounds-automatic-recovery", func(t *testing.T) {
		f := newOutboxFixture(t)
		ctx := t.Context()
		service := operations.NewBackend(f.backend, 1, f.signer)
		created, _, err := service.Create(ctx, outboxRequest(f.node(t)))
		testutil.NoError(t, err)
		operationID := uuid.MustParse(created.ID)
		for i, backoff := range []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second} {
			dispatch := claimOutbox(t, service)
			testutil.NoError(t, service.MarkSentWithEnvelope(ctx, dispatch, dispatch.Envelope))
			f.exec(t, `UPDATE commands SET state='unknown' WHERE id=$1`, dispatch.CommandID)
			f.exec(t, `UPDATE operations SET state='unknown' WHERE id=$1`, operationID)
			var envelope agentv1.CommandEnvelope
			testutil.NoError(t, proto.Unmarshal(dispatch.Envelope, &envelope))
			now := time.Now().UTC().Truncate(time.Microsecond)
			payload, expires, err := operations.PrepareRecoveryEnvelope(&envelope, agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY, now, f.signer)
			testutil.NoError(t, err)
			expiry, err := value.FromTime(expires)
			testutil.NoError(t, err)
			observed, err := value.FromTime(now)
			testutil.NoError(t, err)
			testutil.NoError(t, database.Within(ctx, f.backend, database.ReadCommitted, func(tx database.Tx) error {
				store, err := resultstore.Results(tx)
				if err != nil {
					return err
				}
				return store.ScheduleRecovery(ctx, dispatch.CommandID, payload, expiry, observed, true)
			}))
			var available, published value.Timestamp
			var limit int
			testutil.NoError(t, f.owner.QueryRow(ctx, `SELECT available_at,published_at,receipt_recovery_attempt_limit FROM outbox_events WHERE id=$1`, dispatch.OutboxID).Scan(&available, &published, &limit))
			if limit != 4 {
				t.Fatalf("receipt cap=%d after attempt %d", limit, i+1)
			}
			if i == 3 {
				if published != observed {
					t.Fatalf("receipt fuse published=%+v want %+v", published, observed)
				}
				if jobs, err := service.Claim(ctx, uuid.Must(uuid.NewV7()), 1, time.Minute); err != nil || len(jobs) != 0 {
					t.Fatal("receipt fuse allowed another claim", jobs, err)
				}
				break
			}
			want, err := observed.Add(backoff)
			testutil.NoError(t, err)
			if published.Valid || available != want {
				t.Fatalf("attempt %d available=%+v published=%+v want available=%+v", i+1, available, published, want)
			}
			f.exec(t, `UPDATE outbox_events SET available_at=now() WHERE id=$1`, dispatch.OutboxID)
		}
	})
	t.Run("early-and-duplicate-result", func(t *testing.T) {
		f := newOutboxFixture(t)
		ctx := context.Background()
		service := operations.NewBackend(f.backend, 1, f.signer)
		created, _, err := service.Create(ctx, outboxRequest(f.node(t)))
		testutil.NoError(t, err)
		d := claimOutbox(t, service)
		var envelope agentv1.CommandEnvelope
		testutil.NoError(t, proto.Unmarshal(d.Envelope, &envelope))
		at := timestamppb.Now()
		result := &agentv1.CommandResult{CommandId: envelope.CommandId, IdempotencyKey: envelope.IdempotencyKey,
			PayloadSha256: envelope.SemanticPayloadSha256, SemanticPayloadHashVersion: envelope.SemanticPayloadHashVersion,
			State: agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED, AcceptedAt: at, CompletedAt: at}
		payload, err := proto.Marshal(result)
		testutil.NoError(t, err)
		id := uuid.Must(uuid.NewV7())
		endpoint := sha256.Sum256(d.NodeID[:])
		event := &transportv1.TransportEvent{EventId: id[:], NodeId: d.NodeID[:], EndpointId: endpoint[:],
			Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT, OccurredAt: at, Traceparent: d.Traceparent, Payload: payload}
		ingress := transportevents.NewBackend(f.backend, f.signer)
		for range 2 {
			testutil.NoError(t, ingress.Ingest(ctx, event))
		}
		for range 2 {
			if err := service.MarkSentWithEnvelope(ctx, d, d.Envelope); err != nil {
				t.Fatal("late bookkeeping", err)
			}
		}
		op, err := service.Get(ctx, uuid.MustParse(created.ID))
		if err != nil || op.State != "succeeded" {
			t.Fatal(op, err)
		}
		if f.count(t, "SELECT count(*) FROM agent_command_results") != 1 || f.count(t, "SELECT count(*) FROM node_command_leases") != 0 {
			t.Fatal("duplicate result or leaked claim")
		}
		if f.count(t, `SELECT count(*) FROM operation_events WHERE state='succeeded'`) != 1 {
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
				created, _, err := service.Create(context.Background(), outboxRequest(node))
				testutil.NoError(t, err)
				operation := uuid.MustParse(created.ID)
				if state != "queued" {
					f.exec(t, `UPDATE operations SET state=$1 WHERE id=$2`, state, operation)
					f.exec(t, `UPDATE commands SET state=$1 WHERE operation_id=$2`, state, operation)
				}
				if published {
					at := value.Timestamp{Valid: true, Micros: value.NegativeInfinity}
					f.exec(t, `UPDATE outbox_events SET published_at=$1 WHERE command_id IN (SELECT id FROM commands WHERE operation_id=$2)`, at, operation)
				}
				return operation
			}
			return f, service, seed
		}
		t.Run("quiescent-unknown-does-not-block-other-workspace", func(t *testing.T) {
			f, service, seed := newFixture(t)
			otherWorkspace := uuid.Must(uuid.NewV7())
			at, err := value.FromTime(time.Now().UTC())
			testutil.NoError(t, err)
			f.exec(t, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'Outbox other workspace',$2,$3,$4)`, otherWorkspace, otherWorkspace.String(), at, at)
			f.exec(t, `INSERT INTO workspace_admins(workspace_id,identity_id,created_at) VALUES($1,$2,$3)`, otherWorkspace, outboxIdentityID, at)
			node := uuid.Must(uuid.NewV7())
			f.exec(t, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES($1,$2,$3,'active',$4,$5)`, node, otherWorkspace, node.String(), at, at)
			endpoint := sha256.Sum256(node[:])
			f.exec(t, `INSERT INTO node_endpoint_keys(node_id,endpoint_id,state,bound_at) VALUES($1,$2,'active',$3)`, node, endpoint[:], at)
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
				testutil.NoError(t, err)
			}
			leases := f.count(t, `SELECT count(*) FROM node_command_leases`)
			if claimed.Load() != 2 || leases != 2 {
				t.Fatal("concurrent recovery exceeded the global limit", claimed.Load(), leases)
			}
		})
		t.Run("sent-recovery-retries-keep-global-capacity", func(t *testing.T) {
			f, service, seed := newFixture(t)
			seed(f.node(t), "unknown", false)
			seed(f.node(t), "unknown", false)
			jobs, err := service.Claim(context.Background(), uuid.Must(uuid.NewV7()), 10, time.Minute)
			if err != nil || len(jobs) != 2 {
				t.Fatal("recovery retries were not claimed", jobs, err)
			}
			for _, job := range jobs {
				testutil.NoError(t, service.MarkSentWithEnvelope(context.Background(), job, job.Envelope))
			}
			// The retries are sent with no result yet: their leases are gone
			// and their operations are still Unknown, but they are executing.
			seed(f.node(t), "queued", false)
			if jobs, err := service.Claim(context.Background(), uuid.Must(uuid.NewV7()), 10, time.Minute); err != nil || len(jobs) != 0 {
				t.Fatal("sent recovery retries released global capacity", jobs, err)
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
				f.exec(t, `UPDATE commands SET resource_type='user',resource_key='alice',expected_version=$1 WHERE operation_id=$2`, index+1, operation)
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
			created, _, err := service.Create(ctx, outboxRequest(node))
			testutil.NoError(t, err)
			id := uuid.MustParse(created.ID)
			if source == "certificate-release" {
				f.exec(t, `UPDATE commands SET payload_type='certificate_revoke' WHERE operation_id=$1`, id)
				f.exec(t, `UPDATE outbox_events SET available_at=$1`, value.Timestamp{Valid: true, Micros: value.PositiveInfinity})
			} else {
				f.exec(t, `UPDATE nodes SET status='offline' WHERE id=$1`, node)
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
					if err := store.ReleaseOperation(ctx, uuid.Must(uuid.NewV7()), uuid.Nil); err != nil {
						return err
					}
					return store.ReleaseOperation(ctx, id, uuid.Nil)
				}
				store, err := telemetry.FromTransaction(tx)
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
			testutil.NoError(t, database.Within(ctx, f.backend, database.ReadCommitted, change))
			d := claimOutbox(t, service)
			if d.OperationID != id {
				t.Fatal("released wrong operation", d)
			}
			testutil.NoError(t, service.MarkSentWithEnvelope(ctx, d, d.Envelope))
			// Repeated release/activation must not revive a published command.
			testutil.NoError(t, database.Within(ctx, f.backend, database.ReadCommitted, change))
			assertUnclaimed()
			// Store errors propagate from a closed transaction.
			tx, err := f.backend.Begin(ctx, database.ReadCommitted)
			testutil.NoError(t, err)
			testutil.NoError(t, tx.Rollback(ctx))
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
		testutil.NoError(t, err)
	}
	// Fence rows have independent row locks for a deterministic two-row deadlock rendezvous.
	ready, proceed := make(chan struct{}, 2), make(chan struct{})
	done := make(chan error, 2)
	var attempts atomic.Int32
	for worker := range 2 {
		go func() {
			local := 0
			done <- database.WithinRetry(ctx, f.backend, database.ReadCommitted, func(tx database.Tx) error {
				local++
				attempts.Add(1)
				if n, err := tx.Exec(ctx, `UPDATE connection_owner_fencing SET owner_epoch=owner_epoch+1 WHERE node_id=$1`, nodes[worker][:]); err != nil {
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
				n, err := tx.Exec(ctx, `UPDATE connection_owner_fencing SET owner_epoch=owner_epoch+1 WHERE node_id=$1`, nodes[1-worker][:])
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
		testutil.NoError(t, <-done)
	}
	if attempts.Load() != 3 {
		t.Fatal("did not exercise one real deadlock rollback", attempts.Load())
	}
	for _, node := range nodes {
		if got := f.count(t, `SELECT owner_epoch FROM connection_owner_fencing WHERE node_id=$1`, node[:]); got != 3 {
			t.Fatal("partial transaction leaked", got)
		}
	}
}
