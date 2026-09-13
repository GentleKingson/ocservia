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

func TestRealVersionFifteenResultUpgrade(t *testing.T) {
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
	err = b.migrateChainOn(ctx, conn, chain[:14], "")
	unlock := releaseMigrationConnection(conn, lock)
	if err != nil || unlock != nil {
		t.Fatal(err, unlock)
	}
	receipts := func() []string {
		t.Helper()
		var result []string
		for _, query := range []string{
			`SELECT CONCAT_WS('|',version,parent_checksum,manifest_checksum,state,repair_count,started_at,verified_at) FROM backend_schema_revisions WHERE version<=15 ORDER BY version`,
			`SELECT CONCAT_WS('|',version,ordinal,name,checksum,state,started_at,verified_at) FROM backend_schema_revision_steps WHERE version<=15 ORDER BY version,ordinal`,
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
	workspace, node, operation, command := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	start := time.Date(1000, 1, 1, 0, 0, 0, 1000, time.UTC)
	end := time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
	at := fixtureTimestamp(t, start)
	trace := "00-11111111111111111111111111111111-2222222222222222-01"
	run(`INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES(?,'v16-history',?,?,?)`, UUIDBytes(workspace), workspace.String(), start, start)
	run(`INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at)VALUES(?,?,'historical','offline',?,?)`, UUIDBytes(node), UUIDBytes(workspace), start, start)
	run(`INSERT INTO operations(id,workspace_id,node_id,state,request_id,created_at,updated_at)VALUES(?,?,?,'succeeded',?,?,?)`, UUIDBytes(operation), UUIDBytes(workspace), UUIDBytes(node), operation.String(), at, at)
	run(`INSERT INTO commands(id,operation_id,workspace_id,node_id,state,payload_type,envelope,idempotency_key,expected_version,traceparent,expires_at,created_at,updated_at)VALUES(?,?,?,?,'succeeded','synthetic_noop','x','historical',1,?,?,?,?)`, UUIDBytes(command), UUIDBytes(operation), UUIDBytes(workspace), UUIDBytes(node), trace, fixtureTimestamp(t, end), at, at)
	for _, state := range []string{"succeeded", "rejected"} {
		event := uuid.New()
		run(`INSERT INTO transport_events(event_id,node_id,event_type,occurred_at,received_at,traceparent,payload)VALUES(?,?,'command_result',?,?,?,'')`, UUIDBytes(event), UUIDBytes(node), end, start, trace)
		var accepted, code any
		if state == "succeeded" {
			accepted = start
		} else {
			code = "rejected"
		}
		run(`INSERT INTO agent_command_results(event_id,command_id,idempotency_key,payload_sha256,state,result,error_code,accepted_at,completed_at,replayed,created_at)VALUES(?,?,?,?,?,'',?,?,?,false,?)`, UUIDBytes(event), UUIDBytes(command), UUIDBytes(uuid.New()), bytes.Repeat([]byte{1}, 32), state, code, accepted, end, start)
	}
	quarantine := uuid.New()
	run(`INSERT INTO transport_event_quarantine(event_id,node_id,event_type,payload_sha256,reason_code,reason_detail,observed_at)VALUES(?,?,1,?,'historical','historical',?)`, UUIDBytes(quarantine), UUIDBytes(node), bytes.Repeat([]byte{1}, 32), end)
	run(`INSERT INTO transport_event_cursor(singleton,event_id,valid,updated_at)VALUES(true,?,true,?)`, UUIDBytes(quarantine), start)
	run(`INSERT INTO local_slice_jobs(operation_id,command_envelope,traceparent,available_at,expires_at,created_at)VALUES(?,'historical',?,?,?,?)`, UUIDBytes(operation), trace, start, end, start)
	if err := b.Migrate(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, receipts()) {
		t.Fatal("published v1-v15 receipts changed")
	}
	for _, state := range []string{"succeeded", "rejected"} {
		var accepted, completed, created, occurred, received value.Timestamp
		if err := b.QueryRow(ctx, `SELECT r.accepted_at,r.completed_at,r.created_at,e.occurred_at,e.received_at FROM agent_command_results r JOIN transport_events e ON e.event_id=r.event_id WHERE r.command_id=? AND r.state=?`, UUIDBytes(command), state).Scan(&accepted, &completed, &created, &occurred, &received); err != nil {
			t.Fatal(err)
		}
		wantAccepted := at
		if state == "rejected" {
			wantAccepted = value.Timestamp{}
		}
		if accepted != wantAccepted || completed != fixtureTimestamp(t, end) || created != at || occurred != completed || received != at {
			t.Fatal("result migration changed finite or NULL clock", state, accepted, completed, created)
		}
	}
	var observed, cursor, available, expires, dispatched, created value.Timestamp
	if err := b.QueryRow(ctx, `SELECT q.observed_at,c.updated_at FROM transport_event_quarantine q JOIN transport_event_cursor c ON c.event_id=q.event_id WHERE q.event_id=?`, UUIDBytes(quarantine)).Scan(&observed, &cursor); err != nil || observed != fixtureTimestamp(t, end) || cursor != at {
		t.Fatal("quarantine/cursor migration", observed, cursor, err)
	}
	if err := b.QueryRow(ctx, `SELECT available_at,expires_at,dispatched_at,created_at FROM local_slice_jobs WHERE operation_id=?`, UUIDBytes(operation)).Scan(&available, &expires, &dispatched, &created); err != nil || available != at || expires != fixtureTimestamp(t, end) || dispatched.Valid || created != at {
		t.Fatal("simulator job migration", available, expires, dispatched, created, err)
	}
	if err := b.ValidateSchema(ctx, 35); err != nil {
		t.Fatal(err)
	}
}
