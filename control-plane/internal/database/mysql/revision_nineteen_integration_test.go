package mysql

import (
	"bytes"
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	enrollmentstore "github.com/GentleKingson/ocservia/control-plane/internal/enrollment/store"
	"github.com/google/uuid"
)

func TestRealVersionEighteenEnrollmentUpgrade(t *testing.T) {
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
	err = b.migrateChainOn(ctx, conn, chain[:17], "")
	unlock := releaseMigrationConnection(conn, lock)
	if err != nil || unlock != nil {
		t.Fatal(err, unlock)
	}
	receipts := func() []string {
		t.Helper()
		var result []string
		for _, query := range []string{
			`SELECT CONCAT_WS('|',version,parent_checksum,manifest_checksum,state,repair_count,started_at,verified_at) FROM backend_schema_revisions WHERE version<=18 ORDER BY version`,
			`SELECT CONCAT_WS('|',version,ordinal,name,checksum,state,started_at,verified_at) FROM backend_schema_revision_steps WHERE version<=18 ORDER BY version,ordinal`,
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
	workspace, node := uuid.New(), uuid.New()
	start := time.Date(1000, 1, 1, 0, 0, 0, 1000, time.UTC)
	end := time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
	run(`INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES(?,'token-history',?,?,?)`, UUIDBytes(workspace), workspace.String(), start, start)
	run(`INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at)VALUES(?,?,'historical','pending',?,?)`, UUIDBytes(node), UUIDBytes(workspace), start, start)
	for _, bootstrap := range []bool{false, true} {
		for i := range 2 {
			var consumed, bound, consumedNode any
			if i == 1 {
				consumed, bound, consumedNode = end, bytes.Repeat([]byte{1}, 32), UUIDBytes(node)
			}
			if bootstrap {
				run(`INSERT INTO node_bootstrap_tokens(id,workspace_id,token_hash,expected_environment,expires_at,consumed_at,consumed_node_id,bound_endpoint_id,created_by,created_at)VALUES(?,?,?,'test',?,?,?,?,'historical',?)`, UUIDBytes(uuid.New()), UUIDBytes(workspace), bytes.Repeat([]byte{byte(i + 1)}, 32), end, consumed, consumedNode, bound, start)
			} else {
				run(`INSERT INTO enrollment_tokens(id,workspace_id,token_hash,expected_environment,expected_endpoint_id,expires_at,consumed_at,consumed_node_id,created_by,created_at)VALUES(?,?,?,'test',?,?,?,?,'historical',?)`, UUIDBytes(uuid.New()), UUIDBytes(workspace), bytes.Repeat([]byte{byte(i + 1)}, 32), bytes.Repeat([]byte{1}, 32), end, consumed, consumedNode, start)
			}
		}
	}
	if err := b.Migrate(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, receipts()) {
		t.Fatal("earlier revision receipts changed")
	}
	if err := database.Within(ctx, b, database.ReadCommitted, func(tx database.Tx) error {
		store, err := enrollmentstore.Enrollment(tx)
		if err != nil {
			return err
		}
		for _, bootstrap := range []bool{false, true} {
			for i := range 2 {
				v, err := store.TokenByHash(ctx, bytes.Repeat([]byte{byte(i + 1)}, 32), bootstrap, false)
				if err != nil {
					return err
				}
				wantConsumed := value.Timestamp{}
				if i == 1 {
					wantConsumed = fixtureTimestamp(t, end)
				}
				if v.CreatedAt != fixtureTimestamp(t, start) || v.ExpiresAt != fixtureTimestamp(t, end) || v.ConsumedAt != wantConsumed || (v.ConsumedNode != nil) != (i == 1) || (v.ConsumedNode != nil && *v.ConsumedNode != node) {
					t.Fatal("token migration lost finite/NULL clocks or node binding", v)
				}
				if (len(v.Endpoint) == 32) != (!bootstrap || i == 1) {
					t.Fatal("token migration changed endpoint binding", v)
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.ValidateSchema(ctx, 34); err != nil {
		t.Fatal(err)
	}
}
