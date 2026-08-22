package localslice

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/connectionowner"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/semanticpayload"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const reconnectRecoveryTraceparent = "00-7123456789abcdef0123456789abcdef-7123456789abcdef-01"

// TestCommandResultBeforeMarkSentIntegration reproduces the transport race
// where the Agent result reaches Controller before the worker persists its
// successful send. The result remains authoritative and MarkSent only closes
// the matching attempt and lease; it cannot regress the operation afterward.
func TestCommandResultBeforeMarkSentIntegration(t *testing.T) {
	databaseURL := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}

	workspaceID, nodeID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	endpointID := integrationEndpoint(nodeID)
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at)
		VALUES($1,'Early command result',$2,now(),now())`, workspaceID, "early-command-result-"+workspaceID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at)
		VALUES($1,$2,$3,'active',1,now(),now())`, nodeID, workspaceID, "node-"+nodeID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO node_endpoint_keys(node_id,endpoint_id,state,bound_at)
		VALUES($1,$2,'active',now())`, nodeID, endpointID[:]); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup := context.Background()
		for _, statement := range []string{
			`DELETE FROM agent_command_results WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
			`DELETE FROM transport_event_quarantine WHERE node_id=$2`,
			`DELETE FROM transport_events WHERE node_id=$2`,
			`DELETE FROM node_command_leases WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
			`DELETE FROM command_attempts WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
			`DELETE FROM outbox_events WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
			`DELETE FROM commands WHERE workspace_id=$1`,
			`DELETE FROM operations WHERE workspace_id=$1`,
			`DELETE FROM audit_events WHERE workspace_id=$1`,
			`DELETE FROM security_alerts WHERE workspace_id=$1`,
			`DELETE FROM node_endpoint_keys WHERE node_id=$2`,
			`DELETE FROM nodes WHERE id=$2`,
			`DELETE FROM workspaces WHERE id=$1`,
		} {
			_, _ = pool.Exec(cleanup, statement, workspaceID, nodeID)
		}
		pool.Close()
	})

	signer := integrationCommandSigner()
	operations := operationstore.NewWithSigner(pool, 200, signer)
	term := acquireRecoveryTerm(t, ctx, pool, nodeID, 606)
	t.Cleanup(func() { _ = term.Release(context.Background(), pool) })
	authority := &recoveryTestAuthority{nodeID: nodeID, connectionID: term.ConnectionID(), epoch: term.Epoch()}
	service := NewWithCommandRecovery(pool, signer, operations, authority)
	held, replayed, err := operations.CreateSynthetic(ctx, operationstore.CreateRequest{
		NodeID: nodeID, IdempotencyKey: "unclaimed-result", ExpectedVersion: 1,
		Kind: operationstore.SyntheticNoop, TTL: 10 * time.Minute,
		RequestID: "unclaimed-result", Traceparent: reconnectRecoveryTraceparent,
	})
	if err != nil || replayed || held.CommandID == nil {
		t.Fatalf("create held command = %+v, replayed=%v, err=%v", held, replayed, err)
	}
	heldCommandID := uuid.MustParse(*held.CommandID)
	if _, err := pool.Exec(ctx, `UPDATE outbox_events SET available_at=(SELECT expires_at FROM commands WHERE id=$1) WHERE command_id=$1`, heldCommandID); err != nil {
		t.Fatal(err)
	}
	var heldEnvelope []byte
	if err := pool.QueryRow(ctx, `SELECT envelope FROM commands WHERE id=$1`, heldCommandID).Scan(&heldEnvelope); err != nil {
		t.Fatal(err)
	}
	unclaimedResult := recoveryResultEvent(t, nodeID, endpointID, heldEnvelope, agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED)
	unclaimedEventID := uuid.Must(uuid.FromBytes(unclaimedResult.GetEventId()))
	if err := service.Ingest(ctx, unclaimedResult); err != nil {
		t.Fatalf("quarantine unclaimed queued result: %v", err)
	}
	assertEventQuarantined(t, pool, unclaimedEventID, nodeID, "invalid_command_result")
	assertRecoveryState(t, pool, heldCommandID, "queued", "queued", false)
	var heldResultCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_command_results WHERE command_id=$1`, heldCommandID).Scan(&heldResultCount); err != nil || heldResultCount != 0 {
		t.Fatalf("unclaimed queued result count = %d, err=%v, want 0", heldResultCount, err)
	}

	createClaimed := func(t *testing.T, key string) (uuid.UUID, uuid.UUID, operationstore.Dispatch, []byte) {
		t.Helper()
		var nodeVersion int64
		if err := pool.QueryRow(ctx, `SELECT version FROM nodes WHERE id=$1`, nodeID).Scan(&nodeVersion); err != nil {
			t.Fatal(err)
		}
		operation, replayed, err := operations.CreateSynthetic(ctx, operationstore.CreateRequest{
			NodeID: nodeID, IdempotencyKey: key, ExpectedVersion: nodeVersion,
			Kind: operationstore.SyntheticNoop, TTL: 10 * time.Minute,
			RequestID: key, Traceparent: reconnectRecoveryTraceparent,
		})
		if err != nil || replayed || operation.CommandID == nil {
			t.Fatalf("create synthetic command = %+v, replayed=%v, err=%v", operation, replayed, err)
		}
		commandID := uuid.MustParse(*operation.CommandID)
		jobs, err := operations.Claim(ctx, uuid.Must(uuid.NewV7()), 1, time.Minute)
		if err != nil || len(jobs) != 1 || jobs[0].CommandID != commandID {
			t.Fatalf("claim command = %d jobs, err=%v", len(jobs), err)
		}
		return commandID, uuid.MustParse(operation.ID), jobs[0], fenceRecoveryDispatch(t, signer, endpointID, term, jobs[0])
	}

	commandID, operationID, dispatch, sent := createClaimed(t, "early-success")
	if err := service.Ingest(ctx, recoveryResultEvent(t, nodeID, endpointID, sent, agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED)); err != nil {
		t.Fatalf("ingest early success: %v", err)
	}
	assertRecoveryState(t, pool, commandID, "succeeded", "succeeded", true)
	if err := operations.MarkSentWithEnvelope(ctx, dispatch, sent); err != nil {
		t.Fatalf("finish dispatch after early success: %v", err)
	}
	assertEarlyResultDispatchClosed(t, pool, commandID, operationID, dispatch, "succeeded", true)

	// Hold a fully applied result before its transaction releases the outbox
	// row, then start MarkSent. This pins the real READ COMMITTED wait ordering
	// rather than merely invoking the two operations sequentially.
	runConcurrentResult := func(key string, state agentv1.CommandResultState) (uuid.UUID, uuid.UUID, operationstore.Dispatch, []byte) {
		commandID, operationID, dispatch, sent := createClaimed(t, key)
		barrierDir := t.TempDir()
		if err := service.EnableResultCommitBarrier(barrierDir); err != nil {
			t.Fatalf("enable result commit barrier: %v", err)
		}
		if err := os.WriteFile(fmt.Sprintf("%s/arm", barrierDir), []byte(commandID.String()+"\n"), 0o600); err != nil {
			t.Fatalf("arm result commit barrier: %v", err)
		}
		concurrentResult := recoveryResultEvent(t, nodeID, endpointID, sent, state)
		ingestResult := make(chan error, 1)
		go func() { ingestResult <- service.Ingest(ctx, concurrentResult) }()
		barrierDeadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(fmt.Sprintf("%s/received", barrierDir)); err == nil {
				break
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("read result commit barrier: %v", err)
			}
			if time.Now().After(barrierDeadline) {
				t.Fatal("result ingestion did not reach the commit barrier")
			}
			time.Sleep(10 * time.Millisecond)
		}
		markSent := make(chan error, 1)
		markStarted := make(chan struct{})
		go func() {
			close(markStarted)
			markSent <- operations.MarkSentWithEnvelope(ctx, dispatch, sent)
		}()
		<-markStarted
		markWaitDeadline := time.Now().Add(5 * time.Second)
		for {
			select {
			case err := <-markSent:
				t.Fatalf("MarkSent returned before the result row lock was released: %v", err)
			default:
			}
			var waiting bool
			if err := pool.QueryRow(ctx, `SELECT EXISTS(
				SELECT 1 FROM pg_stat_activity
				WHERE pid<>pg_backend_pid() AND state='active' AND wait_event_type='Lock'
				  AND query LIKE '%mark_sent_outbox_lock%')`).Scan(&waiting); err != nil {
				t.Fatalf("observe MarkSent row-lock wait: %v", err)
			}
			if waiting {
				break
			}
			if time.Now().After(markWaitDeadline) {
				t.Fatal("MarkSent did not wait on the result transaction's outbox lock")
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err := os.WriteFile(fmt.Sprintf("%s/release", barrierDir), []byte(commandID.String()+"\n"), 0o600); err != nil {
			t.Fatalf("release result commit barrier: %v", err)
		}
		if err := <-ingestResult; err != nil {
			t.Fatalf("commit concurrent early result: %v", err)
		}
		if err := <-markSent; err != nil {
			t.Fatalf("finish dispatch after concurrent early result: %v", err)
		}
		return commandID, operationID, dispatch, sent
	}

	commandID, operationID, dispatch, sent = runConcurrentResult("concurrent-early-success", agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED)
	assertEarlyResultDispatchClosed(t, pool, commandID, operationID, dispatch, "succeeded", true)
	var storedSent []byte
	if err := pool.QueryRow(ctx, `SELECT envelope FROM commands WHERE id=$1`, commandID).Scan(&storedSent); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(storedSent, sent) {
		t.Fatal("concurrent result did not retain the exact sent envelope")
	}

	commandID, operationID, dispatch, sent = runConcurrentResult("concurrent-early-unknown", agentv1.CommandResultState_COMMAND_RESULT_STATE_UNKNOWN)
	original := decodeRecoveryEnvelope(t, sent)
	assertEarlyResultDispatchClosed(t, pool, commandID, operationID, dispatch, "unknown", false)
	recovery := claimRecoveryDispatch(t, ctx, operations, commandID)
	assertReconcileOnlyIdentity(t, original, decodeRecoveryEnvelope(t, recovery.Envelope))
}

// TestAuthoritativeReconnectRecoversLostCommandResultIntegration reproduces
// the failover window where transport accepted a command, the old event stream
// lost its result, and a higher owner epoch became fresh. Every retry remains
// reconcile-only and keeps the original logical command identity.
func TestAuthoritativeReconnectRecoversLostCommandResultIntegration(t *testing.T) {
	databaseURL := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}

	workspaceID, nodeID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	endpointID := integrationEndpoint(nodeID)
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at)
		VALUES($1,'Reconnect recovery',$2,now(),now())`, workspaceID, "reconnect-recovery-"+workspaceID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at)
		VALUES($1,$2,$3,'active',1,now(),now())`, nodeID, workspaceID, "node-"+nodeID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO node_endpoint_keys(node_id,endpoint_id,state,bound_at)
		VALUES($1,$2,'active',now())`, nodeID, endpointID[:]); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup := context.Background()
		for _, statement := range []string{
			`DELETE FROM agent_command_results WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
			`DELETE FROM transport_events WHERE node_id=$2`,
			`DELETE FROM node_command_leases WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
			`DELETE FROM command_attempts WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
			`DELETE FROM outbox_events WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
			`DELETE FROM commands WHERE workspace_id=$1`,
			`DELETE FROM operations WHERE workspace_id=$1`,
			`DELETE FROM audit_events WHERE workspace_id=$1`,
			`DELETE FROM node_endpoint_keys WHERE node_id=$2`,
			`DELETE FROM nodes WHERE id=$2`,
			`DELETE FROM workspaces WHERE id=$1`,
		} {
			_, _ = pool.Exec(cleanup, statement, workspaceID, nodeID)
		}
		pool.Close()
	})

	signer := integrationCommandSigner()
	operations := operationstore.NewWithSigner(pool, 200, signer)
	operation, replayed, err := operations.CreateSynthetic(ctx, operationstore.CreateRequest{
		NodeID: nodeID, IdempotencyKey: "lost-result", ExpectedVersion: 1,
		Kind: operationstore.SyntheticNoop, TTL: 10 * time.Minute,
		RequestID: "lost-result", Traceparent: reconnectRecoveryTraceparent,
	})
	if err != nil || replayed || operation.CommandID == nil {
		t.Fatalf("create synthetic command = %+v, replayed=%v, err=%v", operation, replayed, err)
	}
	commandID, err := uuid.Parse(*operation.CommandID)
	if err != nil {
		t.Fatal(err)
	}
	operationID := uuid.MustParse(operation.ID)

	claimed, err := operations.Claim(ctx, uuid.Must(uuid.NewV7()), 1, time.Minute)
	if err != nil || len(claimed) != 1 || claimed[0].CommandID != commandID {
		t.Fatalf("initial claim = %d jobs, err=%v", len(claimed), err)
	}
	original := decodeRecoveryEnvelope(t, claimed[0].Envelope)
	oldTerm := acquireRecoveryTerm(t, ctx, pool, nodeID, 101)
	oldSent := fenceRecoveryDispatch(t, signer, endpointID, oldTerm, claimed[0])
	if err := operations.MarkSentWithEnvelope(ctx, claimed[0], oldSent); err != nil {
		t.Fatalf("mark old-owner dispatch sent: %v", err)
	}
	assertRecoveryState(t, pool, commandID, "dispatched", "dispatched", true)

	if err := oldTerm.Release(ctx, pool); err != nil {
		t.Fatalf("release old owner: %v", err)
	}
	successor := acquireRecoveryTerm(t, ctx, pool, nodeID, 202)
	staleAuthority := &recoveryTestAuthority{nodeID: nodeID, connectionID: oldTerm.ConnectionID(), epoch: oldTerm.Epoch()}
	staleService := NewWithCommandRecovery(pool, signer, operations, staleAuthority)
	authority := &recoveryTestAuthority{nodeID: nodeID, connectionID: successor.ConnectionID(), epoch: successor.Epoch()}
	service := NewWithCommandRecovery(pool, signer, operations, authority)

	// A late connected event from the old owner is durable ingress evidence,
	// but it has no authority to enqueue reconciliation work.
	staleEvent := recoveryConnectedEvent(t, nodeID, endpointID, oldTerm)
	if err := staleService.Ingest(ctx, staleEvent); err != nil {
		t.Fatalf("ingest stale owner event: %v", err)
	}
	assertRecoveryState(t, pool, commandID, "dispatched", "dispatched", true)

	connected := recoveryConnectedEvent(t, nodeID, endpointID, successor)
	if err := service.Ingest(ctx, connected); err != nil {
		t.Fatalf("ingest successor event: %v", err)
	}
	assertRecoveryState(t, pool, commandID, "unknown", "unknown", false)
	recoveryEnvelope := storedRecoveryEnvelope(t, pool, commandID)
	assertReconcileOnlyIdentity(t, original, recoveryEnvelope)
	if unknownEvents := recoveryEventCount(t, pool, operationID); unknownEvents != 1 {
		t.Fatalf("unknown events after first takeover = %d, want 1", unknownEvents)
	}

	// Transport event replay is idempotent and cannot create another outbox
	// publication or another operation transition.
	if err := service.Ingest(ctx, connected); err != nil {
		t.Fatalf("reingest successor event: %v", err)
	}
	if unknownEvents := recoveryEventCount(t, pool, operationID); unknownEvents != 1 {
		t.Fatalf("unknown events after duplicate event = %d, want 1", unknownEvents)
	}

	recovery := claimRecoveryDispatch(t, ctx, operations, commandID)
	successorSent := fenceRecoveryDispatch(t, signer, endpointID, successor, recovery)
	if err := operations.MarkSentWithEnvelope(ctx, recovery, successorSent); err != nil {
		t.Fatalf("mark successor reconciliation sent: %v", err)
	}
	assertRecoveryState(t, pool, commandID, "unknown", "unknown", true)
	assertSentAttemptClosed(t, pool, recovery)
	if dispatchedEvents := recoveryStateEventCount(t, pool, operationID, "dispatched"); dispatchedEvents != 1 {
		t.Fatalf("dispatched events after reconcile-only send = %d, want original dispatch only", dispatchedEvents)
	}
	// A result row records raw Agent evidence, not necessarily the accepted
	// projection. Simulate a terminal-looking receipt that failed verification;
	// it must not suppress continuation or a later owner-term recovery while the
	// authoritative command and operation remain Unknown.
	rawResultEventID := uuid.Must(uuid.NewV7())
	rawResultIdempotencyKey, err := uuid.FromBytes(original.GetIdempotencyKey())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO transport_events
		(event_id,node_id,event_type,occurred_at,traceparent,payload)
		VALUES($1,$2,'command_result',now(),$3,$4)`, rawResultEventID, nodeID, reconnectRecoveryTraceparent, []byte("unaccepted terminal evidence")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO agent_command_results
		(event_id,command_id,idempotency_key,payload_sha256,state,result,accepted_at,completed_at,replayed,created_at,receipt_verification_status,receipt_failure_reason)
		SELECT $1,id,$3,decode(repeat('71',32),'hex'),'succeeded',''::bytea,now(),now(),false,now(),'missing','missing_receipt'
		FROM commands WHERE id=$2`, rawResultEventID, commandID, rawResultIdempotencyKey); err != nil {
		t.Fatal(err)
	}

	// A second fresh event for the same owner term sees that exact term on the
	// persisted sent frame and does not immediately enqueue the command again.
	if err := service.Ingest(ctx, recoveryConnectedEvent(t, nodeID, endpointID, successor)); err != nil {
		t.Fatalf("ingest same-term fresh event: %v", err)
	}
	assertRecoveryState(t, pool, commandID, "unknown", "unknown", true)
	if unknownEvents := recoveryEventCount(t, pool, operationID); unknownEvents != 1 {
		t.Fatalf("unknown events after same-term reconnect = %d, want 1", unknownEvents)
	}

	// Reap leaves a fresh observation attempt alone. Once its bounded response
	// deadline is exceeded, it creates a new RECONCILE_ONLY message without
	// changing the logical Unknown state or extending the command deadline.
	if err := operations.Reap(ctx, 3); err != nil {
		t.Fatalf("reap fresh reconciliation: %v", err)
	}
	assertRecoveryState(t, pool, commandID, "unknown", "unknown", true)
	if _, err := pool.Exec(ctx, `UPDATE command_attempts SET finished_at=now()-interval '1 minute'
		WHERE id=$1`, recovery.AttemptID); err != nil {
		t.Fatal(err)
	}
	if err := operations.Reap(ctx, 3); err != nil {
		t.Fatalf("continue stale reconciliation: %v", err)
	}
	assertRecoveryState(t, pool, commandID, "unknown", "unknown", false)
	continued := claimRecoveryDispatch(t, ctx, operations, commandID)
	continuedEnvelope := decodeRecoveryEnvelope(t, continued.Envelope)
	assertReconcileOnlyIdentity(t, original, continuedEnvelope)
	if bytes.Equal(decodeRecoveryEnvelope(t, recovery.Envelope).GetMessageId(), continuedEnvelope.GetMessageId()) {
		t.Fatal("reconciliation continuation reused the prior message identity")
	}
	if !decodeRecoveryEnvelope(t, recovery.Envelope).GetExpiresAt().AsTime().Equal(continuedEnvelope.GetExpiresAt().AsTime()) {
		t.Fatal("reconciliation continuation extended the logical command deadline")
	}
	continuedSent := fenceRecoveryDispatch(t, signer, endpointID, successor, continued)
	if err := operations.MarkSentWithEnvelope(ctx, continued, continuedSent); err != nil {
		t.Fatalf("mark continued reconciliation sent: %v", err)
	}
	assertRecoveryState(t, pool, commandID, "unknown", "unknown", true)
	assertSentAttemptClosed(t, pool, continued)
	if dispatchedEvents := recoveryStateEventCount(t, pool, operationID, "dispatched"); dispatchedEvents != 1 {
		t.Fatalf("dispatched events after continuation = %d, want original dispatch only", dispatchedEvents)
	}

	// Persist one nonterminal Agent result, dispatch its reconcile-only followup,
	// and fail over again. The prior unknown evidence must not suppress recovery.
	if err := service.Ingest(ctx, recoveryResultEvent(t, nodeID, endpointID, continued.Envelope, agentv1.CommandResultState_COMMAND_RESULT_STATE_UNKNOWN)); err != nil {
		t.Fatalf("ingest unknown journal result: %v", err)
	}
	recovery = claimRecoveryDispatch(t, ctx, operations, commandID)
	successorSent = fenceRecoveryDispatch(t, signer, endpointID, successor, recovery)
	if err := operations.MarkSentWithEnvelope(ctx, recovery, successorSent); err != nil {
		t.Fatalf("mark second successor reconciliation sent: %v", err)
	}
	assertRecoveryState(t, pool, commandID, "unknown", "unknown", true)
	if err := successor.Release(ctx, pool); err != nil {
		t.Fatalf("release successor owner: %v", err)
	}
	third := acquireRecoveryTerm(t, ctx, pool, nodeID, 303)
	authority.connectionID, authority.epoch = third.ConnectionID(), third.Epoch()
	if err := service.Ingest(ctx, recoveryConnectedEvent(t, nodeID, endpointID, third)); err != nil {
		t.Fatalf("ingest third owner event: %v", err)
	}
	assertRecoveryState(t, pool, commandID, "unknown", "unknown", false)

	recovery = claimRecoveryDispatch(t, ctx, operations, commandID)
	thirdSent := fenceRecoveryDispatch(t, signer, endpointID, third, recovery)
	if err := operations.MarkSentWithEnvelope(ctx, recovery, thirdSent); err != nil {
		t.Fatalf("mark third-owner reconciliation sent: %v", err)
	}
	if err := service.Ingest(ctx, recoveryResultEvent(t, nodeID, endpointID, recovery.Envelope, agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED)); err != nil {
		t.Fatalf("ingest replayed journal success: %v", err)
	}
	assertRecoveryState(t, pool, commandID, "succeeded", "succeeded", true)
	var resultCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_command_results WHERE command_id=$1`, commandID).Scan(&resultCount); err != nil || resultCount != 3 {
		t.Fatalf("journal result count = %d, err=%v, want raw evidence plus unknown and replayed success", resultCount, err)
	}

	// Terminal commands remain terminal across later takeovers.
	if err := third.Release(ctx, pool); err != nil {
		t.Fatalf("release third owner: %v", err)
	}
	fourth := acquireRecoveryTerm(t, ctx, pool, nodeID, 404)
	authority.connectionID, authority.epoch = fourth.ConnectionID(), fourth.Epoch()
	t.Cleanup(func() { _ = fourth.Release(context.Background(), pool) })
	unknownEvents := recoveryEventCount(t, pool, operationID)
	if err := service.Ingest(ctx, recoveryConnectedEvent(t, nodeID, endpointID, fourth)); err != nil {
		t.Fatalf("ingest post-terminal owner event: %v", err)
	}
	assertRecoveryState(t, pool, commandID, "succeeded", "succeeded", true)
	if got := recoveryEventCount(t, pool, operationID); got != unknownEvents {
		t.Fatalf("terminal command gained a recovery event: before=%d after=%d", unknownEvents, got)
	}
	jobs, err := operations.Claim(ctx, uuid.Must(uuid.NewV7()), 1, time.Minute)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("terminal command became dispatchable: jobs=%d err=%v", len(jobs), err)
	}

	// The sent-state commit itself is fenced. If a different Controller takes
	// over after transport accepted an old frame, the old owner cannot publish
	// dispatched after the successor's connected sweep has already run.
	var nodeVersion int64
	if err := pool.QueryRow(ctx, `SELECT version FROM nodes WHERE id=$1`, nodeID).Scan(&nodeVersion); err != nil {
		t.Fatal(err)
	}
	lateOperation, replayed, err := operations.CreateSynthetic(ctx, operationstore.CreateRequest{
		NodeID: nodeID, IdempotencyKey: "late-old-owner-send", ExpectedVersion: nodeVersion,
		Kind: operationstore.SyntheticNoop, TTL: 10 * time.Minute,
		RequestID: "late-old-owner-send", Traceparent: reconnectRecoveryTraceparent,
	})
	if err != nil || replayed || lateOperation.CommandID == nil {
		t.Fatalf("create late old-owner command = %+v, replayed=%v, err=%v", lateOperation, replayed, err)
	}
	lateCommandID := uuid.MustParse(*lateOperation.CommandID)
	lateJobs, err := operations.Claim(ctx, uuid.Must(uuid.NewV7()), 1, time.Minute)
	if err != nil || len(lateJobs) != 1 || lateJobs[0].CommandID != lateCommandID {
		t.Fatalf("claim late old-owner dispatch = %d jobs, err=%v", len(lateJobs), err)
	}
	lateOriginal := decodeRecoveryEnvelope(t, lateJobs[0].Envelope)
	lateOldFrame := fenceRecoveryDispatch(t, signer, endpointID, fourth, lateJobs[0])
	if err := fourth.Release(ctx, pool); err != nil {
		t.Fatalf("release fourth owner before late sent commit: %v", err)
	}
	fifth := acquireRecoveryTerm(t, ctx, pool, nodeID, 505)
	t.Cleanup(func() { _ = fifth.Release(context.Background(), pool) })
	if err := operations.MarkSentWithEnvelope(ctx, lateJobs[0], lateOldFrame); !errors.Is(err, connectionowner.ErrNotOwner) {
		t.Fatalf("late old-owner sent commit error = %v, want ErrNotOwner", err)
	}
	assertRecoveryState(t, pool, lateCommandID, "queued", "queued", false)
	if _, err := pool.Exec(ctx, `UPDATE node_command_leases
		SET leased_until=clock_timestamp()-interval '1 second'
		WHERE command_id=$1`, lateCommandID); err != nil {
		t.Fatal(err)
	}
	if err := operations.Reap(ctx, 3); err != nil {
		t.Fatalf("reap accepted old-owner ambiguity: %v", err)
	}
	assertRecoveryState(t, pool, lateCommandID, "unknown", "unknown", false)
	lateRecovery := claimRecoveryDispatch(t, ctx, operations, lateCommandID)
	assertReconcileOnlyIdentity(t, lateOriginal, decodeRecoveryEnvelope(t, lateRecovery.Envelope))
}

type recoveryTestAuthority struct {
	nodeID       uuid.UUID
	connectionID [16]byte
	epoch        int64
}

func (a *recoveryTestAuthority) OwnsTerm(nodeID, connectionID [16]byte, epoch int64) bool {
	return nodeID == [16]byte(a.nodeID) && connectionID == a.connectionID && epoch == a.epoch
}

func acquireRecoveryTerm(t *testing.T, ctx context.Context, pool *pgxpool.Pool, nodeID uuid.UUID, incarnation int64) *connectionowner.Term {
	t.Helper()
	connectionID := uuid.Must(uuid.NewV7())
	term, err := connectionowner.Acquire(ctx, pool, [16]byte(nodeID), connectionowner.Identity{
		InstanceID: uuid.Must(uuid.NewV7()), Incarnation: incarnation,
	}, [16]byte(connectionID), 2*time.Minute)
	if err != nil {
		t.Fatalf("acquire recovery owner %d: %v", incarnation, err)
	}
	return term
}

func fenceRecoveryDispatch(t *testing.T, signer *commandauth.Signer, endpointID [32]byte, term *connectionowner.Term, dispatch operationstore.Dispatch) []byte {
	t.Helper()
	envelope := decodeRecoveryEnvelope(t, dispatch.Envelope)
	now := time.Now().UTC().Truncate(time.Microsecond)
	fenceID := uuid.Must(uuid.NewV7())
	capabilities := []string{"ocserv.fencing.v2", envelope.GetRequiredCapability()}
	slices.Sort(capabilities)
	ownerID := [16]byte(term.Identity().InstanceID)
	fence, err := signer.IssueConnectionFenceV2(
		[16]byte(fenceID), term.NodeID(), endpointID, ownerID,
		uint64(term.Identity().Incarnation), uint64(term.Epoch()), term.ConnectionID(),
		envelope.GetExpectedRevision(), capabilities, term.LeaseUntil(), now, term.LeaseUntil().Add(5*time.Minute),
	)
	if err != nil {
		t.Fatalf("issue dispatch fence: %v", err)
	}
	binding, err := signer.IssueFenceBindingV2(
		agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND,
		[16]byte(dispatch.CommandID), [16]byte(fenceID), term.NodeID(), endpointID, ownerID,
		uint64(term.Identity().Incarnation), uint64(term.Epoch()), term.ConnectionID(),
		envelope.GetExpectedRevision(), envelope.GetRequiredCapability(), now, term.LeaseUntil().Add(5*time.Minute),
	)
	if err != nil {
		t.Fatalf("issue dispatch fence binding: %v", err)
	}
	envelope.ConnectionFence, envelope.FenceBinding = fence, binding
	encoded, err := proto.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func recoveryConnectedEvent(t *testing.T, nodeID uuid.UUID, endpointID [32]byte, term *connectionowner.Term) *transportv1.TransportEvent {
	t.Helper()
	eventID := uuid.Must(uuid.NewV7())
	connectionID := term.ConnectionID()
	return &transportv1.TransportEvent{
		EventId: eventID[:], NodeId: nodeID[:], EndpointId: endpointID[:],
		ConnectionId: connectionID[:], OwnerEpoch: uint64(term.Epoch()),
		Type:       transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_CONNECTED,
		OccurredAt: timestamppb.Now(), Traceparent: reconnectRecoveryTraceparent,
		Payload: []byte("authoritative reconnect"),
	}
}

func recoveryResultEvent(t *testing.T, nodeID uuid.UUID, endpointID [32]byte, envelopeBytes []byte, state agentv1.CommandResultState) *transportv1.TransportEvent {
	t.Helper()
	envelope := decodeRecoveryEnvelope(t, envelopeBytes)
	payloadHash, err := semanticpayload.HashV2(envelope)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	result := &agentv1.CommandResult{
		CommandId: envelope.GetCommandId(), IdempotencyKey: envelope.GetIdempotencyKey(),
		PayloadSha256: payloadHash[:], State: state,
		AcceptedAt: timestamppb.New(now), CompletedAt: timestamppb.New(now), Replayed: true,
		SemanticPayloadHashVersion: agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V2,
	}
	if state == agentv1.CommandResultState_COMMAND_RESULT_STATE_UNKNOWN {
		result.ErrorCode = "outcome_requires_reconciliation"
	}
	payload, err := proto.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	eventID := uuid.Must(uuid.NewV7())
	return &transportv1.TransportEvent{
		EventId: eventID[:], NodeId: nodeID[:], EndpointId: endpointID[:],
		Type:       transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT,
		OccurredAt: timestamppb.New(now), Traceparent: envelope.GetTraceparent(), Payload: payload,
	}
}

func claimRecoveryDispatch(t *testing.T, ctx context.Context, operations *operationstore.Service, commandID uuid.UUID) operationstore.Dispatch {
	t.Helper()
	jobs, err := operations.Claim(ctx, uuid.Must(uuid.NewV7()), 1, time.Minute)
	if err != nil || len(jobs) != 1 || jobs[0].CommandID != commandID {
		t.Fatalf("claim reconcile-only dispatch = %d jobs, err=%v", len(jobs), err)
	}
	envelope := decodeRecoveryEnvelope(t, jobs[0].Envelope)
	if envelope.GetDeliveryMode() != agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY || envelope.GetConnectionFence() != nil || envelope.GetFenceBinding() != nil {
		t.Fatal("claimed recovery dispatch is not an unbound reconcile-only command")
	}
	return jobs[0]
}

func assertRecoveryState(t *testing.T, pool *pgxpool.Pool, commandID uuid.UUID, commandState, operationState string, published bool) {
	t.Helper()
	var gotCommand, gotOperation string
	var gotPublished bool
	if err := pool.QueryRow(context.Background(), `SELECT command.state,operation.state,outbox.published_at IS NOT NULL
		FROM commands AS command JOIN operations AS operation ON operation.id=command.operation_id
		JOIN outbox_events AS outbox ON outbox.command_id=command.id WHERE command.id=$1`, commandID).
		Scan(&gotCommand, &gotOperation, &gotPublished); err != nil {
		t.Fatal(err)
	}
	if gotCommand != commandState || gotOperation != operationState || gotPublished != published {
		t.Fatalf("command/operation/published = %s/%s/%v, want %s/%s/%v", gotCommand, gotOperation, gotPublished, commandState, operationState, published)
	}
}

func assertEarlyResultDispatchClosed(t *testing.T, pool *pgxpool.Pool, commandID, operationID uuid.UUID, dispatch operationstore.Dispatch, state string, published bool) {
	t.Helper()
	assertRecoveryState(t, pool, commandID, state, state, published)
	var attemptState string
	var leaseCount, dispatchedEvents int
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT state FROM command_attempts WHERE id=$1),
		(SELECT count(*) FROM node_command_leases WHERE lease_token=$2),
		(SELECT count(*) FROM operation_events WHERE operation_id=$3 AND state='dispatched')`,
		dispatch.AttemptID, dispatch.LeaseToken, operationID).Scan(&attemptState, &leaseCount, &dispatchedEvents); err != nil {
		t.Fatal(err)
	}
	if attemptState != "sent" || leaseCount != 0 || dispatchedEvents != 0 {
		t.Fatalf("attempt/lease/dispatched events = %s/%d/%d, want sent/0/0", attemptState, leaseCount, dispatchedEvents)
	}
}

func assertSentAttemptClosed(t *testing.T, pool *pgxpool.Pool, dispatch operationstore.Dispatch) {
	t.Helper()
	var attemptState string
	var leaseCount int
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT state FROM command_attempts WHERE id=$1),
		(SELECT count(*) FROM node_command_leases WHERE lease_token=$2)`,
		dispatch.AttemptID, dispatch.LeaseToken).Scan(&attemptState, &leaseCount); err != nil {
		t.Fatal(err)
	}
	if attemptState != "sent" || leaseCount != 0 {
		t.Fatalf("attempt/lease = %s/%d, want sent/0", attemptState, leaseCount)
	}
}

func storedRecoveryEnvelope(t *testing.T, pool *pgxpool.Pool, commandID uuid.UUID) *agentv1.CommandEnvelope {
	t.Helper()
	var commandPayload, outboxPayload []byte
	if err := pool.QueryRow(context.Background(), `SELECT command.envelope,outbox.payload
		FROM commands AS command JOIN outbox_events AS outbox ON outbox.command_id=command.id
		WHERE command.id=$1`, commandID).Scan(&commandPayload, &outboxPayload); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(commandPayload, outboxPayload) {
		t.Fatal("recovery command and outbox payload diverged")
	}
	return decodeRecoveryEnvelope(t, commandPayload)
}

func decodeRecoveryEnvelope(t *testing.T, encoded []byte) *agentv1.CommandEnvelope {
	t.Helper()
	var envelope agentv1.CommandEnvelope
	if err := proto.Unmarshal(encoded, &envelope); err != nil {
		t.Fatal(err)
	}
	return &envelope
}

func assertReconcileOnlyIdentity(t *testing.T, original, recovered *agentv1.CommandEnvelope) {
	t.Helper()
	if recovered.GetDeliveryMode() != agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY ||
		recovered.GetConnectionFence() != nil || recovered.GetFenceBinding() != nil {
		t.Fatal("recovered command is not unbound reconcile-only work")
	}
	if !bytes.Equal(original.GetCommandId(), recovered.GetCommandId()) ||
		!bytes.Equal(original.GetOperationId(), recovered.GetOperationId()) ||
		!bytes.Equal(original.GetIdempotencyKey(), recovered.GetIdempotencyKey()) ||
		!bytes.Equal(original.GetSemanticPayloadSha256(), recovered.GetSemanticPayloadSha256()) ||
		!proto.Equal(original.GetSyntheticNoop(), recovered.GetSyntheticNoop()) {
		t.Fatal("recovered command changed logical identity or payload")
	}
}

func recoveryEventCount(t *testing.T, pool *pgxpool.Pool, operationID uuid.UUID) int {
	return recoveryStateEventCount(t, pool, operationID, "unknown")
}

func recoveryStateEventCount(t *testing.T, pool *pgxpool.Pool, operationID uuid.UUID, state string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM operation_events WHERE operation_id=$1 AND state=$2`, operationID, state).Scan(&count); err != nil {
		t.Fatal(fmt.Errorf("count recovery events: %w", err))
	}
	return count
}
