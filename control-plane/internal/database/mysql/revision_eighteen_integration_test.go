package mysql

import (
	"bytes"
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

func TestRealVersionSeventeenTrustUpgrade(t *testing.T) {
	b, _ := versionTwoFixture(t, false)
	ctx := context.Background()
	chain, err := loadRevisionChain(b.engine)
	if err != nil {
		t.Fatal(err)
	}
	conn, lock, err := migrationConnection(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	err = b.migrateChainOn(ctx, conn, chain[:16], "")
	unlock := releaseMigrationConnection(conn, lock)
	if err != nil || unlock != nil {
		t.Fatal(err, unlock)
	}
	receipts := func() []string {
		t.Helper()
		var result []string
		for _, query := range []string{
			`SELECT CONCAT_WS('|',version,parent_checksum,manifest_checksum,state,repair_count,started_at,verified_at) FROM backend_schema_revisions WHERE version<=17 ORDER BY version`,
			`SELECT CONCAT_WS('|',version,ordinal,name,checksum,state,started_at,verified_at) FROM backend_schema_revision_steps WHERE version<=17 ORDER BY version,ordinal`,
		} {
			rows, err := b.Query(ctx, query)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var row string
				if err := rows.Scan(&row); err != nil {
					rows.Close()
					t.Fatal(err)
				}
				result = append(result, row)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				t.Fatal(err)
			}
		}
		return result
	}
	before := receipts()
	run := func(query string, args ...any) {
		t.Helper()
		if _, err := b.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	workspace, worker := uuid.New(), uuid.New()
	nodes := []uuid.UUID{uuid.New(), uuid.New()}
	start := time.Date(1000, 1, 1, 0, 0, 0, 1000, time.UTC)
	end := time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
	run(`INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES(?,'trust-history',?,?,?)`, UUIDBytes(workspace), workspace.String(), start, start)
	for i, node := range nodes {
		run(`INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at)VALUES(?,?,?,'revoked',?,?)`, UUIDBytes(node), UUIDBytes(workspace), node.String(), start, start)
		var lockedBy, lockedUntil any
		if i == 1 {
			lockedBy, lockedUntil = UUIDBytes(worker), end
		}
		run(`INSERT INTO node_trust_convergence(node_id,endpoint_id,desired_state,revision,reason,close_required,available_at,locked_by,locked_until,created_at,updated_at)VALUES(?,?,'revoked',2,'historical',true,?,?,?,?,?)`, UUIDBytes(node), bytes.Repeat([]byte{1}, 32), start, lockedBy, lockedUntil, start, end)
	}
	if err := b.Migrate(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, receipts()) {
		t.Fatal("published v1-v17 receipts changed")
	}
	for i, node := range nodes {
		var available, locked, created, updated value.Timestamp
		var workerID *uuid.UUID
		if err := b.QueryRow(ctx, `SELECT available_at,locked_until,created_at,updated_at,locked_by FROM node_trust_convergence WHERE node_id=?`, UUIDBytes(node)).Scan(&available, &locked, &created, &updated, &workerID); err != nil {
			t.Fatal(err)
		}
		wantLocked := value.Timestamp{}
		if i == 1 {
			wantLocked = fixtureTimestamp(t, end)
		}
		if available != fixtureTimestamp(t, start) || locked != wantLocked || created != available || updated != fixtureTimestamp(t, end) || (workerID != nil) != (i == 1) || (workerID != nil && *workerID != worker) {
			t.Fatal("trust migration changed clock or lock", available, locked, created, updated, workerID)
		}
	}
	if err := b.ValidateSchema(ctx, 34); err != nil {
		t.Fatal(err)
	}
}
