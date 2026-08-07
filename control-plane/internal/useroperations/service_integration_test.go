package useroperations

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/userstate"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestQuotaExpirySchedulerBatchAndUsageIntegration(t *testing.T) {
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
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES($1,'I14 test',$2,now(),now())`, workspaceID, "i14-"+workspaceID.String()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := cleanupIntegration(ctx, ownerPool, workspaceID); err != nil {
			t.Error(err)
		}
	}()
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at)VALUES($1,$2,$3,'active',1,now(),now())`, nodeID, workspaceID, "node-"+nodeID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO node_capabilities(node_id,capability,approved)VALUES($1,'ocserv.users.write',true)`, nodeID); err != nil {
		t.Fatal(err)
	}
	requesterID, approverID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `INSERT INTO identities(id,issuer,subject,created_at,updated_at)VALUES($1,'integration',$2,now(),now()),($3,'integration',$4,now(),now())`, requesterID, requesterID.String(), approverID, approverID.String()); err != nil {
		t.Fatal(err)
	}
	fingerprint := make([]byte, 32)
	for _, username := range []string{"alice", "bob", "charlie"} {
		if _, err := pool.Exec(ctx, `INSERT INTO desired_users(node_id,username,enabled,version,revision,fingerprint,created_at,updated_at)VALUES($1,$2,true,1,1,$3,now(),now())`, nodeID, username, fingerprint); err != nil {
			t.Fatal(err)
		}
	}

	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	service := New(pool, userstate.New(pool))
	service.now = func() time.Time { return now }
	request := PolicyRequest{NodeID: nodeID, Username: "alice", QuotaPeriod: "monthly", QuotaDirection: "rxtx", QuotaBytes: 300, ExpectedVersion: 0, IdempotencyKey: "alice-policy", ActorID: "operator", Reason: "ticket", RequestID: "request-policy", Traceparent: testTraceparent}
	policy, replayed, err := service.SetPolicy(ctx, request)
	if err != nil || replayed || policy.Version != 1 {
		t.Fatalf("set policy=%+v replayed=%v err=%v", policy, replayed, err)
	}
	if _, replayed, err = service.SetPolicy(ctx, request); err != nil || !replayed {
		t.Fatalf("policy replay=%v err=%v", replayed, err)
	}

	connected := now.Add(-time.Hour)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordUsageTx(ctx, tx, nodeID, []UsageSample{{SessionID: "session-a", Username: "alice", Connected: connected, RXBytes: 100, TXBytes: 200, ObservedAt: now}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	tx, _ = pool.Begin(ctx)
	if err := RecordUsageTx(ctx, tx, nodeID, []UsageSample{{SessionID: "session-a", Username: "alice", Connected: connected, RXBytes: 1, TXBytes: 2, ObservedAt: now}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	tx, _ = pool.Begin(ctx)
	if err := RecordUsageTx(ctx, tx, nodeID, []UsageSample{{SessionID: "session-a", Username: "alice", Connected: connected, RXBytes: 130, TXBytes: 260, ObservedAt: now.Add(time.Minute)}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	tx, _ = pool.Begin(ctx)
	if err := RecordUsageTx(ctx, tx, nodeID, []UsageSample{{SessionID: "session-a", Username: "alice", Connected: connected, RXBytes: 110, TXBytes: 220, ObservedAt: now.Add(30 * time.Second)}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	policy, err = service.GetPolicy(ctx, nodeID, "alice")
	if err != nil || policy.ObservedRXBytes != 130 || policy.ObservedTXBytes != 260 || !policy.Exceeded || policy.Convergence != "pending" {
		t.Fatalf("observed policy=%+v err=%v", policy, err)
	}
	tx, _ = pool.Begin(ctx)
	if err := RecordUsageTx(ctx, tx, nodeID, []UsageSample{{SessionID: "session-a", Username: "bob", Connected: connected, RXBytes: 140, TXBytes: 280, ObservedAt: now.Add(2 * time.Minute)}}); err != ErrInvalidRequest {
		_ = tx.Rollback(ctx)
		t.Fatalf("session username change err=%v", err)
	}
	_ = tx.Rollback(ctx)
	tx, _ = pool.Begin(ctx)
	if err := RecordUsageTx(ctx, tx, nodeID, []UsageSample{
		{SessionID: "saturate-a", Username: "bob", Connected: connected, RXBytes: 1<<63 - 1, TXBytes: 1<<63 - 1, ObservedAt: now.Add(3 * time.Minute)},
		{SessionID: "saturate-b", Username: "bob", Connected: connected.Add(time.Second), RXBytes: 1<<63 - 1, TXBytes: 1<<63 - 1, ObservedAt: now.Add(3 * time.Minute)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var saturatedRX, saturatedTX int64
	if err := pool.QueryRow(ctx, `SELECT rx_bytes,tx_bytes FROM observed_user_usage WHERE node_id=$1 AND username='bob' AND period='lifetime'`, nodeID).Scan(&saturatedRX, &saturatedTX); err != nil {
		t.Fatal(err)
	}
	if saturatedRX != 1<<63-1 || saturatedTX != 1<<63-1 {
		t.Fatalf("usage did not saturate rx=%d tx=%d", saturatedRX, saturatedTX)
	}

	policyKey := stableKey("policy", nodeID.String(), "alice", "1", "quota", monthStart(now).Format(time.RFC3339))
	if _, _, err := service.users.Mutate(ctx, userstate.MutationRequest{NodeID: nodeID, Kind: userstate.UserDisable, Name: "alice", ExpectedVersion: 1, IdempotencyKey: policyKey, TTL: 24 * time.Hour, ActorID: "scheduler", Reason: "quota or expiry policy enforcement", RequestID: policyKey, Traceparent: stableTraceparent(policyKey)}); err != nil {
		t.Fatalf("inject enforcement crash window: %v", err)
	}
	if err := service.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE scheduler_leases SET lease_until=now()-interval '1 second' WHERE lease_name=$1`, leaseName); err != nil {
		t.Fatal(err)
	}
	if err := service.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var enforcementCount, operationCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM user_policy_enforcements WHERE node_id=$1 AND username='alice'`, nodeID).Scan(&enforcementCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM commands WHERE node_id=$1 AND resource_key='alice' AND payload_type='user_disable'`, nodeID).Scan(&operationCount); err != nil {
		t.Fatal(err)
	}
	if enforcementCount != 1 || operationCount != 1 {
		t.Fatalf("repeat scan created duplicates enforcements=%d commands=%d", enforcementCount, operationCount)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO observed_users(node_id,username,enabled,revision,fingerprint,observed_at) VALUES($1,'alice',false,2,$2,now())`, nodeID, fingerprint); err != nil {
		t.Fatal(err)
	}
	policy, err = service.GetPolicy(ctx, nodeID, "alice")
	if err != nil || policy.Convergence != "converged" {
		t.Fatalf("enforced policy convergence=%q err=%v", policy.Convergence, err)
	}
	if _, _, err := service.users.Mutate(ctx, userstate.MutationRequest{NodeID: nodeID, Kind: userstate.UserDisable, Name: "alice", ExpectedVersion: 2, IdempotencyKey: "manual-disable-after-quota", TTL: 24 * time.Hour, ActorID: "operator", Reason: "manual security hold", RequestID: "request-manual-disable", Traceparent: testTraceparent}); err != nil {
		t.Fatalf("manual disable after quota enforcement: %v", err)
	}
	now = time.Date(2026, 9, 1, 0, 0, 1, 0, time.UTC)
	if _, err := pool.Exec(ctx, `UPDATE scheduler_leases SET lease_until=now()-interval '1 second' WHERE lease_name=$1`, leaseName); err != nil {
		t.Fatal(err)
	}
	if err := service.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var enabled bool
	var desiredVersion int64
	if err := pool.QueryRow(ctx, `SELECT enabled,version FROM desired_users WHERE node_id=$1 AND username='alice'`, nodeID).Scan(&enabled, &desiredVersion); err != nil {
		t.Fatal(err)
	}
	if enabled || desiredVersion != 3 {
		t.Fatalf("monthly reset overrode manual disable enabled=%v version=%d", enabled, desiredVersion)
	}

	if _, err := pool.Exec(ctx, `UPDATE nodes SET status='offline' WHERE id=$1`, nodeID); err != nil {
		t.Fatal(err)
	}
	batchID, approvalID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	batchItems := []BatchItemRequest{{NodeID: nodeID, Username: "bob", Action: "disable", ExpectedVersion: 1, Authorized: true}, {NodeID: nodeID, Username: "charlie", Action: "disable", ExpectedVersion: 1, Authorized: false}}
	batchHash := BatchRequestHash(batchItems)
	if _, err := pool.Exec(ctx, `INSERT INTO approval_requests(id,workspace_id,requester_id,action,resource_type,resource_id,reason,status,approver_id,approval_reason,expires_at,approved_at,created_at,request_hash,request_summary)VALUES($1,$2,$3,'user.batch.disable','batch_operation',$4,'integration','approved',$5,'independent',now()+interval '1 hour',now(),now(),$6,$7)`, approvalID, workspaceID, requesterID, batchID, approverID, batchHash[:], `[{"node_id":"`+nodeID.String()+`","username":"bob","action":"disable","expected_version":1},{"node_id":"`+nodeID.String()+`","username":"charlie","action":"disable","expected_version":1}]`); err != nil {
		t.Fatal(err)
	}
	batchRequest := BatchRequest{ID: batchID, WorkspaceID: workspaceID, ActorIdentityID: requesterID, ApprovalID: approvalID, ActorID: "operator", Reason: "ticket", RequestID: "request-batch", Traceparent: testTraceparent, IdempotencyKey: "batch-one", Items: batchItems}
	substituted := batchRequest
	substituted.Items = append([]BatchItemRequest(nil), batchRequest.Items...)
	substituted.Items[0].ExpectedVersion = 2
	if _, _, err := service.CreateBatch(ctx, substituted); !errors.Is(err, approvals.ErrNotReady) {
		t.Fatalf("content-substituted approval err=%v", err)
	}
	batch, replayed, err := service.CreateBatch(ctx, batchRequest)
	if err != nil || replayed || len(batch.Items) != 2 || batch.Items[1].State != "forbidden" {
		t.Fatalf("create batch=%+v replayed=%v err=%v", batch, replayed, err)
	}
	var approvalStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM approval_requests WHERE id=$1`, approvalID).Scan(&approvalStatus); err != nil || approvalStatus != "consumed" {
		t.Fatalf("approval status=%q err=%v", approvalStatus, err)
	}
	if _, replayed, err := service.CreateBatch(ctx, batchRequest); err != nil || !replayed {
		t.Fatalf("batch replay=%v err=%v", replayed, err)
	}
	batchKey := stableKey("batch", batchID.String(), "0")
	if _, _, err := service.users.Mutate(ctx, userstate.MutationRequest{NodeID: nodeID, Kind: userstate.UserDisable, Name: "bob", ExpectedVersion: 1, IdempotencyKey: batchKey, TTL: 24 * time.Hour, ActorID: "operator", ActorIdentityID: requesterID, Reason: "ticket", RequestID: "request-batch:0", Traceparent: testTraceparent}); err != nil {
		t.Fatalf("inject batch post-mutation crash window: %v", err)
	}
	crashedOwner := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `UPDATE batch_operation_items SET state='submitting',lease_owner=$3,lease_until=now()-interval '1 second' WHERE batch_id=$1 AND item_index=$2`, batchID, 0, crashedOwner); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE scheduler_leases SET lease_until=now()-interval '1 second' WHERE lease_name=$1`, leaseName); err != nil {
		t.Fatal(err)
	}
	if err := service.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	batch, err = service.GetBatch(ctx, batch.ID)
	if err != nil || batch.Items[0].ChildOperationID == nil || batch.Items[0].State != "offline_pending" || batch.Items[1].State != "forbidden" || batch.State != "partial_failed" {
		t.Fatalf("batch result=%+v err=%v", batch, err)
	}
	var batchCommandCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM commands WHERE node_id=$1 AND resource_key='bob' AND idempotency_key=$2`, nodeID, batchKey).Scan(&batchCommandCount); err != nil || batchCommandCount != 1 {
		t.Fatalf("batch crash recovery command count=%d err=%v", batchCommandCount, err)
	}
	metrics, err := service.Metrics(ctx, workspaceID)
	if err != nil || metrics.ActiveBatchItemTotal != 1 || metrics.StaleBatchClaimTotal != 0 || metrics.UnknownBatchItemTotal != 0 {
		t.Fatalf("user operation metrics=%+v err=%v", metrics, err)
	}
}

func cleanupIntegration(ctx context.Context, pool *pgxpool.Pool, workspaceID uuid.UUID) error {
	if _, err := pool.Exec(ctx, `ALTER TABLE audit_events DISABLE TRIGGER audit_events_append_only`); err != nil {
		return err
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `ALTER TABLE audit_events ENABLE TRIGGER audit_events_append_only`)
	}()
	statements := []string{
		`UPDATE scheduler_leases SET lease_until=now()-interval '1 second' WHERE lease_name='user-operations' AND $1::uuid IS NOT NULL`,
		`DELETE FROM batch_operation_items WHERE batch_id IN(SELECT id FROM batch_operations WHERE workspace_id=$1)`,
		`DELETE FROM batch_operations WHERE workspace_id=$1`,
		`DELETE FROM user_policy_enforcements WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`,
		`DELETE FROM user_policy_mutations WHERE workspace_id=$1`,
		`DELETE FROM user_usage_cursors WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`,
		`DELETE FROM observed_user_usage WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`,
		`DELETE FROM desired_user_policies WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`,
		`DELETE FROM agent_command_results WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
		`DELETE FROM node_command_leases WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
		`DELETE FROM command_attempts WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
		`DELETE FROM outbox_events WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
		`DELETE FROM operation_events WHERE operation_id IN(SELECT id FROM operations WHERE workspace_id=$1)`,
		`DELETE FROM audit_events WHERE workspace_id=$1`,
		`DELETE FROM commands WHERE workspace_id=$1`,
		`DELETE FROM operations WHERE workspace_id=$1`,
		`DELETE FROM approval_requests WHERE workspace_id=$1`,
		`DELETE FROM desired_users WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`,
		`DELETE FROM node_capabilities WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`,
		`DELETE FROM nodes WHERE workspace_id=$1`,
		`DELETE FROM workspaces WHERE id=$1`,
		`DELETE FROM identities WHERE issuer='integration' AND $1::uuid IS NOT NULL`,
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement, workspaceID); err != nil {
			return err
		}
	}
	return nil
}
