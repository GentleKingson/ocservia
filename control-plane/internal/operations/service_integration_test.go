package operations

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
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

	// Crash after network send but before DB acknowledgement: lease expiry makes
	// the side-effect-free command eligible for at-least-once redelivery.
	time.Sleep(100 * time.Millisecond)
	if err := service.Reap(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	var retried Dispatch
	for range 2 {
		retry, err := service.Claim(context.Background(), uuid.Must(uuid.NewV7()), 8, time.Second)
		if err != nil || len(retry) != 1 {
			t.Fatalf("crash-window retry = %+v, %v", retry, err)
		}
		if retry[0].CommandID == first.CommandID {
			retried = retry[0]
			break
		}
		if err := service.MarkSent(context.Background(), retry[0]); err != nil {
			t.Fatal(err)
		}
	}
	if retried.CommandID == uuid.Nil {
		t.Fatal("lease-expired command was not redelivered")
	}
	if err := service.MarkSent(context.Background(), retried); err != nil {
		t.Fatal(err)
	}

	var attempts int
	var state string
	if err := pool.QueryRow(context.Background(), `SELECT count(*),min(command.state) FROM command_attempts AS attempt JOIN commands AS command ON command.id=attempt.command_id WHERE command.workspace_id=$1 AND command.id=$2 GROUP BY command.id`, workspaceID, first.CommandID).Scan(&attempts, &state); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || state != "dispatched" {
		t.Fatalf("attempts/state = %d/%s", attempts, state)
	}
	metrics, err := service.Metrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if metrics.Unpublished != metrics.Queued || metrics.Queued > 1 || metrics.Unknown != 0 {
		t.Fatalf("metrics after dispatch = %+v", metrics)
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
		_, err = pool.Exec(context.Background(), `INSERT INTO approval_requests(id,workspace_id,requester_id,action,resource_type,resource_id,reason,status,approver_id,approval_reason,expires_at,approved_at,created_at) VALUES($1,$2,$3,$4,'node',$5,'integration test','approved',$6,'independent approval',now()+interval '1 hour',now(),now())`, request.ApprovalID, workspaceID, request.ActorIdentityID, request.Action, request.NodeID, approverID)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func testRequest(nodeID uuid.UUID, key string, kind SyntheticKind, message string) CreateRequest {
	return CreateRequest{NodeID: nodeID, IdempotencyKey: key, ExpectedVersion: 1, Kind: kind, Message: message, TTL: time.Minute, RequestID: "request-" + key, Traceparent: testTraceparent}
}
