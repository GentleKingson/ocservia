package mysql

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"reflect"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/attestationtest"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/configplan"
	"github.com/GentleKingson/ocservia/control-plane/internal/localslice"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/privdattestation"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type configPlanCreator struct {
	service  *operations.Service
	contexts []context.Context
	requests []operations.CreateRequest
}

func (r *configPlanCreator) CreateSynthetic(ctx context.Context, request operations.CreateRequest) (operations.Operation, bool, error) {
	r.contexts = append(r.contexts, ctx)
	r.requests = append(r.requests, request)
	return r.service.CreateSynthetic(ctx, request)
}

// Keep the existing MySQL/MariaDB restricted-runtime fixture, but exercise the
// consumer, signed result ingress and independent approval, not only its Store.
func runConfigPlanServiceChain(t *testing.T, owner, backend *Backend) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := owner.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	id := func() uuid.UUID { return uuid.Must(uuid.NewV7()) }
	workspace, node, requester, approver, session, approverSession, credential := id(), id(), id(), id(), id(), id(), id()
	now := time.Now().UTC().Truncate(time.Microsecond)
	at, expires := fixtureTimestamp(t, now), fixtureTimestamp(t, now.Add(time.Hour))
	exec(`INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES(?,'config-port',?,?,?)`, UUIDBytes(workspace), workspace.String(), at, at)
	exec(`INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at)VALUES(?,?,'config-port','active',3,?,?)`, UUIDBytes(node), UUIDBytes(workspace), at, at)
	for _, actor := range []uuid.UUID{requester, approver} {
		exec(`INSERT INTO identities(id,issuer,subject,created_at,updated_at)VALUES(?,'config-port',?,?,?)`, UUIDBytes(actor), actor.String(), at, at)
		exec(`INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,created_at)VALUES(?,?,?,'PlatformAdmin','workspace',?)`, UUIDBytes(id()), UUIDBytes(actor), UUIDBytes(workspace), at)
	}
	for _, pair := range [][2]uuid.UUID{{session, requester}, {approverSession, approver}} {
		exec(`INSERT INTO auth_sessions(id,identity_id,expires_at,created_at)VALUES(?,?,?,?)`, UUIDBytes(pair[0]), UUIDBytes(pair[1]), expires, at)
	}
	endpoint := sha256.Sum256(node[:])
	private := ed25519.NewKeyFromSeed(endpoint[:])
	public := private.Public().(ed25519.PublicKey)
	exec(`INSERT INTO node_endpoint_keys(node_id,endpoint_id,state,bound_at)VALUES(?,?,'active',?)`, UUIDBytes(node), endpoint[:], at)
	exec(`INSERT INTO privd_attestation_enrollment_credentials(id,node_id,secret_sha256,controller_nonce,credential_context_sha256,expires_at,consumed_at,created_by_identity_id,created_by_session_id,created_at)VALUES(?,?,?,?,?,?,?,?,?,?)`, UUIDBytes(credential), UUIDBytes(node), endpoint[:], endpoint[:], endpoint[:], expires, at, UUIDBytes(requester), UUIDBytes(session), at)
	exec(`INSERT INTO node_privd_attestation_keys(node_id,key_id,algorithm,public_key,state,created_at,approved_at,activated_at,registration_credential_id)VALUES(?,?,'ed25519',?,'active',?,?,?,?)`, UUIDBytes(node), privdattestation.PublicKeyID(public), []byte(public), at, at, at, UUIDBytes(credential))
	for _, capability := range []string{"ocserv.config.plan", "ocserv.config.apply", "config.network", privdattestation.AttestationCapability} {
		exec(`INSERT INTO node_capabilities(node_id,capability,approved)VALUES(?,?,true)`, UUIDBytes(node), capability)
	}
	signer := commandauth.NewSignerFromSeed([32]byte{4})
	creator := &configPlanCreator{service: operations.NewBackend(backend, 50, signer)}
	service := configplan.NewBackend(backend, creator)
	request := configplan.CreateRequest{NodeID: node, Template: configplan.Template{Name: "port", Directives: []configplan.Directive{{Name: "tcp-port", Value: "${port}"}}}, NodeVariables: map[string]string{"port": "443"}, TTL: 10 * time.Minute, IdempotencyKey: "port-create", ActorID: "operator", ActorIdentityID: requester, ActorSessionID: session, RequestID: "port-create-request", Traceparent: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01", Reason: "review candidate"}
	plan, replay, err := service.Create(ctx, request)
	if err != nil || replay || plan.Validation != "pending" || plan.OperationID != plan.ID {
		t.Fatal("consumer Create", plan, replay, err)
	}
	candidate := []byte("# generated by ocservia config-plan/v1\ntcp-port = 443\n")
	hash := sha256.Sum256(candidate)
	want := operations.CreateRequest{NodeID: node, IdempotencyKey: request.IdempotencyKey, ExpectedVersion: 3, Kind: operations.ConfigPlan, Candidate: candidate, CandidateHash: hash[:], TTL: request.TTL, RequestID: request.RequestID, Traceparent: request.Traceparent, ActorID: request.ActorID, ActorIdentityID: requester, ActorSessionID: session, Action: "config.plan", Reason: request.Reason, PlanCapabilities: []string{"config.network", "ocserv.config.plan"}, PlanMetadata: &operations.ConfigPlanMetadata{TemplateName: "port", CandidateRedacted: string(candidate), Warnings: []string{"ocserv_version_unobserved"}, CreatedBy: requester}}
	if len(creator.requests) != 1 || creator.contexts[0] != ctx || !reflect.DeepEqual(creator.requests[0], want) {
		t.Fatal("Create port request", creator.requests)
	}
	if again, replay, err := service.Create(ctx, request); err != nil || !replay || again.ID != plan.ID {
		t.Fatal("consumer replay", again, replay, err)
	}
	changed := request
	changed.Reason = "changed intent"
	if _, _, err := service.Create(ctx, changed); !errors.Is(err, configplan.ErrIdempotency) {
		t.Fatal("consumer conflict", err)
	}
	applyRequest := configplan.ApplyRequest{PlanID: plan.ID, ApprovalID: id(), IdempotencyKey: "port-apply", ActorID: "operator", ActorIdentityID: requester, ActorSessionID: session, RequestID: "port-apply-request", Traceparent: request.Traceparent, Reason: "apply candidate"}
	if _, _, err := service.Apply(ctx, applyRequest); !errors.Is(err, configplan.ErrStaleRevision) || len(creator.requests) != 3 {
		t.Fatal("pending plan reached creator", err, len(creator.requests))
	}
	var encoded []byte
	if err := backend.QueryRow(ctx, `SELECT envelope FROM commands WHERE operation_id=?`, UUIDBytes(plan.OperationID)).Scan(&encoded); err != nil {
		t.Fatal(err)
	}
	var envelope agentv1.CommandEnvelope
	if err := proto.Unmarshal(encoded, &envelope); err != nil {
		t.Fatal(err)
	}
	currentHash := bytes.Repeat([]byte{0x42}, 32)
	resultBytes, err := proto.Marshal(&agentv1.ConfigPlanResult{CandidateHash: hash[:], CurrentHash: currentHash, DiffRedacted: "- <current configuration redacted>\n+ tcp-port = 443\n", CurrentUnchanged: true, StagingCleaned: true})
	if err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE commands SET state='dispatched' WHERE operation_id=?`, UUIDBytes(plan.OperationID))
	exec(`UPDATE operations SET state='dispatched' WHERE id=?`, UUIDBytes(plan.OperationID))
	result := &agentv1.CommandResult{CommandId: envelope.GetCommandId(), IdempotencyKey: envelope.GetIdempotencyKey(), PayloadSha256: envelope.GetSemanticPayloadSha256(), SemanticPayloadHashVersion: envelope.GetSemanticPayloadHashVersion(), State: agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED, Result: resultBytes, AcceptedAt: timestamppb.Now(), CompletedAt: timestamppb.Now()}
	if err := attestationtest.AttachProof(&envelope, result, private, 1); err != nil {
		t.Fatal(err)
	}
	resultBytes, err = proto.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	event := id()
	if err := localslice.NewBackend(backend, signer).Ingest(ctx, &transportv1.TransportEvent{EventId: event[:], NodeId: node[:], EndpointId: endpoint[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT, OccurredAt: timestamppb.Now(), Traceparent: request.Traceparent, Payload: resultBytes}); err != nil {
		t.Fatal("verified plan result", err)
	}
	if validated, err := service.Get(ctx, plan.ID); err != nil || validated.Validation != "valid" {
		t.Fatal("validated plan", validated, err)
	}
	approvalService := approvals.NewBackend(backend)
	pending, err := approvalService.Create(ctx, approvals.Request{WorkspaceID: workspace, RequesterID: requester, ResourceID: plan.ID, SessionID: session, Action: "config.apply", ResourceType: "config_plan", Reason: "review", TTL: time.Hour, RequestID: "port-approval", RequestHash: hash[:], RequestSummary: []byte(`{}`), AuthorityResources: []approvals.AuthorityResource{{WorkspaceID: workspace, Type: "workspace"}}})
	if err != nil {
		t.Fatal("request approval", err)
	}
	if _, err := approvalService.Approve(ctx, approvals.Decision{ApprovalID: pending.ID, ApproverID: approver, SessionID: approverSession, Reason: "reviewed", RequestID: "port-approve", ExpectedRequestHash: pending.RequestHash}); err != nil {
		t.Fatal("approve", err)
	}
	applyRequest.ApprovalID = pending.ID
	t.Run("apply-rollback", func(t *testing.T) {
		exec(`ALTER TABLE audit_events ADD CONSTRAINT pr04_config_apply_audit CHECK(action <> 'config.apply')`)
		t.Cleanup(func() {
			if _, err := owner.Exec(context.Background(), `ALTER TABLE audit_events DROP CONSTRAINT pr04_config_apply_audit`); err != nil {
				t.Error(err)
			}
		})
		if _, _, err := service.Apply(ctx, applyRequest); err == nil {
			t.Fatal("audit failure did not roll back Apply")
		}
		var status string
		if err := backend.QueryRow(ctx, `SELECT status FROM approval_requests WHERE id=?`, UUIDBytes(pending.ID)).Scan(&status); err != nil || status != "approved" {
			t.Fatal("failed Apply consumed approval", status, err)
		}
		var ops, commands, plans, applies, outbox int
		if err := backend.QueryRow(ctx, `SELECT (SELECT count(*) FROM operations WHERE node_id=?),(SELECT count(*) FROM commands WHERE node_id=?),(SELECT count(*) FROM config_plans WHERE node_id=?),(SELECT count(*) FROM config_apply_operations WHERE node_id=?),(SELECT count(*) FROM outbox_events b JOIN commands c ON c.id=b.command_id WHERE c.node_id=?)`, UUIDBytes(node), UUIDBytes(node), UUIDBytes(node), UUIDBytes(node), UUIDBytes(node)).Scan(&ops, &commands, &plans, &applies, &outbox); err != nil || ops != 1 || commands != 1 || plans != 1 || applies != 0 || outbox != 1 {
			t.Fatalf("failed Apply rows=%d/%d/%d/%d/%d: %v", ops, commands, plans, applies, outbox, err)
		}
	})
	op, replay, err := service.Apply(ctx, applyRequest)
	if err != nil || replay || op.State != "queued" {
		t.Fatal("consumer Apply", op, replay, err)
	}
	want = operations.CreateRequest{NodeID: node, IdempotencyKey: applyRequest.IdempotencyKey, ExpectedVersion: 3, Kind: operations.ConfigApply, Candidate: candidate, CandidateHash: hash[:], ExpectedCurrentHash: currentHash, DesiredRevision: 1, ApplyMetadata: &operations.ConfigApplyMetadata{PlanID: plan.ID}, ApprovalID: pending.ID, TTL: 15 * time.Minute, RequestID: applyRequest.RequestID, Traceparent: applyRequest.Traceparent, ActorID: applyRequest.ActorID, ActorIdentityID: requester, ActorSessionID: session, Action: "config.apply", Reason: applyRequest.Reason}
	if len(creator.requests) != 5 || creator.contexts[4] != ctx || !reflect.DeepEqual(creator.requests[4], want) {
		t.Fatal("Apply port request", creator.requests)
	}
	var status string
	var outbox, audit int
	if err := backend.QueryRow(ctx, `SELECT a.status,(SELECT count(*) FROM outbox_events b JOIN commands c ON c.id=b.command_id WHERE c.operation_id=x.operation_id),(SELECT count(*) FROM audit_events e WHERE e.resource_id=x.operation_id AND e.action='config.apply') FROM config_apply_operations x JOIN approval_requests a ON a.id=x.approval_id WHERE x.operation_id=?`, UUIDBytes(uuid.MustParse(op.ID))).Scan(&status, &outbox, &audit); err != nil || status != "consumed" || outbox != 1 || audit != 1 {
		t.Fatal("Apply committed association", status, outbox, audit, err)
	}
}
