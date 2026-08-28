package operations

import (
	"bytes"
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/attestationtest"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

const testTraceparent = "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"

func TestTransactionalCreateIdempotencyAndTypedPayloadIntegration(t *testing.T) {
	service, pool, workspaceID, nodeID := integrationService(t)
	request := testRequest(nodeID, "same-key", SyntheticEcho, "hello")
	first, replayed, err := service.CreateSynthetic(context.Background(), request)
	if err != nil || replayed {
		t.Fatalf("first create = %+v, %v, %v", first, replayed, err)
	}
	second, replayed, err := service.CreateSynthetic(context.Background(), request)
	if err != nil || !replayed || second.ID != first.ID {
		t.Fatalf("replay = %+v, %v, %v", second, replayed, err)
	}

	var operationCount, commandCount, outboxCount, auditCount int
	var envelope []byte
	err = pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM operations WHERE workspace_id=$1),(SELECT count(*) FROM commands WHERE workspace_id=$1),(SELECT count(*) FROM outbox_events o JOIN commands c ON c.id=o.command_id WHERE c.workspace_id=$1),(SELECT count(*) FROM audit_events WHERE workspace_id=$1),(SELECT envelope FROM commands WHERE workspace_id=$1)`, workspaceID).Scan(&operationCount, &commandCount, &outboxCount, &auditCount, &envelope)
	if err != nil || operationCount != 1 || commandCount != 1 || outboxCount != 1 || auditCount != 1 {
		t.Fatalf("atomic rows = %d/%d/%d/%d, %v", operationCount, commandCount, outboxCount, auditCount, err)
	}
	var command agentv1.CommandEnvelope
	if err := proto.Unmarshal(envelope, &command); err != nil {
		t.Fatal(err)
	}
	if command.GetSyntheticEcho().GetMessage() != "hello" || command.GetExpectedRevision() != 1 {
		t.Fatalf("unexpected typed envelope: %v", &command)
	}

	conflict := request
	conflict.Message = "different"
	if _, _, err := service.CreateSynthetic(context.Background(), conflict); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflicting replay error = %v", err)
	}
	stale := testRequest(nodeID, "stale", SyntheticNoop, "")
	stale.ExpectedVersion = 2
	if _, _, err := service.CreateSynthetic(context.Background(), stale); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("stale revision error = %v", err)
	}
}

func TestAuditFailureRejectsBusinessWriteIntegration(t *testing.T) {
	ownerURL := os.Getenv("OCSERV_TEST_OWNER_DATABASE_URL")
	if ownerURL == "" {
		t.Skip("OCSERV_TEST_OWNER_DATABASE_URL is not set")
	}
	service, pool, workspaceID, nodeID := integrationService(t)
	ctx := context.Background()
	owner, err := pgxpool.New(ctx, ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if _, err := owner.Exec(ctx, `CREATE OR REPLACE FUNCTION i12_reject_audit() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'injected audit failure'; END;$$; CREATE TRIGGER i12_reject_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION i12_reject_audit()`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = owner.Exec(context.Background(), `DROP TRIGGER IF EXISTS i12_reject_audit ON audit_events; DROP FUNCTION IF EXISTS i12_reject_audit()`)
	}()
	if _, _, err := service.CreateSynthetic(ctx, testRequest(nodeID, "audit-failure", SyntheticNoop, "")); err == nil {
		t.Fatal("business write succeeded while audit failed")
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM operations WHERE workspace_id=$1`, workspaceID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("operations after audit failure = %d, %v", count, err)
	}
}

func TestIdempotencyKeyCannotReplayAcrossNodesIntegration(t *testing.T) {
	service, pool, workspaceID, nodeID := integrationService(t)
	otherNodeID := uuid.Must(uuid.NewV7())
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at)VALUES($1,$2,$3,'active',1,now(),now())`, otherNodeID, workspaceID, "node-"+otherNodeID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := attestationtest.InstallKey(ctx, pool, otherNodeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO node_capabilities(node_id,capability,approved) VALUES($1,'ocserv.service.reload',true),($2,'ocserv.service.reload',true)`, nodeID, otherNodeID); err != nil {
		t.Fatal(err)
	}
	request := controlledTestRequest(nodeID, "cross-node-key", ServiceReload, "service.reload", "", "", "")
	approveOperation(t, pool, workspaceID, &request)
	first, replayed, err := service.CreateSynthetic(ctx, request)
	if err != nil || replayed || first.NodeID == nil || *first.NodeID != nodeID.String() {
		t.Fatalf("first node operation = %+v, %v, %v", first, replayed, err)
	}
	request.NodeID = otherNodeID
	if _, _, err := service.CreateSynthetic(ctx, request); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("cross-node replay error = %v", err)
	}
}

func TestControlledOperationsRequireApprovedCapabilityAndObservedTargetIntegration(t *testing.T) {
	service, pool, workspaceID, nodeID := integrationService(t)
	bootID := uuid.NewString()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO node_observed_snapshots(node_id,observed_at,boot_id,agent_instance_id,agent_version,ocserv_version,os_release,ocserv,system,path,last_heartbeat_at)
		VALUES($1,now(),$2,$3,'test','1.4.2','test','{}','{}','{}',now())`,
		nodeID, bootID, uuid.Must(uuid.NewV7())); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO node_sessions(node_id,session_id,username,client_ip,connected_at,bytes_in,bytes_out,observed_at)
		VALUES($1,'42','alice','192.0.2.10',now(),0,0,now())`, nodeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO node_ip_bans(node_id,ip,seconds_remaining,observed_at)
		VALUES($1,'192.0.2.9',60,now())`, nodeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO node_capabilities(node_id,capability,approved) VALUES
		($1,'ocserv.session.disconnect',true),($1,'ocserv.session.terminate',true),
		($1,'ocserv.ip_ban.remove',true),($1,'ocserv.service.reload',true)`, nodeID); err != nil {
		t.Fatal(err)
	}

	requests := map[string]CreateRequest{
		"disconnect": controlledTestRequest(nodeID, "disconnect", SessionDisconnect, "session.disconnect", bootID, "42", ""),
		"terminate":  controlledTestRequest(nodeID, "terminate", SessionTerminate, "session.terminate", bootID, "42", ""),
		"unban":      controlledTestRequest(nodeID, "unban", IPBanRemove, "ip_ban.remove", "", "", "192.0.2.9"),
		"reload":     controlledTestRequest(nodeID, "reload", ServiceReload, "service.reload", "", "", ""),
	}
	reload := requests["reload"]
	approveOperation(t, pool, workspaceID, &reload)
	requests["reload"] = reload
	for name, request := range requests {
		t.Run(name, func(t *testing.T) {
			operation, replayed, err := service.CreateSynthetic(ctx, request)
			if err != nil || replayed || operation.State != "queued" {
				t.Fatalf("create controlled operation = %+v, %v, %v", operation, replayed, err)
			}
			replayedOperation, replayed, err := service.CreateSynthetic(ctx, request)
			if err != nil || !replayed || replayedOperation.ID != operation.ID {
				t.Fatalf("replay controlled operation = %+v, %v, %v", replayedOperation, replayed, err)
			}
		})
	}

	missing := controlledTestRequest(nodeID, "missing", SessionDisconnect, "session.disconnect", bootID, "43", "")
	if _, _, err := service.CreateSynthetic(ctx, missing); !errors.Is(err, ErrTargetNotObserved) {
		t.Fatalf("missing observed target error = %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE node_capabilities SET approved=false WHERE node_id=$1 AND capability='ocserv.service.reload'`, nodeID); err != nil {
		t.Fatal(err)
	}
	denied := controlledTestRequest(nodeID, "denied", ServiceReload, "service.reload", "", "", "")
	denied.ActorIdentityID, denied.ActorSessionID, denied.ApprovalID = uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if _, _, err := service.CreateSynthetic(ctx, denied); !errors.Is(err, ErrCapabilityMissing) {
		t.Fatalf("missing capability error = %v", err)
	}
}

func TestAgentUpgradeApprovalBindsExactReleaseIdentityIntegration(t *testing.T) {
	// The fixture stages an older observed agent version: the authoritative
	// creation fence reads it, and its cleanup drops the upgrade family rows
	// that restrict the shared operations cleanup.
	service, pool, workspaceID, nodeID := upgradeReconciliationFixture(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO node_capabilities(node_id,capability,approved) VALUES($1,'ocserv.agent.upgrade.v1',true)`, nodeID); err != nil {
		t.Fatal(err)
	}
	base := CreateRequest{NodeID: nodeID, IdempotencyKey: "upgrade-exact", ExpectedVersion: 1, Kind: AgentUpgrade, ActorID: "operator", Action: "agent.upgrade", Reason: "integration test", TargetVersion: "1.2.3", PackageSHA256: bytes.Repeat([]byte{0x43}, 32), Architecture: "amd64", TTL: time.Minute, RequestID: "request-upgrade", Traceparent: testTraceparent}
	approveOperation(t, pool, workspaceID, &base)
	operation, replayed, err := service.CreateSynthetic(ctx, base)
	if err != nil || replayed || operation.State != "queued" {
		t.Fatalf("create approved upgrade = %+v, %v, %v", operation, replayed, err)
	}
	if _, replayed, err := service.CreateSynthetic(ctx, base); err != nil || !replayed {
		t.Fatalf("replay approved upgrade = %v, %v", replayed, err)
	}

	// The approval was consumed for exactly this release identity, so a
	// drifted version, digest, or architecture must fail closed.
	for _, tc := range []struct {
		name   string
		mutate func(*CreateRequest)
	}{
		{"target version", func(r *CreateRequest) {
			r.TargetVersion = "1.2.4"
			r.IdempotencyKey = "upgrade-drift-version"
			r.RequestID = "request-drift-version"
		}},
		{"package digest", func(r *CreateRequest) {
			r.PackageSHA256[0] ^= 0xff
			r.IdempotencyKey = "upgrade-drift-digest"
			r.RequestID = "request-drift-digest"
		}},
		{"architecture", func(r *CreateRequest) {
			r.Architecture = "arm64"
			r.IdempotencyKey = "upgrade-drift-arch"
			r.RequestID = "request-drift-arch"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := base
			request.PackageSHA256 = append([]byte(nil), base.PackageSHA256...)
			tc.mutate(&request)
			if _, _, err := service.CreateSynthetic(ctx, request); !errors.Is(err, approvals.ErrNotReady) {
				t.Fatalf("drifted %s error = %v", tc.name, err)
			}
		})
	}

	// An action-level approval never satisfies the release-identity binding.
	generic := base
	generic.IdempotencyKey, generic.RequestID = "upgrade-generic", "request-upgrade-generic"
	generic.ActorIdentityID, generic.ActorSessionID, generic.ApprovalID = uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	approverID := uuid.Must(uuid.NewV7())
	genericHash, genericSummary := approvals.GenericBinding("agent.upgrade", "node", nodeID)
	if _, err := pool.Exec(ctx, `INSERT INTO identities(id,issuer,subject,created_at,updated_at) VALUES($1,'test',$2,now(),now()),($3,'test',$4,now(),now())`, generic.ActorIdentityID, "requester-"+generic.ActorIdentityID.String(), approverID, "approver-"+approverID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO auth_sessions(id,identity_id,expires_at,created_at) VALUES($1,$2,now()+interval '1 hour',now())`, generic.ActorSessionID, generic.ActorIdentityID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO approval_requests(id,workspace_id,requester_id,action,resource_type,resource_id,reason,status,approver_id,approval_reason,expires_at,approved_at,created_at,request_hash,request_summary) VALUES($1,$2,$3,'agent.upgrade','node',$4,'integration test','approved',$5,'independent approval',now()+interval '1 hour',now(),now(),$6,$7)`, generic.ApprovalID, workspaceID, generic.ActorIdentityID, nodeID, approverID, genericHash, genericSummary); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.CreateSynthetic(ctx, generic); !errors.Is(err, approvals.ErrNotReady) {
		t.Fatalf("action-level approval error = %v", err)
	}
}

func TestAgentUpgradeAuthoritativeVersionFenceIntegration(t *testing.T) {
	service, pool, workspaceID, nodeID := integrationService(t)
	ctx := context.Background()
	observeUpgradeNode(t, pool, nodeID, "2.1.0", 0)
	if _, err := pool.Exec(ctx, `INSERT INTO node_capabilities(node_id,capability,approved) VALUES($1,'ocserv.agent.upgrade.v1',true)`, nodeID); err != nil {
		t.Fatal(err)
	}
	request := CreateRequest{NodeID: nodeID, IdempotencyKey: "upgrade-authoritative-fence", ExpectedVersion: 1, Kind: AgentUpgrade, ActorID: "operator", Action: "agent.upgrade", Reason: "integration test", TargetVersion: "2.0.0", PackageSHA256: bytes.Repeat([]byte{0x43}, 32), Architecture: "amd64", TTL: time.Minute, RequestID: "request-authoritative-fence", Traceparent: testTraceparent}
	approveOperation(t, pool, workspaceID, &request)
	if _, _, err := service.CreateSynthetic(ctx, request); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("target older than authoritative observed version error = %v", err)
	}
}

func TestConcurrentWorkersNodeLeaseAndCrashWindowsIntegration(t *testing.T) {
	service, pool, workspaceID, nodeID := integrationService(t)
	for _, key := range []string{"one", "two"} {
		if _, _, err := service.CreateSynthetic(context.Background(), testRequest(nodeID, key, SyntheticNoop, "")); err != nil {
			t.Fatal(err)
		}
	}
	workers := []uuid.UUID{uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())}
	results := make(chan []Dispatch, 2)
	var group sync.WaitGroup
	for _, worker := range workers {
		group.Add(1)
		go func(workerID uuid.UUID) {
			defer group.Done()
			jobs, err := service.Claim(context.Background(), workerID, 8, 80*time.Millisecond)
			if err != nil {
				t.Error(err)
			}
			results <- jobs
		}(worker)
	}
	group.Wait()
	close(results)
	claimed := 0
	var first Dispatch
	for jobs := range results {
		claimed += len(jobs)
		if len(jobs) > 0 {
			first = jobs[0]
		}
	}
	if claimed != 1 {
		t.Fatalf("concurrent claims = %d, want one per node", claimed)
	}

	// A crash around the network write has an ambiguous outcome. Lease expiry
	// must schedule journal reconciliation before any effect-capable retry.
	time.Sleep(100 * time.Millisecond)
	if err := service.Reap(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	retried, err := service.Claim(context.Background(), uuid.Must(uuid.NewV7()), 8, time.Second)
	if err != nil || len(retried) != 1 || retried[0].CommandID != first.CommandID {
		t.Fatalf("crash-window reconciliation = %+v, %v", retried, err)
	}
	var recovered agentv1.CommandEnvelope
	if err := proto.Unmarshal(retried[0].Envelope, &recovered); err != nil {
		t.Fatal(err)
	}
	if recovered.GetDeliveryMode() != agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY {
		t.Fatalf("crash-window delivery mode = %s", recovered.GetDeliveryMode())
	}
	if err := service.MarkSent(context.Background(), retried[0]); err != nil {
		t.Fatal(err)
	}

	var attempts int
	var state string
	if err := pool.QueryRow(context.Background(), `SELECT count(*),min(command.state) FROM command_attempts AS attempt JOIN commands AS command ON command.id=attempt.command_id WHERE command.workspace_id=$1 AND command.id=$2 GROUP BY command.id`, workspaceID, first.CommandID).Scan(&attempts, &state); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || state != "unknown" {
		t.Fatalf("attempts/state = %d/%s", attempts, state)
	}
	metrics, err := service.Metrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if metrics.Unpublished != 1 || metrics.Queued != 1 || metrics.Unknown != 1 {
		t.Fatalf("metrics after dispatch = %+v", metrics)
	}
}

// TestClaimedCommandExpiryDoesNotLeakNodeLeaseIntegration reproduces the
// worker-crash window where a claim dies before SendCommand and the command
// TTL lapses while the lease is still unreconciled. Expiry must leave the
// claimed command to lease reconciliation; otherwise the orphaned lease
// blocks every later dispatch for that node.
func TestClaimedCommandExpiryDoesNotLeakNodeLeaseIntegration(t *testing.T) {
	service, pool, _, nodeID := integrationService(t)
	ctx := context.Background()
	operation, replayed, err := service.CreateSynthetic(ctx, testRequest(nodeID, "crash-window-expiry", SyntheticNoop, ""))
	if err != nil || replayed || operation.CommandID == nil {
		t.Fatalf("create crash-window command = %+v, replayed=%v, err=%v", operation, replayed, err)
	}
	commandID := uuid.MustParse(*operation.CommandID)
	crashed, err := service.Claim(ctx, uuid.Must(uuid.NewV7()), 1, time.Minute)
	if err != nil || len(crashed) != 1 || crashed[0].CommandID != commandID {
		t.Fatalf("claim crash-window command = %d jobs, err=%v", len(crashed), err)
	}

	// The command TTL lapses first while the dead worker still holds the lease.
	if _, err := pool.Exec(ctx, `UPDATE commands SET expires_at=clock_timestamp() WHERE id=$1`, commandID); err != nil {
		t.Fatal(err)
	}
	if err := service.Expire(ctx); err != nil {
		t.Fatalf("expire while claimed: %v", err)
	}
	var state, attemptState string
	var published, locked bool
	var leases int
	if err := pool.QueryRow(ctx, `SELECT command.state,outbox.published_at IS NOT NULL,outbox.locked_by IS NOT NULL,
		(SELECT count(*) FROM node_command_leases WHERE command_id=command.id),
		(SELECT state FROM command_attempts WHERE id=$2)
		FROM commands AS command JOIN outbox_events AS outbox ON outbox.command_id=command.id
		WHERE command.id=$1`, commandID, crashed[0].AttemptID).
		Scan(&state, &published, &locked, &leases, &attemptState); err != nil {
		t.Fatal(err)
	}
	if state != "queued" || published || !locked || leases != 1 || attemptState != "sending" {
		t.Fatalf("expiry stole the claimed crash window: state=%s published=%v locked=%v leases=%d attempt=%s",
			state, published, locked, leases, attemptState)
	}

	// Lease expiry must reconcile the ambiguous attempt and release the node.
	if _, err := pool.Exec(ctx, `UPDATE node_command_leases SET leased_until=clock_timestamp()-interval '1 second' WHERE command_id=$1`, commandID); err != nil {
		t.Fatal(err)
	}
	if err := service.Reap(ctx, 3); err != nil {
		t.Fatalf("reap expired crash-window lease: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT command.state,
		(SELECT count(*) FROM node_command_leases WHERE command_id=command.id),
		(SELECT state FROM command_attempts WHERE id=$2)
		FROM commands AS command WHERE command.id=$1`, commandID, crashed[0].AttemptID).
		Scan(&state, &leases, &attemptState); err != nil {
		t.Fatal(err)
	}
	if state != "unknown" || leases != 0 || attemptState != "unknown" {
		t.Fatalf("crash-window reconciliation = state=%s leases=%d attempt=%s", state, leases, attemptState)
	}

	// The reconciled command dispatches again, and a later API command on the
	// same node still obtains claims and sent attempts instead of starving.
	recovery, err := service.Claim(ctx, uuid.Must(uuid.NewV7()), 1, time.Minute)
	if err != nil || len(recovery) != 1 || recovery[0].CommandID != commandID {
		t.Fatalf("claim reconcile-only recovery = %d jobs, err=%v", len(recovery), err)
	}
	if err := service.MarkSent(ctx, recovery[0]); err != nil {
		t.Fatalf("mark recovery sent: %v", err)
	}
	later, replayed, err := service.CreateSynthetic(ctx, testRequest(nodeID, "post-crash-window", SyntheticNoop, ""))
	if err != nil || replayed || later.CommandID == nil {
		t.Fatalf("create later command = %+v, replayed=%v, err=%v", later, replayed, err)
	}
	laterJobs, err := service.Claim(ctx, uuid.Must(uuid.NewV7()), 1, time.Minute)
	if err != nil || len(laterJobs) != 1 || laterJobs[0].CommandID.String() != *later.CommandID {
		t.Fatalf("later command claim = %d jobs, err=%v", len(laterJobs), err)
	}
	if err := service.MarkSent(ctx, laterJobs[0]); err != nil {
		t.Fatalf("mark later command sent: %v", err)
	}
	assertSentCommandProjection(t, pool, uuid.MustParse(*later.CommandID), uuid.MustParse(later.ID), laterJobs[0], "dispatched", "dispatched", true)
}

func TestSentCommandWithoutResultEntersAndContinuesReconciliationIntegration(t *testing.T) {
	service, pool, _, nodeID := integrationService(t)
	ctx := context.Background()
	operation, replayed, err := service.CreateSynthetic(ctx, testRequest(nodeID, "sent-result-missing", SyntheticNoop, ""))
	if err != nil || replayed || operation.CommandID == nil {
		t.Fatalf("create missing-result command = %+v, replayed=%v, err=%v", operation, replayed, err)
	}
	operationID := uuid.MustParse(operation.ID)
	commandID := uuid.MustParse(*operation.CommandID)

	jobs, err := service.Claim(ctx, uuid.Must(uuid.NewV7()), 1, time.Minute)
	if err != nil || len(jobs) != 1 || jobs[0].CommandID != commandID {
		t.Fatalf("claim normal command = %+v, err=%v", jobs, err)
	}
	var normal agentv1.CommandEnvelope
	if err := proto.Unmarshal(jobs[0].Envelope, &normal); err != nil {
		t.Fatal(err)
	}
	if normal.GetDeliveryMode() != agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_EXECUTE_OR_REPLAY {
		t.Fatalf("normal delivery mode = %s", normal.GetDeliveryMode())
	}
	if err := service.MarkSentWithEnvelope(ctx, jobs[0], jobs[0].Envelope); err != nil {
		t.Fatalf("mark normal command sent: %v", err)
	}
	assertSentCommandProjection(t, pool, commandID, operationID, jobs[0], "dispatched", "dispatched", true)
	if err := service.Reap(ctx, 3); err != nil {
		t.Fatalf("reap fresh sent command: %v", err)
	}
	assertSentCommandProjection(t, pool, commandID, operationID, jobs[0], "dispatched", "dispatched", true)

	// Move only PostgreSQL's dispatch-attempt clock. The production scan uses
	// the same clock, so this exercises both sides of the exact timeout boundary
	// without sleeping.
	if _, err := pool.Exec(ctx, `UPDATE command_attempts
		SET finished_at=clock_timestamp()-$2::interval
		WHERE id=$1 AND state='sent'`, jobs[0].AttemptID, (commandResultResponseTimeout - time.Second).String()); err != nil {
		t.Fatal(err)
	}
	if err := service.Reap(ctx, 3); err != nil {
		t.Fatalf("reap sent command before response timeout: %v", err)
	}
	assertSentCommandProjection(t, pool, commandID, operationID, jobs[0], "dispatched", "dispatched", true)

	if _, err := pool.Exec(ctx, `UPDATE command_attempts
		SET finished_at=clock_timestamp()-$2::interval
		WHERE id=$1 AND state='sent'`, jobs[0].AttemptID, (commandResultResponseTimeout + time.Second).String()); err != nil {
		t.Fatal(err)
	}
	if err := service.Reap(ctx, 3); err != nil {
		t.Fatalf("reap sent command with no result: %v", err)
	}

	var commandState, operationState string
	var published, locked, leased bool
	var recoveredBytes, outboxBytes []byte
	var recoveredExpiry time.Time
	var unknownEvents int
	if err := pool.QueryRow(ctx, `SELECT command.state,operation.state,
		outbox.published_at IS NOT NULL,outbox.locked_by IS NOT NULL,
		EXISTS(SELECT 1 FROM node_command_leases AS lease WHERE lease.command_id=command.id),
		command.envelope,outbox.payload,command.expires_at,
		(SELECT count(*) FROM operation_events AS event WHERE event.operation_id=operation.id AND event.state='unknown')
		FROM commands AS command
		JOIN operations AS operation ON operation.id=command.operation_id
		JOIN outbox_events AS outbox ON outbox.command_id=command.id
		WHERE command.id=$1`, commandID).Scan(
		&commandState, &operationState, &published, &locked, &leased,
		&recoveredBytes, &outboxBytes, &recoveredExpiry, &unknownEvents,
	); err != nil {
		t.Fatal(err)
	}
	if commandState != "unknown" || operationState != "unknown" || published || locked || leased || unknownEvents != 1 {
		t.Fatalf("initial reconciliation state = %s/%s published=%v locked=%v leased=%v unknown_events=%d",
			commandState, operationState, published, locked, leased, unknownEvents)
	}
	if !bytes.Equal(recoveredBytes, outboxBytes) {
		t.Fatal("initial reconciliation command and outbox envelopes differ")
	}
	var recovered agentv1.CommandEnvelope
	if err := proto.Unmarshal(recoveredBytes, &recovered); err != nil {
		t.Fatal(err)
	}
	assertReconciliationEnvelope(t, &normal, &recovered, commandID, operationID, nodeID)
	if !recovered.GetExpiresAt().AsTime().Equal(recoveredExpiry) || !recoveredExpiry.After(time.Now()) {
		t.Fatalf("initial reconciliation expiry = %v/%v", recovered.GetExpiresAt(), recoveredExpiry)
	}

	recoveryJobs, err := service.Claim(ctx, uuid.Must(uuid.NewV7()), 1, time.Minute)
	if err != nil || len(recoveryJobs) != 1 || recoveryJobs[0].CommandID != commandID {
		t.Fatalf("claim reconcile-only command = %+v, err=%v", recoveryJobs, err)
	}
	var sentRecovery agentv1.CommandEnvelope
	if err := proto.Unmarshal(recoveryJobs[0].Envelope, &sentRecovery); err != nil {
		t.Fatal(err)
	}
	if err := service.MarkSentWithEnvelope(ctx, recoveryJobs[0], recoveryJobs[0].Envelope); err != nil {
		t.Fatalf("mark reconcile-only command sent: %v", err)
	}
	assertSentCommandProjection(t, pool, commandID, operationID, recoveryJobs[0], "unknown", "unknown", true)

	if _, err := pool.Exec(ctx, `UPDATE command_attempts
		SET finished_at=clock_timestamp()-$2::interval
		WHERE id=$1 AND state='sent'`, recoveryJobs[0].AttemptID, (commandResultResponseTimeout + time.Second).String()); err != nil {
		t.Fatal(err)
	}
	if err := service.Reap(ctx, 3); err != nil {
		t.Fatalf("continue stale reconcile-only command: %v", err)
	}

	var continuedBytes, continuedOutboxBytes []byte
	if err := pool.QueryRow(ctx, `SELECT command.envelope,outbox.payload,outbox.published_at IS NOT NULL,
		(SELECT count(*) FROM operation_events AS event WHERE event.operation_id=command.operation_id AND event.state='unknown')
		FROM commands AS command JOIN outbox_events AS outbox ON outbox.command_id=command.id
		WHERE command.id=$1`, commandID).Scan(&continuedBytes, &continuedOutboxBytes, &published, &unknownEvents); err != nil {
		t.Fatal(err)
	}
	if published || unknownEvents != 1 || !bytes.Equal(continuedBytes, continuedOutboxBytes) {
		t.Fatalf("continued reconciliation published=%v unknown_events=%d envelopes_equal=%v",
			published, unknownEvents, bytes.Equal(continuedBytes, continuedOutboxBytes))
	}
	var continued agentv1.CommandEnvelope
	if err := proto.Unmarshal(continuedBytes, &continued); err != nil {
		t.Fatal(err)
	}
	assertReconciliationEnvelope(t, &normal, &continued, commandID, operationID, nodeID)
	if bytes.Equal(sentRecovery.GetMessageId(), continued.GetMessageId()) {
		t.Fatal("reconciliation continuation reused the prior message identity")
	}
	if !continued.GetExpiresAt().AsTime().Equal(sentRecovery.GetExpiresAt().AsTime()) {
		t.Fatalf("reconciliation continuation extended expiry from %v to %v", sentRecovery.GetExpiresAt(), continued.GetExpiresAt())
	}
}

func TestMissingResultReapSkipsOutboxLockedByTerminalResultIntegration(t *testing.T) {
	service, pool, _, nodeID := integrationService(t)
	ctx := context.Background()
	operation, replayed, err := service.CreateSynthetic(ctx, testRequest(nodeID, "sent-result-race", SyntheticNoop, ""))
	if err != nil || replayed || operation.CommandID == nil {
		t.Fatalf("create result-race command = %+v, replayed=%v, err=%v", operation, replayed, err)
	}
	operationID := uuid.MustParse(operation.ID)
	commandID := uuid.MustParse(*operation.CommandID)
	jobs, err := service.Claim(ctx, uuid.Must(uuid.NewV7()), 1, time.Minute)
	if err != nil || len(jobs) != 1 || jobs[0].CommandID != commandID {
		t.Fatalf("claim result-race command = %+v, err=%v", jobs, err)
	}
	if err := service.MarkSentWithEnvelope(ctx, jobs[0], jobs[0].Envelope); err != nil {
		t.Fatalf("mark result-race command sent: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE command_attempts
		SET finished_at=clock_timestamp()-$2::interval
		WHERE id=$1 AND state='sent'`, jobs[0].AttemptID, (commandResultResponseTimeout + time.Second).String()); err != nil {
		t.Fatal(err)
	}

	// Result ingestion takes the outbox lock before advancing the command and
	// operation. Hold that exact commit window while Reap scans with SKIP LOCKED.
	resultTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resultTx.Rollback(context.Background()) }()
	var outboxID uuid.UUID
	if err := resultTx.QueryRow(ctx, `SELECT id FROM outbox_events WHERE command_id=$1 FOR UPDATE`, commandID).Scan(&outboxID); err != nil {
		t.Fatal(err)
	}
	if _, err := resultTx.Exec(ctx, `UPDATE commands SET state='succeeded',updated_at=clock_timestamp() WHERE id=$1 AND state='dispatched'`, commandID); err != nil {
		t.Fatal(err)
	}
	if _, err := resultTx.Exec(ctx, `UPDATE operations SET state='succeeded',version=version+1,updated_at=clock_timestamp(),completed_at=clock_timestamp() WHERE id=$1 AND state='dispatched'`, operationID); err != nil {
		t.Fatal(err)
	}

	reapCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := service.Reap(reapCtx, 3); err != nil {
		t.Fatalf("reap while terminal result holds outbox: %v", err)
	}
	var visibleCommandState, visibleOperationState string
	var published bool
	if err := pool.QueryRow(ctx, `SELECT command.state,operation.state FROM commands AS command
		JOIN operations AS operation ON operation.id=command.operation_id WHERE command.id=$1`, commandID).
		Scan(&visibleCommandState, &visibleOperationState); err != nil {
		t.Fatal(err)
	}
	if visibleCommandState != "dispatched" || visibleOperationState != "dispatched" {
		t.Fatalf("Reap changed the row hidden behind the result lock: %s/%s", visibleCommandState, visibleOperationState)
	}
	if err := resultTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT command.state,operation.state,outbox.published_at IS NOT NULL
		FROM commands AS command JOIN operations AS operation ON operation.id=command.operation_id
		JOIN outbox_events AS outbox ON outbox.command_id=command.id WHERE command.id=$1`, commandID).
		Scan(&visibleCommandState, &visibleOperationState, &published); err != nil {
		t.Fatal(err)
	}
	if visibleCommandState != "succeeded" || visibleOperationState != "succeeded" || !published {
		t.Fatalf("terminal result did not win: %s/%s published=%v", visibleCommandState, visibleOperationState, published)
	}
}

func assertSentCommandProjection(t *testing.T, pool *pgxpool.Pool, commandID, operationID uuid.UUID, dispatch Dispatch, commandWant, operationWant string, publishedWant bool) {
	t.Helper()
	var commandState, operationState, attemptState string
	var published bool
	var leaseCount int
	if err := pool.QueryRow(context.Background(), `SELECT command.state,operation.state,outbox.published_at IS NOT NULL,
		(SELECT state FROM command_attempts WHERE id=$3),
		(SELECT count(*) FROM node_command_leases WHERE lease_token=$4)
		FROM commands AS command JOIN operations AS operation ON operation.id=command.operation_id
		JOIN outbox_events AS outbox ON outbox.command_id=command.id
		WHERE command.id=$1 AND operation.id=$2`, commandID, operationID, dispatch.AttemptID, dispatch.LeaseToken).
		Scan(&commandState, &operationState, &published, &attemptState, &leaseCount); err != nil {
		t.Fatal(err)
	}
	if commandState != commandWant || operationState != operationWant || published != publishedWant || attemptState != "sent" || leaseCount != 0 {
		t.Fatalf("sent projection = %s/%s published=%v attempt=%s leases=%d, want %s/%s published=%v sent/0",
			commandState, operationState, published, attemptState, leaseCount, commandWant, operationWant, publishedWant)
	}
}

func assertReconciliationEnvelope(t *testing.T, original, reconciled *agentv1.CommandEnvelope, commandID, operationID, nodeID uuid.UUID) {
	t.Helper()
	if reconciled.GetDeliveryMode() != agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY ||
		reconciled.GetConnectionFence() != nil || reconciled.GetFenceBinding() != nil {
		t.Fatalf("reconciliation delivery metadata is invalid: %v", reconciled)
	}
	if !bytes.Equal(reconciled.GetCommandId(), commandID[:]) ||
		!bytes.Equal(reconciled.GetOperationId(), operationID[:]) ||
		!bytes.Equal(reconciled.GetNodeId(), nodeID[:]) ||
		!bytes.Equal(reconciled.GetIdempotencyKey(), original.GetIdempotencyKey()) ||
		!bytes.Equal(reconciled.GetSemanticPayloadSha256(), original.GetSemanticPayloadSha256()) ||
		reconciled.GetSyntheticNoop() == nil {
		t.Fatal("reconciliation changed the logical command identity or payload")
	}
	if bytes.Equal(reconciled.GetMessageId(), original.GetMessageId()) {
		t.Fatal("reconciliation reused the normal dispatch message identity")
	}
}

func TestBacklogRecoverySupersededExpiryAndUnknownIntegration(t *testing.T) {
	service, pool, workspaceID, nodeID := integrationService(t)
	old, _, err := service.CreateSynthetic(context.Background(), testRequest(nodeID, "old", SyntheticEcho, "old"))
	if err != nil {
		t.Fatal(err)
	}
	replacement := testRequest(nodeID, "replacement", SyntheticEcho, "new")
	replacement.SupersedePending = true
	if _, _, err := service.CreateSynthetic(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	var oldState string
	if err := pool.QueryRow(context.Background(), `SELECT state FROM operations WHERE id=$1`, old.ID).Scan(&oldState); err != nil || oldState != "superseded" {
		t.Fatalf("old state=%s err=%v", oldState, err)
	}

	jobs, err := service.Claim(context.Background(), uuid.Must(uuid.NewV7()), 10, 30*time.Millisecond)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("claim=%d err=%v", len(jobs), err)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		time.Sleep(40 * time.Millisecond)
		if err := service.Reap(context.Background(), 3); err != nil {
			t.Fatal(err)
		}
		if attempt < 3 {
			jobs, err = service.Claim(context.Background(), uuid.Must(uuid.NewV7()), 10, 30*time.Millisecond)
			if err != nil || len(jobs) != 1 {
				t.Fatalf("retry %d=%d err=%v", attempt, len(jobs), err)
			}
		}
	}
	var unknown, published bool
	if err := pool.QueryRow(context.Background(), `SELECT command.state='unknown',outbox.published_at IS NOT NULL FROM commands command JOIN outbox_events outbox ON outbox.command_id=command.id WHERE command.workspace_id=$1 AND command.id=$2`, workspaceID, jobs[0].CommandID).Scan(&unknown, &published); err != nil {
		t.Fatal(err)
	}
	if !unknown || !published {
		t.Fatalf("unknown stop = %v/%v", unknown, published)
	}

	expiring := testRequest(nodeID, "expires", SyntheticNoop, "")
	expiring.TTL = time.Second
	if _, _, err := service.CreateSynthetic(context.Background(), expiring); err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(context.Background(), `UPDATE commands SET created_at=now()-interval '2 seconds',expires_at=now()-interval '1 second' WHERE workspace_id=$1 AND idempotency_key='expires'`, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Expire(context.Background()); err != nil {
		t.Fatal(err)
	}
	var expired string
	if err := pool.QueryRow(context.Background(), `SELECT state FROM commands WHERE workspace_id=$1 AND idempotency_key='expires'`, workspaceID).Scan(&expired); err != nil || expired != "expired" {
		t.Fatalf("expired=%s err=%v", expired, err)
	}
}

func integrationService(t *testing.T) (*Service, *pgxpool.Pool, uuid.UUID, uuid.UUID) {
	t.Helper()
	databaseURL := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	workspaceID, nodeID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES($1,'I09 test',$2,now(),now())`, workspaceID, "i09-"+workspaceID.String()); err != nil {
		pool.Close()
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at)VALUES($1,$2,$3,'active',1,now(),now())`, nodeID, workspaceID, "node-"+nodeID.String()); err != nil {
		pool.Close()
		t.Fatal(err)
	}
	if _, err := attestationtest.InstallKey(ctx, pool, nodeID); err != nil {
		pool.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		for _, statement := range []string{
			`DELETE FROM agent_command_results WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
			`DELETE FROM node_command_leases WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
			`DELETE FROM command_attempts WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
			`DELETE FROM outbox_events WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
			`DELETE FROM commands WHERE workspace_id=$1`, `DELETE FROM operations WHERE workspace_id=$1`,
			`DELETE FROM audit_events WHERE workspace_id=$1`, `DELETE FROM node_ip_bans WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`,
			`DELETE FROM node_sessions WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`,
			`DELETE FROM node_observed_snapshots WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`,
			`DELETE FROM node_capabilities WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`,
			`DELETE FROM nodes WHERE workspace_id=$1`,
			`DELETE FROM workspaces WHERE id=$1`,
		} {
			_, _ = pool.Exec(cleanupCtx, statement, workspaceID)
		}
		pool.Close()
	})
	return NewWithSigner(pool, 50, testCommandSigner(t)), pool, workspaceID, nodeID
}

func controlledTestRequest(nodeID uuid.UUID, key string, kind SyntheticKind, action, bootID, sessionID, ip string) CreateRequest {
	return CreateRequest{NodeID: nodeID, IdempotencyKey: key, ExpectedVersion: 1, Kind: kind, BootID: bootID, SessionID: sessionID, IP: ip, ActorID: "operator", Action: action, Reason: "integration test", TTL: time.Minute, RequestID: "request-" + key, Traceparent: testTraceparent}
}

func approveOperation(t *testing.T, pool *pgxpool.Pool, workspaceID uuid.UUID, request *CreateRequest) {
	t.Helper()
	request.ActorIdentityID, request.ActorSessionID, request.ApprovalID = uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	approverID := uuid.Must(uuid.NewV7())
	_, err := pool.Exec(context.Background(), `INSERT INTO identities(id,issuer,subject,created_at,updated_at) VALUES($1,'test',$2,now(),now()),($3,'test',$4,now(),now())`, request.ActorIdentityID, "requester-"+request.ActorIdentityID.String(), approverID, "approver-"+approverID.String())
	if err == nil {
		_, err = pool.Exec(context.Background(), `INSERT INTO auth_sessions(id,identity_id,expires_at,created_at) VALUES($1,$2,now()+interval '1 hour',now())`, request.ActorSessionID, request.ActorIdentityID)
	}
	if err == nil {
		var approvalHash, approvalSummary []byte
		if request.Kind == AgentUpgrade {
			approvalHash, approvalSummary = approvals.AgentUpgradeBinding(request.NodeID, request.TargetVersion, request.PackageSHA256, request.Architecture)
		} else {
			approvalHash, approvalSummary = approvals.GenericBinding(request.Action, "node", request.NodeID)
		}
		_, err = pool.Exec(context.Background(), `INSERT INTO approval_requests(id,workspace_id,requester_id,action,resource_type,resource_id,reason,status,approver_id,approval_reason,expires_at,approved_at,created_at,request_hash,request_summary) VALUES($1,$2,$3,$4,'node',$5,'integration test','approved',$6,'independent approval',now()+interval '1 hour',now(),now(),$7,$8)`, request.ApprovalID, workspaceID, request.ActorIdentityID, request.Action, request.NodeID, approverID, approvalHash, approvalSummary)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func testRequest(nodeID uuid.UUID, key string, kind SyntheticKind, message string) CreateRequest {
	return CreateRequest{NodeID: nodeID, IdempotencyKey: key, ExpectedVersion: 1, Kind: kind, Message: message, TTL: time.Minute, RequestID: "request-" + key, Traceparent: testTraceparent}
}
