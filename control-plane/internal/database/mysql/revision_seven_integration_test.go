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

func TestRealVersionSixCertificateUpgrade(t *testing.T) {
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
	err = b.migrateChainOn(ctx, conn, chain[:5], "")
	unlock := releaseMigrationConnection(conn, lock)
	if err != nil || unlock != nil {
		t.Fatal(err, unlock)
	}
	receipts := func() []string {
		t.Helper()
		var result []string
		for _, query := range []string{
			`SELECT CONCAT_WS('|',version,parent_checksum,manifest_checksum,state,repair_count,started_at,verified_at) FROM backend_schema_revisions WHERE version<=6 ORDER BY version`,
			`SELECT CONCAT_WS('|',version,ordinal,name,checksum,state,started_at,verified_at) FROM backend_schema_revision_steps WHERE version<=6 ORDER BY version,ordinal`,
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
	workspace, node, operation, certificate := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	at := time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
	run(`INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES(?,'v7-history',?,?,?)`, UUIDBytes(workspace), workspace.String(), at, at)
	run(`INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at)VALUES(?,?,'v7-node','offline',?,?)`, UUIDBytes(node), UUIDBytes(workspace), at, at)
	run(`INSERT INTO operations(id,workspace_id,node_id,state,request_id,created_at,updated_at)VALUES(?,?,?,'succeeded',?,?,?)`, UUIDBytes(operation), UUIDBytes(workspace), UUIDBytes(node), operation.String(), at, at)
	event := uuid.New()
	run(`UPDATE operations SET completed_at=? WHERE id=?`, at, UUIDBytes(operation))
	run(`INSERT INTO operation_events(id,operation_id,state,occurred_at)VALUES(?,?,'succeeded',?)`, UUIDBytes(event), UUIDBytes(operation), at)
	command, outbox := uuid.New(), uuid.New()
	commandAt := at.Add(-time.Hour)
	run(`INSERT INTO commands(id,operation_id,workspace_id,node_id,state,payload_type,envelope,idempotency_key,expected_version,traceparent,expires_at,created_at,updated_at)VALUES(?,?,?,?,'succeeded','synthetic_noop','x','historical',1,'00-11111111111111111111111111111111-2222222222222222-01',?,?,?)`, UUIDBytes(command), UUIDBytes(operation), UUIDBytes(workspace), UUIDBytes(node), at, commandAt, commandAt)
	run(`INSERT INTO outbox_events(id,command_id,event_type,payload,available_at,published_at,created_at)VALUES(?,?,'command.dispatch','x',?,?,?)`, UUIDBytes(outbox), UUIDBytes(command), commandAt, at, commandAt)
	attempt, worker := uuid.New(), uuid.New()
	run(`INSERT INTO command_attempts(id,command_id,outbox_event_id,worker_id,attempt_number,state,started_at,finished_at)VALUES(?,?,?,?,1,'sent',?,?)`, UUIDBytes(attempt), UUIDBytes(command), UUIDBytes(outbox), UUIDBytes(worker), commandAt, at)
	pendingAttempt := uuid.New()
	run(`INSERT INTO command_attempts(id,command_id,outbox_event_id,worker_id,attempt_number,state,started_at)VALUES(?,?,?,?,2,'sending',?)`, UUIDBytes(pendingAttempt), UUIDBytes(command), UUIDBytes(outbox), UUIDBytes(worker), at)
	run(`INSERT INTO node_command_leases(node_id,command_id,lease_token,worker_id,leased_until,created_at)VALUES(?,?,?,?,?,?)`, UUIDBytes(node), UUIDBytes(command), UUIDBytes(uuid.New()), UUIDBytes(worker), at, commandAt)
	run(`INSERT INTO agent_upgrade_operations(operation_id,workspace_id,node_id,target_version,package_sha256,architecture,state,completed_at,created_at,updated_at)VALUES(?,?,?,'2.0.0',?,'amd64','succeeded',?,?,?)`, UUIDBytes(operation), UUIDBytes(workspace), UUIDBytes(node), bytes.Repeat([]byte{1}, 32), at, commandAt, at)
	dns, err := value.ParseJSONB([]byte(`["example.test",null,[1.25],{"nested":true}]`))
	if err != nil {
		t.Fatal(err)
	}
	infinity := value.Timestamp{Valid: true, Micros: value.PositiveInfinity}
	rollout, approval, actor, session := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	logicalAt := fixtureTimestamp(t, at)
	run(`INSERT INTO identities(id,issuer,subject,created_at,updated_at)VALUES(?,'historical',?,?,?)`, UUIDBytes(actor), actor.String(), logicalAt, logicalAt)
	run(`INSERT INTO auth_sessions(id,identity_id,expires_at,created_at)VALUES(?,?,?,?)`, UUIDBytes(session), UUIDBytes(actor), infinity, logicalAt)
	run(`INSERT INTO approval_requests(id,workspace_id,requester_id,action,resource_type,resource_id,reason,status,expires_at,created_at)VALUES(?,?,?,'agent.rollout','batch_operation',?,'historical','pending',?,?)`, UUIDBytes(approval), UUIDBytes(workspace), UUIDBytes(actor), UUIDBytes(rollout), infinity, logicalAt)
	run(`INSERT INTO agent_rollouts(id,workspace_id,target_version,state,batch_size,stop_on_failure,reason,approval_id,request_hash,created_by,actor_session_id,exclusions,idempotency_key,created_at,updated_at)VALUES(?,?,'2.0.0','queued',1,true,'historical',?,?,?,?,?,'historical',?,?)`, UUIDBytes(rollout), UUIDBytes(workspace), UUIDBytes(approval), bytes.Repeat([]byte{1}, 32), UUIDBytes(actor), UUIDBytes(session), dns.Bytes(), commandAt, at)
	run(`INSERT INTO agent_rollout_nodes(rollout_id,node_id,ordinal,batch,state,updated_at)VALUES(?,?,0,0,'pending',?)`, UUIDBytes(rollout), UUIDBytes(node), at)
	run(`INSERT INTO config_plans(id,workspace_id,node_id,operation_id,template_name,expected_revision,candidate_hash,candidate_redacted,warnings,expires_at,created_at)VALUES(?,?,?,?,'historical',0,?,'safe',?,?,?)`, UUIDBytes(operation), UUIDBytes(workspace), UUIDBytes(node), UUIDBytes(operation), bytes.Repeat([]byte{1}, 32), dns.Bytes(), at, commandAt)
	run(`INSERT INTO node_config_state(node_id,revision,desired_revision,updated_at)VALUES(?,1,2,?)`, UUIDBytes(node), at)
	run(`INSERT INTO certificates(id,workspace_id,node_id,operation_id,common_name,dns_names,key_bits,state,not_before,not_after,created_at,updated_at,csr_receipt_verified_at)VALUES(?,?,?,?,'example.test',?,2048,'csr_pending',?,?,?,?,?)`, UUIDBytes(certificate), UUIDBytes(workspace), UUIDBytes(node), UUIDBytes(operation), dns.Bytes(), at, infinity, at, at, at)
	secret := uuid.New()
	run(`INSERT INTO secret_provider_refs(id,workspace_id,provider,key_path,version,state,created_at,updated_at)VALUES(?,?,'isolated','historical/key','1','active',?,?)`, UUIDBytes(secret), UUIDBytes(workspace), at, at)
	if err := b.Migrate(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, receipts()) {
		t.Fatal("published v1-v6 history changed")
	}
	var names value.JSONB
	var start, end, revoked, created, updated, verified value.Timestamp
	if err := b.QueryRow(ctx, `SELECT dns_names,not_before,not_after,revoked_at,created_at,updated_at,csr_receipt_verified_at FROM certificates WHERE id=?`, UUIDBytes(certificate)).Scan(&names, &start, &end, &revoked, &created, &updated, &verified); err != nil {
		t.Fatal(err)
	}
	want := fixtureTimestamp(t, at)
	if !bytes.Equal(names.Bytes(), dns.Bytes()) || start != want || end != infinity || revoked.Valid || created != want || updated != want || verified != want {
		t.Fatal("certificate migration changed logical values", start, end, revoked, created, updated, verified, string(names.Bytes()))
	}
	if err := b.QueryRow(ctx, `SELECT rotated_at,created_at,updated_at FROM secret_provider_refs WHERE id=?`, UUIDBytes(secret)).Scan(&revoked, &created, &updated); err != nil || revoked.Valid || created != want || updated != want {
		t.Fatal("secret reference migration changed values", revoked, created, updated, err)
	}
	var expires, completed, occurred value.Timestamp
	if err := b.QueryRow(ctx, `SELECT created_at,updated_at,expires_at,completed_at FROM operations WHERE id=?`, UUIDBytes(operation)).Scan(&created, &updated, &expires, &completed); err != nil || created != want || updated != want || expires.Valid || completed != want {
		t.Fatal("operation migration changed values", created, updated, expires, completed, err)
	}
	if err := b.QueryRow(ctx, `SELECT occurred_at FROM operation_events WHERE id=?`, UUIDBytes(event)).Scan(&occurred); err != nil || occurred != want {
		t.Fatal("operation event migration changed value", occurred, err)
	}
	commandWant := fixtureTimestamp(t, commandAt)
	if err := b.QueryRow(ctx, `SELECT created_at,updated_at,expires_at FROM commands WHERE id=?`, UUIDBytes(command)).Scan(&created, &updated, &expires); err != nil || created != commandWant || updated != commandWant || expires != want {
		t.Fatal("command migration changed values", created, updated, expires, err)
	}
	if err := b.QueryRow(ctx, `SELECT created_at,available_at,published_at,locked_until FROM outbox_events WHERE id=?`, UUIDBytes(outbox)).Scan(&created, &updated, &completed, &expires); err != nil || created != commandWant || updated != commandWant || completed != want || expires.Valid {
		t.Fatal("outbox migration changed values", created, updated, completed, expires, err)
	}
	if err := b.QueryRow(ctx, `SELECT started_at,finished_at FROM command_attempts WHERE id=?`, UUIDBytes(attempt)).Scan(&created, &completed); err != nil || created != commandWant || completed != want {
		t.Fatal("dispatch attempt migration changed values", created, completed, err)
	}
	if err := b.QueryRow(ctx, `SELECT started_at,finished_at FROM command_attempts WHERE id=?`, UUIDBytes(pendingAttempt)).Scan(&created, &completed); err != nil || created != want || completed.Valid {
		t.Fatal("dispatch attempt migration changed NULL", created, completed, err)
	}
	if err := b.QueryRow(ctx, `SELECT created_at,leased_until FROM node_command_leases WHERE node_id=?`, UUIDBytes(node)).Scan(&created, &expires); err != nil || created != commandWant || expires != want {
		t.Fatal("dispatch lease migration changed values", created, expires, err)
	}
	if err := b.QueryRow(ctx, `SELECT warnings,expires_at,created_at FROM config_plans WHERE id=?`, UUIDBytes(operation)).Scan(&names, &expires, &created); err != nil || !bytes.Equal(names.Bytes(), dns.Bytes()) || expires != want || created != commandWant {
		t.Fatal("configuration plan migration changed values", string(names.Bytes()), expires, created, err)
	}
	if err := b.QueryRow(ctx, `SELECT updated_at FROM node_config_state WHERE node_id=?`, UUIDBytes(node)).Scan(&updated); err != nil || updated != want {
		t.Fatal("configuration state migration changed clock", updated, err)
	}
	if err := b.QueryRow(ctx, `SELECT scheduled_at,completed_at,created_at,updated_at FROM agent_upgrade_operations WHERE operation_id=?`, UUIDBytes(operation)).Scan(&expires, &completed, &created, &updated); err != nil || expires.Valid || completed != want || created != commandWant || updated != want {
		t.Fatal("upgrade migration changed clocks", expires, completed, created, updated, err)
	}
	if err := b.QueryRow(ctx, `SELECT exclusions,created_at,updated_at FROM agent_rollouts WHERE id=?`, UUIDBytes(rollout)).Scan(&names, &created, &updated); err != nil || !bytes.Equal(names.Bytes(), dns.Bytes()) || created != commandWant || updated != want {
		t.Fatal("rollout migration changed values", string(names.Bytes()), created, updated, err)
	}
	if err := b.QueryRow(ctx, `SELECT dispatch_lease_until,updated_at FROM agent_rollout_nodes WHERE rollout_id=? AND node_id=?`, UUIDBytes(rollout), UUIDBytes(node)).Scan(&expires, &updated); err != nil || expires.Valid || updated != want {
		t.Fatal("rollout node migration changed clocks", expires, updated, err)
	}
	if err := b.ValidateSchema(ctx, 34); err != nil {
		t.Fatal(err)
	}
}
