package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/coordination"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/connection"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/mysql"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/useroperations"
	userstore "github.com/GentleKingson/ocservia/control-plane/internal/useroperations/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The startup harness supplies a dedicated PostgreSQL database or a unique
// MySQL database/account. These tests never revoke a shared database grant.
type policyCleanupState struct {
	owner, runtime *connection.Connection
	item           userstore.Candidate
	service        *useroperations.Service
	account        string
}

func seedPolicyCleanup(t *testing.T, ctx context.Context, owner, runtime *connection.Connection, backend, account string) *policyCleanupState {
	t.Helper()
	state := &policyCleanupState{owner: owner, runtime: runtime, account: account}
	workspace, node := uuid.New(), uuid.New()
	at, _ := value.FromTime(time.Now().UTC())
	period, _ := value.FromTime(time.Unix(0, 0))
	state.item = userstore.Candidate{NodeID: node, Username: "cleanup_orphan", UserVersion: 1, PolicyVersion: 1, Cause: "expiry", PeriodStart: period}
	queries := []string{
		`INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES($1,'cleanup',$2,$3,$4)`,
		`INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at)VALUES($1,$2,'cleanup','active',$3,$4)`,
		`INSERT INTO desired_users(node_id,username,enabled,version,revision,fingerprint,created_at,updated_at)VALUES($1,'cleanup_orphan',false,1,1,$2,$3,$4)`,
	}
	args := [][]any{{workspace, workspace.String(), at, at}, {node, workspace, at, at}, {node, make([]byte, 32), at, at}}
	if backend != "postgres" {
		queries = []string{
			`INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES(?,'cleanup',?,?,?)`,
			`INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at)VALUES(?,?,'cleanup','active',?,?)`,
			`INSERT INTO desired_users(node_id,username,enabled,version,revision,fingerprint,created_at,updated_at)VALUES(?,'cleanup_orphan',false,1,1,?,?,?)`,
		}
		for _, row := range args {
			for i, arg := range row {
				if id, ok := arg.(uuid.UUID); ok {
					row[i] = mysql.UUIDBytes(id)
				}
			}
		}
	}
	for i, query := range queries {
		if _, err := owner.Store.Exec(ctx, query, args[i]...); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.Within(ctx, runtime.Store, database.ReadCommitted, func(tx database.Tx) error {
		store, err := userstore.From(tx)
		if err != nil {
			return err
		}
		if err := store.PutPolicy(ctx, userstore.Policy{NodeID: node, Username: state.item.Username, QuotaPeriod: "none", QuotaDirection: "rxtx", Version: 1, ExpiresAt: at, CreatedAt: at, UpdatedAt: at}); err != nil {
			return err
		}
		return store.EnsureEnforcement(ctx, state.item, at)
	}); err != nil {
		t.Fatal(err)
	}
	// A disabled orphan must be cleaned without reaching the mutation port.
	state.service = useroperations.NewWithConcurrencyBackend(runtime.Store, nil, 1)
	return state
}

func (s *policyCleanupState) revoke(t *testing.T, ctx context.Context, backend string) {
	t.Helper()
	quoted := pgx.Identifier{s.account}.Sanitize()
	if backend != "postgres" {
		user, host, ok := strings.Cut(s.account, "@")
		if !ok {
			t.Fatal("fixture account must include host")
		}
		quoted = "'" + user + "'@'" + host + "'"
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.owner.GrantRuntimePrivileges(cleanup, s.account); err != nil {
			t.Error("restore isolated runtime authorization", err)
		}
	})
	if _, err := s.owner.Store.Exec(ctx, "REVOKE DELETE ON user_policy_enforcements FROM "+quoted); err != nil {
		t.Fatal(err)
	}
	if err := s.delete(ctx); !errors.Is(err, database.ErrPermission) {
		t.Fatal("effective runtime DELETE must be denied", err)
	}
}

func (s *policyCleanupState) delete(ctx context.Context) error {
	return database.Within(ctx, s.runtime.Store, database.ReadCommitted, func(tx database.Tx) error {
		store, err := userstore.From(tx)
		if err != nil {
			return err
		}
		return store.DeleteEnforcement(ctx, s.item, true)
	})
}

func (s *policyCleanupState) assertPermissions(t *testing.T, ctx context.Context, backend string) {
	t.Helper()
	var effective string
	if err := s.runtime.Store.QueryRow(ctx, `SELECT CURRENT_USER`).Scan(&effective); err != nil || effective != s.account {
		t.Fatal("unexpected effective runtime identity", effective, err)
	}
	if backend == "postgres" {
		var restricted bool
		if err := s.runtime.Store.QueryRow(ctx, `SELECT NOT rolsuper AND NOT rolcreaterole AND NOT rolcreatedb AND NOT rolbypassrls AND NOT pg_has_role(current_user,'ocservia_owner','MEMBER') AND has_table_privilege(current_user,'user_policy_enforcements','DELETE') AND NOT has_table_privilege(current_user,'user_policy_enforcements','DELETE WITH GRANT OPTION') FROM pg_roles WHERE rolname=current_user`).Scan(&restricted); err != nil || !restricted {
			t.Fatal("runtime inherited elevated privileges or lacks cleanup DELETE", restricted, err)
		}
	} else {
		var broad int
		if err := s.runtime.Store.QueryRow(ctx, `SELECT (SELECT count(*) FROM information_schema.USER_PRIVILEGES WHERE PRIVILEGE_TYPE<>'USAGE')+(SELECT count(*) FROM information_schema.SCHEMA_PRIVILEGES)+(SELECT count(*) FROM information_schema.TABLE_PRIVILEGES WHERE IS_GRANTABLE='YES')`).Scan(&broad); err != nil || broad != 0 {
			t.Fatal("runtime has global, schema, or grant-option privileges", broad, err)
		}
	}
	for _, query := range []string{
		"DELETE FROM desired_user_policies WHERE false", "DELETE FROM user_policy_mutations WHERE false",
		"DELETE FROM scheduler_leadership WHERE false", "DELETE FROM batch_operations WHERE false",
		"TRUNCATE TABLE user_policy_enforcements", "CREATE TABLE cleanup_forbidden(id int)",
		"ALTER TABLE user_policy_enforcements ADD COLUMN cleanup_forbidden int",
	} {
		if _, err := s.runtime.Store.Exec(ctx, query); !errors.Is(err, database.ErrPermission) {
			t.Fatalf("runtime privilege boundary: %s: %v", query, err)
		}
	}
}

func (s *policyCleanupState) grants(t *testing.T, ctx context.Context, backend string) []string {
	t.Helper()
	query := `SELECT table_name,privilege_type,is_grantable FROM information_schema.table_privileges WHERE grantee=current_user AND table_schema='public' ORDER BY table_name,privilege_type`
	var args []any
	if backend != "postgres" {
		user, host, _ := strings.Cut(s.account, "@")
		query = `SELECT TABLE_NAME,PRIVILEGE_TYPE,IS_GRANTABLE FROM information_schema.TABLE_PRIVILEGES WHERE GRANTEE=? AND TABLE_SCHEMA=DATABASE() ORDER BY TABLE_NAME,PRIVILEGE_TYPE`
		args = []any{"'" + user + "'@'" + host + "'"}
	}
	rows, err := s.runtime.Store.Query(ctx, query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var grants []string
	for rows.Next() {
		var table, privilege, grantable string
		if err := rows.Scan(&table, &privilege, &grantable); err != nil {
			t.Fatal(err)
		}
		grants = append(grants, table+":"+privilege+":"+grantable)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return grants
}

func checkCleanupCLIRecovery(t *testing.T, ctx context.Context, owner, runtime *connection.Connection, backend, account string, migrate func() error) {
	t.Helper()
	state := seedPolicyCleanup(t, ctx, owner, runtime, backend, account)
	state.assertPermissions(t, ctx, backend)
	newGrants := state.grants(t, ctx, backend)
	var oldGrants []string
	for _, grant := range newGrants {
		if grant != "user_policy_enforcements:DELETE:NO" {
			oldGrants = append(oldGrants, grant)
		}
	}
	if len(newGrants) != len(oldGrants)+1 {
		t.Fatal("missing exact table-level DELETE grant", newGrants)
	}
	// New-install authorization works through the real Store, then recreate
	// the old unfinished receipt and old permissions without changing schema.
	if err := state.delete(ctx); err != nil {
		t.Fatal("new runtime cleanup", err)
	}
	if err := database.Within(ctx, runtime.Store, database.ReadCommitted, func(tx database.Tx) error {
		store, err := userstore.From(tx)
		if err != nil {
			return err
		}
		at, _ := value.FromTime(time.Now().UTC())
		return store.EnsureEnforcement(ctx, state.item, at)
	}); err != nil {
		t.Fatal(err)
	}
	state.revoke(t, ctx, backend)
	if got := state.grants(t, ctx, backend); !reflect.DeepEqual(got, oldGrants) {
		t.Fatal("old runtime grant fixture differs by more than cleanup DELETE", got)
	}
	ticks := make(chan time.Time)
	log := schedulerLog{quietLogger().Handler(), make(chan slog.Record, 16)}
	logger := slog.New(log)
	identity, _ := coordination.NewIdentity()
	leader := coordination.NewRunnerBackend(runtime.Store, identity, 15*time.Second, 5*time.Second, logger)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var steps []string
	completed := make(chan struct{}, 1)
	step := func(name string) func(context.Context) error {
		return func(context.Context) error { steps = append(steps, name); return nil }
	}
	work := maintenanceWork{users: state.service.RunOnce, rollouts: step("rollouts"), telemetry: step("telemetry"), certificates: step("certificates"), audit: step("audit")}
	work.evidence = func(context.Context, *coordination.Session) error {
		steps = append(steps, "evidence")
		completed <- struct{}{}
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- runScheduler(runCtx, leader, ticks, work, 1, logger) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Error("scheduler shutdown", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("scheduler failed to stop")
		}
		// The next lifecycle case must not wait for this fixture's lease.
		query := `UPDATE scheduler_leadership SET lease_until='-infinity' WHERE id=1`
		if backend != "postgres" {
			query = `UPDATE scheduler_leadership SET lease_until=-9223372036854775808 WHERE id=1`
		}
		if _, err := owner.Store.Exec(ctx, query); err != nil {
			t.Error(err)
		}
	}()
	for {
		select {
		case record := <-log.records:
			if record.Message == "user operations scheduler completed" {
				t.Fatal("failed pass logged success")
			}
			if record.Level != slog.LevelError {
				continue
			}
			var cleanup *useroperations.EnforcementCleanupError
			record.Attrs(func(attr slog.Attr) bool {
				if attr.Key == "error" {
					errors.As(attr.Value.Any().(error), &cleanup)
				}
				return true
			})
			if cleanup == nil || !errors.Is(cleanup, database.ErrPermission) || len(steps) != 0 {
				t.Fatal("wrong failure or later maintenance executed", record, steps)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("real cleanup failure not observed")
		}
		break
	}
	for i := 0; i < 2; i++ {
		if err := migrate(); err != nil {
			t.Fatal("existing CLI reauthorization", i, err)
		}
		state.assertPermissions(t, ctx, backend)
		if got := state.grants(t, ctx, backend); !reflect.DeepEqual(got, newGrants) {
			t.Fatal("reauthorization changed unrelated grants", got)
		}
	}
	select {
	case ticks <- time.Now():
	case <-time.After(5 * time.Second):
		t.Fatal("scheduler exited rather than waiting for next tick")
	}
	select {
	case <-completed:
		if !reflect.DeepEqual(steps, []string{"rollouts", "telemetry", "certificates", "audit", "evidence"}) {
			t.Fatal("maintenance recovery order", steps)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("maintenance did not recover")
	}
	var count int
	query := `SELECT count(*) FROM user_policy_enforcements WHERE node_id=$1`
	var node any = state.item.NodeID
	if backend != "postgres" {
		query, node = `SELECT count(*) FROM user_policy_enforcements WHERE node_id=?`, mysql.UUIDBytes(state.item.NodeID)
	}
	if err := runtime.Store.QueryRow(ctx, query, node).Scan(&count); err != nil || count != 0 {
		t.Fatal("old receipt not cleaned", count, err)
	}
	t.Log(fmt.Sprintf("%s: restricted runtime cleanup, old-grant CLI repair twice, next-tick maintenance and cancellation verified", backend))
}
