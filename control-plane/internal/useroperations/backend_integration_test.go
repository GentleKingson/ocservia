package useroperations

import (
	"context"
	"errors"
	"fmt"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/coordination"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/testfixture"
	userstore "github.com/GentleKingson/ocservia/control-plane/internal/useroperations/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/userstate"
	"github.com/google/uuid"
)

func userOperationsBackend(t *testing.T) (database.Backend, database.Backend) {
	t.Helper()
	return userOperationsBackendWithCleanup(t, t)
}

func userOperationsBackendWithCleanup(t, cleanupT *testing.T) (database.Backend, database.Backend) {
	t.Helper()
	runtime, owner := testfixture.OpenIsolated(t, cleanupT, "policy_cleanup")
	return postgres.WrapPool(runtime), postgres.WrapPool(owner)
}

func TestUserOperationsBackendIntegration(t *testing.T) {
	b, _ := userOperationsBackend(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := b.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	row := func(query string, args ...any) database.Row {
		return b.QueryRow(ctx, query, args...)
	}
	stamp := func(v time.Time) value.Timestamp {
		t.Helper()
		at, err := value.FromTime(v)
		testutil.NoError(t, err)
		return at
	}
	workspace, node, requester, session := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC().Truncate(time.Second)
	at, expires := stamp(now), stamp(now.Add(time.Hour))
	exec(`INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES($1,'policies',$2,now(),now())`, workspace, workspace.String())
	exec(`INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at)VALUES($1,$2,'policies','active',now(),now())`, node, workspace)
	exec(`INSERT INTO node_capabilities(node_id,capability,approved)VALUES($1,'ocserv.users.write',true)`, node)
	for _, id := range []uuid.UUID{requester} {
		exec(`INSERT INTO identities(id,issuer,subject,created_at,updated_at)VALUES($1,'user-operations',$2,$3,$4)`, id, id.String(), at, at)
		exec(`INSERT INTO workspace_admins(workspace_id,identity_id,created_at)VALUES($1,$2,$3)`, workspace, id, at)
	}
	for _, pair := range [][2]uuid.UUID{{session, requester}} {
		exec(`INSERT INTO auth_sessions(id,identity_id,expires_at,created_at)VALUES($1,$2,$3,$4)`, pair[0], pair[1], expires, at)
	}
	for _, name := range []string{"alice", "bob", "charlie", "dave", "expired"} {
		exec(`INSERT INTO desired_users(node_id,username,enabled,version,revision,fingerprint,created_at,updated_at)VALUES($1,$2,true,1,1,$3,$4,$5)`, node, name, make([]byte, 32), at, at)
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
			{"revoked after selection", userstate.ErrNodeUnavailable, 0, false},
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
				if err := row(`SELECT count(*) FROM user_policy_enforcements WHERE node_id=$1 AND username='alice' AND cause=$2 AND operation_id IS NULL`, node, cause).Scan(&count); err != nil || count != test.retained {
					t.Fatal("enforcement cleanup", count, err)
				}
			})
		}
		mutator.err = nil
		mutator.requests, mutator.contexts = nil, nil
	}
	checkRevokedCandidates := func(t *testing.T, run func(context.Context, int) (int, error)) {
		t.Helper()
		exec(`UPDATE nodes SET status='revoked' WHERE id=$1`, node)
		defer exec(`UPDATE nodes SET status='active' WHERE id=$1`, node)
		mutator.requests, mutator.contexts = nil, nil
		if n, err := run(ctx, 10); err != nil || n != 0 || len(mutator.requests) != 0 {
			t.Fatalf("revoked policy candidate: processed=%d mutations=%d err=%v", n, len(mutator.requests), err)
		}
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
	key := operationIdempotencyKey("policy", node.String(), "alice", "1", "quota", monthStart(now).Format(time.RFC3339))
	mutation := userstate.MutationRequest{NodeID: node, Kind: userstate.UserDisable, Name: "alice", ExpectedVersion: 1, IdempotencyKey: key, TTL: 24 * time.Hour, ActorID: "scheduler", Reason: "quota or expiry policy enforcement", RequestID: key, Traceparent: stableTraceparent(key)}
	t.Run("quota-revoked-node", func(t *testing.T) { checkRevokedCandidates(t, s.enforcePolicies) })
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
	if err := row(`SELECT count(*) FROM commands WHERE node_id=$1 AND resource_key='alice'`, node).Scan(&count); err != nil || count != 1 {
		t.Fatal("duplicate command", count, err)
	}
	exec(`INSERT INTO observed_users(node_id,username,enabled,revision,fingerprint,observed_at)VALUES($1,'alice',false,2,$2,$3)`, node, make([]byte, 32), at)
	exec(`UPDATE commands SET state='succeeded' WHERE node_id=$1 AND resource_key='alice'`, node)
	now = monthStart(now).AddDate(0, 1, 0).Add(time.Second)
	resetKey := operationIdempotencyKey("policy-reset", node.String(), "alice", "1", monthStart(now).Format(time.RFC3339))
	resetRequest := userstate.MutationRequest{NodeID: node, Kind: userstate.UserEnable, Name: "alice", ExpectedVersion: 2, IdempotencyKey: resetKey, TTL: 24 * time.Hour, ActorID: "scheduler", Reason: "monthly quota reset", RequestID: resetKey, Traceparent: stableTraceparent(resetKey)}
	t.Run("reset-revoked-node", func(t *testing.T) { checkRevokedCandidates(t, s.resetMonthlyPolicies) })
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
	if err := row(`SELECT enabled,version FROM desired_users WHERE node_id=$1 AND username='alice'`, node).Scan(&enabled, &version); err != nil || !enabled || version != 3 {
		t.Fatal("reset state", enabled, version, err)
	}
	exec(`UPDATE user_policy_enforcements SET operation_id=NULL,resulting_user_version=NULL WHERE node_id=$1 AND cause='quota_reset'`, node)
	if n, err := s.resetMonthlyPolicies(ctx, 10); err != nil || n != 1 {
		t.Fatal("recover reset receipt", n, err)
	}
	mutator.assertLast(t, ctx, 1, resetRequest)
	exec(`UPDATE commands SET state='succeeded' WHERE node_id=$1 AND resource_key='alice'`, node)
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
	exec(`UPDATE desired_user_policies SET expires_at=$1 WHERE node_id=$2`, positive, node)
	policy, err = s.GetPolicy(ctx, node, "alice")
	if err != nil || policy.Expired || policy.ExpiresAt == nil || *policy.ExpiresAt != positive {
		t.Fatal("infinite expiry", policy, err)
	}
	exec(`UPDATE desired_user_policies SET expires_at=$1 WHERE node_id=$2`, negative, node)
	policy, err = s.GetPolicy(ctx, node, "alice")
	if err != nil || !policy.Expired {
		t.Fatal("negative expiry", policy, err)
	}
	exec(`UPDATE desired_user_policies SET expires_at=NULL WHERE node_id=$1`, node)
	expiredAt := now.Add(-time.Hour)
	if _, _, err := s.SetPolicy(ctx, PolicyRequest{NodeID: node, Username: "expired", QuotaPeriod: "none", QuotaDirection: "rxtx", ExpiresAt: &expiredAt, IdempotencyKey: "expired-policy", ActorID: "operator", Reason: "expiry", RequestID: "expired-policy", Traceparent: testTraceparent}); err != nil {
		t.Fatal("expiry policy", err)
	}
	mutator.requests, mutator.contexts = nil, nil
	if n, err := s.enforcePolicies(ctx, 10); err != nil || n != 1 {
		t.Fatal("expiry enforcement", n, err)
	}
	expiryKey := operationIdempotencyKey("policy", node.String(), "expired", "1", "expiry", "1970-01-01T00:00:00Z")
	mutator.assertLast(t, ctx, 1, userstate.MutationRequest{NodeID: node, Kind: userstate.UserDisable, Name: "expired", ExpectedVersion: 1, IdempotencyKey: expiryKey, TTL: 24 * time.Hour, ActorID: "scheduler", Reason: "quota or expiry policy enforcement", RequestID: expiryKey, Traceparent: stableTraceparent(expiryKey)})
	identity, err := coordination.NewIdentity()
	testutil.NoError(t, err)
	leader, err := coordination.AcquireBackend(ctx, b, identity, time.Minute)
	testutil.NoError(t, err)
	fenced := coordination.WithFence(ctx, leader)
	if err := s.RunOnce(fenced); err != nil {
		t.Fatal("fenced scheduler", err)
	}
	exec(`UPDATE scheduler_leadership SET epoch=epoch+1 WHERE id=1`)
	if _, _, err := s.SetPolicy(fenced, request); !errors.Is(err, coordination.ErrNotLeader) {
		t.Fatal("fenced policy replay", err)
	}
	if err := s.RunOnce(fenced); !errors.Is(err, coordination.ErrNotLeader) {
		t.Fatal("fenced scheduler", err)
	}
	if _, err := audit.NewBackendManager(b, nil).Verify(ctx, workspace); err != nil {
		t.Fatal("audit chain", err)
	}
}
