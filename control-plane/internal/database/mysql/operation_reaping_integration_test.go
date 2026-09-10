package mysql

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

func TestRealOperationReaping(t *testing.T) {
	owner, _, options := migrateFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	workspace, worker := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	run := func(q string, args ...any) {
		t.Helper()
		if _, err := owner.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	run(`INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES(?,'reaping',?,?,?)`, UUIDBytes(workspace), workspace.String(), fixtureTimestamp(t, now), fixtureTimestamp(t, now))
	if err := owner.GrantTestPrivileges(ctx); err != nil {
		t.Fatal(err)
	}
	cfg, err := driver.ParseDSN(options.DSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.User, cfg.Passwd = "ocservia_app", "pr02-runtime-test-only"
	options.DSN = cfg.FormatDSN()
	backend, err := Open(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	signer, err := commandauth.NewRandomSigner()
	if err != nil {
		t.Fatal(err)
	}
	service := operations.NewBackend(backend, 50, signer)
	createClaim := func(count int) []operations.Dispatch {
		t.Helper()
		for range count {
			node := uuid.Must(uuid.NewV7())
			run(`INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at)VALUES(?,?,?,'active',?,?)`, UUIDBytes(node), UUIDBytes(workspace), node.String(), fixtureTimestamp(t, now), fixtureTimestamp(t, now))
			_, _, err := service.CreateSynthetic(ctx, operations.CreateRequest{NodeID: node, IdempotencyKey: uuid.NewString(), ExpectedVersion: 1, Kind: operations.SyntheticEcho, Message: "recovery", TTL: time.Hour, RequestID: uuid.NewString(), Traceparent: "00-" + strings.Repeat("1", 32) + "-" + strings.Repeat("2", 16) + "-01"})
			if err != nil {
				t.Fatal(err)
			}
		}
		jobs, err := service.Claim(ctx, worker, count, time.Minute)
		if err != nil || len(jobs) != count {
			t.Fatal("claim", jobs, err)
		}
		return jobs
	}
	negative, positive := value.Timestamp{Valid: true, Micros: value.NegativeInfinity}, value.Timestamp{Valid: true, Micros: value.PositiveInfinity}
	assertState := func(id uuid.UUID, want string) {
		t.Helper()
		v, err := service.Get(ctx, id)
		if err != nil || v.State != want {
			t.Fatal("state", v, err, "want", want)
		}
	}
	readEnvelope := func(id uuid.UUID) *agentv1.CommandEnvelope {
		t.Helper()
		var encoded []byte
		if err := owner.QueryRow(ctx, `SELECT envelope FROM commands WHERE id=?`, UUIDBytes(id)).Scan(&encoded); err != nil {
			t.Fatal(err)
		}
		var envelope agentv1.CommandEnvelope
		if err := proto.Unmarshal(encoded, &envelope); err != nil {
			t.Fatal(err)
		}
		return &envelope
	}
	d := createClaim(1)[0]
	original := readEnvelope(d.CommandID)
	run(`UPDATE node_command_leases SET leased_until=? WHERE lease_token=?`, positive, UUIDBytes(d.LeaseToken))
	if err := service.Reap(ctx, 3); err != nil {
		t.Fatal("infinite lease", err)
	}
	assertState(d.OperationID, "queued")
	run(`UPDATE node_command_leases SET leased_until=? WHERE lease_token=?`, negative, UUIDBytes(d.LeaseToken))
	// A concurrent result owns the outbox first. Reaping must skip it without
	// deleting its still-sending lease or waiting while holding command locks.
	locked, err := owner.Begin(ctx, database.ReadCommitted)
	if err != nil {
		t.Fatal(err)
	}
	var outbox uuid.UUID
	if err := locked.QueryRow(ctx, `SELECT id FROM outbox_events WHERE id=? FOR UPDATE`, UUIDBytes(d.OutboxID)).Scan(&outbox); err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
	err = service.Reap(bounded, 3)
	cancel()
	_ = locked.Rollback(ctx)
	if err != nil {
		t.Fatal("locked result outbox was not skipped", err)
	}
	assertState(d.OperationID, "queued")
	if err := service.Reap(ctx, 3); err != nil {
		t.Fatal("expired send", err)
	}
	assertState(d.OperationID, "unknown")
	first := readEnvelope(d.CommandID)
	if first.GetDeliveryMode() != agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY || !bytes.Equal(first.GetCommandId(), original.GetCommandId()) || !bytes.Equal(first.GetSemanticPayloadSha256(), original.GetSemanticPayloadSha256()) || bytes.Equal(first.GetMessageId(), original.GetMessageId()) {
		t.Fatal("recovery changed logical command", first)
	}
	jobs, err := service.Claim(ctx, worker, 1, time.Minute)
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	if err := service.MarkSent(ctx, jobs[0]); err != nil {
		t.Fatal(err)
	}
	run(`UPDATE command_attempts SET finished_at=? WHERE id=?`, negative, UUIDBytes(jobs[0].AttemptID))
	if err := service.Reap(ctx, 3); err != nil {
		t.Fatal("sent continuation", err)
	}
	continued := readEnvelope(d.CommandID)
	if !proto.Equal(first.GetExpiresAt(), continued.GetExpiresAt()) || bytes.Equal(first.GetMessageId(), continued.GetMessageId()) {
		t.Fatal("continuation extended deadline or reused message")
	}
	jobs, err = service.Claim(ctx, worker, 1, time.Minute)
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	run(`UPDATE node_command_leases SET leased_until=? WHERE lease_token=?`, negative, UUIDBytes(jobs[0].LeaseToken))
	for range 2 {
		if err := service.Reap(ctx, 3); err != nil {
			t.Fatal("attempt limit", err)
		}
	}
	var published value.Timestamp
	var leases, events int
	if err := owner.QueryRow(ctx, `SELECT published_at,(SELECT count(*) FROM node_command_leases WHERE command_id=?),(SELECT count(*) FROM operation_events WHERE operation_id=? AND state='unknown') FROM outbox_events WHERE id=?`, UUIDBytes(d.CommandID), UUIDBytes(d.OperationID), UUIDBytes(d.OutboxID)).Scan(&published, &leases, &events); err != nil || !published.Valid || leases != 0 || events != 1 {
		t.Fatal("reconciliation limit leaked state", published, leases, events, err)
	}
	assertState(d.OperationID, "unknown")

	// Corrupt a later candidate after the earlier candidate was eligible. The
	// service must roll back the entire batch, not just the failing command.
	pair := createClaim(2)
	for _, job := range pair {
		run(`UPDATE node_command_leases SET leased_until=? WHERE lease_token=?`, negative, UUIDBytes(job.LeaseToken))
	}
	run(`UPDATE commands SET envelope=? WHERE id=?`, []byte{0xff}, UUIDBytes(pair[1].CommandID))
	if err := service.Reap(ctx, 3); err == nil {
		t.Fatal("corrupt recovery envelope accepted")
	}
	for _, job := range pair {
		assertState(job.OperationID, "queued")
		var state string
		if err := owner.QueryRow(ctx, `SELECT state FROM command_attempts WHERE id=?`, UUIDBytes(job.AttemptID)).Scan(&state); err != nil || state != "sending" {
			t.Fatal("batch partially committed", state, err)
		}
	}
	run(`UPDATE commands SET envelope=? WHERE id=?`, pair[1].Envelope, UUIDBytes(pair[1].CommandID))
	if err := service.Reap(ctx, 3); err != nil {
		t.Fatal("retry rolled-back batch", err)
	}
	jobs, err = service.Claim(ctx, worker, 2, time.Minute)
	if err != nil || len(jobs) != 2 {
		t.Fatal(jobs, err)
	}
	for _, job := range jobs {
		if err := service.MarkSent(ctx, job); err != nil {
			t.Fatal(err)
		}
	}

	d = createClaim(1)[0]
	if err := service.MarkSent(ctx, d); err != nil {
		t.Fatal(err)
	}
	run(`UPDATE command_attempts SET finished_at=? WHERE id=?`, positive, UUIDBytes(d.AttemptID))
	if err := service.Reap(ctx, 3); err != nil {
		t.Fatal(err)
	}
	assertState(d.OperationID, "dispatched")
	run(`UPDATE command_attempts SET finished_at=? WHERE id=?`, negative, UUIDBytes(d.AttemptID))
	if err := service.Reap(ctx, 3); err != nil {
		t.Fatal("missing result", err)
	}
	assertState(d.OperationID, "unknown")
	if got := readEnvelope(d.CommandID); got.GetDeliveryMode() != agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY {
		t.Fatal("missing result retried effect", got)
	}
}
