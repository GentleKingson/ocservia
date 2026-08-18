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
	assertRecoveryState(t, pool, commandID, "dispatched", "unknown", true)

	// A second fresh event for the same owner term sees that exact term on the
	// persisted sent frame and does not enqueue the command again.
	if err := service.Ingest(ctx, recoveryConnectedEvent(t, nodeID, endpointID, successor)); err != nil {
		t.Fatalf("ingest same-term fresh event: %v", err)
	}
	assertRecoveryState(t, pool, commandID, "dispatched", "unknown", true)
	if unknownEvents := recoveryEventCount(t, pool, operationID); unknownEvents != 1 {
		t.Fatalf("unknown events after same-term reconnect = %d, want 1", unknownEvents)
	}

	// Persist one nonterminal Agent result, dispatch its reconcile-only followup,
	// and fail over again. The prior unknown evidence must not suppress recovery.
	if err := service.Ingest(ctx, recoveryResultEvent(t, nodeID, endpointID, recovery.Envelope, agentv1.CommandResultState_COMMAND_RESULT_STATE_UNKNOWN)); err != nil {
		t.Fatalf("ingest unknown journal result: %v", err)
	}
	recovery = claimRecoveryDispatch(t, ctx, operations, commandID)
	successorSent = fenceRecoveryDispatch(t, signer, endpointID, successor, recovery)
	if err := operations.MarkSentWithEnvelope(ctx, recovery, successorSent); err != nil {
		t.Fatalf("mark second successor reconciliation sent: %v", err)
	}
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
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_command_results WHERE command_id=$1`, commandID).Scan(&resultCount); err != nil || resultCount != 2 {
		t.Fatalf("journal result count = %d, err=%v, want unknown plus replayed success", resultCount, err)
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
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM operation_events WHERE operation_id=$1 AND state='unknown'`, operationID).Scan(&count); err != nil {
		t.Fatal(fmt.Errorf("count recovery events: %w", err))
	}
	return count
}
