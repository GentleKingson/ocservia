package mysql

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit/audittest"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
)

func TestRealAuditRBACController(t *testing.T) {
	owner, _, options := migrateFixture(t)
	ctx := context.Background()
	workspace, actor, target := uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC().Truncate(time.Microsecond)
	stamp, err := value.FromTime(now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = owner.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES(?,?,?,?,?)`, UUIDBytes(workspace), "audit", "audit-"+workspace.String(), now, now); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{actor, target} {
		if _, err = owner.Exec(ctx, `INSERT INTO identities(id,issuer,subject,email,display_name,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, UUIDBytes(id), "audit-test", id.String(), "", "audit", fixtureTimestamp(t, now), fixtureTimestamp(t, now)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = owner.Exec(ctx, `INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,created_at) VALUES(?,?,?,'PlatformAdmin','workspace',?)`, UUIDBytes(uuid.New()), UUIDBytes(actor), UUIDBytes(workspace), stamp); err != nil {
		t.Fatal(err)
	}
	if err = owner.GrantTestPrivileges(ctx); err != nil {
		t.Fatal(err)
	}
	cfg, err := driver.ParseDSN(options.DSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.User, cfg.Passwd = "ocservia_app", "pr02-runtime-test-only"
	options.DSN = cfg.FormatDSN()
	b, err := Open(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	audittest.Controller(t, b, workspace, actor, target, func(id, resource uuid.UUID, hash []byte, expiry time.Time) error {
		expires, _ := value.FromTime(expiry)
		created, _ := value.FromTime(expiry.Add(-time.Hour))
		_, err := owner.Exec(ctx, `INSERT INTO approval_requests(id,workspace_id,requester_id,action,resource_type,resource_id,reason,status,approver_id,approval_reason,expires_at,approved_at,created_at,request_hash,request_summary) VALUES(?,?,?,'role_binding.elevate','role_binding',?,'test','approved',?,'test',?,?,?,?,?)`, UUIDBytes(id), UUIDBytes(workspace), UUIDBytes(actor), UUIDBytes(resource), UUIDBytes(target), expires, fixtureTimestamp(t, expiry.Add(-time.Minute)), created, hash, `{}`)
		return err
	})
	for _, query := range []string{`UPDATE audit_events SET event_hash=event_hash WHERE workspace_id=?`, `UPDATE audit_events SET event_hash=REPEAT(0x00,32) WHERE workspace_id=?`, `DELETE FROM audit_events WHERE workspace_id=?`} {
		if _, err = b.Exec(ctx, query, UUIDBytes(workspace)); err == nil {
			t.Fatal("runtime changed audit history")
		}
	}
	// A transaction whose snapshot predates another append must still link to
	// the current tail, not to its old repeatable-read snapshot.
	tx, err := b.Begin(ctx, database.RepeatableRead)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var n int
	if err = tx.QueryRow(ctx, `SELECT COUNT(*) FROM audit_events WHERE workspace_id=?`, UUIDBytes(workspace)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if err = database.Within(ctx, b, database.ReadCommitted, func(other database.Tx) error {
		return audit.AppendChainTx(ctx, other, audit.ChainRecord{WorkspaceID: workspace, ActorType: "controller", ActorID: "audit-test", Action: "concurrent", ResourceType: "workspace", ResourceID: workspace, RequestID: uuid.NewString()})
	}); err != nil {
		t.Fatal(err)
	}
	err = audit.AppendChainTx(ctx, tx, audit.ChainRecord{WorkspaceID: workspace, ActorType: "controller", ActorID: "audit-test", Action: "after-snapshot", ResourceType: "workspace", ResourceID: workspace, RequestID: uuid.NewString()})
	if err != nil && !errors.Is(err, database.ErrSerialization) {
		t.Fatal(err)
	}
	if err == nil {
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	} else {
		_ = tx.Rollback(ctx)
	}
	verification, err := audit.NewBackendManager(b, bytes.Repeat([]byte{7}, 32)).Verify(ctx, workspace)
	if err != nil || !verification.Valid {
		t.Fatalf("RR audit fork: %+v %v", verification, err)
	}
}
