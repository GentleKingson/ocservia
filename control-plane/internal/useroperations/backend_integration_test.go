package useroperations

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/coordination"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/mysql"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	userstore "github.com/GentleKingson/ocservia/control-plane/internal/useroperations/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/userstate"
	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func userOperationsBackend(t *testing.T) database.Backend {
	t.Helper()
	ctx := context.Background()
	if dsn := os.Getenv("PR02_DSN"); dsn != "" {
		options := mysql.Options{Engine: mysql.Engine(os.Getenv("PR02_ENGINE")), Environment: "test", DSN: dsn}
		admin, err := mysql.Open(ctx, options)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = admin.Close() })
		name := "pr02_user_operations_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		if _, err := admin.Exec(ctx, "CREATE DATABASE `"+name+"`"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = admin.Exec(ctx, "DROP DATABASE `"+name+"`") })
		if _, err := admin.Exec(ctx, "GRANT ALL ON `"+name+"`.* TO 'ocservia_owner'@'%' WITH GRANT OPTION"); err != nil {
			t.Fatal(err)
		}
		cfg, err := driver.ParseDSN(dsn)
		if err != nil {
			t.Fatal(err)
		}
		cfg.DBName, cfg.User, cfg.Passwd = name, "ocservia_owner", "pr02-owner-test-only"
		options.DSN = cfg.FormatDSN()
		owner, err := mysql.Open(ctx, options)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = owner.Close() })
		if err := owner.Migrate(ctx, ""); err != nil {
			t.Fatal(err)
		}
		if err := owner.GrantTestPrivileges(ctx); err != nil {
			t.Fatal(err)
		}
		cfg.User, cfg.Passwd = "ocservia_app", "pr02-runtime-test-only"
		options.DSN = cfg.FormatDSN()
		b, err := mysql.Open(ctx, options)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = b.Close() })
		return b
	}
	dsn := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("real PostgreSQL or PR02 backend required")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return postgres.WrapPool(pool)
}

func TestUserOperationsBackendIntegration(t *testing.T) {
	b := userOperationsBackend(t)
	ctx := context.Background()
	_, my := b.(*mysql.Backend)
	query := func(pg, sql string, args ...any) (string, []any) {
		if my {
			pg = sql
			for i, v := range args {
				if id, ok := v.(uuid.UUID); ok {
					args[i] = mysql.UUIDBytes(id)
				}
			}
		}
		return pg, args
	}
	exec := func(pg, sql string, args ...any) {
		t.Helper()
		q, args := query(pg, sql, args...)
		if _, err := b.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	row := func(pg, sql string, args ...any) database.Row {
		q, args := query(pg, sql, args...)
		return b.QueryRow(ctx, q, args...)
	}
	stamp := func(v time.Time) value.Timestamp {
		t.Helper()
		at, err := value.FromTime(v)
		if err != nil {
			t.Fatal(err)
		}
		return at
	}
	workspace, node, requester, approver, session, approverSession := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC().Truncate(time.Second)
	at, expires := stamp(now), stamp(now.Add(time.Hour))
	exec(`INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES($1,'policies',$2,now(),now())`, `INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES(?,'policies',?,TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)),TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)))`, workspace, workspace.String())
	exec(`INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at)VALUES($1,$2,'policies','active',now(),now())`, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at)VALUES(?,?,'policies','active',TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)),TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)))`, node, workspace)
	exec(`INSERT INTO node_capabilities(node_id,capability,approved)VALUES($1,'ocserv.users.write',true)`, `INSERT INTO node_capabilities(node_id,capability,approved)VALUES(?,'ocserv.users.write',true)`, node)
	for _, id := range []uuid.UUID{requester, approver} {
		exec(`INSERT INTO identities(id,issuer,subject,created_at,updated_at)VALUES($1,'user-operations',$2,$3,$4)`, `INSERT INTO identities(id,issuer,subject,created_at,updated_at)VALUES(?,'user-operations',?,?,?)`, id, id.String(), at, at)
		exec(`INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,created_at)VALUES($1,$2,$3,'PlatformAdmin','workspace',$4)`, `INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,created_at)VALUES(?,?,?,'PlatformAdmin','workspace',?)`, uuid.New(), id, workspace, at)
	}
	for _, pair := range [][2]uuid.UUID{{session, requester}, {approverSession, approver}} {
		exec(`INSERT INTO auth_sessions(id,identity_id,expires_at,created_at)VALUES($1,$2,$3,$4)`, `INSERT INTO auth_sessions(id,identity_id,expires_at,created_at)VALUES(?,?,?,?)`, pair[0], pair[1], expires, at)
	}
	for _, name := range []string{"alice", "bob", "charlie", "dave"} {
		exec(`INSERT INTO desired_users(node_id,username,enabled,version,revision,fingerprint,created_at,updated_at)VALUES($1,$2,true,1,1,$3,$4,$5)`, `INSERT INTO desired_users(node_id,username,enabled,version,revision,fingerprint,created_at,updated_at)VALUES(?,?,true,1,1,?,?,?)`, node, name, make([]byte, 32), at, at)
	}
	seed := [32]byte{3}
	s := NewBackend(b, userstate.NewWithSignerBackend(b, commandauth.NewSignerFromSeed(seed)))
	s.now = func() time.Time { return now }
	request := PolicyRequest{NodeID: node, Username: "alice", QuotaPeriod: "monthly", QuotaDirection: "rxtx", QuotaBytes: 300, IdempotencyKey: uuid.NewString(), ActorID: "operator", Reason: "ticket", RequestID: uuid.NewString(), Traceparent: testTraceparent}
	policy, replay, err := s.SetPolicy(ctx, request)
	if err != nil || replay || policy.Version != 1 {
		t.Fatal("set policy", policy, replay, err)
	}
	if _, replay, err := s.SetPolicy(ctx, request); err != nil || !replay {
		t.Fatal("policy replay", replay, err)
	}
	changed := request
	changed.QuotaBytes++
	if _, _, err := s.SetPolicy(ctx, changed); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatal("policy hash", err)
	}
	changed.IdempotencyKey = uuid.NewString()
	if _, _, err := s.SetPolicy(ctx, changed); !errors.Is(err, ErrVersionConflict) {
		t.Fatal("policy version", err)
	}
	if err := database.Within(ctx, b, database.ReadCommitted, func(tx database.Tx) error {
		return RecordUsageTx(ctx, tx, node, []UsageSample{{SessionID: "alice-session", Username: "alice", Connected: now.Add(-time.Hour), RXBytes: 100, TXBytes: 200, ObservedAt: now}})
	}); err != nil {
		t.Fatal(err)
	}
	policy, err = s.GetPolicy(ctx, node, "alice")
	if err != nil || !policy.Exceeded || policy.ObservedAt == nil || *policy.ObservedAt != at {
		t.Fatal("observed policy", policy, err)
	}
	// Commit the child but not its enforcement receipt, then recover by the
	// same stable key without creating a second signed command.
	candidate := userstore.Candidate{NodeID: node, Username: "alice", PolicyVersion: 1, UserVersion: 1, Cause: "quota", PeriodStart: stamp(monthStart(now)), Enabled: true}
	if err := s.withStore(ctx, func(store userstore.Store) error { return store.EnsureEnforcement(ctx, candidate, at) }); err != nil {
		t.Fatal(err)
	}
	key := stableKey("policy", node.String(), "alice", "1", "quota", monthStart(now).Format(time.RFC3339))
	mutation := userstate.MutationRequest{NodeID: node, Kind: userstate.UserDisable, Name: "alice", ExpectedVersion: 1, IdempotencyKey: key, TTL: 24 * time.Hour, ActorID: "scheduler", Reason: "quota or expiry policy enforcement", RequestID: key, Traceparent: stableTraceparent(key)}
	if _, _, err := s.users.Mutate(ctx, mutation); err != nil {
		t.Fatal(err)
	}
	if n, err := s.enforcePolicies(ctx, 10); err != nil || n != 1 {
		t.Fatal("recover enforcement", n, err)
	}
	if n, err := s.enforcePolicies(ctx, 10); err != nil || n != 0 {
		t.Fatal("repeat enforcement", n, err)
	}
	var count int
	if err := row(`SELECT count(*) FROM commands WHERE node_id=$1 AND resource_key='alice'`, `SELECT count(*) FROM commands WHERE node_id=? AND resource_key='alice'`, node).Scan(&count); err != nil || count != 1 {
		t.Fatal("duplicate command", count, err)
	}
	exec(`INSERT INTO observed_users(node_id,username,enabled,revision,fingerprint,observed_at)VALUES($1,'alice',false,2,$2,$3)`, `INSERT INTO observed_users(node_id,username,enabled,revision,fingerprint,observed_at)VALUES(?,'alice',false,2,?,?)`, node, make([]byte, 32), at)
	exec(`UPDATE commands SET state='succeeded' WHERE node_id=$1 AND resource_key='alice'`, `UPDATE commands SET state='succeeded' WHERE node_id=? AND resource_key='alice'`, node)
	now = monthStart(now).AddDate(0, 1, 0).Add(time.Second)
	if n, err := s.resetMonthlyPolicies(ctx, 10); err != nil || n != 1 {
		t.Fatal("monthly reset", n, err)
	}
	if n, err := s.resetMonthlyPolicies(ctx, 10); err != nil || n != 0 {
		t.Fatal("repeat reset", n, err)
	}
	var enabled bool
	var version int64
	if err := row(`SELECT enabled,version FROM desired_users WHERE node_id=$1 AND username='alice'`, `SELECT enabled,version FROM desired_users WHERE node_id=? AND username='alice'`, node).Scan(&enabled, &version); err != nil || !enabled || version != 3 {
		t.Fatal("reset state", enabled, version, err)
	}
	exec(`UPDATE user_policy_enforcements SET operation_id=NULL,resulting_user_version=NULL WHERE node_id=$1 AND cause='quota_reset'`, `UPDATE user_policy_enforcements SET operation_id=NULL,resulting_user_version=NULL WHERE node_id=? AND cause='quota_reset'`, node)
	if n, err := s.resetMonthlyPolicies(ctx, 10); err != nil || n != 1 {
		t.Fatal("recover reset receipt", n, err)
	}
	exec(`UPDATE commands SET state='succeeded' WHERE node_id=$1 AND resource_key='alice'`, `UPDATE commands SET state='succeeded' WHERE node_id=? AND resource_key='alice'`, node)
	if _, _, err := s.users.Mutate(ctx, userstate.MutationRequest{NodeID: node, Kind: userstate.UserDisable, Name: "alice", ExpectedVersion: 3, IdempotencyKey: uuid.NewString(), TTL: time.Hour, ActorID: "operator", Reason: "manual hold", RequestID: uuid.NewString(), Traceparent: testTraceparent}); err != nil {
		t.Fatal("manual hold", err)
	}
	now = monthStart(now).AddDate(0, 1, 0).Add(time.Second)
	if n, err := s.resetMonthlyPolicies(ctx, 10); err != nil || n != 0 {
		t.Fatal("reset must preserve manual hold", n, err)
	}
	// Extended storage timestamps are returned literally and compared without
	// forcing them through Go's or JavaScript's finite date representation.
	negative, positive := value.Timestamp{Valid: true, Micros: value.NegativeInfinity}, value.Timestamp{Valid: true, Micros: value.PositiveInfinity}
	exec(`UPDATE desired_user_policies SET expires_at=$1 WHERE node_id=$2`, `UPDATE desired_user_policies SET expires_at=? WHERE node_id=?`, positive, node)
	policy, err = s.GetPolicy(ctx, node, "alice")
	if err != nil || policy.Expired || policy.ExpiresAt == nil || *policy.ExpiresAt != positive {
		t.Fatal("infinite expiry", policy, err)
	}
	exec(`UPDATE desired_user_policies SET expires_at=$1 WHERE node_id=$2`, `UPDATE desired_user_policies SET expires_at=? WHERE node_id=?`, negative, node)
	policy, err = s.GetPolicy(ctx, node, "alice")
	if err != nil || !policy.Expired {
		t.Fatal("negative expiry", policy, err)
	}
	exec(`UPDATE desired_user_policies SET expires_at=NULL WHERE node_id=$1`, `UPDATE desired_user_policies SET expires_at=NULL WHERE node_id=?`, node)
	exec(`UPDATE nodes SET status='offline' WHERE id=$1`, `UPDATE nodes SET status='offline' WHERE id=?`, node)
	items := []BatchItemRequest{{NodeID: node, Username: "bob", Action: "disable", ExpectedVersion: 1, Authorized: true}, {NodeID: node, Username: "charlie", Action: "disable", ExpectedVersion: 1, Authorized: false}}
	batchID := uuid.Must(uuid.NewV7())
	hash := BatchRequestHash(items)
	summary, _ := json.Marshal(items)
	approvalService := approvals.NewBackend(b)
	pending, err := approvalService.Create(ctx, approvals.Request{WorkspaceID: workspace, RequesterID: requester, ResourceID: batchID, SessionID: session, Action: "user.batch.disable", ResourceType: "batch_operation", Reason: "review batch", TTL: time.Hour, RequestID: uuid.NewString(), RequestHash: hash[:], RequestSummary: summary, AuthorityResources: []approvals.AuthorityResource{{WorkspaceID: workspace, Type: "workspace"}}, BatchItems: []approvals.BoundBatchItem{{NodeID: node, Username: "bob", Action: "disable", ExpectedVersion: 1}, {NodeID: node, Username: "charlie", Action: "disable", ExpectedVersion: 1}}})
	if err != nil {
		t.Fatal("request approval", err)
	}
	if _, err := approvalService.Approve(ctx, approvals.Decision{ApprovalID: pending.ID, ApproverID: approver, SessionID: approverSession, Reason: "reviewed", RequestID: uuid.NewString(), ExpectedRequestHash: pending.RequestHash}); err != nil {
		t.Fatal("approve", err)
	}
	batchRequest := BatchRequest{ID: batchID, WorkspaceID: workspace, ActorIdentityID: requester, ActorSessionID: session, ApprovalID: pending.ID, ActorID: "operator", Reason: "ticket", RequestID: uuid.NewString(), Traceparent: testTraceparent, IdempotencyKey: uuid.NewString(), Items: items}
	altered := batchRequest
	altered.Items = append([]BatchItemRequest(nil), items...)
	altered.Items[0].ExpectedVersion++
	if _, _, err := s.CreateBatch(ctx, altered); !errors.Is(err, approvals.ErrNotReady) {
		t.Fatal("approval binding", err)
	}
	batch, replay, err := s.CreateBatch(ctx, batchRequest)
	if err != nil || replay || len(batch.Items) != 2 || batch.Items[1].State != "forbidden" {
		t.Fatal("create batch", batch, replay, err)
	}
	if _, replay, err := s.CreateBatch(ctx, batchRequest); err != nil || !replay {
		t.Fatal("batch replay", replay, err)
	}
	key = stableKey("batch", batchID.String(), "0")
	if _, _, err := s.users.Mutate(ctx, userstate.MutationRequest{NodeID: node, Kind: userstate.UserDisable, Name: "bob", ExpectedVersion: 1, IdempotencyKey: key, TTL: 24 * time.Hour, ActorID: "operator", ActorIdentityID: requester, ActorSessionID: session, Reason: "ticket", RequestID: batchRequest.RequestID + ":0", Traceparent: testTraceparent}); err != nil {
		t.Fatal("batch crash window", err)
	}
	exec(`UPDATE batch_operation_items SET state='submitting',lease_owner=$1,lease_until=$2 WHERE batch_id=$3 AND item_index=0`, `UPDATE batch_operation_items SET state='submitting',lease_owner=?,lease_until=? WHERE batch_id=? AND item_index=0`, uuid.New(), negative, batchID)
	if err := s.submitBatchItems(ctx, uuid.New(), 10); err != nil {
		t.Fatal("recover batch", err)
	}
	if err := s.refreshBatches(ctx); err != nil {
		t.Fatal("refresh batch", err)
	}
	batch, err = s.GetBatch(ctx, batchID)
	if err != nil || batch.State != "partial_failed" || batch.Items[0].State != "offline_pending" || batch.Items[0].ChildOperationID == nil {
		t.Fatal("batch outcome", batch, err)
	}
	metrics, err := s.Metrics(ctx, workspace)
	if err != nil || metrics.ActiveBatchItemTotal != 1 || metrics.StaleBatchClaimTotal != 0 {
		t.Fatal("metrics", metrics, err)
	}
	op := *batch.Items[0].ChildOperationID
	for _, state := range []string{"unknown", "succeeded"} {
		exec(`UPDATE operations SET state=$1 WHERE id=$2`, `UPDATE operations SET state=? WHERE id=?`, state, op)
		if err := s.refreshBatches(ctx); err != nil {
			t.Fatal(err)
		}
		batch, err = s.GetBatch(ctx, batchID)
		if err != nil || batch.Items[0].State != state {
			t.Fatal("child refresh", batch, err)
		}
	}
	claimBatch, _, err := s.CreateBatch(ctx, BatchRequest{WorkspaceID: workspace, ActorID: "operator", Reason: "claim checks", RequestID: uuid.NewString(), Traceparent: testTraceparent, IdempotencyKey: uuid.NewString(), Items: []BatchItemRequest{{NodeID: node, Username: "dave", Action: "enable", ExpectedVersion: 1, Authorized: true}, {NodeID: node, Username: "charlie", Action: "enable", ExpectedVersion: 1, Authorized: true}}})
	if err != nil {
		t.Fatal("claim batch", err)
	}
	firstOwner, secondOwner := uuid.New(), uuid.New()
	firstTx, err := b.Begin(ctx, database.ReadCommitted)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(firstTx)
	firstStore, err := userstore.From(firstTx)
	if err != nil {
		t.Fatal(err)
	}
	firstClaim, err := firstStore.ClaimBatchItems(ctx, firstOwner, 1)
	if err != nil || len(firstClaim) != 1 {
		t.Fatal("first claim", firstClaim, err)
	}
	bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var secondClaim []userstore.BatchItem
	if err := s.withStore(bounded, func(store userstore.Store) (err error) {
		secondClaim, err = store.ClaimBatchItems(bounded, secondOwner, 1)
		return
	}); err != nil || len(secondClaim) != 1 || firstClaim[0].Index == secondClaim[0].Index {
		t.Fatal("skip locked claim", secondClaim, err)
	}
	if err := firstTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.withStore(ctx, func(store userstore.Store) error {
		return store.FinishBatchItem(ctx, secondClaim[0], firstOwner, nil, "wrong_owner")
	}); err != nil {
		t.Fatal(err)
	}
	claimBatch, err = s.GetBatch(ctx, claimBatch.ID)
	if err != nil || claimBatch.Items[secondClaim[0].Index].State != "submitting" {
		t.Fatal("claim owner fence", claimBatch, err)
	}
	if err := s.withStore(ctx, func(store userstore.Store) error { return store.ReleaseBatchClaims(ctx, firstOwner) }); err != nil {
		t.Fatal(err)
	}
	if err := s.withStore(ctx, func(store userstore.Store) error {
		return store.FinishBatchItem(ctx, secondClaim[0], secondOwner, nil, "stale_revision")
	}); err != nil {
		t.Fatal(err)
	}
	leaseOwner := uuid.New()
	defer exec(`UPDATE scheduler_leases SET lease_until=$1 WHERE lease_name=$2 AND owner_id=$3`, `UPDATE scheduler_leases SET lease_until=? WHERE lease_name=? AND owner_id=?`, negative, leaseName, leaseOwner)
	if ok, err := s.acquireLease(ctx, leaseOwner, time.Minute); err != nil || !ok {
		t.Fatal("acquire lease", ok, err)
	}
	if ok, err := s.acquireLease(ctx, uuid.New(), time.Minute); err != nil || ok {
		t.Fatal("held lease", ok, err)
	}
	if ok, err := s.acquireLease(ctx, leaseOwner, time.Minute); err != nil || !ok {
		t.Fatal("renew lease", ok, err)
	}
	identity, err := coordination.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	leader, err := coordination.AcquireBackend(ctx, b, identity, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	fenced := coordination.WithFence(ctx, leader)
	exec(`UPDATE scheduler_leadership SET epoch=epoch+1 WHERE id=1`, `UPDATE scheduler_leadership SET epoch=epoch+1 WHERE id=1`)
	if _, _, err := s.SetPolicy(fenced, request); !errors.Is(err, coordination.ErrNotLeader) {
		t.Fatal("fenced policy replay", err)
	}
	if _, _, err := s.CreateBatch(fenced, batchRequest); !errors.Is(err, coordination.ErrNotLeader) {
		t.Fatal("fenced batch replay", err)
	}
	if err := s.RunOnce(fenced); !errors.Is(err, coordination.ErrNotLeader) {
		t.Fatal("fenced scheduler", err)
	}
	if _, err := s.claimBatchItems(fenced, uuid.New(), 10); !errors.Is(err, coordination.ErrNotLeader) {
		t.Fatal("fenced claim", err)
	}
	claimBatch, err = s.GetBatch(ctx, claimBatch.ID)
	if err != nil || claimBatch.Items[firstClaim[0].Index].State != "queued" || claimBatch.Items[secondClaim[0].Index].ErrorType != "stale_revision" {
		t.Fatal("claim rollback and failure", claimBatch, err)
	}
	exec(`UPDATE batch_operations SET created_at=$1,updated_at=$2 WHERE id=$3`, `UPDATE batch_operations SET created_at=?,updated_at=? WHERE id=?`, negative, positive, claimBatch.ID)
	claimBatch, err = s.GetBatch(ctx, claimBatch.ID)
	if err != nil || claimBatch.CreatedAt != negative || claimBatch.UpdatedAt != positive {
		t.Fatal("batch logical clocks", claimBatch, err)
	}
	if _, err := audit.NewBackendManager(b, nil).Verify(ctx, workspace); err != nil {
		t.Fatal("audit chain", err)
	}
}
