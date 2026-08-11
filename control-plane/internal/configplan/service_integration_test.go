package configplan

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/localslice"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/semanticpayload"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestConfigPlanCreateReplayStaleAndTypedEnvelopeIntegration(t *testing.T) {
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
	ownerPool := pool
	if ownerURL := os.Getenv("OCSERV_TEST_OWNER_DATABASE_URL"); ownerURL != "" {
		ownerPool, err = pgxpool.New(ctx, ownerURL)
		if err != nil {
			t.Fatal(err)
		}
		defer ownerPool.Close()
	}
	workspaceID, nodeID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if _, err = pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES($1,'I15 test',$2,now(),now())`, workspaceID, "i15-"+workspaceID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at)VALUES($1,$2,$3,'active',1,now(),now())`, nodeID, workspaceID, "node-"+nodeID.String()); err != nil {
		t.Fatal(err)
	}
	endpointID := sha256.Sum256(append([]byte("ocservia/configplan-integration/"), nodeID[:]...))
	if _, err = pool.Exec(ctx, `INSERT INTO node_endpoint_keys(node_id,endpoint_id,state,bound_at) VALUES($1,$2,'active',now())`, nodeID, endpointID[:]); err != nil {
		t.Fatal(err)
	}
	for _, capability := range []string{"ocserv.config.plan", "ocserv.config.apply", "config.network", "config.limits", "config.runtime", "config.auth"} {
		if _, err = pool.Exec(ctx, `INSERT INTO node_capabilities(node_id,capability,approved)VALUES($1,$2,true)`, nodeID, capability); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		if err := cleanupConfigPlanIntegration(context.Background(), ownerPool, workspaceID); err != nil {
			t.Error(err)
		}
	}()

	var commandSeed [32]byte
	commandSeed[0] = 4
	commandSigner := commandauth.NewSignerFromSeed(commandSeed)
	service := New(pool, operations.NewWithSigner(pool, 50, commandSigner))
	request := CreateRequest{NodeID: nodeID, ExpectedRevision: 0, Template: Template{Name: "baseline", Directives: []Directive{
		{Name: "auth", Value: "plain[passwd=/etc/ocserv/ocpasswd]"}, {Name: "max-clients", Value: "128"},
		{Name: "socket-file", Value: "/run/ocserv.socket"}, {Name: "tcp-port", Value: "443"},
	}}, TTL: 15 * time.Minute, IdempotencyKey: "i15-plan", ActorID: "developer", RequestID: "request-i15", Traceparent: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01", Reason: "integration plan"}
	failing := request
	failing.IdempotencyKey = "i15-plan-atomic-failure"
	failing.ActorIdentityID = uuid.Must(uuid.NewV7())
	if _, _, err := service.Create(ctx, failing); err == nil {
		t.Fatal("missing created_by identity did not fail plan persistence")
	}
	var orphaned int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM operations WHERE workspace_id=$1 AND idempotency_key=$2`, workspaceID, failing.IdempotencyKey).Scan(&orphaned); err != nil || orphaned != 0 {
		t.Fatalf("atomic failure left %d operation rows: %v", orphaned, err)
	}
	plan, replayed, err := service.Create(ctx, request)
	if err != nil || replayed || plan.ExpectedRevision != 0 || plan.Validation != "pending" {
		t.Fatalf("create plan=%+v replayed=%v err=%v", plan, replayed, err)
	}
	second, replayed, err := service.Create(ctx, request)
	if err != nil || !replayed || second.ID != plan.ID {
		t.Fatalf("replay plan=%+v replayed=%v err=%v", second, replayed, err)
	}
	var envelopeBytes []byte
	if err := pool.QueryRow(ctx, `SELECT c.envelope FROM commands c WHERE c.operation_id=$1`, plan.OperationID).Scan(&envelopeBytes); err != nil {
		t.Fatal(err)
	}
	var envelope agentv1.CommandEnvelope
	if err := proto.Unmarshal(envelopeBytes, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.GetConfigPlan() == nil || len(envelope.GetConfigPlan().GetCandidateHash()) != 32 || envelope.GetExpectedRevision() != 1 || envelope.GetConfigPlan().GetExpectedRevision() != 0 {
		t.Fatalf("typed envelope=%v", &envelope)
	}
	commandID := uuid.Must(uuid.FromBytes(envelope.GetCommandId()))
	eventID := uuid.Must(uuid.NewV7())
	hash, _ := hex.DecodeString(plan.CandidateHash)
	var candidateRedacted string
	if err := pool.QueryRow(ctx, `SELECT candidate_redacted FROM config_plans WHERE id=$1`, plan.ID).Scan(&candidateRedacted); err != nil {
		t.Fatal(err)
	}
	currentHash := make([]byte, 32)
	for index := range currentHash {
		currentHash[index] = 0x42
	}
	resultBytes, err := proto.Marshal(&agentv1.ConfigPlanResult{CandidateHash: hash, DiffRedacted: safeDiff(candidateRedacted), CurrentUnchanged: true, StagingCleaned: true, CurrentHash: currentHash})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO transport_events(event_id,node_id,event_type,occurred_at,traceparent,payload)VALUES($1,$2,'command_result',now(),$3,''::bytea)`, eventID, nodeID, request.Traceparent); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO agent_command_results(event_id,command_id,idempotency_key,payload_sha256,semantic_payload_hash_version,state,result,accepted_at,completed_at,replayed,created_at)VALUES($1,$2,$3,$4,1,'succeeded',$5,now(),now(),false,now())`, eventID, commandID, uuid.Must(uuid.FromBytes(envelope.GetIdempotencyKey())), envelope.GetSemanticPayloadSha256(), resultBytes); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE commands SET state='succeeded',updated_at=now() WHERE id=$1`, commandID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE operations SET state='succeeded',updated_at=now() WHERE id=$1`, plan.OperationID); err != nil {
		t.Fatal(err)
	}
	validated, err := service.Get(ctx, plan.ID)
	if err != nil || validated.Validation != "valid" || !validated.CurrentUnchanged || !validated.StagingCleaned {
		t.Fatalf("validated plan=%+v err=%v", validated, err)
	}
	requesterID, approverID, approvalID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `INSERT INTO identities(id,issuer,subject,created_at,updated_at)VALUES($1::uuid,'test','i16-'||$1::text,now(),now()),($2::uuid,'test','i16-'||$2::text,now(),now())`, requesterID, approverID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO approval_requests(id,workspace_id,requester_id,action,resource_type,resource_id,reason,status,approver_id,approval_reason,expires_at,approved_at,created_at,request_hash,request_summary)
		VALUES($1,$2,$3,'config.apply','config_plan',$4,'reviewed','approved',$5,'approved',now()+interval '10 minutes',now(),now(),$6,'{}'::jsonb)`, approvalID, workspaceID, requesterID, plan.ID, approverID, hash); err != nil {
		t.Fatal(err)
	}
	apply, replayed, err := service.Apply(ctx, ApplyRequest{PlanID: plan.ID, ApprovalID: approvalID, IdempotencyKey: "i16-apply", ActorID: requesterID.String(), ActorIdentityID: requesterID, ActorSessionID: uuid.Must(uuid.NewV7()), RequestID: "request-i16", Traceparent: request.Traceparent, Reason: "apply approved plan"})
	if err != nil || replayed || apply.State != "queued" {
		t.Fatalf("apply=%+v replayed=%v err=%v", apply, replayed, err)
	}
	var applyEnvelopeBytes []byte
	var approvalStatus string
	if err := pool.QueryRow(ctx, `SELECT c.envelope,a.status FROM commands c JOIN config_apply_operations x ON x.operation_id=c.operation_id JOIN approval_requests a ON a.id=x.approval_id WHERE x.operation_id=$1`, apply.ID).Scan(&applyEnvelopeBytes, &approvalStatus); err != nil {
		t.Fatal(err)
	}
	var applyEnvelope agentv1.CommandEnvelope
	if proto.Unmarshal(applyEnvelopeBytes, &applyEnvelope) != nil || applyEnvelope.GetConfigApply() == nil || !bytes.Equal(applyEnvelope.GetConfigApply().GetCandidateHash(), hash) || !bytes.Equal(applyEnvelope.GetConfigApply().GetExpectedCurrentHash(), currentHash) || applyEnvelope.GetConfigApply().GetDesiredRevision() != 1 || approvalStatus != "consumed" {
		t.Fatalf("apply envelope/status invalid: %v %s", &applyEnvelope, approvalStatus)
	}
	applyOperationID := uuid.Must(uuid.Parse(apply.ID))
	applyCommandID := uuid.Must(uuid.FromBytes(applyEnvelope.GetCommandId()))
	if _, err := pool.Exec(ctx, `UPDATE operations SET state='dispatched' WHERE id=$1`, applyOperationID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE commands SET state='dispatched' WHERE id=$1`, applyCommandID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE config_apply_operations SET state='dispatched' WHERE operation_id=$1`, applyOperationID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE commands SET expires_at=created_at+interval '1 microsecond' WHERE id=$1`, applyCommandID); err != nil {
		t.Fatal(err)
	}
	if err := operations.NewWithSigner(pool, 50, commandSigner).Reap(ctx, 3); err != nil {
		t.Fatal(err)
	}
	var commandState, operationState, missingOutcomeState string
	var commandExpiry, operationExpiry time.Time
	var missingOutcomeEnvelopeBytes []byte
	var missingOutcomeUnpublished bool
	if err := pool.QueryRow(ctx, `SELECT c.state,o.state,x.state,c.expires_at,o.expires_at,c.envelope,b.published_at IS NULL FROM commands c JOIN operations o ON o.id=c.operation_id JOIN config_apply_operations x ON x.operation_id=o.id JOIN outbox_events b ON b.command_id=c.id WHERE c.id=$1`, applyCommandID).Scan(&commandState, &operationState, &missingOutcomeState, &commandExpiry, &operationExpiry, &missingOutcomeEnvelopeBytes, &missingOutcomeUnpublished); err != nil {
		t.Fatal(err)
	}
	var missingOutcomeEnvelope agentv1.CommandEnvelope
	if proto.Unmarshal(missingOutcomeEnvelopeBytes, &missingOutcomeEnvelope) != nil || commandState != "unknown" || operationState != "unknown" || missingOutcomeState != "unknown" || !commandExpiry.After(time.Now()) || !operationExpiry.Equal(commandExpiry) || missingOutcomeEnvelope.GetDeliveryMode() != agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY || bytes.Equal(missingOutcomeEnvelope.GetMessageId(), applyEnvelope.GetMessageId()) || !missingOutcomeUnpublished {
		t.Fatalf("missing outcome command=%s operation=%s apply=%s command_expiry=%s operation_expiry=%s envelope=%v unpublished=%v", commandState, operationState, missingOutcomeState, commandExpiry, operationExpiry, &missingOutcomeEnvelope, missingOutcomeUnpublished)
	}
	if _, err := pool.Exec(ctx, `UPDATE commands SET state='dispatched',expires_at=now()+interval '10 minutes' WHERE id=$1`, applyCommandID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE operations SET state='dispatched' WHERE id=$1`, applyOperationID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE config_apply_operations SET state='dispatched' WHERE operation_id=$1`, applyOperationID); err != nil {
		t.Fatal(err)
	}
	resultBytes, err = proto.Marshal(&agentv1.ConfigApplyResult{CandidateHash: hash, PreviousHash: currentHash, Healthy: false, FailedCritical: true, FailureCode: "rollback_failed"})
	if err != nil {
		t.Fatal(err)
	}
	payloadHash, err := semanticpayload.HashV2(&applyEnvelope)
	if err != nil {
		t.Fatal(err)
	}
	invalidResultBytes, err := proto.Marshal(&agentv1.CommandResult{CommandId: applyEnvelope.GetCommandId(), IdempotencyKey: applyEnvelope.GetIdempotencyKey(), PayloadSha256: payloadHash[:], SemanticPayloadHashVersion: agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V2, State: agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED, AcceptedAt: timestamppb.Now(), CompletedAt: timestamppb.Now()})
	if err != nil {
		t.Fatal(err)
	}
	invalidEventID := uuid.Must(uuid.NewV7())
	if err := localslice.NewWithSigner(pool, commandSigner).Ingest(ctx, &transportv1.TransportEvent{EventId: invalidEventID[:], NodeId: nodeID[:], EndpointId: endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT, OccurredAt: timestamppb.Now(), Traceparent: request.Traceparent, Payload: invalidResultBytes}); err != nil {
		t.Fatalf("ingest invalid configuration evidence for reconciliation: %v", err)
	}
	var preEvidenceState string
	var preEvidenceRevision int64
	if err := pool.QueryRow(ctx, `SELECT x.state,COALESCE(s.revision,0) FROM config_apply_operations x LEFT JOIN node_config_state s ON s.node_id=x.node_id WHERE x.operation_id=$1`, applyOperationID).Scan(&preEvidenceState, &preEvidenceRevision); err != nil || preEvidenceState != "unknown" || preEvidenceRevision != 0 {
		t.Fatalf("invalid evidence changed state=%s revision=%d err=%v", preEvidenceState, preEvidenceRevision, err)
	}
	var recoveryEnvelopeBytes []byte
	var recoveryUnpublished bool
	if err := pool.QueryRow(ctx, `SELECT c.envelope,o.published_at IS NULL FROM commands c JOIN outbox_events o ON o.command_id=c.id WHERE c.id=$1`, applyCommandID).Scan(&recoveryEnvelopeBytes, &recoveryUnpublished); err != nil {
		t.Fatal(err)
	}
	var recoveryEnvelope agentv1.CommandEnvelope
	if proto.Unmarshal(recoveryEnvelopeBytes, &recoveryEnvelope) != nil || recoveryEnvelope.GetDeliveryMode() != agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY || !recoveryUnpublished {
		t.Fatalf("invalid evidence did not schedule reconciliation: %v unpublished=%v", &recoveryEnvelope, recoveryUnpublished)
	}
	if _, err := pool.Exec(ctx, `UPDATE commands SET state='dispatched' WHERE id=$1`, applyCommandID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE operations SET state='dispatched' WHERE id=$1`, applyOperationID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE config_apply_operations SET state='dispatched' WHERE operation_id=$1`, applyOperationID); err != nil {
		t.Fatal(err)
	}
	commandResultBytes, err := proto.Marshal(&agentv1.CommandResult{CommandId: applyEnvelope.GetCommandId(), IdempotencyKey: applyEnvelope.GetIdempotencyKey(), PayloadSha256: payloadHash[:], SemanticPayloadHashVersion: agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V2, State: agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED, Result: resultBytes, AcceptedAt: timestamppb.Now(), CompletedAt: timestamppb.Now()})
	if err != nil {
		t.Fatal(err)
	}
	applyEventID := uuid.Must(uuid.NewV7())
	if err := localslice.NewWithSigner(pool, commandSigner).Ingest(ctx, &transportv1.TransportEvent{EventId: applyEventID[:], NodeId: nodeID[:], EndpointId: endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT, OccurredAt: timestamppb.Now(), Traceparent: request.Traceparent, Payload: commandResultBytes}); err != nil {
		t.Fatal(err)
	}
	var applyState, lockReason string
	var automationLocked bool
	var criticalAlerts int
	if err := pool.QueryRow(ctx, `SELECT x.state,s.automation_locked,COALESCE(s.automation_lock_reason,''),(SELECT count(*) FROM security_alerts WHERE workspace_id=x.workspace_id AND kind='config_apply.rollback_failed') FROM config_apply_operations x JOIN node_config_state s ON s.node_id=x.node_id WHERE x.operation_id=$1`, applyOperationID).Scan(&applyState, &automationLocked, &lockReason, &criticalAlerts); err != nil {
		t.Fatal(err)
	}
	metrics, err := operations.New(pool).Metrics(ctx)
	if err != nil || applyState != "failed_critical" || !automationLocked || lockReason != "config_apply_rollback_failed" || criticalAlerts != 1 || metrics.ConfigFailedCritical < 1 {
		t.Fatalf("critical apply state=%s locked=%v reason=%s alerts=%d metrics=%+v err=%v", applyState, automationLocked, lockReason, criticalAlerts, metrics, err)
	}
	publicApply, err := operations.New(pool).Get(ctx, applyOperationID)
	if err != nil || publicApply.ConfigApplyState != "failed_critical" || publicApply.ConfigApplyFailureCode != "rollback_failed" || publicApply.NodeID == nil || *publicApply.NodeID != nodeID.String() {
		t.Fatalf("public critical apply=%+v err=%v", publicApply, err)
	}
	replayedResultBytes, err := proto.Marshal(&agentv1.CommandResult{CommandId: applyEnvelope.GetCommandId(), IdempotencyKey: applyEnvelope.GetIdempotencyKey(), PayloadSha256: payloadHash[:], SemanticPayloadHashVersion: agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V2, State: agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED, Result: resultBytes, AcceptedAt: timestamppb.Now(), CompletedAt: timestamppb.Now(), Replayed: true})
	if err != nil {
		t.Fatal(err)
	}
	replayEventID := uuid.Must(uuid.NewV7())
	if err := localslice.NewWithSigner(pool, commandSigner).Ingest(ctx, &transportv1.TransportEvent{EventId: replayEventID[:], NodeId: nodeID[:], EndpointId: endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT, OccurredAt: timestamppb.Now(), Traceparent: request.Traceparent, Payload: replayedResultBytes}); err != nil {
		t.Fatalf("critical result replay failed: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM security_alerts WHERE workspace_id=$1 AND kind='config_apply.rollback_failed'`, workspaceID).Scan(&criticalAlerts); err != nil || criticalAlerts != 1 {
		t.Fatalf("critical replay alerts=%d err=%v", criticalAlerts, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE node_config_state SET revision=1,automation_locked=false,automation_lock_reason=NULL WHERE node_id=$1`, nodeID); err != nil {
		t.Fatal(err)
	}
	request.IdempotencyKey = "stale-plan"
	if _, _, err := service.Create(ctx, request); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("stale revision error=%v", err)
	}
}

func cleanupConfigPlanIntegration(ctx context.Context, pool *pgxpool.Pool, workspaceID uuid.UUID) error {
	if _, err := pool.Exec(ctx, `ALTER TABLE audit_events DISABLE TRIGGER audit_events_append_only`); err != nil {
		return err
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `ALTER TABLE audit_events ENABLE TRIGGER audit_events_append_only`)
	}()
	statements := []string{
		`DELETE FROM config_apply_operations WHERE workspace_id=$1`,
		`DELETE FROM config_plans WHERE workspace_id=$1`,
		`DELETE FROM node_config_state WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`,
		`DELETE FROM agent_command_results WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
		`DELETE FROM transport_events WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`,
		`DELETE FROM node_command_leases WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
		`DELETE FROM command_attempts WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
		`DELETE FROM outbox_events WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
		`DELETE FROM operation_events WHERE operation_id IN(SELECT id FROM operations WHERE workspace_id=$1)`,
		`DELETE FROM audit_events WHERE workspace_id=$1`,
		`DELETE FROM security_alerts WHERE workspace_id=$1`,
		`DELETE FROM approval_requests WHERE workspace_id=$1`,
		`DELETE FROM commands WHERE workspace_id=$1`,
		`DELETE FROM operations WHERE workspace_id=$1`,
		`DELETE FROM node_capabilities WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`,
		`DELETE FROM node_endpoint_keys WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`,
		`DELETE FROM nodes WHERE workspace_id=$1`,
		`DELETE FROM identities WHERE issuer='test' AND subject LIKE 'i16-%'`,
		`DELETE FROM workspaces WHERE id=$1`,
	}
	for _, statement := range statements {
		args := []any(nil)
		if strings.Contains(statement, "$1") {
			args = append(args, workspaceID)
		}
		if _, err := pool.Exec(ctx, statement, args...); err != nil {
			return err
		}
	}
	return nil
}
