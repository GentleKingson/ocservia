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

func TestRealVersionTwentyUserOperationsUpgrade(t *testing.T) {
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
	err = b.migrateChainOn(ctx, conn, chain[:19], "")
	unlock := releaseMigrationConnection(conn, lock)
	if err != nil || unlock != nil {
		t.Fatal(err, unlock)
	}
	receipts := func() []string {
		t.Helper()
		var result []string
		for _, query := range []string{
			`SELECT CONCAT_WS('|',version,parent_checksum,manifest_checksum,state,repair_count,started_at,verified_at) FROM backend_schema_revisions WHERE version<=20 ORDER BY version`,
			`SELECT CONCAT_WS('|',version,ordinal,name,checksum,state,started_at,verified_at) FROM backend_schema_revision_steps WHERE version<=20 ORDER BY version,ordinal`,
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
	workspace, node, batch, mutation, owner := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	start := time.Date(1000, 1, 1, 0, 0, 0, 1000, time.UTC)
	end := time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
	hash := bytes.Repeat([]byte{1}, 32)
	name := "alice"
	keyName := strings.Repeat("user", 150)
	run(`INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES(?,'policy-history',?,?,?)`, UUIDBytes(workspace), workspace.String(), start, start)
	run(`INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at)VALUES(?,?,'historical','active',?,?)`, UUIDBytes(node), UUIDBytes(workspace), start, start)
	run(`INSERT INTO desired_users(node_id,username,enabled,version,revision,fingerprint,created_at,updated_at)VALUES(?,?,true,1,1,?,?,?)`, UUIDBytes(node), name, hash, fixtureTimestamp(t, start), fixtureTimestamp(t, end))
	run(`INSERT INTO desired_user_policies(node_id,username,quota_period,quota_direction,quota_bytes,expires_at,version,created_at,updated_at)VALUES(?,?,'monthly','rx',100,NULL,1,?,?)`, UUIDBytes(node), name, start, end)
	run(`INSERT INTO user_policy_mutations(id,workspace_id,node_id,username,idempotency_key,request_hash,policy_version,created_at)VALUES(?,?,?,?,'history',?,1,?)`, UUIDBytes(mutation), UUIDBytes(workspace), UUIDBytes(node), name, hash, start)
	run(`INSERT INTO batch_operations(id,workspace_id,state,actor_id,reason,request_id,traceparent,idempotency_key,request_hash,created_at,updated_at)VALUES(?,?,'queued','operator','history','history',?,'history',?,?,?)`, UUIDBytes(batch), UUIDBytes(workspace), "00-11111111111111111111111111111111-2222222222222222-01", hash, start, end)
	run(`INSERT INTO batch_operation_items(batch_id,item_index,node_id,username,action,expected_version,state,updated_at)VALUES(?,0,?,?,'enable',1,'queued',?)`, UUIDBytes(batch), UUIDBytes(node), name, start)
	run(`INSERT INTO batch_operation_items(batch_id,item_index,node_id,username,action,expected_version,state,lease_owner,lease_until,updated_at)VALUES(?,1,?,?,'enable',1,'submitting',?,?,?)`, UUIDBytes(batch), UUIDBytes(node), name, UUIDBytes(owner), end, start)
	run(`INSERT INTO scheduler_leases(lease_name,owner_id,lease_until,updated_at)VALUES('history',?,?,?)`, UUIDBytes(owner), end, start)
	for _, period := range []time.Time{start, end} {
		run(`INSERT INTO user_policy_enforcements(node_id,username,policy_version,cause,period_start,source_user_version,created_at)VALUES(?,?,1,'quota',?,1,?)`, UUIDBytes(node), keyName, period, start)
	}
	if err := b.Migrate(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, receipts()) {
		t.Fatal("earlier receipts changed")
	}
	for _, table := range []string{"desired_user_policies", "batch_operations", "scheduler_leases"} {
		var created, updated value.Timestamp
		query := "SELECT created_at,updated_at FROM " + table
		wantCreated, wantUpdated := fixtureTimestamp(t, start), fixtureTimestamp(t, end)
		if table == "scheduler_leases" {
			query = "SELECT updated_at,lease_until FROM scheduler_leases"
		}
		if err := b.QueryRow(ctx, query).Scan(&created, &updated); err != nil || created != wantCreated || updated != wantUpdated {
			t.Fatal(table, created, updated, err)
		}
	}
	var expires, created value.Timestamp
	if err := b.QueryRow(ctx, `SELECT expires_at FROM desired_user_policies`).Scan(&expires); err != nil || expires.Valid {
		t.Fatal("nullable expiry", expires, err)
	}
	if err := b.QueryRow(ctx, `SELECT created_at FROM user_policy_mutations`).Scan(&created); err != nil || created != fixtureTimestamp(t, start) {
		t.Fatal("mutation", created, err)
	}
	for index := 0; index < 2; index++ {
		var lease, updated value.Timestamp
		if err := b.QueryRow(ctx, `SELECT lease_until,updated_at FROM batch_operation_items WHERE item_index=?`, index).Scan(&lease, &updated); err != nil {
			t.Fatal(err)
		}
		if updated != fixtureTimestamp(t, start) || index == 0 && lease.Valid || index == 1 && lease != fixtureTimestamp(t, end) {
			t.Fatal("batch claim", lease, updated)
		}
	}
	for _, period := range []time.Time{start, end} {
		var got value.Timestamp
		if err := b.QueryRow(ctx, `SELECT created_at FROM user_policy_enforcements WHERE period_start=?`, fixtureTimestamp(t, period)).Scan(&got); err != nil || got != fixtureTimestamp(t, start) {
			t.Fatal("enforcement time", got, err)
		}
		if _, err := b.Exec(ctx, `INSERT INTO user_policy_enforcements(node_id,username,policy_version,cause,period_start,source_user_version,created_at)VALUES(?,?,1,'quota',?,1,?)`, UUIDBytes(node), keyName, fixtureTimestamp(t, period), fixtureTimestamp(t, start)); !errors.Is(err, database.ErrUnique) {
			t.Fatal("rekey did not enforce duplicate", err)
		}
	}
	for _, micros := range []int64{value.NegativeInfinity, value.PositiveInfinity, value.MinTimestamp, value.EndTimestamp - 1} {
		at := value.Timestamp{Micros: micros, Valid: true}
		run(`INSERT INTO user_policy_enforcements(node_id,username,policy_version,cause,period_start,source_user_version,created_at)VALUES(?,?,1,'quota',?,1,?)`, UUIDBytes(node), keyName, at, at)
		run(`UPDATE desired_user_policies SET expires_at=?,updated_at=?`, at, at)
		run(`UPDATE batch_operation_items SET lease_owner=?,lease_until=?,updated_at=? WHERE item_index=1`, UUIDBytes(owner), at, at)
		var got value.Timestamp
		if err := b.QueryRow(ctx, `SELECT expires_at FROM desired_user_policies`).Scan(&got); err != nil || got != at {
			t.Fatal("logical clock", got, err)
		}
	}
	if _, err := b.Exec(ctx, `UPDATE batch_operation_items SET lease_until=NULL WHERE item_index=1`); err == nil {
		t.Fatal("broken lease pair accepted")
	}
	if _, err := b.Exec(ctx, `UPDATE desired_user_policies SET updated_at=?`, value.EndTimestamp); err == nil {
		t.Fatal("invalid time accepted")
	}
	if err := b.ValidateSchema(ctx, 35); err != nil {
		t.Fatal(err)
	}
}
