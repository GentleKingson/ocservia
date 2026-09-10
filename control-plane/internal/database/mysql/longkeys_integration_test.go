package mysql

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
)

func longKeyFixture(t *testing.T) *Backend {
	t.Helper()
	b, _, _ := migrateFixture(t)
	return b
}

func fixtureTimestamp(t *testing.T, now time.Time) value.Timestamp {
	t.Helper()
	stamp, err := value.FromTime(now)
	if err != nil {
		t.Fatal(err)
	}
	return stamp
}

func TestRealLongKeyWrites(t *testing.T) {
	b := longKeyFixture(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	large := strings.Repeat("x", 4096)
	workspace, node := UUIDBytes(uuid.New()), UUIDBytes(uuid.New())
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := b.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES(?,'long',?,?,?)`, workspace, large, fixtureTimestamp(t, now), fixtureTimestamp(t, now))
	exec(`INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES(?,?,?,'active',?,?)`, node, workspace, large, fixtureTimestamp(t, now), fixtureTimestamp(t, now))
	for _, value := range []string{large + " ", strings.ToUpper(large), large + "  "} {
		exec(`INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES(?,'long',?,?,?)`, UUIDBytes(uuid.New()), value, fixtureTimestamp(t, now), fixtureTimestamp(t, now))
	}
	if _, err := b.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES(?,'duplicate',?,?,?)`, UUIDBytes(uuid.New()), large, fixtureTimestamp(t, now), fixtureTimestamp(t, now)); !errors.Is(err, database.ErrUnique) {
		t.Fatal("long duplicate accepted", err)
	}
	for _, values := range [][2]string{{large, "a"}, {large + "a", ""}, {large, "a "}} {
		exec(`INSERT INTO identities(id,issuer,subject,created_at,updated_at) VALUES(?,?,?,?,?)`, UUIDBytes(uuid.New()), values[0], values[1], fixtureTimestamp(t, now), fixtureTimestamp(t, now))
	}
	op := UUIDBytes(uuid.New())
	exec(`INSERT INTO operations(id,workspace_id,state,request_id,idempotency_key,request_hash,created_at,updated_at) VALUES(?,?,'draft','long',?,REPEAT('x',32),?,?)`, op, workspace, large, fixtureTimestamp(t, now), fixtureTimestamp(t, now))
	for range 2 {
		exec(`INSERT INTO operations(id,workspace_id,state,request_id,created_at,updated_at) VALUES(?,?,'draft','null',?,?)`, UUIDBytes(uuid.New()), workspace, fixtureTimestamp(t, now), fixtureTimestamp(t, now))
	}
	if _, err := b.Exec(ctx, `INSERT INTO operations(id,workspace_id,state,request_id,idempotency_key,request_hash,created_at,updated_at) VALUES(?,?,'draft','long',?,REPEAT('x',32),?,?)`, UUIDBytes(uuid.New()), workspace, large, fixtureTimestamp(t, now), fixtureTimestamp(t, now)); !errors.Is(err, database.ErrUnique) {
		t.Fatal("partial duplicate accepted", err)
	}
	exec(`UPDATE operations SET idempotency_key=NULL,request_hash=NULL WHERE id=?`, op)
	exec(`INSERT INTO operations(id,workspace_id,state,request_id,idempotency_key,request_hash,created_at,updated_at) VALUES(?,?,'draft','reuse',?,REPEAT('x',32),?,?)`, UUIDBytes(uuid.New()), workspace, large, fixtureTimestamp(t, now), fixtureTimestamp(t, now))
	for _, table := range []string{"telemetry_rollups_5m", "telemetry_rollups_1h"} {
		stamp, err := value.FromTime(now)
		if err != nil {
			t.Fatal(err)
		}
		q := "INSERT INTO " + table + "(node_id,metric,bucket_at,sample_count,min_value,max_value,avg_value) VALUES(?,?,?,1,1,1,1)"
		exec(q, node, large, stamp)
		exec(q, node, large+" ", stamp)
		if _, err := b.Exec(ctx, q, node, large, stamp); !errors.Is(err, database.ErrUnique) {
			t.Fatal(table, "duplicate accepted", err)
		}
	}
	exec(`INSERT INTO user_policy_enforcements(node_id,username,policy_version,cause,period_start,source_user_version,created_at) VALUES(?,?,1,'quota',?,1,?)`, node, large, fixtureTimestamp(t, now), fixtureTimestamp(t, now))
	exec(`INSERT INTO upstream_sync_records(id,repository,old_ref,old_commit,new_ref,new_commit,classification,rollback_ref,synced_at) VALUES(?,?,'old',?,'new',?,'{}','rollback',?)`, UUIDBytes(uuid.New()), large, strings.Repeat("a", 40), strings.Repeat("b", 40), fixtureTimestamp(t, now))
	var got string
	if err := b.QueryRow(ctx, `SELECT name FROM nodes WHERE id=?`, node).Scan(&got); err != nil || got != large {
		t.Fatal("long value changed", err)
	}
	for _, query := range []string{
		`SELECT slug FROM workspaces WHERE id=?`,
		`SELECT idempotency_key FROM operations WHERE workspace_id=? AND idempotency_key IS NOT NULL`,
	} {
		if err := b.QueryRow(ctx, query, workspace).Scan(&got); err != nil || got != large {
			t.Fatal("natural key round trip changed", err)
		}
	}
	for _, query := range []string{
		`SELECT metric FROM telemetry_rollups_5m WHERE node_id=? ORDER BY OCTET_LENGTH(metric) LIMIT 1`,
		`SELECT metric FROM telemetry_rollups_1h WHERE node_id=? ORDER BY OCTET_LENGTH(metric) LIMIT 1`,
		`SELECT username FROM user_policy_enforcements WHERE node_id=?`,
	} {
		if err := b.QueryRow(ctx, query, node).Scan(&got); err != nil || got != large {
			t.Fatal("composite key round trip changed", err)
		}
	}
	if err := b.QueryRow(ctx, `SELECT repository FROM upstream_sync_records WHERE CAST(repository AS BINARY)=?`, []byte(large)).Scan(&got); err != nil || got != large {
		t.Fatal("repository round trip changed", err)
	}
	// Delete cascading through node -> rollup -> exact key must reclaim bytes,
	// even though InnoDB does not run row triggers for an FK cascade.
	exec(`DELETE FROM nodes WHERE id=?`, node)
	for _, table := range []string{"exact_telemetry_rollups_5m", "exact_user_policy_enforcements"} {
		var count int
		if err := b.QueryRow(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil || count != 0 {
			t.Fatal("cascaded key not reclaimed", table, count, err)
		}
	}
}

func TestRealLongKeyUpdateAndConcurrency(t *testing.T) {
	b := longKeyFixture(t)
	ctx := context.Background()
	now := fixtureTimestamp(t, time.Now().UTC())
	key := strings.Repeat("x", 4096)
	first, second := UUIDBytes(uuid.New()), UUIDBytes(uuid.New())
	for index, id := range [][]byte{first, second} {
		if _, err := b.Exec(ctx, `INSERT INTO identities(id,issuer,subject,created_at,updated_at) VALUES(?,?,?,?,?)`, id, key, strings.Repeat("s", index+1), now, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.Exec(ctx, `UPDATE identities SET subject='s' WHERE id=?`, second); !errors.Is(err, database.ErrUnique) {
		t.Fatal("duplicate update accepted", err)
	}
	replacement := UUIDBytes(uuid.New())
	if _, err := b.Exec(ctx, `UPDATE identities SET id=?,subject='replacement' WHERE id=?`, replacement, first); err != nil {
		t.Fatal("owner update cascade failed", err)
	}
	var owner []byte
	if err := b.QueryRow(ctx, `SELECT owner_id FROM exact_identities WHERE owner_id=?`, replacement).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	for range 2 {
		go func() {
			results <- database.Within(ctx, b, database.ReadCommitted, func(tx database.Tx) error {
				_, err := tx.Exec(ctx, `INSERT INTO identities(id,issuer,subject,created_at,updated_at) VALUES(?,?,?,?,?)`, UUIDBytes(uuid.New()), key, "concurrent", now, now)
				return err
			})
		}()
	}
	success, duplicate := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			success++
		} else if errors.Is(err, database.ErrUnique) {
			duplicate++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || duplicate != 1 {
		t.Fatal("concurrent uniqueness", success, duplicate)
	}
	// A repeatable-read snapshot must not hide a conflicting committed row.
	tx, err := b.Begin(ctx, database.RepeatableRead)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var before int
	if err = tx.QueryRow(ctx, `SELECT COUNT(*) FROM exact_identities`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Exec(ctx, `INSERT INTO identities(id,issuer,subject,created_at,updated_at) VALUES(?,?,?,?,?)`, UUIDBytes(uuid.New()), key, "after-snapshot", now, now); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO identities(id,issuer,subject,created_at,updated_at) VALUES(?,?,?,?,?)`, UUIDBytes(uuid.New()), key, "after-snapshot", now, now); !errors.Is(err, database.ErrUnique) && !errors.Is(err, database.ErrSerialization) {
		t.Fatal("stale snapshot bypassed uniqueness", err)
	}
	if errors.Is(err, database.ErrSerialization) {
		sentinel := UUIDBytes(uuid.New())
		if _, err = tx.Exec(ctx, `INSERT INTO identities(id,issuer,subject,created_at,updated_at) VALUES(?,?,?,?,?)`, sentinel, key, "must-not-autocommit", now, now); !errors.Is(err, database.ErrTxAborted) {
			t.Fatal("server rollback allowed subsequent autocommit", err)
		}
		if err = tx.Commit(ctx); !errors.Is(err, database.ErrTxAborted) {
			t.Fatal("server-aborted transaction committed", err)
		}
		var count int
		if err = b.QueryRow(ctx, `SELECT COUNT(*) FROM identities WHERE id=?`, sentinel).Scan(&count); err != nil || count != 0 {
			t.Fatal("post-abort write persisted", count, err)
		}
	} else if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	// A later row's violation rolls back earlier rows and private key writes.
	newID := UUIDBytes(uuid.New())
	if _, err = b.Exec(ctx, `INSERT INTO identities(id,issuer,subject,created_at,updated_at) VALUES(?,?,?,?,?),(?,?,?,?,?)`, newID, key, "multirow-new", now, now, UUIDBytes(uuid.New()), key, "concurrent", now, now); !errors.Is(err, database.ErrUnique) {
		t.Fatal("multi-row duplicate accepted", err)
	}
	for _, query := range []string{`SELECT COUNT(*) FROM identities WHERE id=?`, `SELECT COUNT(*) FROM exact_identities WHERE owner_id=?`} {
		var count int
		if err = b.QueryRow(ctx, query, newID).Scan(&count); err != nil || count != 0 {
			t.Fatal("multi-row statement left a partial row", count, err)
		}
	}
	if _, err = b.Exec(ctx, `UPDATE identities SET subject=CASE WHEN id=? THEN 'ss' ELSE 'replacement' END WHERE id IN (?,?)`, replacement, replacement, second); !errors.Is(err, database.ErrUnique) {
		t.Fatal("non-deferrable unique swap accepted", err)
	}
}

func TestRealLongIdentityUpsert(t *testing.T) {
	b := longKeyFixture(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	issuer, subject := strings.Repeat("issuer/", 1024), strings.Repeat("subject", 1024)
	ids := make(chan uuid.UUID, 2)
	errorsCh := make(chan error, 2)
	for range 2 {
		go func() {
			var id uuid.UUID
			err := database.Within(ctx, b, database.ReadCommitted, func(tx database.Tx) error {
				var err error
				id, err = UpsertIdentity(ctx, tx, uuid.New(), issuer, subject, "", "name", now)
				return err
			})
			ids <- id
			errorsCh <- err
		}()
	}
	for range 2 {
		if err := <-errorsCh; err != nil {
			t.Fatal(err)
		}
	}
	id := <-ids
	if id != <-ids {
		t.Fatal("concurrent upsert returned different identities")
	}
	if _, err := b.Exec(ctx, `UPDATE identities SET disabled_at=? WHERE id=?`, fixtureTimestamp(t, now), UUIDBytes(id)); err != nil {
		t.Fatal(err)
	}
	err := database.Within(ctx, b, database.ReadCommitted, func(tx database.Tx) error {
		_, err := UpsertIdentity(ctx, tx, uuid.New(), issuer, subject, "new", "new", now)
		return err
	})
	if !errors.Is(err, database.ErrNotFound) {
		t.Fatal("disabled identity revived", err)
	}
}

func TestRealLongReceiptAndResourceKey(t *testing.T) {
	b := longKeyFixture(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	workspace, node, operation, command := UUIDBytes(uuid.New()), UUIDBytes(uuid.New()), UUIDBytes(uuid.New()), UUIDBytes(uuid.New())
	large := strings.Repeat("receipt", 1024)
	trace := "00-" + strings.Repeat("a", 32) + "-" + strings.Repeat("b", 16) + "-01"
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := b.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES(?,'receipt','receipt',?,?)`, workspace, fixtureTimestamp(t, now), fixtureTimestamp(t, now))
	exec(`INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES(?,?,'receipt','active',?,?)`, node, workspace, fixtureTimestamp(t, now), fixtureTimestamp(t, now))
	exec(`INSERT INTO operations(id,workspace_id,node_id,state,request_id,created_at,updated_at) VALUES(?,?,?,'draft','receipt',?,?)`, operation, workspace, node, fixtureTimestamp(t, now), fixtureTimestamp(t, now))
	exec(`INSERT INTO commands(id,operation_id,workspace_id,node_id,state,payload_type,envelope,idempotency_key,expected_version,traceparent,expires_at,created_at,updated_at,resource_type,resource_key) VALUES(?,?,?,?,'queued','synthetic_noop','x','receipt',1,?,?,?,?,'user',?)`, command, operation, workspace, node, trace, fixtureTimestamp(t, now.Add(time.Minute)), fixtureTimestamp(t, now), fixtureTimestamp(t, now), large)
	var read string
	if err := b.QueryRow(ctx, `SELECT resource_key FROM commands WHERE id=?`, command).Scan(&read); err != nil || read != large {
		t.Fatal("resource key truncated", err)
	}
	insert := func(key, status string) ([]byte, error) {
		id := UUIDBytes(uuid.New())
		exec(`INSERT INTO transport_events(event_id,node_id,event_type,occurred_at,traceparent,payload) VALUES(?,?,'command_result',?,?,'')`, id, node, fixtureTimestamp(t, now), trace)
		_, err := b.Exec(ctx, `INSERT INTO agent_command_results(event_id,command_id,idempotency_key,state,result,error_code,completed_at,replayed,created_at,receipt_verification_status,privd_attestation_key_id,effect_record_id,effect_sequence,receipt_sha256,privileged_result_proof) VALUES(?,?,?,'rejected','','rejected',?,FALSE,?,?,?,REPEAT('x',16),1,REPEAT('x',32),'proof')`, id, command, UUIDBytes(uuid.New()), fixtureTimestamp(t, now), fixtureTimestamp(t, now), status, key)
		return id, err
	}
	first, err := insert(large, "verified")
	if err != nil {
		t.Fatal(err)
	}
	if err = b.QueryRow(ctx, `SELECT privd_attestation_key_id FROM agent_command_results WHERE event_id=?`, first).Scan(&read); err != nil || read != large {
		t.Fatal("receipt key round trip changed", err)
	}
	if _, err = insert(large, "verified"); !errors.Is(err, database.ErrUnique) {
		t.Fatal("receipt duplicate accepted", err)
	}
	for _, status := range []string{"legacy", "missing"} {
		if _, err = insert(large, status); err != nil {
			t.Fatal("partial-index excluded row rejected", err)
		}
	}
	if _, err = insert(large+" ", "verified"); err != nil {
		t.Fatal("receipt trailing space collapsed", err)
	}
	exec(`UPDATE agent_command_results SET receipt_verification_status='legacy' WHERE event_id=?`, first)
	if _, err = insert(large, "verified"); err != nil {
		t.Fatal("partial-index entry was not removed", err)
	}
}

func TestRealLongKeyPermissions(t *testing.T) {
	b := longKeyFixture(t)
	ctx := context.Background()
	if err := b.GrantTestPrivileges(ctx); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := b.QueryRow(ctx, `SELECT DATABASE()`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Exec(ctx, "GRANT SELECT,UPDATE(key_name) ON `"+name+"`.exact_key_guards TO 'ocservia_app'@'%'"); err != nil {
		t.Fatal(err)
	}
	o := testOptions(t)
	c, err := driver.ParseDSN(o.DSN)
	if err != nil {
		t.Fatal(err)
	}
	c.DBName, c.User, c.Passwd = name, "ocservia_app", "pr02-runtime-test-only"
	o.DSN = c.FormatDSN()
	runtime, err := Open(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if err = database.Within(ctx, runtime, database.ReadCommitted, func(tx database.Tx) error {
		_, err := UpsertIdentity(ctx, tx, uuid.New(), strings.Repeat("issuer", 1024), "runtime", "", "", time.Now().UTC())
		return err
	}); err != nil {
		t.Fatal("runtime cannot write through owner trigger", err)
	}
	for _, query := range []string{
		`INSERT INTO exact_key_guards VALUES ('forged')`,
		`UPDATE exact_key_guards SET key_name='forged' WHERE key_name='identities'`,
		`UPDATE exact_key_guards SET key_name=key_name WHERE key_name='identities'`,
		`DELETE FROM exact_key_guards`,
		`INSERT INTO exact_identities(owner_id,key_value) VALUES(REPEAT('x',16),'forged')`,
		`UPDATE exact_identities SET key_value='forged'`,
		`DELETE FROM exact_identities`,
	} {
		if _, err = runtime.Exec(ctx, query); !errors.Is(err, database.ErrPermission) {
			t.Fatal("private exact-key state writable", err)
		}
	}
}

func TestRealQueryRowPoisonsBeforeScan(t *testing.T) {
	if testOptions(t).Engine != MariaDB {
		t.Skip("MariaDB snapshot-isolation error 1020")
	}
	b, _, _ := fixture(t)
	ctx := context.Background()
	if _, err := b.Exec(ctx, `CREATE TABLE queryrow_abort_probe(id INTEGER PRIMARY KEY,value INTEGER NOT NULL) ENGINE=InnoDB`); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Exec(ctx, `INSERT INTO queryrow_abort_probe VALUES(1,0)`); err != nil {
		t.Fatal(err)
	}
	tx, err := b.Begin(ctx, database.RepeatableRead)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var first int
	if err = tx.QueryRow(ctx, `SELECT value FROM queryrow_abort_probe WHERE id=1`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Exec(ctx, `UPDATE queryrow_abort_probe SET value=1 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	delayed := tx.QueryRow(ctx, `UPDATE queryrow_abort_probe SET value=2 WHERE id=1`)
	if _, err = tx.Exec(ctx, `INSERT INTO queryrow_abort_probe VALUES(2,99)`); !errors.Is(err, database.ErrTxAborted) {
		t.Fatal("unscanned server error allowed autocommit", err)
	}
	if err = delayed.Scan(&first); !errors.Is(err, database.ErrSerialization) {
		t.Fatal("expected actual MariaDB snapshot rollback", err)
	}
	if err = tx.Commit(ctx); !errors.Is(err, database.ErrTxAborted) {
		t.Fatal("unscanned server rollback allowed commit", err)
	}
	var count int
	if err = b.QueryRow(ctx, `SELECT COUNT(*) FROM queryrow_abort_probe WHERE id=2`).Scan(&count); err != nil || count != 0 {
		t.Fatal("post-rollback sentinel persisted", count, err)
	}
}

func TestRealRowsClosePoisonsDrainError(t *testing.T) {
	b, _, _ := fixture(t)
	ctx := context.Background()
	if _, err := b.Exec(ctx, `CREATE PROCEDURE rows_abort_probe() BEGIN SELECT 1; SIGNAL SQLSTATE '40001' SET MYSQL_ERRNO=1213,MESSAGE_TEXT='test drain rollback'; END`); err != nil {
		t.Fatal(err)
	}
	tx, err := b.Begin(ctx, database.ReadCommitted)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `CALL rows_abort_probe()`)
	if err != nil {
		t.Fatal("initial result header failed instead of drain", err)
	}
	rows.Close()
	if _, err = tx.Exec(ctx, `SELECT 1`); !errors.Is(err, database.ErrTxAborted) {
		t.Fatal("closed result's server error did not poison transaction", err)
	}
	if !errors.Is(rows.Err(), database.ErrDeadlock) {
		t.Fatal("missing trailing server error", rows.Err())
	}
	if err = tx.Commit(ctx); !errors.Is(err, database.ErrTxAborted) {
		t.Fatal("closed result's server error allowed commit", err)
	}
}
