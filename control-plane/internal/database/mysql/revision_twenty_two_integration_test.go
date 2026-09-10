package mysql

import (
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

func TestRealVersionTwentyOneUsageUpgrade(t *testing.T) {
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
	err = b.migrateChainOn(ctx, conn, chain[:20], "")
	unlock := releaseMigrationConnection(conn, lock)
	if err != nil || unlock != nil {
		t.Fatal(err, unlock)
	}
	receipts := func() []string {
		t.Helper()
		var result []string
		for _, q := range []string{
			`SELECT CONCAT_WS('|',version,parent_checksum,manifest_checksum,state,repair_count,started_at,verified_at) FROM backend_schema_revisions WHERE version<=21 ORDER BY version`,
			`SELECT CONCAT_WS('|',version,ordinal,name,checksum,state,started_at,verified_at) FROM backend_schema_revision_steps WHERE version<=21 ORDER BY version,ordinal`,
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
	workspace, node := uuid.New(), uuid.New()
	start := time.Date(1000, 1, 1, 0, 0, 0, 1000, time.UTC)
	end := time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
	session := strings.Repeat("s", 256)
	run(`INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES(?,'usage-history',?,?,?)`, UUIDBytes(workspace), workspace.String(), start, end)
	run(`INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at)VALUES(?,?,'usage-history','active',?,?)`, UUIDBytes(node), UUIDBytes(workspace), start, end)
	for _, at := range []time.Time{start, end} {
		run(`INSERT INTO user_usage_cursors(node_id,session_id,connected_at,username,rx_bytes,tx_bytes,observed_at)VALUES(?,?,?,'alice',7,11,?)`, UUIDBytes(node), session, at, end)
		run(`INSERT INTO observed_user_usage(node_id,username,period,period_start,rx_bytes,tx_bytes,observed_at)VALUES(?,'alice','monthly',?,7,11,?)`, UUIDBytes(node), at, end)
	}
	if err := b.Migrate(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, receipts()) {
		t.Fatal("earlier receipts changed")
	}
	for _, at := range []time.Time{start, end} {
		want := fixtureTimestamp(t, at)
		for _, q := range []string{
			`SELECT connected_at,observed_at,rx_bytes,tx_bytes FROM user_usage_cursors WHERE connected_at=?`,
			`SELECT period_start,observed_at,rx_bytes,tx_bytes FROM observed_user_usage WHERE period_start=?`,
		} {
			var key, observed value.Timestamp
			var rx, tx int64
			if err := b.QueryRow(ctx, q, want).Scan(&key, &observed, &rx, &tx); err != nil || key != want || observed != fixtureTimestamp(t, end) || rx != 7 || tx != 11 {
				t.Fatal("converted usage", key, observed, rx, tx, err)
			}
		}
		if _, err := b.Exec(ctx, `INSERT INTO user_usage_cursors(node_id,session_id,connected_at,username,rx_bytes,tx_bytes,observed_at)VALUES(?,?,?,'alice',0,0,?)`, UUIDBytes(node), session, want, want); !errors.Is(err, database.ErrUnique) {
			t.Fatal("cursor key", err)
		}
		if _, err := b.Exec(ctx, `INSERT INTO observed_user_usage(node_id,username,period,period_start,rx_bytes,tx_bytes,observed_at)VALUES(?,'alice','monthly',?,0,0,?)`, UUIDBytes(node), want, want); !errors.Is(err, database.ErrUnique) {
			t.Fatal("period key", err)
		}
	}
	for _, micros := range []int64{value.NegativeInfinity, value.PositiveInfinity, value.MinTimestamp, value.EndTimestamp - 1} {
		at := value.Timestamp{Valid: true, Micros: micros}
		run(`INSERT INTO user_usage_cursors(node_id,session_id,connected_at,username,rx_bytes,tx_bytes,observed_at)VALUES(?,?,?,'alice',0,0,?)`, UUIDBytes(node), session, at, at)
		run(`INSERT INTO observed_user_usage(node_id,username,period,period_start,rx_bytes,tx_bytes,observed_at)VALUES(?,'alice','monthly',?,0,0,?)`, UUIDBytes(node), at, at)
		var observed value.Timestamp
		if err := b.QueryRow(ctx, `SELECT observed_at FROM user_usage_cursors WHERE connected_at=?`, at).Scan(&observed); err != nil || observed != at {
			t.Fatal("logical cursor", observed, err)
		}
		if err := b.QueryRow(ctx, `SELECT observed_at FROM observed_user_usage WHERE period_start=?`, at).Scan(&observed); err != nil || observed != at {
			t.Fatal("logical period", observed, err)
		}
	}
	for _, q := range []string{`UPDATE user_usage_cursors SET connected_at=?`, `UPDATE user_usage_cursors SET observed_at=?`, `UPDATE observed_user_usage SET period_start=?`, `UPDATE observed_user_usage SET observed_at=?`} {
		for _, invalid := range []any{nil, value.EndTimestamp} {
			if _, err := b.Exec(ctx, q, invalid); err == nil {
				t.Fatal("invalid usage clock accepted")
			}
		}
	}
	if err := b.Migrate(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, receipts()) {
		t.Fatal("prior receipts changed on replay")
	}
	if err := b.ValidateSchema(ctx, 34); err != nil {
		t.Fatal(err)
	}
}
