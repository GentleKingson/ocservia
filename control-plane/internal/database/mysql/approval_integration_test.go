package mysql

import (
	"context"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/approvals/approvaltest"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
)

func TestRealApprovalControllerWorkflow(t *testing.T) {
	owner, _, options := migrateFixture(t)
	ctx := context.Background()
	workspace, requester, approver, session, approverSession := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC().Truncate(time.Microsecond)
	stamp, _ := value.FromTime(now)
	expires, _ := value.FromTime(now.Add(time.Hour))
	if _, err := owner.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES(?,?,?,?,?)`, UUIDBytes(workspace), "approval", "approval-"+workspace.String(), fixtureTimestamp(t, now), fixtureTimestamp(t, now)); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{requester, approver} {
		if _, err := owner.Exec(ctx, `INSERT INTO identities(id,issuer,subject,email,display_name,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, UUIDBytes(id), "approval-test", id.String(), "", "approval", stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	for _, pair := range [][2]uuid.UUID{{session, requester}, {approverSession, approver}} {
		if _, err := owner.Exec(ctx, `INSERT INTO auth_sessions(id,identity_id,expires_at,created_at) VALUES(?,?,?,?)`, UUIDBytes(pair[0]), UUIDBytes(pair[1]), expires, stamp); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := owner.Exec(ctx, `INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,created_at) VALUES(?,?,?,'SecurityAdmin','workspace',?)`, UUIDBytes(uuid.New()), UUIDBytes(approver), UUIDBytes(workspace), stamp); err != nil {
		t.Fatal(err)
	}
	if err := owner.GrantTestPrivileges(ctx); err != nil {
		t.Fatal(err)
	}
	cfg, err := driver.ParseDSN(options.DSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.User, cfg.Passwd = "ocservia_app", "pr02-runtime-test-only"
	options.DSN = cfg.FormatDSN()
	runtime, err := Open(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	approvaltest.Workflow(t, runtime, workspace, requester, approver, session, approverSession)
	approvaltest.ExtendedTimes(t, runtime, workspace, requester, approver, session, approverSession, func(id uuid.UUID, created, expires value.Timestamp) {
		if _, err := owner.Exec(ctx, `UPDATE approval_requests SET created_at=?,expires_at=? WHERE id=?`, created, expires, UUIDBytes(id)); err != nil {
			t.Fatal(err)
		}
	})
	node := uuid.New()
	if _, err := owner.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES(?,?,'approval-node','active',?,?)`, UUIDBytes(node), UUIDBytes(workspace), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	approvaltest.ExpiryAfterLockWait(t, runtime, workspace, requester, approver, session, approverSession,
		func(ctx context.Context, tx database.Tx) error {
			return tx.QueryRow(ctx, `SELECT id FROM nodes WHERE id=? FOR UPDATE`, UUIDBytes(node)).Scan(new([]byte))
		},
		func(ctx context.Context, tx database.Tx, id uuid.UUID) error {
			return tx.QueryRow(ctx, `SELECT id FROM approval_requests WHERE id=? FOR UPDATE`, UUIDBytes(id)).Scan(new([]byte))
		},
		func(id uuid.UUID, created, expires value.Timestamp) {
			if _, err := owner.Exec(ctx, `UPDATE approval_requests SET created_at=?,expires_at=? WHERE id=?`, created, expires, UUIDBytes(id)); err != nil {
				t.Fatal(err)
			}
		})
}
