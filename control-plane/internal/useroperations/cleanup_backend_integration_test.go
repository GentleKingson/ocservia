package useroperations

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/coordination"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/mysql"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	userstore "github.com/GentleKingson/ocservia/control-plane/internal/useroperations/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/userstate"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type cleanupFixture struct {
	b, owner database.Backend
	s        *Service
	node     uuid.UUID
	op       uuid.UUID
	now      time.Time
	at       value.Timestamp
	my       bool
}

func newCleanupFixture(t *testing.T) *cleanupFixture {
	t.Helper()
	b, owner := userOperationsBackend(t)
	_, my := b.(*mysql.Backend)
	f := &cleanupFixture{b: b, owner: owner, node: uuid.New(), op: uuid.New(), now: time.Now().UTC().Truncate(time.Second), my: my}
	f.at, _ = value.FromTime(f.now)
	f.s = NewWithConcurrencyBackend(b, userstate.NewWithSignerBackend(b, commandauth.NewSignerFromSeed([32]byte{3})), 1)
	f.s.now = func() time.Time { return f.now }
	workspace := uuid.New()
	f.exec(t, `INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES($1,'cleanup',$2,$3,$4)`, `INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES(?,'cleanup',?,?,?)`, workspace, workspace.String(), f.at, f.at)
	f.exec(t, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at)VALUES($1,$2,'cleanup','active',$3,$4)`, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at)VALUES(?,?,'cleanup','active',?,?)`, f.node, workspace, f.at, f.at)
	f.exec(t, `INSERT INTO node_capabilities(node_id,capability,approved)VALUES($1,'ocserv.users.write',true)`, `INSERT INTO node_capabilities(node_id,capability,approved)VALUES(?,'ocserv.users.write',true)`, f.node)
	f.exec(t, `INSERT INTO operations(id,workspace_id,node_id,state,version,request_id,idempotency_key,request_hash,created_at,updated_at)VALUES($1,$2,$3,'succeeded',1,'prior','prior',$4,$5,$6)`, `INSERT INTO operations(id,workspace_id,node_id,state,version,request_id,idempotency_key,request_hash,created_at,updated_at)VALUES(?,?,?,'succeeded',1,'prior','prior',?,?,?)`, f.op, workspace, f.node, make([]byte, 32), f.at, f.at)
	return f
}

func (f *cleanupFixture) query(pg, my string, args []any) (string, []any) {
	if f.my {
		for i, arg := range args {
			if id, ok := arg.(uuid.UUID); ok {
				args[i] = mysql.UUIDBytes(id)
			}
		}
		return my, args
	}
	return pg, args
}

func (f *cleanupFixture) exec(t *testing.T, pg, my string, args ...any) {
	t.Helper()
	q, args := f.query(pg, my, args)
	if _, err := f.owner.Exec(t.Context(), q, args...); err != nil {
		t.Fatal(err)
	}
}

func (f *cleanupFixture) seed(t *testing.T, name string, enabled, reset bool) userstore.Candidate {
	t.Helper()
	f.exec(t, `INSERT INTO desired_users(node_id,username,enabled,version,revision,fingerprint,created_at,updated_at)VALUES($1,$2,$3,2,2,$4,$5,$6)`, `INSERT INTO desired_users(node_id,username,enabled,version,revision,fingerprint,created_at,updated_at)VALUES(?,?,?,2,2,?,?,?)`, f.node, name, enabled, make([]byte, 32), f.at, f.at)
	period, _ := value.FromTime(time.Unix(0, 0))
	policy := userstore.Policy{NodeID: f.node, Username: name, Version: 1, QuotaPeriod: "none", QuotaDirection: "rxtx", ExpiresAt: f.at, CreatedAt: f.at, UpdatedAt: f.at}
	cause := "expiry"
	if reset {
		period, _ = value.FromTime(monthStart(f.now))
		policy.QuotaPeriod, policy.QuotaBytes, policy.ExpiresAt = "monthly", 100, value.Timestamp{}
		cause = "quota_reset"
	}
	item := userstore.Candidate{NodeID: f.node, Username: name, PolicyVersion: 1, UserVersion: 2, Cause: cause, PeriodStart: period, Enabled: enabled}
	if err := f.s.withStore(t.Context(), func(store userstore.Store) error {
		if err := store.PutPolicy(t.Context(), policy); err != nil {
			return err
		}
		if reset {
			prior := item
			prior.Cause = "quota"
			prior.PeriodStart, _ = value.FromTime(monthStart(f.now).AddDate(0, -1, 0))
			if err := store.EnsureEnforcement(t.Context(), prior, f.at); err != nil {
				return err
			}
			if err := store.CompleteEnforcement(t.Context(), prior, f.op, 2); err != nil {
				return err
			}
		}
		return store.EnsureEnforcement(t.Context(), item, f.at)
	}); err != nil {
		t.Fatal(err)
	}
	return item
}

func (f *cleanupFixture) count(t *testing.T, item userstore.Candidate) int {
	t.Helper()
	q, args := f.query(`SELECT count(*) FROM user_policy_enforcements WHERE node_id=$1 AND username=$2 AND policy_version=$3 AND cause=$4 AND period_start=$5`, `SELECT count(*) FROM user_policy_enforcements WHERE node_id=? AND BINARY username=? AND policy_version=? AND cause=? AND period_start=?`, []any{item.NodeID, item.Username, item.PolicyVersion, item.Cause, item.PeriodStart})
	var n int
	if err := f.b.QueryRow(t.Context(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *cleanupFixture) revokeDelete(t *testing.T) func() {
	t.Helper()
	var account, schema string
	var revoke, grant string
	if f.my {
		if err := f.b.QueryRow(t.Context(), `SELECT CURRENT_USER(),DATABASE()`).Scan(&account, &schema); err != nil || account != "ocservia_app@%" {
			t.Fatal("expected restricted effective user@host", account, err)
		}
		revoke = "REVOKE DELETE ON `" + schema + "`.user_policy_enforcements FROM 'ocservia_app'@'%'"
		grant = "GRANT DELETE ON `" + schema + "`.user_policy_enforcements TO 'ocservia_app'@'%'"
	} else {
		if err := f.b.QueryRow(t.Context(), `SELECT current_user`).Scan(&account); err != nil {
			t.Fatal(err)
		}
		revoke = "REVOKE DELETE ON user_policy_enforcements FROM " + pgx.Identifier{account}.Sanitize()
		grant = "GRANT DELETE ON user_policy_enforcements TO " + pgx.Identifier{account}.Sanitize()
	}
	restore := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := f.owner.Exec(ctx, grant); err != nil {
			t.Error("restore isolated fixture grant", err)
		}
	}
	t.Cleanup(restore)
	if _, err := f.owner.Exec(t.Context(), revoke); err != nil {
		t.Fatal(err)
	}
	return restore
}

func TestEnforcementCleanupBackendIntegration(t *testing.T) {
	for _, name := range []string{"enforce-orphan", "enforce-conflict", "reset-orphan", "reset-conflict"} {
		t.Run(name, func(t *testing.T) {
			f := newCleanupFixture(t)
			reset, conflict := strings.HasPrefix(name, "reset"), strings.HasSuffix(name, "conflict")
			item := f.seed(t, "alice", reset != conflict, reset)
			mutator := &recordingUserMutator{err: userstate.ErrVersionConflict}
			f.s.users = mutator
			run := f.s.enforcePolicies
			if reset {
				run = f.s.resetMonthlyPolicies
			}
			restore := f.revokeDelete(t)
			for i := 0; i < 2; i++ {
				var cleanup *EnforcementCleanupError
				n, err := run(t.Context(), 1)
				if n != 0 || !errors.As(err, &cleanup) || !errors.Is(err, database.ErrPermission) || cleanup.NodeID != item.NodeID || cleanup.Cause != item.Cause || f.count(t, item) != 1 {
					t.Fatal("cleanup failure was lost or removed recovery record", n, err)
				}
			}
			restore()
			if n, err := run(t.Context(), 1); err != nil || n != 0 || f.count(t, item) != 0 {
				t.Fatal("cleanup did not recover", n, err)
			}
			wantCalls := 0
			if conflict {
				wantCalls = 3
			}
			if len(mutator.requests) != wantCalls {
				t.Fatal("unexpected mutations", len(mutator.requests))
			}
			for _, ctx := range mutator.contexts {
				if ctx != t.Context() {
					t.Fatal("lost business context")
				}
			}
			var commands int
			if err := f.b.QueryRow(t.Context(), `SELECT count(*) FROM commands`).Scan(&commands); err != nil || commands != 0 {
				t.Fatal("cleanup created commands", commands, err)
			}
		})
	}
	t.Run("conditional-delete-and-fencing", func(t *testing.T) {
		f := newCleanupFixture(t)
		item := f.seed(t, "alice", false, false)
		variants := []userstore.Candidate{item, item, item, item, item, item}
		variants[0].NodeID = uuid.New()
		variants[1].Username = "bob"
		variants[2].PolicyVersion++
		variants[3].Cause = "quota"
		variants[4].PeriodStart.Micros++
		variants[5].PolicyVersion += 2
		// Related fixture rows need their actual FK parents, but no policy.
		f.exec(t, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) SELECT $1,workspace_id,'other','active',created_at,updated_at FROM nodes WHERE id=$2`, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) SELECT ?,workspace_id,'other','active',created_at,updated_at FROM nodes WHERE id=?`, variants[0].NodeID, f.node)
		for _, v := range []userstore.Candidate{variants[0], variants[1]} {
			f.exec(t, `INSERT INTO desired_users(node_id,username,enabled,version,revision,fingerprint,created_at,updated_at)VALUES($1,$2,false,2,2,$3,$4,$5)`, `INSERT INTO desired_users(node_id,username,enabled,version,revision,fingerprint,created_at,updated_at)VALUES(?,?,false,2,2,?,?,?)`, v.NodeID, v.Username, make([]byte, 32), f.at, f.at)
		}
		for _, v := range variants {
			if err := f.s.withStore(t.Context(), func(store userstore.Store) error { return store.EnsureEnforcement(t.Context(), v, f.at) }); err != nil {
				t.Fatal(err)
			}
		}
		completed := variants[5]
		if err := f.s.withStore(t.Context(), func(store userstore.Store) error { return store.CompleteEnforcement(t.Context(), completed, f.op, 2) }); err != nil {
			t.Fatal(err)
		}
		wrongSource := item
		wrongSource.UserVersion++
		if err := f.s.cleanupEnforcement(t.Context(), wrongSource, true); err != nil || f.count(t, item) != 1 {
			t.Fatal("source mismatch was deleted or treated as failure", err)
		}
		identity, _ := coordination.NewIdentity()
		leader, err := coordination.AcquireBackend(t.Context(), f.b, identity, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		f.exec(t, `UPDATE scheduler_leadership SET epoch=epoch+1 WHERE id=1`, `UPDATE scheduler_leadership SET epoch=epoch+1 WHERE id=1`)
		if err := f.s.cleanupEnforcement(coordination.WithFence(t.Context(), leader), item, true); !errors.Is(err, coordination.ErrNotLeader) || f.count(t, item) != 1 {
			t.Fatal("stale fencing delete committed", err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := f.s.cleanupEnforcement(ctx, item, false); !errors.Is(err, context.Canceled) || f.count(t, item) != 1 {
			t.Fatal("canceled cleanup succeeded", err)
		}
		failure := errors.New("injected commit failure")
		broken := NewBackend(cleanupCommitBackend{Backend: f.b, failure: failure}, nil)
		if err := broken.cleanupEnforcement(t.Context(), item, false); !errors.Is(err, failure) || f.count(t, item) != 1 {
			t.Fatal("commit failure lost rollback", err)
		}
		if err := f.s.cleanupEnforcement(t.Context(), item, true); err != nil || f.count(t, item) != 0 {
			t.Fatal("matching cleanup failed", err)
		}
		if err := f.s.cleanupEnforcement(t.Context(), item, true); err != nil {
			t.Fatal("already deleted is not an error", err)
		}
		if err := f.s.cleanupEnforcement(t.Context(), completed, false); err != nil {
			t.Fatal(err)
		}
		for _, v := range variants {
			if f.count(t, v) != 1 {
				t.Fatalf("deleted unrelated or completed record: %+v", v)
			}
		}
	})
	t.Run("bounded-candidate-progress", func(t *testing.T) {
		f := newCleanupFixture(t)
		old := f.seed(t, "a_orphan", false, false)
		next := f.seed(t, "z_valid", true, false)
		identity, _ := coordination.NewIdentity()
		leader, err := coordination.AcquireBackend(t.Context(), f.b, identity, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		ctx := coordination.WithFence(t.Context(), leader)
		restore := f.revokeDelete(t)
		if err := f.s.RunOnce(ctx); !errors.Is(err, database.ErrPermission) || f.count(t, old) != 1 {
			t.Fatal("old residue failure missing", err)
		}
		restore()
		for i := 0; i < 3; i++ {
			if err := f.s.RunOnce(ctx); err != nil {
				t.Fatal("next pass", err)
			}
		}
		if f.count(t, old) != 0 || f.count(t, next) != 1 {
			t.Fatal("bounded scan did not advance")
		}
		var commands, receipts int
		if err := f.b.QueryRow(t.Context(), `SELECT count(*) FROM commands`).Scan(&commands); err != nil || commands != 1 {
			t.Fatal("next candidate missing or duplicated", commands, err)
		}
		if err := f.b.QueryRow(t.Context(), `SELECT count(*) FROM user_policy_enforcements WHERE operation_id IS NOT NULL`).Scan(&receipts); err != nil || receipts != 1 {
			t.Fatal("missing recovery receipt", receipts, err)
		}
	})
}

type cleanupCommitBackend struct {
	database.Backend
	failure error
}

func (b cleanupCommitBackend) Begin(ctx context.Context, isolation database.Isolation) (database.Tx, error) {
	tx, err := b.Backend.Begin(ctx, isolation)
	if err != nil {
		return nil, err
	}
	store, err := userstore.From(tx)
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return cleanupCommitTx{Tx: tx, store: store, failure: b.failure}, nil
}

type cleanupCommitTx struct {
	database.Tx
	store   userstore.Store
	failure error
}

func (tx cleanupCommitTx) UserOperationsStore() userstore.Store { return tx.store }
func (tx cleanupCommitTx) Commit(context.Context) error         { return fmt.Errorf("commit: %w", tx.failure) }
