package configplan

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/attestationtest"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/configprofile"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/localslice"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestCompleteConfigPlanTransactionAndPhysicalResultIntegration(t *testing.T) {
	if os.Getenv("OCSERV_TEST_DATABASE_URL") == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, os.Getenv("OCSERV_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	owner, err := pgxpool.New(ctx, os.Getenv("OCSERV_TEST_OWNER_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	workspace, node, requester, approver := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	exec(`INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES($1,'complete',$2,now(),now())`, workspace, "complete-"+workspace.String())
	exec(`INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at)VALUES($1,$2,'complete','active',1,now(),now())`, node, workspace)
	defer func() {
		if err := cleanupConfigPlanIntegration(context.Background(), owner, workspace); err != nil {
			t.Error(err)
		}
	}()
	exec(`INSERT INTO identities(id,issuer,subject,created_at,updated_at)VALUES($1,'test',$2,now(),now()),($3,'test',$4,now(),now())`, requester, "i16-"+requester.String(), approver, "i16-"+approver.String())
	exec(`INSERT INTO node_observed_snapshots(node_id,observed_at,boot_id,agent_instance_id,agent_version,ocserv_version,os_release,ocserv,system,path,last_heartbeat_at) VALUES($1,now(),'complete',$2,'0.7.0','1.2.4','test','{}','{}','{}',now())`, node, uuid.Must(uuid.NewV7()))
	for _, capability := range []string{configprofile.PlanCapability, configprofile.ApplyCapability} {
		exec(`INSERT INTO node_capabilities(node_id,capability,approved)VALUES($1,$2,true)`, node, capability)
	}
	key, err := attestationtest.InstallKey(ctx, pool, node)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := sha256.Sum256(node[:])
	exec(`INSERT INTO node_endpoint_keys(node_id,endpoint_id,state,bound_at)VALUES($1,$2,'active',now())`, node, endpoint[:])
	signer := commandauth.NewSignerFromSeed([32]byte{4})
	ops := operations.NewBackend(postgres.WrapPool(pool), 50, signer)
	service := NewBackend(postgres.WrapPool(pool), ops)
	input, oldNode := completeRenderFixture()
	input.Template.Directives[7].SecretRef.Key = strings.Replace(input.Template.Directives[7].SecretRef.Key, oldNode.String(), node.String(), 1)
	rendered, err := renderComplete(input, node, 0)
	if err != nil {
		t.Fatal(err)
	}
	trace := "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"
	planOperation, _, err := ops.CreateSynthetic(ctx, operations.CreateRequest{NodeID: node, IdempotencyKey: "complete-plan", ExpectedVersion: 1, Kind: operations.ConfigPlan, Candidate: rendered.Candidate, CompleteCandidate: rendered.CompleteCandidate, CandidateHash: rendered.Hash[:], PlanCapabilities: rendered.RequiredCapabilities, OcservVersion: "1.2.4", TTL: 15 * time.Minute, ActorID: requester.String(), ActorIdentityID: requester, Action: "config.plan", Reason: "complete plan", RequestID: "complete-plan", Traceparent: trace, PlanMetadata: &operations.ConfigPlanMetadata{TemplateName: "complete", CandidateRedacted: rendered.Redacted, Warnings: []string{}}})
	if err != nil {
		t.Fatal(err)
	}
	planID := uuid.MustParse(planOperation.ID)
	if _, err := service.ApprovalBinding(ctx, planID); !errors.Is(err, ErrApprovalNotReady) {
		t.Fatal("unverified plan approval", err)
	}
	envelope := func(operationID string) *agentv1.CommandEnvelope {
		t.Helper()
		var raw []byte
		if err := pool.QueryRow(ctx, `SELECT envelope FROM commands WHERE operation_id=$1`, operationID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var command agentv1.CommandEnvelope
		if proto.Unmarshal(raw, &command) != nil {
			t.Fatal("invalid envelope")
		}
		return &command
	}
	ingest := func(operationID string, result proto.Message) {
		t.Helper()
		command := envelope(operationID)
		payload, err := proto.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		exec(`UPDATE commands SET state='dispatched' WHERE operation_id=$1`, operationID)
		exec(`UPDATE operations SET state='dispatched' WHERE id=$1`, operationID)
		outcome := &agentv1.CommandResult{CommandId: command.CommandId, IdempotencyKey: command.IdempotencyKey, PayloadSha256: command.SemanticPayloadSha256, SemanticPayloadHashVersion: command.SemanticPayloadHashVersion, State: agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED, Result: payload, AcceptedAt: timestamppb.Now(), CompletedAt: timestamppb.Now()}
		if err := attestationtest.AttachProof(command, outcome, key, 1); err != nil {
			t.Fatal(err)
		}
		encoded, err := proto.Marshal(outcome)
		if err != nil {
			t.Fatal(err)
		}
		event := uuid.Must(uuid.NewV7())
		if err := localslice.NewBackend(postgres.WrapPool(pool), signer).Ingest(ctx, &transportv1.TransportEvent{EventId: event[:], NodeId: node[:], EndpointId: endpoint[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT, OccurredAt: timestamppb.Now(), Traceparent: trace, Payload: encoded}); err != nil {
			t.Fatal(err)
		}
	}
	if envelope(planOperation.ID).GetCompleteConfigPlan() == nil {
		t.Fatal("complete plan silently used legacy payload")
	}
	current, physical := bytes.Repeat([]byte{0x41}, 32), bytes.Repeat([]byte{0x42}, 32)
	ingest(planOperation.ID, &agentv1.ConfigPlanResult{CandidateHash: rendered.Hash[:], MaterializedHash: physical, CurrentHash: current, DiffRedacted: safeDiff(rendered.Redacted), CurrentUnchanged: true, StagingCleaned: true})
	binding, err := service.ApprovalBinding(ctx, planID)
	if err != nil {
		plan, readErr := service.Get(ctx, planID)
		t.Fatalf("binding: %v; plan=%+v read=%v", err, plan, readErr)
	}
	approval := uuid.Must(uuid.NewV7())
	exec(`INSERT INTO approval_requests(id,workspace_id,requester_id,action,resource_type,resource_id,reason,status,approver_id,approval_reason,expires_at,approved_at,created_at,request_hash,request_summary)VALUES($1,$2,$3,'config.apply','config_plan',$4,'reviewed','approved',$5,'reviewed',now()+interval '10 minutes',now(),now(),$6,$7::jsonb)`, approval, workspace, requester, planID, approver, binding.RequestHash, binding.RequestSummary)
	request := ApplyRequest{PlanID: planID, ApprovalID: approval, IdempotencyKey: "complete-apply", ActorID: requester.String(), ActorIdentityID: requester, ActorSessionID: uuid.Must(uuid.NewV7()), RequestID: "complete-apply", Traceparent: trace, Reason: "approved complete apply"}
	// Consuming the approval happens before capability checking, but a rejected
	// transaction must roll it back together with its command/outbox intent.
	exec(`UPDATE node_capabilities SET approved=false WHERE node_id=$1 AND capability=$2`, node, configprofile.ApplyCapability)
	if _, _, err := service.Apply(ctx, request); !errors.Is(err, operations.ErrCapabilityMissing) {
		t.Fatal("old node accepted complete apply", err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM approval_requests WHERE id=$1`, approval).Scan(&status); err != nil || status != "approved" {
		t.Fatal("failed transaction consumed approval", status, err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM operations WHERE workspace_id=$1 AND idempotency_key='complete-apply'`, workspace).Scan(&count); err != nil || count != 0 {
		t.Fatal("rejected transaction left intent", count, err)
	}
	exec(`UPDATE node_capabilities SET approved=true WHERE node_id=$1 AND capability=$2`, node, configprofile.ApplyCapability)
	applied, replay, err := service.Apply(ctx, request)
	if err != nil || replay {
		t.Fatal("apply", err)
	}
	repeated, replay, err := service.Apply(ctx, request)
	if err != nil || !replay || repeated.ID != applied.ID {
		t.Fatal("same-key replay", err)
	}
	command := envelope(applied.ID)
	payload := command.GetCompleteConfigApply()
	if payload == nil || !bytes.Equal(payload.MaterializedHash, physical) || !bytes.Equal(command.ApprovalRequestSha256, binding.RequestHash) || command.RequiredCapability != configprofile.ApplyCapability {
		t.Fatal("incomplete signed approval binding")
	}
	ingest(applied.ID, &agentv1.ConfigApplyResult{CandidateHash: rendered.Hash[:], PreviousHash: current, ObservedHash: physical, AppliedRevision: 1, Healthy: true})
	var revision int64
	var observed []byte
	if err := pool.QueryRow(ctx, `SELECT revision,candidate_hash FROM node_config_state WHERE node_id=$1`, node).Scan(&revision, &observed); err != nil || revision != 1 || !bytes.Equal(observed, rendered.Hash[:]) {
		t.Fatal("logical state projection after physical result", revision, observed, err)
	}
	request.IdempotencyKey = "reused-approval"
	if _, _, err := service.Apply(ctx, request); err == nil {
		t.Fatal("consumed approval expanded to another operation")
	}
}
