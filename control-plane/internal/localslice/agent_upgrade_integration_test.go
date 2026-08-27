package localslice

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/attestationtest"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/semanticpayload"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestAgentUpgradeScheduledAckStaysNonTerminalIntegration proves the core
// scheduling semantics: a succeeded command result carrying the bound
// AgentUpgradeScheduledResult acknowledgement only moves the operation to the
// non-terminal accepted state with a scheduling timestamp, and a mismatched
// acknowledgement can never claim success.
func TestAgentUpgradeScheduledAckStaysNonTerminalIntegration(t *testing.T) {
	databaseURL := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL is not set")
	}
	// The ingested acknowledgement rows are runtime-role append-only, so
	// only the owner role can remove them; the scripted rollback checks
	// assert no agent upgrade command history survives the suite.
	ownerURL := os.Getenv("OCSERV_TEST_OWNER_DATABASE_URL")
	if ownerURL == "" {
		t.Skip("OCSERV_TEST_OWNER_DATABASE_URL is not set")
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
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'upgrade ack',$2,now(),now())`, workspaceID, "upgrade-ack-"+workspaceID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at) VALUES($1,$2,'upgrade-node','active',1,now(),now())`, nodeID, workspaceID); err != nil {
		t.Fatal(err)
	}
	// The scheduling acknowledgement is a privileged privd effect, so the
	// test drives it with a real verified receipt.
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
			`DELETE FROM node_command_leases WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
			`DELETE FROM command_attempts WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
			`DELETE FROM outbox_events WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
			`DELETE FROM node_agent_upgrade_results WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`,
			`DELETE FROM agent_upgrade_operations WHERE workspace_id=$1`,
			`DELETE FROM operation_events WHERE operation_id IN(SELECT id FROM operations WHERE workspace_id=$1)`,
			`DELETE FROM commands WHERE workspace_id=$1`, `DELETE FROM operations WHERE workspace_id=$1`,
			`DELETE FROM audit_events WHERE workspace_id=$1`, `DELETE FROM node_endpoint_keys WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`,
			`DELETE FROM node_privd_attestation_keys WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`,
			`DELETE FROM nodes WHERE workspace_id=$1`, `DELETE FROM workspaces WHERE id=$1`,
		} {
			_, _ = cleanupPool.Exec(context.Background(), statement, workspaceID)
		}
	}()

	signer := integrationCommandSigner()
	target, digest := "2.0.0", bytes.Repeat([]byte{0x43}, 32)
	stageUpgrade := func(key string) (uuid.UUID, *agentv1.CommandEnvelope) {
		t.Helper()
		operationID, commandID, messageID, idempotencyKey := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
		now := time.Now().UTC()
		envelope := agentv1.CommandEnvelope{ProtocolVersion: commandauth.ProtocolVersion, MessageId: messageID[:], CommandId: commandID[:], IdempotencyKey: idempotencyKey[:], NodeId: nodeID[:], Sequence: 1, IssuedAt: timestamppb.New(now), ExpiresAt: timestamppb.New(now.Add(time.Hour)), ExpectedRevision: 1, Traceparent: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01", ActorId: "operator", Reason: "scheduled acknowledgement", OperationId: operationID[:], Action: "agent.upgrade", RequiredCapability: "ocserv.agent.upgrade.v1", DeliveryMode: agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_EXECUTE_OR_REPLAY, Payload: &agentv1.CommandEnvelope_AgentUpgrade{AgentUpgrade: &agentv1.AgentUpgrade{TargetVersion: target, PackageSha256: digest, Architecture: "amd64"}}}
		if err := semanticpayload.PopulateV1(&envelope); err != nil {
			t.Fatal(err)
		}
		if err := signer.Authorize(&envelope); err != nil {
			t.Fatal(err)
		}
		envelopeBytes, err := proto.Marshal(&envelope)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO operations(id,workspace_id,node_id,command_id,state,request_id,trace_id,created_at,updated_at,idempotency_key,request_hash,expires_at) VALUES($1,$2,$3,$4,'dispatched',$5,'0123456789abcdef0123456789abcdef',now(),now(),$6,$7,$8)`, operationID, workspaceID, nodeID, commandID, "request-"+key, key, make([]byte, 32), now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO commands(id,operation_id,workspace_id,node_id,state,payload_type,envelope,idempotency_key,expected_version,sequence,traceparent,expires_at,created_at,updated_at) VALUES($1,$2,$3,$4,'dispatched','agent_upgrade',$5,$6,1,1,$7,$8,$9,$9)`, commandID, operationID, workspaceID, nodeID, envelopeBytes, key, envelope.GetTraceparent(), now.Add(time.Hour), now); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO agent_upgrade_operations(operation_id,workspace_id,node_id,target_version,package_sha256,architecture,from_version,state,created_at,updated_at) VALUES($1,$2,$3,$4,$5,'amd64','1.2.0','queued',now(),now())`, operationID, workspaceID, nodeID, target, digest); err != nil {
			t.Fatal(err)
		}
		return operationID, &envelope
	}
	ingestAck := func(envelope *agentv1.CommandEnvelope, scheduled *agentv1.AgentUpgradeScheduledResult, effectSequence uint64) {
		t.Helper()
		payloadHash, err := semanticpayload.HashV1(envelope)
		if err != nil {
			t.Fatal(err)
		}
		resultBytes, err := proto.Marshal(scheduled)
		if err != nil {
			t.Fatal(err)
		}
		result := &agentv1.CommandResult{CommandId: envelope.GetCommandId(), IdempotencyKey: envelope.GetIdempotencyKey(), PayloadSha256: payloadHash[:], State: agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED, Result: resultBytes, AcceptedAt: timestamppb.Now(), CompletedAt: timestamppb.Now(), SemanticPayloadHashVersion: agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V1}
		if err := attestationtest.AttachProof(envelope, result, attestationKey, effectSequence); err != nil {
			t.Fatal(err)
		}
		payload, err := proto.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		eventID := uuid.Must(uuid.NewV7())
		if err := NewWithSigner(pool, signer).Ingest(ctx, &transportv1.TransportEvent{EventId: eventID[:], NodeId: nodeID[:], EndpointId: endpoint[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT, OccurredAt: timestamppb.Now(), Traceparent: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01", Payload: payload}); err != nil {
			t.Fatal(err)
		}
	}

	matched, matchedEnvelope := stageUpgrade("upgrade-ack-bound")
	ingestAck(matchedEnvelope, &agentv1.AgentUpgradeScheduledResult{OperationId: matched[:], TargetVersion: target, PackageSha256: digest}, 1)
	var operationState, commandState, projectionState string
	var scheduled bool
	if err := pool.QueryRow(ctx, `SELECT o.state,c.state,u.state,u.scheduled_at IS NOT NULL
		FROM operations o JOIN commands c ON c.operation_id=o.id JOIN agent_upgrade_operations u ON u.operation_id=o.id
		WHERE o.id=$1`, matched).Scan(&operationState, &commandState, &projectionState, &scheduled); err != nil {
		t.Fatal(err)
	}
	if operationState != "accepted" || commandState != "accepted" || projectionState != "accepted" || !scheduled {
		t.Fatalf("acknowledged upgrade operation/command/projection/scheduled = %q/%q/%q/%v, want accepted with a scheduling timestamp", operationState, commandState, projectionState, scheduled)
	}
	var completed, published any
	if err := pool.QueryRow(ctx, `SELECT o.completed_at,x.published_at FROM operations o LEFT JOIN outbox_events x ON x.command_id=o.command_id WHERE o.id=$1`, matched).Scan(&completed, &published); err != nil {
		t.Fatal(err)
	}
	if completed != nil || published != nil {
		t.Fatalf("acknowledged upgrade completed/published = %v/%v, both must stay unset", completed, published)
	}

	// An acknowledgement echoing a different release identity must not even
	// reach accepted: the outcome stays unknown for reconciliation.
	drifted, driftedEnvelope := stageUpgrade("upgrade-ack-drifted")
	ingestAck(driftedEnvelope, &agentv1.AgentUpgradeScheduledResult{OperationId: drifted[:], TargetVersion: "9.9.9", PackageSha256: digest}, 2)
	if err := pool.QueryRow(ctx, `SELECT o.state,u.state FROM operations o JOIN agent_upgrade_operations u ON u.operation_id=o.id WHERE o.id=$1`, drifted).Scan(&operationState, &projectionState); err != nil {
		t.Fatal(err)
	}
	if operationState != "unknown" || projectionState != "unknown" {
		t.Fatalf("drifted acknowledgement operation/projection = %q/%q, want unknown", operationState, projectionState)
	}
}
