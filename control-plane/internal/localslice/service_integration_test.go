package localslice

import (
	"context"
	"os"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestDisconnectedEventPreservesUntrustedNodeStatesIntegration(t *testing.T) {
	databaseURL := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	workspaceID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id,name,slug,created_at,updated_at) VALUES ($1,'Revoked event test',$2,now(),now())`, workspaceID, "revoked-event-"+workspaceID.String()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM workspaces WHERE id=$1`, workspaceID) })
	for _, initialStatus := range []string{"pending", "revoked"} {
		nodeID := uuid.Must(uuid.NewV7())
		if _, err := pool.Exec(ctx, `INSERT INTO nodes (id,workspace_id,name,status,created_at,updated_at) VALUES ($1,$2,$3,$4,now(),now())`, nodeID, workspaceID, initialStatus+"-node", initialStatus); err != nil {
			t.Fatal(err)
		}
		eventID := uuid.Must(uuid.NewV7())
		err = New(pool).Ingest(ctx, &transportv1.TransportEvent{EventId: eventID[:], NodeId: nodeID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_DISCONNECTED, OccurredAt: timestamppb.Now(), Traceparent: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01", Payload: []byte(initialStatus + " disconnect")})
		if err != nil {
			t.Fatal(err)
		}
		var status string
		if err := pool.QueryRow(ctx, `SELECT status FROM nodes WHERE id=$1`, nodeID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != initialStatus {
			t.Fatalf("%s node status after disconnect = %q", initialStatus, status)
		}
	}
}

func TestStructuredAgentResultPersistsUnknownBeforeReconciledSuccessIntegration(t *testing.T) {
	databaseURL := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	workspaceID, nodeID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'I10 test',$2,now(),now())`, workspaceID, "i10-"+workspaceID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at) VALUES($1,$2,$3,'active',1,now(),now())`, nodeID, workspaceID, "node-"+nodeID.String()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, statement := range []string{
			`DELETE FROM agent_command_results WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
			`DELETE FROM transport_events WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`,
			`DELETE FROM node_command_leases WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
			`DELETE FROM command_attempts WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
			`DELETE FROM outbox_events WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
			`DELETE FROM commands WHERE workspace_id=$1`, `DELETE FROM operations WHERE workspace_id=$1`,
			`DELETE FROM audit_events WHERE workspace_id=$1`, `DELETE FROM nodes WHERE workspace_id=$1`,
			`DELETE FROM workspaces WHERE id=$1`,
		} {
			_, _ = pool.Exec(context.Background(), statement, workspaceID)
		}
	})
	operationID, commandID, idempotencyKey := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	messageID := uuid.Must(uuid.NewV7())
	now := time.Now().UTC()
	envelope := agentv1.CommandEnvelope{ProtocolVersion: "1.0", MessageId: messageID[:], CommandId: commandID[:], IdempotencyKey: idempotencyKey[:], NodeId: nodeID[:], Sequence: 1, IssuedAt: timestamppb.New(now), ExpiresAt: timestamppb.New(now.Add(time.Minute)), ExpectedRevision: 1, Traceparent: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01", ActorId: "test", Reason: "I10 result ingestion", Payload: &agentv1.CommandEnvelope_SyntheticEcho{SyntheticEcho: &agentv1.SyntheticEcho{Message: "hello"}}}
	envelopeBytes, err := proto.Marshal(&envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO operations(id,workspace_id,node_id,command_id,state,request_id,trace_id,created_at,updated_at,idempotency_key,request_hash,expires_at) VALUES($1,$2,$3,$4,'dispatched','i10-result','0123456789abcdef0123456789abcdef',now(),now(),'journal-result',$5,$6)`, operationID, workspaceID, nodeID, commandID, make([]byte, 32), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO commands(id,operation_id,workspace_id,node_id,state,payload_type,envelope,idempotency_key,expected_version,sequence,traceparent,expires_at,created_at,updated_at) VALUES($1,$2,$3,$4,'dispatched','synthetic_echo',$5,'journal-result',1,1,$6,$7,$8,$8)`, commandID, operationID, workspaceID, nodeID, envelopeBytes, envelope.GetTraceparent(), now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	payloadHash, err := agentPayloadHash(&envelope)
	if err != nil {
		t.Fatal(err)
	}
	ingest := func(state agentv1.CommandResultState, replayed bool) {
		t.Helper()
		result := &agentv1.CommandResult{CommandId: envelope.GetCommandId(), IdempotencyKey: envelope.GetIdempotencyKey(), PayloadSha256: payloadHash[:], State: state, Result: []byte("hello"), AcceptedAt: timestamppb.Now(), CompletedAt: timestamppb.Now(), Replayed: replayed}
		payload, err := proto.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		eventID := uuid.Must(uuid.NewV7())
		if err := New(pool).Ingest(ctx, &transportv1.TransportEvent{EventId: eventID[:], NodeId: nodeID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT, OccurredAt: timestamppb.Now(), Traceparent: envelope.GetTraceparent(), Payload: payload}); err != nil {
			t.Fatal(err)
		}
	}
	ingest(agentv1.CommandResultState_COMMAND_RESULT_STATE_UNKNOWN, false)
	var state string
	if err := pool.QueryRow(ctx, `SELECT state FROM operations WHERE id=$1`, operationID).Scan(&state); err != nil || state != "unknown" {
		t.Fatalf("unknown state = %q, %v", state, err)
	}
	ingest(agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED, true)
	var resultCount int
	if err := pool.QueryRow(ctx, `SELECT operation.state,count(result.event_id) FROM operations AS operation JOIN commands AS command ON command.operation_id=operation.id JOIN agent_command_results AS result ON result.command_id=command.id WHERE operation.id=$1 GROUP BY operation.state`, operationID).Scan(&state, &resultCount); err != nil {
		t.Fatal(err)
	}
	if state != "succeeded" || resultCount != 2 {
		t.Fatalf("reconciled state/results = %q/%d", state, resultCount)
	}
}
