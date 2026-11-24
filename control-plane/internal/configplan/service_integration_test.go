package configplan

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"os"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/configprofile"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/semanticpayload"
	"github.com/GentleKingson/ocservia/control-plane/internal/testfixture"
	"github.com/GentleKingson/ocservia/control-plane/internal/transportevents"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestConfigPlanCreateReplayStaleAndTypedEnvelopeIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	pool := testfixture.OpenRuntimePool(t)
	ownerPool := pool
	if os.Getenv("OCSERV_TEST_OWNER_DATABASE_URL") != "" {
		ownerPool = testfixture.OpenOwnerPool(t)
	}
	workspaceID, nodeID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	testutil.Exec(t, ctx, pool, `INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES($1,'Configuration plan test',$2,now(),now())`, workspaceID, "config-plan-"+workspaceID.String())
	testutil.Exec(t, ctx, pool, `INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at)VALUES($1,$2,$3,'active',1,now(),now())`, nodeID, workspaceID, "node-"+nodeID.String())
	testutil.Exec(t, ctx, pool, `INSERT INTO node_observed_snapshots(node_id,observed_at,boot_id,agent_instance_id,agent_version,ocserv_version,os_release,ocserv,system,path,last_heartbeat_at) VALUES($1,now(),'complete',$2,'0.7.0','1.2.4','test','{}','{}','{}',now())`, nodeID, uuid.Must(uuid.NewV7()))
	attestationKey, err := testfixture.InstallKey(ctx, pool, nodeID)
	testutil.NoError(t, err)
	endpointID := sha256.Sum256(append([]byte("ocservia/configplan-integration/"), nodeID[:]...))
	testutil.Exec(t, ctx, pool, `INSERT INTO node_endpoint_keys(node_id,endpoint_id,state,bound_at) VALUES($1,$2,'active',now())`, nodeID, endpointID[:])
	for _, capability := range []string{configprofile.PlanCapability, configprofile.ApplyCapability} {
		testutil.Exec(t, ctx, pool, `INSERT INTO node_capabilities(node_id,capability,approved)VALUES($1,$2,true)`, nodeID, capability)
	}
	defer func() {
		if err := cleanupConfigPlanIntegration(context.Background(), ownerPool, workspaceID); err != nil {
			t.Error(err)
		}
	}()

	var commandSeed [32]byte
	commandSeed[0] = 4
	commandSigner := commandauth.NewSignerFromSeed(commandSeed)
	creator := &recordingOperationCreator{delegate: operations.NewBackend(postgres.WrapPool(pool), 50, commandSigner)}
	service := NewBackend(postgres.WrapPool(pool), creator)
	renderInput, oldNode := completeRenderFixture()
	renderInput.Template.Directives[7].SecretRef.Key = strings.Replace(renderInput.Template.Directives[7].SecretRef.Key, oldNode.String(), nodeID.String(), 1)
	rendered, err := renderComplete(renderInput, nodeID, 0)
	testutil.NoError(t, err)
	secretRefID := renderInput.Template.Directives[7].SecretRef.ID
	if _, err := pool.Exec(ctx, `INSERT INTO secret_provider_refs(id,workspace_id,provider,key_path,version,state,created_at,updated_at)VALUES($1,$2,$3,$4,$5,'active',now(),now())`, secretRefID, workspaceID, configprofile.Provider, renderInput.Template.Directives[7].SecretRef.Key, renderInput.Template.Directives[7].SecretRef.Version); err != nil {
		t.Fatal(err)
	}
	requestTemplate := renderInput.Template
	for index := range requestTemplate.Directives {
		if requestTemplate.Directives[index].SecretRef != nil {
			requestTemplate.Directives[index].SecretRef = &SecretRef{ID: secretRefID}
		}
	}
	requesterID, requesterSessionID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `INSERT INTO identities(id,issuer,subject,created_at,updated_at)VALUES($1::uuid,'test','config-apply-'||$1::text,now(),now())`, requesterID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO auth_sessions(id,identity_id,expires_at,created_at)VALUES($1,$2,now()+interval '1 hour',now())`, requesterSessionID, requesterID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_admins(workspace_id,identity_id,created_at)VALUES($1,$2,now()-interval '1 minute')`, workspaceID, requesterID); err != nil {
		t.Fatal(err)
	}
	request := CreateRequest{NodeID: nodeID, ExpectedRevision: 0, Template: requestTemplate, TTL: 15 * time.Minute, IdempotencyKey: "config-plan-create", ActorID: requesterID.String(), ActorIdentityID: requesterID, ActorSessionID: requesterSessionID, RequestID: "config-plan-create-request", Traceparent: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01", Reason: "integration plan"}
	failing := request
	failing.IdempotencyKey = "config-plan-atomic-failure"
	t.Run("create-rollback", func(t *testing.T) {
		if _, err := ownerPool.Exec(ctx, `ALTER TABLE config_plans ADD CONSTRAINT test_config_plan_insert_failure CHECK (workspace_id <> '`+workspaceID.String()+`') NOT VALID`); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, err := ownerPool.Exec(context.Background(), `ALTER TABLE config_plans DROP CONSTRAINT test_config_plan_insert_failure`)
			testutil.NoError(t, err)
		})
		if _, _, err := service.Create(ctx, failing); err == nil || !strings.Contains(err.Error(), "test_config_plan_insert_failure") {
			t.Fatalf("plan persistence error = %v", err)
		}
	})
	var orphaned int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM operations WHERE workspace_id=$1 AND idempotency_key=$2`, workspaceID, failing.IdempotencyKey).Scan(&orphaned); err != nil || orphaned != 0 {
		t.Fatalf("atomic failure left %d operation rows: %v", orphaned, err)
	}
	for _, table := range []string{"commands", "config_plans", "audit_events"} {
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE workspace_id=$1`, workspaceID).Scan(&orphaned); err != nil || orphaned != 0 {
			t.Fatalf("atomic failure left %s rows=%d: %v", table, orphaned, err)
		}
	}
	if len(creator.requests) != 1 || creator.contexts[0] != ctx {
		t.Fatal("Create did not use injected creator once with original context")
	}
	creator.requests, creator.contexts = nil, nil
	creator.err = fmt.Errorf("creator: %w", operations.ErrBacklogExceeded)
	if _, replay, err := service.Create(ctx, request); err != creator.err || !errors.Is(err, operations.ErrBacklogExceeded) || replay {
		t.Fatal("downstream error changed", replay, err)
	}
	creator.err = nil
	plan, replayed, err := service.Create(ctx, request)
	if err != nil || replayed || plan.ExpectedRevision != 0 || plan.Validation != "pending" {
		t.Fatalf("create plan=%+v replayed=%v err=%v", plan, replayed, err)
	}
	second, replayed, err := service.Create(ctx, request)
	if err != nil || !replayed || second.ID != plan.ID {
		t.Fatalf("replay plan=%+v replayed=%v err=%v", second, replayed, err)
	}
	wantCreate := operations.CreateRequest{
		NodeID:          nodeID,
		IdempotencyKey:  request.IdempotencyKey,
		ExpectedVersion: 1,
		TTL:             request.TTL,
		RequestID:       request.RequestID,
		Traceparent:     request.Traceparent,
		ActorID:         request.ActorID,
		ActorIdentityID: requesterID,
		ActorSessionID:  requesterSessionID,
		Action:          "config.plan",
		Reason:          request.Reason,
		Payload: operations.ConfigPlanPayload{
			Candidate:     rendered,
			OcservVersion: "1.2.4",
			PlanMetadata:  &operations.ConfigPlanMetadata{TemplateName: "complete", CandidateRedacted: rendered.Redacted(), Warnings: []string{}, CreatedBy: requesterID},
		},
	}
	creator.assertLast(t, ctx, 3, wantCreate)
	changed := request
	changed.Reason = "different content under the same key"
	if _, _, err := service.Create(ctx, changed); !errors.Is(err, operations.ErrIdempotencyConflict) {
		t.Fatal("conflicting replay", err)
	}
	if _, _, err := service.Apply(ctx, ApplyRequest{PlanID: plan.ID, IdempotencyKey: "pending", Reason: "pending plan"}); !errors.Is(err, ErrStaleRevision) {
		t.Fatal("pending plan accepted", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE node_capabilities SET approved=false WHERE node_id=$1 AND capability=$2`, nodeID, configprofile.PlanCapability); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Create(ctx, request); !errors.Is(err, ErrCapability) {
		t.Fatal("missing capability accepted", err)
	}
	if len(creator.requests) != 5 {
		t.Fatal("operation transaction must check the capability")
	}
	if _, err := pool.Exec(ctx, `UPDATE node_capabilities SET approved=true WHERE node_id=$1 AND capability=$2`, nodeID, configprofile.PlanCapability); err != nil {
		t.Fatal(err)
	}
	var envelopeBytes []byte
	testutil.NoError(t, pool.QueryRow(ctx, `SELECT c.envelope FROM commands c WHERE c.operation_id=$1`, plan.OperationID).Scan(&envelopeBytes))
	var envelope agentv1.CommandEnvelope
	testutil.NoError(t, proto.Unmarshal(envelopeBytes, &envelope))
	if envelope.GetCompleteConfigPlan() == nil || len(envelope.GetCompleteConfigPlan().GetCandidateHash()) != 32 || envelope.GetExpectedRevision() != 1 || envelope.GetCompleteConfigPlan().GetCandidate().GetExpectedRevision() != 0 {
		t.Fatalf("typed envelope=%v", &envelope)
	}
	commandID := uuid.Must(uuid.FromBytes(envelope.GetCommandId()))
	eventID := uuid.Must(uuid.NewV7())
	hash, _ := hex.DecodeString(plan.CandidateHash)
	var candidateRedacted string
	testutil.NoError(t, pool.QueryRow(ctx, `SELECT candidate_redacted FROM config_plans WHERE id=$1`, plan.ID).Scan(&candidateRedacted))
	currentHash := make([]byte, 32)
	materializedHash := make([]byte, 32)
	for index := range currentHash {
		currentHash[index] = 0x42
		materializedHash[index] = 0x43
	}
	resultBytes, err := proto.Marshal(&agentv1.ConfigPlanResult{CandidateHash: hash, MaterializedHash: materializedHash, DiffRedacted: safeDiff(candidateRedacted), CurrentUnchanged: true, StagingCleaned: true, CurrentHash: currentHash})
	testutil.NoError(t, err)
	if _, err = pool.Exec(ctx, `UPDATE commands SET state='dispatched',updated_at=now() WHERE id=$1`, commandID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE operations SET state='dispatched',updated_at=now() WHERE id=$1`, plan.OperationID); err != nil {
		t.Fatal(err)
	}
	completedPlan := time.Now().UTC().Truncate(time.Microsecond)
	planResult := &agentv1.CommandResult{
		CommandId: envelope.GetCommandId(), IdempotencyKey: envelope.GetIdempotencyKey(),
		PayloadSha256: envelope.GetSemanticPayloadSha256(), SemanticPayloadHashVersion: envelope.GetSemanticPayloadHashVersion(),
		State: agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED, Result: resultBytes,
		AcceptedAt: timestamppb.New(completedPlan), CompletedAt: timestamppb.New(completedPlan),
	}
	testutil.NoError(t, testfixture.AttachProof(&envelope, planResult, attestationKey, 1))
	encodedPlanResult, err := proto.Marshal(planResult)
	testutil.NoError(t, err)
	if err := transportevents.NewBackend(postgres.WrapPool(pool), commandSigner).Ingest(ctx, &transportv1.TransportEvent{
		EventId: eventID[:], NodeId: nodeID[:], EndpointId: endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT,
		OccurredAt: timestamppb.New(completedPlan), Traceparent: request.Traceparent, Payload: encodedPlanResult,
	}); err != nil {
		t.Fatal(err)
	}
	validated, err := service.Get(ctx, plan.ID)
	if err != nil || validated.Validation != "valid" || !validated.CurrentUnchanged || !validated.StagingCleaned {
		t.Fatalf("validated plan=%+v err=%v", validated, err)
	}
	applyRequest := ApplyRequest{PlanID: plan.ID, IdempotencyKey: "config-plan-apply", ActorID: requesterID.String(), ActorIdentityID: requesterID, ActorSessionID: requesterSessionID, RequestID: "config-plan-apply-request", Traceparent: request.Traceparent, Reason: "apply validated plan"}
	creator.err = fmt.Errorf("creator: %w", operations.ErrStaleRevision)
	if _, replay, err := service.Apply(ctx, applyRequest); err != creator.err || !errors.Is(err, ErrStaleRevision) || replay {
		t.Fatal("Apply downstream error changed", replay, err)
	}
	creator.err = nil
	t.Run("apply-rollback", func(t *testing.T) {
		if _, err := ownerPool.Exec(ctx, `ALTER TABLE audit_events ADD CONSTRAINT test_config_apply_audit_failure CHECK (workspace_id <> '`+workspaceID.String()+`' OR action <> 'config.apply') NOT VALID`); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if _, err := ownerPool.Exec(context.Background(), `ALTER TABLE audit_events DROP CONSTRAINT test_config_apply_audit_failure`); err != nil {
				t.Error(err)
			}
		})
		if _, _, err := service.Apply(ctx, applyRequest); err == nil {
			t.Fatal("audit failure did not roll back Apply")
		}
		var operationsCount, commandsCount, plansCount, applyCount, outboxCount, auditCount int
		if err := pool.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM operations WHERE workspace_id=$1),
			(SELECT count(*) FROM commands WHERE workspace_id=$1),
			(SELECT count(*) FROM config_plans WHERE workspace_id=$1),
			(SELECT count(*) FROM config_apply_operations WHERE workspace_id=$1),
			(SELECT count(*) FROM outbox_events b JOIN commands c ON c.id=b.command_id WHERE c.workspace_id=$1),
			(SELECT count(*) FROM audit_events WHERE workspace_id=$1 AND action='config.apply')`, workspaceID).Scan(&operationsCount, &commandsCount, &plansCount, &applyCount, &outboxCount, &auditCount); err != nil || operationsCount != 1 || commandsCount != 1 || plansCount != 1 || applyCount != 0 || outboxCount != 1 || auditCount != 0 {
			t.Fatalf("rollback rows = %d/%d/%d/%d/%d/%d: %v", operationsCount, commandsCount, plansCount, applyCount, outboxCount, auditCount, err)
		}
	})
	apply, replayed, err := service.Apply(ctx, applyRequest)
	if err != nil || replayed || apply.State != "queued" {
		t.Fatalf("apply=%+v replayed=%v err=%v", apply, replayed, err)
	}
	expires := validated.ExpiresAt.UTC()
	creator.assertLast(t, ctx, 8, operations.CreateRequest{
		NodeID:          nodeID,
		IdempotencyKey:  applyRequest.IdempotencyKey,
		ExpectedVersion: 1,
		TTL:             15 * time.Minute,
		RequestID:       applyRequest.RequestID,
		Traceparent:     applyRequest.Traceparent,
		ActorID:         applyRequest.ActorID,
		ActorIdentityID: requesterID,
		ActorSessionID:  applyRequest.ActorSessionID,
		Action:          "config.apply",
		Reason:          applyRequest.Reason,
		Payload: operations.ConfigApplyPayload{
			Candidate:           rendered,
			ExpectedCurrentHash: currentHash,
			MaterializedHash:    materializedHash,
			DesiredRevision:     1,
			PlanExpiresAt:       timestamppb.New(expires),
			ApplyMetadata:       &operations.ConfigApplyMetadata{PlanID: plan.ID},
		},
	})
	var applyEnvelopeBytes []byte
	testutil.NoError(t, pool.QueryRow(ctx, `SELECT c.envelope FROM commands c JOIN config_apply_operations x ON x.operation_id=c.operation_id WHERE x.operation_id=$1`, apply.ID).Scan(&applyEnvelopeBytes))
	var applyEnvelope agentv1.CommandEnvelope
	if proto.Unmarshal(applyEnvelopeBytes, &applyEnvelope) != nil || applyEnvelope.GetCompleteConfigApply() == nil || !bytes.Equal(applyEnvelope.GetCompleteConfigApply().GetCandidateHash(), hash) || !bytes.Equal(applyEnvelope.GetCompleteConfigApply().GetExpectedCurrentHash(), currentHash) || applyEnvelope.GetCompleteConfigApply().GetDesiredRevision() != 1 {
		t.Fatalf("apply envelope invalid: %v", &applyEnvelope)
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
	testutil.NoError(t, operations.NewBackend(postgres.WrapPool(pool), 50, commandSigner).Reap(ctx, 3))
	var commandState, operationState, missingOutcomeState string
	var commandExpiry, operationExpiry time.Time
	var missingOutcomeEnvelopeBytes []byte
	var missingOutcomeUnpublished bool
	testutil.NoError(t, pool.QueryRow(ctx, `SELECT c.state,o.state,x.state,c.expires_at,o.expires_at,c.envelope,b.published_at IS NULL FROM commands c JOIN operations o ON o.id=c.operation_id JOIN config_apply_operations x ON x.operation_id=o.id JOIN outbox_events b ON b.command_id=c.id WHERE c.id=$1`, applyCommandID).Scan(&commandState, &operationState, &missingOutcomeState, &commandExpiry, &operationExpiry, &missingOutcomeEnvelopeBytes, &missingOutcomeUnpublished))
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
	testutil.NoError(t, err)
	payloadHash, err := semanticpayload.HashV2(&applyEnvelope)
	testutil.NoError(t, err)
	invalidResultBytes, err := proto.Marshal(&agentv1.CommandResult{CommandId: applyEnvelope.GetCommandId(), IdempotencyKey: applyEnvelope.GetIdempotencyKey(), PayloadSha256: payloadHash[:], SemanticPayloadHashVersion: agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V2, State: agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED, AcceptedAt: timestamppb.Now(), CompletedAt: timestamppb.Now()})
	testutil.NoError(t, err)
	invalidEventID := uuid.Must(uuid.NewV7())
	if err := transportevents.NewBackend(postgres.WrapPool(pool), commandSigner).Ingest(ctx, &transportv1.TransportEvent{EventId: invalidEventID[:], NodeId: nodeID[:], EndpointId: endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT, OccurredAt: timestamppb.Now(), Traceparent: request.Traceparent, Payload: invalidResultBytes}); err != nil {
		t.Fatalf("ingest invalid configuration evidence for reconciliation: %v", err)
	}
	var preEvidenceState string
	var preEvidenceRevision int64
	if err := pool.QueryRow(ctx, `SELECT x.state,COALESCE(s.revision,0) FROM config_apply_operations x LEFT JOIN node_config_state s ON s.node_id=x.node_id WHERE x.operation_id=$1`, applyOperationID).Scan(&preEvidenceState, &preEvidenceRevision); err != nil || preEvidenceState != "unknown" || preEvidenceRevision != 0 {
		t.Fatalf("invalid evidence changed state=%s revision=%d err=%v", preEvidenceState, preEvidenceRevision, err)
	}
	var recoveryEnvelopeBytes []byte
	var recoveryUnpublished bool
	testutil.NoError(t, pool.QueryRow(ctx, `SELECT c.envelope,o.published_at IS NULL FROM commands c JOIN outbox_events o ON o.command_id=c.id WHERE c.id=$1`, applyCommandID).Scan(&recoveryEnvelopeBytes, &recoveryUnpublished))
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
	validResult := &agentv1.CommandResult{CommandId: applyEnvelope.GetCommandId(), IdempotencyKey: applyEnvelope.GetIdempotencyKey(), PayloadSha256: payloadHash[:], SemanticPayloadHashVersion: agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V2, State: agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED, Result: resultBytes, AcceptedAt: timestamppb.Now(), CompletedAt: timestamppb.Now()}
	testutil.NoError(t, testfixture.AttachProof(&applyEnvelope, validResult, attestationKey, 2))
	commandResultBytes, err := proto.Marshal(validResult)
	testutil.NoError(t, err)
	applyEventID := uuid.Must(uuid.NewV7())
	testutil.NoError(t, transportevents.NewBackend(postgres.WrapPool(pool), commandSigner).Ingest(ctx, &transportv1.TransportEvent{EventId: applyEventID[:], NodeId: nodeID[:], EndpointId: endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT, OccurredAt: timestamppb.Now(), Traceparent: request.Traceparent, Payload: commandResultBytes}))
	var applyState, lockReason string
	var automationLocked bool
	var criticalAlerts int
	testutil.NoError(t, pool.QueryRow(ctx, `SELECT x.state,s.automation_locked,COALESCE(s.automation_lock_reason,''),(SELECT count(*) FROM security_alerts WHERE workspace_id=x.workspace_id AND kind='config_apply.rollback_failed') FROM config_apply_operations x JOIN node_config_state s ON s.node_id=x.node_id WHERE x.operation_id=$1`, applyOperationID).Scan(&applyState, &automationLocked, &lockReason, &criticalAlerts))
	metrics, err := operations.NewBackend(postgres.WrapPool(pool), 50, nil).Metrics(ctx)
	if err != nil || applyState != "failed_critical" || !automationLocked || lockReason != "config_apply_rollback_failed" || criticalAlerts != 1 || metrics.ConfigFailedCritical < 1 {
		t.Fatalf("critical apply state=%s locked=%v reason=%s alerts=%d metrics=%+v err=%v", applyState, automationLocked, lockReason, criticalAlerts, metrics, err)
	}
	publicApply, err := operations.NewBackend(postgres.WrapPool(pool), 50, nil).Get(ctx, applyOperationID)
	if err != nil || publicApply.ConfigApplyState != "failed_critical" || publicApply.ConfigApplyFailureCode != "rollback_failed" || publicApply.NodeID == nil || *publicApply.NodeID != nodeID.String() {
		t.Fatalf("public critical apply=%+v err=%v", publicApply, err)
	}
	replayedResultBytes, err := proto.Marshal(validResult)
	testutil.NoError(t, err)
	replayEventID := uuid.Must(uuid.NewV7())
	if err := transportevents.NewBackend(postgres.WrapPool(pool), commandSigner).Ingest(ctx, &transportv1.TransportEvent{EventId: replayEventID[:], NodeId: nodeID[:], EndpointId: endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT, OccurredAt: timestamppb.Now(), Traceparent: request.Traceparent, Payload: replayedResultBytes}); err != nil {
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
		`DELETE FROM secret_provider_refs WHERE workspace_id=$1`,
		`DELETE FROM node_config_state WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`,
		`DELETE FROM node_observed_snapshots WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`,
		`DELETE FROM agent_command_results WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
		`DELETE FROM transport_events WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`,
		`DELETE FROM node_command_leases WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
		`DELETE FROM command_attempts WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
		`DELETE FROM outbox_events WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
		`DELETE FROM operation_events WHERE operation_id IN(SELECT id FROM operations WHERE workspace_id=$1)`,
		`DELETE FROM audit_events WHERE workspace_id=$1`,
		`DELETE FROM security_alerts WHERE workspace_id=$1`,
		`DELETE FROM commands WHERE workspace_id=$1`,
		`DELETE FROM operations WHERE workspace_id=$1`,
		`DELETE FROM node_capabilities WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`,
		`DELETE FROM node_endpoint_keys WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`,
		`DELETE FROM nodes WHERE workspace_id=$1`,
		`DELETE FROM workspace_admins WHERE workspace_id=$1`,
		`DELETE FROM auth_sessions WHERE identity_id IN(SELECT id FROM identities WHERE issuer='test' AND subject LIKE 'config-apply-%')`,
		`DELETE FROM identities WHERE issuer='test' AND subject LIKE 'config-apply-%'`,
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
