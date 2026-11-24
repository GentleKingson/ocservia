package app

import (
	"context"
	"errors"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/connection"
	"github.com/GentleKingson/ocservia/control-plane/internal/useroperations"
	userstore "github.com/GentleKingson/ocservia/control-plane/internal/useroperations/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The startup harness supplies a dedicated PostgreSQL database. These tests
// never revoke a shared database grant.
type policyCleanupState struct {
	owner, runtime *connection.Connection
	item           userstore.Candidate
	service        *useroperations.Service
	account        string
}

func seedPolicyCleanup(t *testing.T, ctx context.Context, owner, runtime *connection.Connection, account string) *policyCleanupState {
	t.Helper()
	state := &policyCleanupState{owner: owner, runtime: runtime, account: account}
	workspace, node := uuid.New(), uuid.New()
	at := time.Now().UTC()
	period := time.Unix(0, 0).UTC()
	state.item = userstore.Candidate{NodeID: node, Username: "cleanup_orphan", UserVersion: 1, PolicyVersion: 1, Cause: "expiry", PeriodStart: period}
	queries := []string{
		`INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES($1,'cleanup',$2,$3,$4)`,
		`INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at)VALUES($1,$2,'cleanup','active',$3,$4)`,
		`INSERT INTO desired_users(node_id,username,enabled,version,revision,created_at,updated_at)VALUES($1,'cleanup_orphan',false,1,1,$2,$3)`,
	}
	args := [][]any{{workspace, workspace.String(), at, at}, {node, workspace, at, at}, {node, at, at}}
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
		exp := at
		if err := store.PutPolicy(ctx, userstore.Policy{NodeID: node, Username: state.item.Username, QuotaPeriod: "none", QuotaDirection: "rxtx", Version: 1, ExpiresAt: &exp, CreatedAt: at, UpdatedAt: at}); err != nil {
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

func (s *policyCleanupState) revoke(t *testing.T, ctx context.Context) {
	t.Helper()
	quoted := pgx.Identifier{s.account}.Sanitize()
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

func (s *policyCleanupState) assertPermissions(t *testing.T, ctx context.Context) {
	t.Helper()
	var effective string
	if err := s.runtime.Store.QueryRow(ctx, `SELECT CURRENT_USER`).Scan(&effective); err != nil || effective != s.account {
		t.Fatal("unexpected effective runtime identity", effective, err)
	}
	var restricted bool
	if err := s.runtime.Store.QueryRow(ctx, `SELECT NOT rolsuper AND NOT rolcreaterole AND NOT rolcreatedb AND NOT rolbypassrls AND NOT pg_has_role(current_user,'ocservia_owner','MEMBER') AND has_table_privilege(current_user,'user_policy_enforcements','DELETE') AND NOT has_table_privilege(current_user,'user_policy_enforcements','DELETE WITH GRANT OPTION') FROM pg_roles WHERE rolname=current_user`).Scan(&restricted); err != nil || !restricted {
		t.Fatal("runtime inherited elevated privileges or lacks cleanup DELETE", restricted, err)
	}
	for _, query := range []string{
		"DELETE FROM desired_user_policies WHERE false", "DELETE FROM user_policy_mutations WHERE false",
		"DELETE FROM observed_user_usage WHERE false",
		"TRUNCATE TABLE user_policy_enforcements", "CREATE TABLE cleanup_forbidden(id int)",
		"ALTER TABLE user_policy_enforcements ADD COLUMN cleanup_forbidden int",
	} {
		if _, err := s.runtime.Store.Exec(ctx, query); !errors.Is(err, database.ErrPermission) {
			t.Fatalf("runtime privilege boundary: %s: %v", query, err)
		}
	}
}

func (s *policyCleanupState) grants(t *testing.T, ctx context.Context) []string {
	t.Helper()
	query := `SELECT table_name,privilege_type,is_grantable FROM information_schema.table_privileges WHERE grantee=current_user AND table_schema='public' ORDER BY table_name,privilege_type`
	rows, err := s.runtime.Store.Query(ctx, query)
	testutil.NoError(t, err)
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

func checkCleanupCLIRecovery(t *testing.T, ctx context.Context, owner, runtime *connection.Connection, account string, migrate func() error) {
	t.Helper()
	state := seedPolicyCleanup(t, ctx, owner, runtime, account)
	state.assertPermissions(t, ctx)
	newGrants := state.grants(t, ctx)
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
		return store.EnsureEnforcement(ctx, state.item, time.Now().UTC())
	}); err != nil {
		t.Fatal(err)
	}
	state.revoke(t, ctx)
	if got := state.grants(t, ctx); !reflect.DeepEqual(got, oldGrants) {
		t.Fatal("old runtime grant fixture differs by more than cleanup DELETE", got)
	}
	ticks := make(chan time.Time)
	log := schedulerLog{quietLogger().Handler(), make(chan slog.Record, 16)}
	logger := slog.New(log)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var steps []string
	completed := make(chan []string, 1)
	step := func(name string) func(context.Context) error {
		return func(context.Context) error { steps = append(steps, name); return nil }
	}
	work := maintenanceWork{
		users:        state.service.RunOnce,
		rollouts:     step("rollouts"),
		telemetry:    step("telemetry"),
		certificates: step("certificates"),
		audit: func(context.Context) error {
			steps = append(steps, "audit")
			completed <- steps
			steps = nil
			return nil
		},
	}
	done := make(chan error, 1)
	go func() { done <- runScheduler(runCtx, ticks, work, 1, logger) }()
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
			if cleanup == nil || !errors.Is(cleanup, database.ErrPermission) {
				t.Fatal("wrong failure", record)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("real cleanup failure not observed")
		}
		break
	}
	wantSteps := []string{"rollouts", "telemetry", "certificates", "audit"}
	select {
	case pass := <-completed:
		if !reflect.DeepEqual(pass, wantSteps) {
			t.Fatal("maintenance did not continue after policy cleanup failure", pass)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("policy cleanup prevented independent maintenance")
	}
	for i := 0; i < 2; i++ {
		if err := migrate(); err != nil {
			t.Fatal("existing CLI reauthorization", i, err)
		}
		state.assertPermissions(t, ctx)
		if got := state.grants(t, ctx); !reflect.DeepEqual(got, newGrants) {
			t.Fatal("reauthorization changed unrelated grants", got)
		}
	}
	select {
	case ticks <- time.Now():
	case <-time.After(5 * time.Second):
		t.Fatal("scheduler exited rather than waiting for next tick")
	}
	select {
	case pass := <-completed:
		if !reflect.DeepEqual(pass, wantSteps) {
			t.Fatal("maintenance recovery order", pass)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("maintenance did not recover")
	}
	var count int
	if err := runtime.Store.QueryRow(ctx, `SELECT count(*) FROM user_policy_enforcements WHERE node_id=$1`, state.item.NodeID).Scan(&count); err != nil || count != 0 {
		t.Fatal("old receipt not cleaned", count, err)
	}
	t.Log("postgres: restricted runtime cleanup, old-grant CLI repair twice, next-tick maintenance and cancellation verified")
}
