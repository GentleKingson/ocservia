package mysql

import (
	"context"
	"errors"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/semantictest"
	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
)

func TestRealObservedStateTransactions(t *testing.T) {
	owner := typeFixture(t)
	ctx := context.Background()
	if err := owner.GrantTestPrivileges(ctx); err != nil {
		t.Fatal(err)
	}
	options := testOptions(t)
	config, err := driver.ParseDSN(options.DSN)
	if err != nil {
		t.Fatal(err)
	}
	if err = owner.QueryRow(ctx, `SELECT DATABASE()`).Scan(&config.DBName); err != nil {
		t.Fatal(err)
	}
	config.User, config.Passwd = "ocservia_app", "pr02-runtime-test-only"
	options.DSN = config.FormatDSN()
	b, err := Open(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	workspace, node := uuid.New(), uuid.New()
	if _, err = b.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES(?,'types',?,TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)),TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)))`, UUIDBytes(workspace), workspace.String()); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES(?,?,'types','active',TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)),TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)))`, UUIDBytes(node), UUIDBytes(workspace)); err != nil {
		t.Fatal(err)
	}
	semantictest.ObservedStateTransactions(t, b, node)
	for _, raw := range []string{`{"n":1e131072}`, `{"n":"\u0000"}`, `[]`} {
		_, err := b.Exec(ctx, `INSERT INTO telemetry_security_events(event_id,node_id,observed_at,severity,event_type,detail) VALUES(?,?,0,'info','invalid',?)`, UUIDBytes(uuid.New()), UUIDBytes(node), []byte(raw))
		if !errors.Is(err, database.ErrConstraint) {
			t.Fatalf("runtime raw JSON bypassed trigger: %v", err)
		}
	}
	for _, raw := range []string{`{"dimensions":[{"length":1,"lower_bound":2147483647}],"elements":["x"]}`, `{"dimensions":[{"length":1,"lower_bound":0}],"elements":["\u0000"]}`} {
		_, err := b.Exec(ctx, `INSERT INTO observed_groups(node_id,group_name,members,revision,fingerprint,observed_at) VALUES(?,'invalid',?,0,?,0)`, UUIDBytes(node), []byte(raw), make([]byte, 32))
		if !errors.Is(err, database.ErrConstraint) {
			t.Fatalf("runtime raw array bypassed trigger: %v", err)
		}
	}
}
