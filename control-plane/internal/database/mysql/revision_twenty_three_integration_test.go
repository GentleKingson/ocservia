package mysql

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

func TestRealVersionTwentyTwoSharedUpgrade(t *testing.T) {
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
	err = b.migrateChainOn(ctx, conn, chain[:21], "")
	unlock := releaseMigrationConnection(conn, lock)
	if err != nil || unlock != nil {
		t.Fatal(err, unlock)
	}
	receipts := func() []string {
		t.Helper()
		var result []string
		for _, q := range []string{
			`SELECT CONCAT_WS('|',version,parent_checksum,manifest_checksum,state,repair_count,started_at,verified_at) FROM backend_schema_revisions WHERE version<=22 ORDER BY version`,
			`SELECT CONCAT_WS('|',version,ordinal,name,checksum,state,started_at,verified_at) FROM backend_schema_revision_steps WHERE version<=22 ORDER BY version,ordinal`,
		} {
			rows, err := b.Query(ctx, q)
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
	run := func(q string, args ...any) {
		t.Helper()
		if _, err := b.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Date(1000, 1, 1, 0, 0, 0, 1000, time.UTC)
	end := time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
	workspace, node, record := uuid.New(), uuid.New(), uuid.New()
	slug := strings.Repeat("s", 4096)
	labels := `[null,9007199254740993,"unchanged"]`
	classification := `{"exact":9007199254740993,"kind":"fixture"}`
	run(`INSERT INTO workspaces(id,name,slug,created_at,updated_at,archived_at)VALUES(?,'shared-history',?,?,?,?)`, UUIDBytes(workspace), slug, start, end, end)
	run(`INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at,labels)VALUES(?,?,'shared-history','revoked',?,?,?)`, UUIDBytes(node), UUIDBytes(workspace), start, end, labels)
	run(`INSERT INTO node_endpoint_keys(node_id,endpoint_id,state,bound_at,revoked_at)VALUES(?,?,'revoked',?,?)`, UUIDBytes(node), bytes.Repeat([]byte{1}, 32), start, end)
	run(`INSERT INTO node_sealing_keys(node_id,purpose,version,key_id,public_key_sha256,created_at)VALUES(?,1,1,'fixture',?,?)`, UUIDBytes(node), bytes.Repeat([]byte{2}, 32), start)
	run(`INSERT INTO upstream_sync_records(id,repository,old_ref,old_commit,new_ref,new_commit,classification,rollback_ref,synced_at)VALUES(?,?,'old',?,'new',?,?,'rollback',?)`, UUIDBytes(record), slug, strings.Repeat("a", 40), strings.Repeat("b", 40), classification, end)
	readJSON := func(table, column string, id uuid.UUID) value.JSONB {
		t.Helper()
		var result value.JSONB
		if err := b.QueryRow(ctx, "SELECT "+column+" FROM "+table+" WHERE id=?", UUIDBytes(id)).Scan(&result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	oldLabels, oldClassification := readJSON("nodes", "labels", node), readJSON("upstream_sync_records", "classification", record)
	if err := b.Migrate(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, receipts()) || !bytes.Equal(oldLabels.Bytes(), readJSON("nodes", "labels", node).Bytes()) || !bytes.Equal(oldClassification.Bytes(), readJSON("upstream_sync_records", "classification", record).Bytes()) {
		t.Fatal("historical receipt or JSON changed")
	}
	columns := []struct {
		table, column, key string
		id                 uuid.UUID
		want               time.Time
		nullable           bool
	}{
		{"workspaces", "created_at", "id", workspace, start, false},
		{"workspaces", "updated_at", "id", workspace, end, false},
		{"workspaces", "archived_at", "id", workspace, end, true},
		{"nodes", "created_at", "id", node, start, false},
		{"nodes", "updated_at", "id", node, end, false},
		{"node_endpoint_keys", "bound_at", "node_id", node, start, false},
		{"node_endpoint_keys", "revoked_at", "node_id", node, end, true},
		{"node_sealing_keys", "created_at", "node_id", node, start, false},
		{"upstream_sync_records", "synced_at", "id", record, end, false},
	}
	for _, c := range columns {
		q := "SELECT " + c.column + " FROM " + c.table + " WHERE " + c.key + "=?"
		update := "UPDATE " + c.table + " SET " + c.column + "=? WHERE " + c.key + "=?"
		var got value.Timestamp
		if err := b.QueryRow(ctx, q, UUIDBytes(c.id)).Scan(&got); err != nil || got != fixtureTimestamp(t, c.want) {
			t.Fatal(c.table, c.column, got, err)
		}
		for _, micros := range []int64{value.NegativeInfinity, value.MinTimestamp, value.EndTimestamp - 1, value.PositiveInfinity} {
			at := value.Timestamp{Valid: true, Micros: micros}
			run(update, at, UUIDBytes(c.id))
			if err := b.QueryRow(ctx, q, UUIDBytes(c.id)).Scan(&got); err != nil || got != at {
				t.Fatal("logical shared clock", c.table, c.column, got, err)
			}
		}
		if _, err := b.Exec(ctx, update, value.EndTimestamp, UUIDBytes(c.id)); !errors.Is(err, database.ErrConstraint) {
			t.Fatal("invalid time accepted", c.table, c.column, err)
		}
		if !c.nullable {
			if _, err := b.Exec(ctx, update, nil, UUIDBytes(c.id)); !errors.Is(err, database.ErrConstraint) {
				t.Fatal("null accepted", c.table, c.column, err)
			}
		}
	}
	run(`UPDATE workspaces SET archived_at=NULL WHERE id=?`, UUIDBytes(workspace))
	run(`UPDATE node_endpoint_keys SET state='active',revoked_at=NULL WHERE node_id=?`, UUIDBytes(node))
	if _, err := b.Exec(ctx, `UPDATE node_endpoint_keys SET state='revoked' WHERE node_id=?`, UUIDBytes(node)); !errors.Is(err, database.ErrConstraint) {
		t.Fatal("endpoint revocation pair not enforced", err)
	}
	for _, spec := range []struct {
		table, column string
		id            uuid.UUID
	}{
		{"nodes", "labels", node}, {"upstream_sync_records", "classification", record},
	} {
		q := "UPDATE " + spec.table + " SET " + spec.column + "=? WHERE id=?"
		for _, raw := range []string{`{"large":1e1000,"small":0.0000000000000000001}`, `{"duplicate":1,"duplicate":9007199254740993}`} {
			want, err := value.ParseJSONB([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			run(q, []byte(raw), UUIDBytes(spec.id))
			if got := readJSON(spec.table, spec.column, spec.id); !bytes.Equal(got.Bytes(), want.Bytes()) {
				t.Fatal("lossy shared JSON", spec.table, string(got.Bytes()))
			}
		}
		for _, invalid := range []any{nil, []byte(`{"n":1e131072}`), []byte(`{"bad":"\u0000"}`), []byte(`{"bad":"\ud800"}`)} {
			if _, err := b.Exec(ctx, q, invalid, UUIDBytes(spec.id)); !errors.Is(err, database.ErrConstraint) {
				t.Fatal("invalid shared JSON accepted", spec.table, err)
			}
		}
		if spec.table == "nodes" {
			for _, raw := range []string{`null`, `[]`, `true`, `1e1000`, `"label"`} {
				run(q, []byte(raw), UUIDBytes(spec.id))
			}
		} else if _, err := b.Exec(ctx, q, []byte(`[]`), UUIDBytes(spec.id)); !errors.Is(err, database.ErrConstraint) {
			t.Fatal("classification object constraint lost", err)
		}
	}
	if _, err := b.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES(?,'duplicate',?,0,0)`, UUIDBytes(uuid.New()), slug); !errors.Is(err, database.ErrUnique) {
		t.Fatal("workspace exact key lost", err)
	}
	if err := b.Migrate(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, receipts()) {
		t.Fatal("earlier receipts changed on replay")
	}
	if err := b.ValidateSchema(ctx, 35); err != nil {
		t.Fatal(err)
	}
}
