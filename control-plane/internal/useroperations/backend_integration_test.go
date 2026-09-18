package useroperations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
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
	"github.com/GentleKingson/ocservia/control-plane/migrations"
	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func userOperationsBackend(t *testing.T) (database.Backend, database.Backend) {
	t.Helper()
	return userOperationsBackendWithCleanup(t, t)
}

func userOperationsBackendWithCleanup(t, cleanupT *testing.T) (database.Backend, database.Backend) {
	t.Helper()
	// Report setup errors in the current child, but let its parent own the resources.
	ctx := context.Background()
	if dsn := os.Getenv("PR02_DSN"); dsn != "" {
		options := mysql.Options{Engine: mysql.Engine(os.Getenv("PR02_ENGINE")), Environment: "test", DSN: dsn}
		admin, err := mysql.Open(ctx, options)
		if err != nil {
			t.Fatal(err)
		}
		cleanupT.Cleanup(func() { _ = admin.Close() })
		name := "pr02_user_operations_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		if _, err := admin.Exec(ctx, "CREATE DATABASE `"+name+"`"); err != nil {
			t.Fatal(err)
		}
		cleanupT.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := admin.Exec(ctx, "DROP DATABASE `"+name+"`"); err != nil {
				cleanupT.Error("drop user operations fixture", err)
			}
		})
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
		cleanupT.Cleanup(func() { _ = owner.Close() })
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
		cleanupT.Cleanup(func() { _ = b.Close() })
		return b, owner
	}
	dsn := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("real PostgreSQL or PR02 backend required")
	}
	ownerURL := os.Getenv("OCSERV_TEST_OWNER_DATABASE_URL")
	if ownerURL == "" {
		t.Fatal("separate PostgreSQL owner fixture required; runtime must not act as owner")
	}
	admin, err := pgxpool.New(ctx, ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	cleanupT.Cleanup(admin.Close)
	name := "policy_cleanup_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+identifier); err != nil {
		t.Fatal(err)
	}
	cleanupT.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP DATABASE "+identifier); err != nil {
			cleanupT.Error("drop user operations fixture", err)
		}
	})
	open := func(url string) *pgxpool.Pool {
		t.Helper()
		cfg, err := pgxpool.ParseConfig(url)
		if err != nil {
			t.Fatal(err)
		}
		cfg.ConnConfig.Database = name
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		cleanupT.Cleanup(pool.Close)
		return pool
	}
	ownerPool, pool := open(ownerURL), open(dsn)
	if err := migrations.Migrate(ctx, ownerPool); err != nil {
		t.Fatal(err)
	}
	if err := migrations.GrantRuntimePrivileges(ctx, ownerPool, pool.Config().ConnConfig.User); err != nil {
		t.Fatal(err)
	}
	return postgres.WrapPool(pool), postgres.WrapPool(ownerPool)
}

func TestUserOperationsBackendIntegration(t *testing.T) {
	b, _ := userOperationsBackend(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
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
	for _, name := range []string{"alice", "bob", "charlie", "dave", "expired"} {
		exec(`INSERT INTO desired_users(node_id,username,enabled,version,revision,fingerprint,created_at,updated_at)VALUES($1,$2,true,1,1,$3,$4,$5)`, `INSERT INTO desired_users(node_id,username,enabled,version,revision,fingerprint,created_at,updated_at)VALUES(?,?,true,1,1,?,?,?)`, node, name, make([]byte, 32), at, at)
	}
	seed := [32]byte{3}
	users := userstate.NewWithSignerBackend(b, commandauth.NewSignerFromSeed(seed))
	mutator := &recordingUserMutator{delegate: users}
	s := NewBackend(b, mutator)
	s.now = func() time.Time { return now }
	policyErrors := NewBackend(b, mutator)
	policyErrors.now = s.now
	checkPolicyErrors := func(t *testing.T, run func(context.Context, int) (int, error), want userstate.MutationRequest, cause string) {
		t.Helper()
		unexpected := errors.New("injected mutation failure")
		for _, test := range []struct {
			name     string
			err      error
			retained int
			returned bool
		}{
			{"backlog", userstate.ErrBacklogExceeded, 1, false},
			{"version", userstate.ErrVersionConflict, 0, false},
			{"pending", userstate.ErrRevisionPending, 0, false},
			{"recovery", userstate.ErrRevisionRecovery, 0, false},
			{"other", unexpected, 1, true},
		} {
			t.Run(test.name, func(t *testing.T) {
				mutator.requests, mutator.contexts = nil, nil
				mutator.err = fmt.Errorf("mutator: %w", test.err)
				n, err := run(ctx, 10)
				if n != 0 || test.returned && err != mutator.err || !test.returned && err != nil {
					t.Fatal("policy error classification", n, err)
				}
				mutator.assertLast(t, ctx, 1, want)
				var count int
				if err := row(`SELECT count(*) FROM user_policy_enforcements WHERE node_id=$1 AND username='alice' AND cause=$2 AND operation_id IS NULL`, `SELECT count(*) FROM user_policy_enforcements WHERE node_id=? AND username='alice' AND cause=? AND operation_id IS NULL`, node, cause).Scan(&count); err != nil || count != test.retained {
					t.Fatal("enforcement cleanup", count, err)
				}
			})
		}
		mutator.err = nil
		mutator.requests, mutator.contexts = nil, nil
	}
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
	t.Run("quota-port-errors", func(t *testing.T) { checkPolicyErrors(t, policyErrors.enforcePolicies, mutation, "quota") })
	if _, _, err := users.Mutate(ctx, mutation); err != nil {
		t.Fatal(err)
	}
	if n, err := s.enforcePolicies(ctx, 10); err != nil || n != 1 {
		t.Fatal("recover enforcement", n, err)
	}
	if n, err := s.enforcePolicies(ctx, 10); err != nil || n != 0 {
		t.Fatal("repeat enforcement", n, err)
	}
	if len(mutator.requests) != 0 {
		t.Fatal("receipt recovery resubmitted a committed mutation")
	}
	var count int
	if err := row(`SELECT count(*) FROM commands WHERE node_id=$1 AND resource_key='alice'`, `SELECT count(*) FROM commands WHERE node_id=? AND resource_key='alice'`, node).Scan(&count); err != nil || count != 1 {
		t.Fatal("duplicate command", count, err)
	}
	exec(`INSERT INTO observed_users(node_id,username,enabled,revision,fingerprint,observed_at)VALUES($1,'alice',false,2,$2,$3)`, `INSERT INTO observed_users(node_id,username,enabled,revision,fingerprint,observed_at)VALUES(?,'alice',false,2,?,?)`, node, make([]byte, 32), at)
	exec(`UPDATE commands SET state='succeeded' WHERE node_id=$1 AND resource_key='alice'`, `UPDATE commands SET state='succeeded' WHERE node_id=? AND resource_key='alice'`, node)
	now = monthStart(now).AddDate(0, 1, 0).Add(time.Second)
	resetKey := stableKey("policy-reset", node.String(), "alice", "1", monthStart(now).Format(time.RFC3339))
	resetRequest := userstate.MutationRequest{NodeID: node, Kind: userstate.UserEnable, Name: "alice", ExpectedVersion: 2, IdempotencyKey: resetKey, TTL: 24 * time.Hour, ActorID: "scheduler", Reason: "monthly quota reset", RequestID: resetKey, Traceparent: stableTraceparent(resetKey)}
	t.Run("reset-port-errors", func(t *testing.T) {
		checkPolicyErrors(t, policyErrors.resetMonthlyPolicies, resetRequest, "quota_reset")
	})
	if n, err := s.resetMonthlyPolicies(ctx, 10); err != nil || n != 1 {
		t.Fatal("monthly reset", n, err)
	}
	if n, err := s.resetMonthlyPolicies(ctx, 10); err != nil || n != 0 {
		t.Fatal("repeat reset", n, err)
	}
	mutator.assertLast(t, ctx, 1, resetRequest)
	var enabled bool
	var version int64
	if err := row(`SELECT enabled,version FROM desired_users WHERE node_id=$1 AND username='alice'`, `SELECT enabled,version FROM desired_users WHERE node_id=? AND username='alice'`, node).Scan(&enabled, &version); err != nil || !enabled || version != 3 {
		t.Fatal("reset state", enabled, version, err)
	}
	exec(`UPDATE user_policy_enforcements SET operation_id=NULL,resulting_user_version=NULL WHERE node_id=$1 AND cause='quota_reset'`, `UPDATE user_policy_enforcements SET operation_id=NULL,resulting_user_version=NULL WHERE node_id=? AND cause='quota_reset'`, node)
	if n, err := s.resetMonthlyPolicies(ctx, 10); err != nil || n != 1 {
		t.Fatal("recover reset receipt", n, err)
	}
	mutator.assertLast(t, ctx, 1, resetRequest)
	exec(`UPDATE commands SET state='succeeded' WHERE node_id=$1 AND resource_key='alice'`, `UPDATE commands SET state='succeeded' WHERE node_id=? AND resource_key='alice'`, node)
	if _, _, err := users.Mutate(ctx, userstate.MutationRequest{NodeID: node, Kind: userstate.UserDisable, Name: "alice", ExpectedVersion: 3, IdempotencyKey: uuid.NewString(), TTL: time.Hour, ActorID: "operator", Reason: "manual hold", RequestID: uuid.NewString(), Traceparent: testTraceparent}); err != nil {
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
	expiredAt := now.Add(-time.Hour)
	if _, _, err := s.SetPolicy(ctx, PolicyRequest{NodeID: node, Username: "expired", QuotaPeriod: "none", QuotaDirection: "rxtx", ExpiresAt: &expiredAt, IdempotencyKey: "expired-policy", ActorID: "operator", Reason: "expiry", RequestID: "expired-policy", Traceparent: testTraceparent}); err != nil {
		t.Fatal("expiry policy", err)
	}
	mutator.requests, mutator.contexts = nil, nil
	if n, err := s.enforcePolicies(ctx, 10); err != nil || n != 1 {
		t.Fatal("expiry enforcement", n, err)
	}
	expiryKey := stableKey("policy", node.String(), "expired", "1", "expiry", "1970-01-01T00:00:00Z")
	mutator.assertLast(t, ctx, 1, userstate.MutationRequest{NodeID: node, Kind: userstate.UserDisable, Name: "expired", ExpectedVersion: 1, IdempotencyKey: expiryKey, TTL: 24 * time.Hour, ActorID: "scheduler", Reason: "quota or expiry policy enforcement", RequestID: expiryKey, Traceparent: stableTraceparent(expiryKey)})
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
	batchMutation := userstate.MutationRequest{NodeID: node, Kind: userstate.UserDisable, Name: "bob", ExpectedVersion: 1, IdempotencyKey: key, TTL: 24 * time.Hour, ActorID: "operator", ActorIdentityID: requester, ActorSessionID: session, Reason: "ticket", RequestID: batchRequest.RequestID + ":0", Traceparent: testTraceparent}
	if _, _, err := users.Mutate(ctx, batchMutation); err != nil {
		t.Fatal("batch crash window", err)
	}
	exec(`UPDATE batch_operation_items SET state='submitting',lease_owner=$1,lease_until=$2 WHERE batch_id=$3 AND item_index=0`, `UPDATE batch_operation_items SET state='submitting',lease_owner=?,lease_until=? WHERE batch_id=? AND item_index=0`, uuid.New(), negative, batchID)
	mutator.requests, mutator.contexts = nil, nil
	if err := s.submitBatchItems(ctx, uuid.New(), 10); err != nil {
		t.Fatal("recover batch", err)
	}
	mutator.assertLast(t, ctx, 1, batchMutation)
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
	var outboxCount, auditCount, commandCount int
	if err := row(`SELECT (SELECT count(*) FROM commands WHERE operation_id=$1),(SELECT count(*) FROM outbox_events b JOIN commands c ON c.id=b.command_id WHERE c.operation_id=$2),(SELECT count(*) FROM audit_events WHERE resource_id=$3)`, `SELECT (SELECT count(*) FROM commands WHERE operation_id=?),(SELECT count(*) FROM outbox_events b JOIN commands c ON c.id=b.command_id WHERE c.operation_id=?),(SELECT count(*) FROM audit_events WHERE resource_id=?)`, op, op, op).Scan(&commandCount, &outboxCount, &auditCount); err != nil || commandCount != 1 || outboxCount != 1 || auditCount != 1 {
		t.Fatal("batch recovery duplicated durable intent", commandCount, outboxCount, auditCount, err)
	}
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
	t.Run("batch-port-errors", func(t *testing.T) {
		for _, test := range []struct {
			name, errorType string
			err             error
		}{
			{"backlog", "", userstate.ErrBacklogExceeded},
			{"version", "stale_revision", userstate.ErrVersionConflict},
			{"pending", "revision_pending", userstate.ErrRevisionPending},
			{"recovery", "recovery_required", userstate.ErrRevisionRecovery},
			{"capability", "capability_unavailable", userstate.ErrCapabilityMissing},
			{"node", "node_unavailable", userstate.ErrNodeUnavailable},
			{"other", "submission_failed", errors.New("injected failure")},
		} {
			t.Run(test.name, func(t *testing.T) {
				request := BatchRequest{WorkspaceID: workspace, ActorIdentityID: requester, ActorSessionID: session, ActorID: "operator", Reason: "port errors", RequestID: "port-errors", Traceparent: testTraceparent, IdempotencyKey: uuid.NewString(), Items: []BatchItemRequest{{NodeID: node, Username: "dave", Action: "enable", ExpectedVersion: 1, Authorized: true}, {NodeID: node, Username: "charlie", Action: "enable", ExpectedVersion: 1, Authorized: true}}}
				batch, _, err := s.CreateBatch(ctx, request)
				if err != nil {
					t.Fatal(err)
				}
				mutator.requests, mutator.contexts = nil, nil
				mutator.err = fmt.Errorf("mutator: %w", test.err)
				if err := s.submitBatchItems(ctx, uuid.New(), 2); err != nil {
					t.Fatal("batch must classify per-item errors", err)
				}
				wantCalls, wantState := 2, "failed"
				if test.name == "backlog" {
					wantCalls, wantState = 1, "queued"
				}
				if len(mutator.requests) != wantCalls {
					t.Fatalf("Mutate calls=%d, want %d", len(mutator.requests), wantCalls)
				}
				for index, got := range mutator.requests {
					key := stableKey("batch", batch.ID.String(), fmt.Sprint(index))
					want := userstate.MutationRequest{NodeID: node, Kind: userstate.UserEnable, Name: request.Items[index].Username, ExpectedVersion: 1, IdempotencyKey: key, TTL: 24 * time.Hour, ActorID: request.ActorID, ActorIdentityID: requester, ActorSessionID: session, Reason: request.Reason, RequestID: request.RequestID + ":" + fmt.Sprint(index), Traceparent: request.Traceparent}
					if !reflect.DeepEqual(got, want) || mutator.contexts[index] != ctx {
						t.Fatalf("batch item %d request=%+v, want %+v", index, got, want)
					}
				}
				batch, err = s.GetBatch(ctx, batch.ID)
				if err != nil {
					t.Fatal(err)
				}
				for _, item := range batch.Items {
					if item.State != wantState || item.ErrorType != test.errorType || item.ChildOperationID != nil {
						t.Fatal("batch error result", item)
					}
				}
				var claims int
				if err := row(`SELECT count(*) FROM batch_operation_items WHERE batch_id=$1 AND (lease_owner IS NOT NULL OR lease_until IS NOT NULL)`, `SELECT count(*) FROM batch_operation_items WHERE batch_id=? AND (lease_owner IS NOT NULL OR lease_until IS NOT NULL)`, batch.ID).Scan(&claims); err != nil || claims != 0 {
					t.Fatal("batch error retained claims", claims, err)
				}
				// Retire only this injected-backlog fixture before the next case.
				exec(`UPDATE batch_operation_items SET state='failed' WHERE batch_id=$1`, `UPDATE batch_operation_items SET state='failed' WHERE batch_id=?`, batch.ID)
			})
		}
		mutator.err = nil
		mutator.requests, mutator.contexts = nil, nil
	})
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
	mutator.requests, mutator.contexts = nil, nil
	mutator.err = userstate.ErrBacklogExceeded
	if err := s.submitBatchItems(fenced, uuid.New(), 1); err != nil || len(mutator.contexts) != 1 || mutator.contexts[0] != fenced || coordination.FenceFromContext(mutator.contexts[0]) != leader {
		t.Fatal("mutation lost scheduler fencing context", err)
	}
	mutator.err = nil
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
	if len(mutator.requests) != 1 {
		t.Fatal("stale fence reached user mutator")
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
