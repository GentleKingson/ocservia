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

func TestRealVersionNineteenDesiredUpgrade(t *testing.T) {
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
	err = b.migrateChainOn(ctx, conn, chain[:18], "")
	unlock := releaseMigrationConnection(conn, lock)
	if err != nil || unlock != nil {
		t.Fatal(err, unlock)
	}
	receipts := func() []string {
		t.Helper()
		var result []string
		for _, query := range []string{
			`SELECT CONCAT_WS('|',version,parent_checksum,manifest_checksum,state,repair_count,started_at,verified_at) FROM backend_schema_revisions WHERE version<=19 ORDER BY version`,
			`SELECT CONCAT_WS('|',version,ordinal,name,checksum,state,started_at,verified_at) FROM backend_schema_revision_steps WHERE version<=19 ORDER BY version,ordinal`,
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
	run(`INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES(?,'desired-history',?,?,?)`, UUIDBytes(workspace), workspace.String(), start, start)
	run(`INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at)VALUES(?,?,'historical','active',?,?)`, UUIDBytes(node), UUIDBytes(workspace), start, start)
	run(`INSERT INTO desired_users(node_id,username,enabled,version,revision,fingerprint,created_at,updated_at)VALUES(?,'alice',true,3,2,?,?,?)`, UUIDBytes(node), bytes.Repeat([]byte{1}, 32), start, end)
	for _, name := range []string{"empty", "members"} {
		members := `[]`
		if name == "members" {
			members = `["alpha","",null,"\u6c49\u5b57","alpha"]`
		}
		run(`INSERT INTO desired_groups(node_id,group_name,members,version,revision,fingerprint,created_at,updated_at)VALUES(?,?,?,3,2,?,?,?)`, UUIDBytes(node), name, members, bytes.Repeat([]byte{1}, 32), start, end)
	}
	if err := b.Migrate(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, receipts()) {
		t.Fatal("earlier receipts changed")
	}
	var created, updated value.Timestamp
	if err := b.QueryRow(ctx, `SELECT created_at,updated_at FROM desired_users WHERE node_id=?`, UUIDBytes(node)).Scan(&created, &updated); err != nil {
		t.Fatal(err)
	}
	if created != fixtureTimestamp(t, start) || updated != fixtureTimestamp(t, end) {
		t.Fatal("user time conversion", created, updated)
	}
	for _, name := range []string{"empty", "members"} {
		var members value.TextArray
		var version, revision int64
		if err := b.QueryRow(ctx, `SELECT members,created_at,updated_at,version,revision FROM desired_groups WHERE node_id=? AND group_name=?`, UUIDBytes(node), name).Scan(&members, &created, &updated, &version, &revision); err != nil {
			t.Fatal(err)
		}
		if created != fixtureTimestamp(t, start) || updated != fixtureTimestamp(t, end) || version != 3 || revision != 2 {
			t.Fatal("group clocks or revisions changed")
		}
		want := value.TextList([]string{})
		if name == "members" {
			want = value.TextList([]string{"alpha", "", "\u6c49\u5b57", "\u6c49\u5b57", "alpha"})
			want.Elements[2] = nil
		}
		gotBytes, _ := members.Bytes()
		wantBytes, _ := want.Bytes()
		if !bytes.Equal(gotBytes, wantBytes) {
			t.Fatalf("members %s: %s != %s", name, gotBytes, wantBytes)
		}
	}
	for _, bad := range []any{[]byte(`{"dimensions":[],"elements":[null]}`), []byte(`[]`), nil} {
		if _, err := b.Exec(ctx, `UPDATE desired_groups SET members=? WHERE node_id=?`, bad, UUIDBytes(node)); err == nil {
			t.Fatal("invalid logical array accepted", bad)
		}
	}
	if _, err := b.Exec(ctx, `UPDATE desired_users SET updated_at=? WHERE node_id=?`, value.EndTimestamp, UUIDBytes(node)); err == nil {
		t.Fatal("out-of-range time accepted")
	}
	if err := b.ValidateSchema(ctx, 35); err != nil {
		t.Fatal(err)
	}
}
