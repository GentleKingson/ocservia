package transportevents

import (
	"context"
	"os"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/attestationtest"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/semanticpayload"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestPrivilegedResultTrustIntegration covers the Controller side of two root
// trust rules for a fixed host action (ServiceReload):
//   - an unsigned Rejected result is never terminal evidence, whether the
//     command was undecided or already Unknown, and a later signed result still
//     settles it;
//   - the Agent's unsigned effect_absent never authorizes an executable retry.
func TestPrivilegedResultTrustIntegration(t *testing.T) {
	databaseURL, ownerURL := os.Getenv("OCSERV_TEST_DATABASE_URL"), os.Getenv("OCSERV_TEST_OWNER_DATABASE_URL")
	if databaseURL == "" || ownerURL == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL and OCSERV_TEST_OWNER_DATABASE_URL are required")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	cleanupPool, err := pgxpool.New(ctx, ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupPool.Close()
	workspaceID, nodeID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'privileged trust',$2,now(),now())`, workspaceID, "privileged-trust-"+workspaceID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at) VALUES($1,$2,'privileged-node','active',1,now(),now())`, nodeID, workspaceID); err != nil {
		t.Fatal(err)
	}
	attestationKey, err := attestationtest.InstallKey(ctx, pool, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := integrationEndpoint(nodeID)
	if _, err := pool.Exec(ctx, `INSERT INTO node_endpoint_keys(node_id,endpoint_id,state,bound_at) VALUES($1,$2,'active',now())`, nodeID, endpoint[:]); err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, statement := range []string{
			`DELETE FROM agent_command_results WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
			`DELETE FROM transport_events WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`,
			`DELETE FROM outbox_events WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
			`DELETE FROM operation_events WHERE operation_id IN(SELECT id FROM operations WHERE workspace_id=$1)`,
			`DELETE FROM commands WHERE workspace_id=$1`, `DELETE FROM operations WHERE workspace_id=$1`,
			`DELETE FROM security_alerts WHERE workspace_id=$1`,
			`DELETE FROM audit_events WHERE workspace_id=$1`, `DELETE FROM node_endpoint_keys WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`,
			`DELETE FROM node_privd_attestation_keys WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`,
			`DELETE FROM nodes WHERE workspace_id=$1`, `DELETE FROM workspaces WHERE id=$1`,
		} {
			_, _ = cleanupPool.Exec(context.Background(), statement, workspaceID)
		}
	}()

	signer := integrationCommandSigner()
	service := NewBackend(postgres.WrapPool(pool), signer)
	// stage seeds a command whose delivery was already recorded: its outbox row
	// is published, as after a successful send.
	stage := func(state string, mode agentv1.CommandDeliveryMode) (uuid.UUID, uuid.UUID, uuid.UUID) {
		t.Helper()
		operationID, commandID, messageID, idempotencyKey := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
		now := time.Now().UTC()
		envelope := agentv1.CommandEnvelope{ProtocolVersion: commandauth.ProtocolVersion, MessageId: messageID[:], CommandId: commandID[:], IdempotencyKey: idempotencyKey[:], NodeId: nodeID[:], Sequence: 1, IssuedAt: timestamppb.New(now), ExpiresAt: timestamppb.New(now.Add(time.Hour)), ExpectedRevision: 1, Traceparent: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01", ActorId: "operator", Reason: "privileged result trust", OperationId: operationID[:], Action: "service.reload", RequiredCapability: "ocserv.service.reload", DeliveryMode: mode, Payload: &agentv1.CommandEnvelope_ServiceReload{ServiceReload: &agentv1.ServiceReload{}}}
		if err := semanticpayload.PopulateV1(&envelope); err != nil {
			t.Fatal(err)
		}
		if err := signer.Authorize(&envelope); err != nil {
			t.Fatal(err)
		}
		encoded, err := proto.Marshal(&envelope)
		if err != nil {
			t.Fatal(err)
		}
		key := operationID.String()
		if _, err := pool.Exec(ctx, `INSERT INTO operations(id,workspace_id,node_id,command_id,state,request_id,trace_id,created_at,updated_at,idempotency_key,request_hash,expires_at) VALUES($1,$2,$3,$4,$5,$6,'0123456789abcdef0123456789abcdef',now(),now(),$6,$7,$8)`, operationID, workspaceID, nodeID, commandID, state, key, make([]byte, 32), now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO commands(id,operation_id,workspace_id,node_id,state,payload_type,envelope,idempotency_key,expected_version,sequence,traceparent,expires_at,created_at,updated_at) VALUES($1,$2,$3,$4,$5,'service_reload',$6,$7,1,1,$8,$9,$10,$10)`, commandID, operationID, workspaceID, nodeID, state, encoded, key, envelope.GetTraceparent(), now.Add(time.Hour), now); err != nil {
			t.Fatal(err)
		}
		outboxID := uuid.Must(uuid.NewV7())
		if _, err := pool.Exec(ctx, `INSERT INTO outbox_events(id,command_id,event_type,payload,available_at,published_at,created_at) VALUES($1,$2,'command.dispatch',$3,$4,$4,$4)`, outboxID, commandID, encoded, now); err != nil {
			t.Fatal(err)
		}
		return operationID, commandID, outboxID
	}
	storedEnvelope := func(commandID uuid.UUID) *agentv1.CommandEnvelope {
		t.Helper()
		var encoded []byte
		if err := pool.QueryRow(ctx, `SELECT envelope FROM commands WHERE id=$1`, commandID).Scan(&encoded); err != nil {
			t.Fatal(err)
		}
		var envelope agentv1.CommandEnvelope
		if err := proto.Unmarshal(encoded, &envelope); err != nil {
			t.Fatal(err)
		}
		return &envelope
	}
	ingest := func(envelope *agentv1.CommandEnvelope, result *agentv1.CommandResult) {
		t.Helper()
		payload, err := proto.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		eventID := uuid.Must(uuid.NewV7())
		if err := service.Ingest(ctx, &transportv1.TransportEvent{EventId: eventID[:], NodeId: nodeID[:], EndpointId: endpoint[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT, OccurredAt: timestamppb.Now(), Traceparent: envelope.GetTraceparent(), Payload: payload}); err != nil {
			t.Fatal(err)
		}
	}
	outbox := func(outboxID uuid.UUID) (bool, agentv1.CommandDeliveryMode) {
		t.Helper()
		var payload []byte
		var unpublished bool
		if err := pool.QueryRow(ctx, `SELECT payload,published_at IS NULL FROM outbox_events WHERE id=$1`, outboxID).Scan(&payload, &unpublished); err != nil {
			t.Fatal(err)
		}
		var envelope agentv1.CommandEnvelope
		if err := proto.Unmarshal(payload, &envelope); err != nil {
			t.Fatal(err)
		}
		return unpublished, envelope.GetDeliveryMode()
	}
	states := func(operationID uuid.UUID) (string, string) {
		t.Helper()
		var operation, command string
		if err := pool.QueryRow(ctx, `SELECT o.state,c.state FROM operations o JOIN commands c ON c.id=o.command_id WHERE o.id=$1`, operationID).Scan(&operation, &command); err != nil {
			t.Fatal(err)
		}
		return operation, command
	}
	base := func(envelope *agentv1.CommandEnvelope, state agentv1.CommandResultState) *agentv1.CommandResult {
		hash, err := semanticpayload.HashV1(envelope)
		if err != nil {
			t.Fatal(err)
		}
		now := timestamppb.Now()
		return &agentv1.CommandResult{CommandId: envelope.GetCommandId(), IdempotencyKey: envelope.GetIdempotencyKey(), PayloadSha256: hash[:], SemanticPayloadHashVersion: agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V1, State: state, AcceptedAt: now, CompletedAt: now}
	}

	for _, start := range []string{"dispatched", "unknown"} {
		t.Run("unsigned_rejected_from_"+start, func(t *testing.T) {
			operationID, commandID, outboxID := stage(start, agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_EXECUTE_OR_REPLAY)
			envelope := storedEnvelope(commandID)
			rejected := &agentv1.CommandResult{CommandId: envelope.GetCommandId(), IdempotencyKey: envelope.GetIdempotencyKey(), State: agentv1.CommandResultState_COMMAND_RESULT_STATE_REJECTED, ErrorCode: "capability_rejected", CompletedAt: timestamppb.Now()}
			ingest(envelope, rejected)
			if operation, command := states(operationID); operation != "unknown" || command != "unknown" {
				t.Fatalf("unsigned rejection settled the command: operation=%s command=%s", operation, command)
			}
			if unpublished, mode := outbox(outboxID); !unpublished || mode != agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY {
				t.Fatalf("reconciliation stopped after unsigned rejection: unpublished=%v mode=%s", unpublished, mode)
			}
			var status, reason, errorCode string
			if err := pool.QueryRow(ctx, `SELECT state,receipt_verification_status,error_code FROM agent_command_results WHERE command_id=$1`, commandID).Scan(&status, &reason, &errorCode); err != nil || status != "rejected" || reason != "missing" || errorCode != "capability_rejected" {
				t.Fatal("original rejection evidence was not retained", status, reason, errorCode, err)
			}
			// A repeated unsigned rejection of the now-Unknown command keeps reconciling.
			ingest(storedEnvelope(commandID), rejected)
			if operation, command := states(operationID); operation != "unknown" || command != "unknown" {
				t.Fatalf("repeated rejection settled the command: operation=%s command=%s", operation, command)
			}
			if unpublished, _ := outbox(outboxID); !unpublished {
				t.Fatal("repeated unsigned rejection completed the outbox")
			}
			// Genuine signed completion still settles the command and its outbox.
			envelope = storedEnvelope(commandID)
			success := base(envelope, agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED)
			if err := attestationtest.AttachProof(envelope, success, attestationKey, 1); err != nil {
				t.Fatal(err)
			}
			ingest(envelope, success)
			if operation, command := states(operationID); operation != "succeeded" || command != "succeeded" {
				t.Fatalf("signed result did not settle the command: operation=%s command=%s", operation, command)
			}
			if unpublished, _ := outbox(outboxID); unpublished {
				t.Fatal("signed result left the outbox open")
			}
		})
	}

	t.Run("unsigned_effect_absent_never_authorizes_retry", func(t *testing.T) {
		operationID, commandID, outboxID := stage("unknown", agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY)
		envelope := storedEnvelope(commandID)
		absent := base(envelope, agentv1.CommandResultState_COMMAND_RESULT_STATE_UNKNOWN)
		absent.ErrorCode, absent.Replayed = "effect_absent", true
		ingest(envelope, absent)
		if operation, command := states(operationID); operation != "unknown" || command != "unknown" {
			t.Fatalf("effect absence changed the state: operation=%s command=%s", operation, command)
		}
		if unpublished, mode := outbox(outboxID); unpublished || mode != agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY {
			t.Fatalf("unsigned effect absence scheduled a retry: unpublished=%v mode=%s", unpublished, mode)
		}
		if mode := storedEnvelope(commandID).GetDeliveryMode(); mode != agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY {
			t.Fatalf("stored command became executable again: %s", mode)
		}
		// The signed completion from the original execution still recovers it.
		success := base(envelope, agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED)
		if err := attestationtest.AttachProof(envelope, success, attestationKey, 2); err != nil {
			t.Fatal(err)
		}
		ingest(envelope, success)
		if operation, command := states(operationID); operation != "succeeded" || command != "succeeded" {
			t.Fatalf("signed recovery did not settle the command: operation=%s command=%s", operation, command)
		}
	})
}
