package mysql

import (
	"bytes"
	"context"
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/localslice"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestRealTransportResults(t *testing.T) {
	owner, _, options := migrateFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	workspace := uuid.Must(uuid.NewV7())
	run := func(q string, args ...any) {
		t.Helper()
		if _, err := owner.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	run(`INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES(?,'transport',?,?,?)`, UUIDBytes(workspace), workspace.String(), fixtureTimestamp(t, now), fixtureTimestamp(t, now))
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
	operations := operations.NewBackend(backend, 50, signer)
	ingress := localslice.NewBackend(backend, signer)
	worker := uuid.Must(uuid.NewV7())
	trace := "00-" + strings.Repeat("1", 32) + "-" + strings.Repeat("2", 16) + "-01"
	create := func() (uuid.UUID, uuid.UUID, *agentv1.CommandEnvelope) {
		t.Helper()
		node := uuid.Must(uuid.NewV7())
		endpoint := sha256.Sum256(node[:])
		run(`INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at)VALUES(?,?,?,'active',?,?)`, UUIDBytes(node), UUIDBytes(workspace), node.String(), fixtureTimestamp(t, now), fixtureTimestamp(t, now))
		run(`INSERT INTO node_endpoint_keys(node_id,endpoint_id,state,bound_at)VALUES(?,?,'active',?)`, UUIDBytes(node), endpoint[:], fixtureTimestamp(t, now))
		v, _, err := operations.CreateSynthetic(ctx, operationRequest(node, trace))
		if err != nil {
			t.Fatal(err)
		}
		command := uuid.MustParse(*v.CommandID)
		var encoded []byte
		if err := owner.QueryRow(ctx, `SELECT envelope FROM commands WHERE id=?`, UUIDBytes(command)).Scan(&encoded); err != nil {
			t.Fatal(err)
		}
		var envelope agentv1.CommandEnvelope
		if err := proto.Unmarshal(encoded, &envelope); err != nil {
			t.Fatal(err)
		}
		return node, uuid.MustParse(v.ID), &envelope
	}
	resultEvent := func(node uuid.UUID, envelope *agentv1.CommandEnvelope, state agentv1.CommandResultState, code string) *transportv1.TransportEvent {
		t.Helper()
		at := timestamppb.Now()
		result := &agentv1.CommandResult{CommandId: envelope.GetCommandId(), IdempotencyKey: envelope.GetIdempotencyKey(), PayloadSha256: envelope.GetSemanticPayloadSha256(), SemanticPayloadHashVersion: envelope.GetSemanticPayloadHashVersion(), State: state, ErrorCode: code, AcceptedAt: at, CompletedAt: at}
		encoded, err := proto.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		id := uuid.Must(uuid.NewV7())
		endpoint := sha256.Sum256(node[:])
		return &transportv1.TransportEvent{EventId: id[:], NodeId: node[:], EndpointId: endpoint[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT, OccurredAt: at, Traceparent: trace, Payload: encoded}
	}
	assertState := func(id uuid.UUID, want string) {
		t.Helper()
		v, err := operations.Get(ctx, id)
		if err != nil || v.State != want {
			t.Fatal("operation state", v, err, "want", want)
		}
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := owner.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	node, operation, envelope := create()
	queued := resultEvent(node, envelope, agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED, "")
	if err := ingress.Ingest(ctx, queued); err != nil {
		t.Fatal(err)
	}
	assertState(operation, "queued")
	if count(`SELECT count(*) FROM transport_event_quarantine WHERE event_id=?`, queued.GetEventId()) != 1 || count(`SELECT count(*) FROM transport_events WHERE event_id=?`, queued.GetEventId()) != 0 {
		t.Fatal("queued result was not quarantined atomically")
	}
	if err := ingress.Ingest(ctx, queued); err != nil {
		t.Fatal("quarantine replay", err)
	}
	collision := proto.Clone(queued).(*transportv1.TransportEvent)
	collision.Payload = []byte{0x80}
	if err := ingress.Ingest(ctx, collision); err == nil {
		t.Fatal("quarantine evidence collision accepted")
	}
	if count(`SELECT count(*) FROM security_alerts WHERE resource_id=?`, queued.GetEventId()) != 1 {
		t.Fatal("quarantine replay duplicated alert")
	}
	jobs, err := operations.Claim(ctx, worker, 1, time.Minute)
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	d := jobs[0]
	valid := resultEvent(node, envelope, agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED, "")
	if err := ingress.Ingest(ctx, valid); err != nil {
		t.Fatal("result before MarkSent", err)
	}
	assertState(operation, "succeeded")
	if err := operations.MarkSent(ctx, d); err != nil {
		t.Fatal("late MarkSent", err)
	}
	if err := ingress.Ingest(ctx, valid); err != nil {
		t.Fatal("duplicate event", err)
	}
	if count(`SELECT count(*) FROM agent_command_results WHERE command_id=?`, UUIDBytes(d.CommandID)) != 1 || count(`SELECT count(*) FROM node_command_leases WHERE node_id=?`, UUIDBytes(node)) != 0 {
		t.Fatal("result duplication or lease leak")
	}
	var accepted, completed, created value.Timestamp
	if err := owner.QueryRow(ctx, `SELECT accepted_at,completed_at,created_at FROM agent_command_results WHERE event_id=?`, valid.GetEventId()).Scan(&accepted, &completed, &created); err != nil || !accepted.Valid || !completed.Valid || !created.Valid {
		t.Fatal("logical result clocks", accepted, completed, created, err)
	}
	wrong := proto.Clone(valid).(*transportv1.TransportEvent)
	badID := uuid.Must(uuid.NewV7())
	wrong.EventId = badID[:]
	wrong.EndpointId = bytes.Repeat([]byte{7}, 32)
	if err := ingress.Ingest(ctx, wrong); err != nil {
		t.Fatal(err)
	}
	if count(`SELECT count(*) FROM transport_event_quarantine WHERE event_id=?`, wrong.GetEventId()) != 1 {
		t.Fatal("untrusted endpoint accepted")
	}
	assertState(operation, "succeeded")

	node, operation, envelope = create()
	jobs, err = operations.Claim(ctx, worker, 1, time.Minute)
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	if err := operations.MarkSent(ctx, jobs[0]); err != nil {
		t.Fatal(err)
	}
	unknown := resultEvent(node, envelope, agentv1.CommandResultState_COMMAND_RESULT_STATE_UNKNOWN, "privd_outcome_unknown")
	run(`UPDATE commands SET updated_at=? WHERE id=?`, value.Timestamp{Valid: true, Micros: value.PositiveInfinity}, envelope.GetCommandId())
	if err := ingress.Ingest(ctx, unknown); err != nil {
		t.Fatal("unknown result", err)
	}
	assertState(operation, "unknown")
	jobs, err = operations.Claim(ctx, worker, 1, time.Minute)
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	var reconcile agentv1.CommandEnvelope
	if err := proto.Unmarshal(jobs[0].Envelope, &reconcile); err != nil || reconcile.GetDeliveryMode() != agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY || !bytes.Equal(reconcile.GetCommandId(), envelope.GetCommandId()) {
		t.Fatal("recovery identity/mode", &reconcile, err)
	}
	absent := resultEvent(node, &reconcile, agentv1.CommandResultState_COMMAND_RESULT_STATE_UNKNOWN, "effect_absent")
	if err := ingress.Ingest(ctx, absent); err != nil {
		t.Fatal("verified absence mode", err)
	}
	if err := operations.MarkSent(ctx, jobs[0]); err != nil {
		t.Fatal("late reconciliation MarkSent", err)
	}
	var encoded []byte
	if err := owner.QueryRow(ctx, `SELECT envelope FROM commands WHERE id=?`, reconcile.GetCommandId()).Scan(&encoded); err != nil {
		t.Fatal(err)
	}
	var retry agentv1.CommandEnvelope
	if err := proto.Unmarshal(encoded, &retry); err != nil || retry.GetDeliveryMode() != agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RETRY_IF_EFFECT_ABSENT {
		t.Fatal("absence did not permit bounded retry", &retry, err)
	}
	// Keep unrelated recovery out of the following claim.
	run(`UPDATE outbox_events SET available_at=? WHERE command_id=?`, value.Timestamp{Valid: true, Micros: value.PositiveInfinity}, retry.GetCommandId())

	node, operation, envelope = create()
	jobs, err = operations.Claim(ctx, worker, 1, time.Minute)
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	valid = resultEvent(node, envelope, agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED, "")
	before, err := ingress.LastEventID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run(`CREATE TRIGGER transport_result_test_failure BEFORE INSERT ON operation_events FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='isolated result rollback'`)
	err = ingress.Ingest(ctx, valid)
	run(`DROP TRIGGER transport_result_test_failure`)
	if err == nil {
		t.Fatal("injected result failure committed")
	}
	assertState(operation, "queued")
	after, err := ingress.LastEventID(ctx)
	if err != nil || !bytes.Equal(before, after) || count(`SELECT count(*) FROM agent_command_results WHERE event_id=?`, valid.GetEventId()) != 0 || count(`SELECT count(*) FROM transport_events WHERE event_id=?`, valid.GetEventId()) != 0 {
		t.Fatal("failure advanced cursor or partially persisted", err)
	}
	if err := ingress.Ingest(ctx, valid); err != nil {
		t.Fatal("retry rolled-back result", err)
	}
	assertState(operation, "succeeded")
	runResultProjectionWorkflows(t, owner, backend, workspace, signer)
	if err := owner.ValidateSchema(ctx, 35); err != nil {
		t.Fatal(err)
	}
}

func operationRequest(node uuid.UUID, trace string) operations.CreateRequest {
	return operations.CreateRequest{NodeID: node, IdempotencyKey: uuid.NewString(), ExpectedVersion: 1, Kind: operations.SyntheticEcho, Message: "transport", TTL: time.Hour, RequestID: uuid.NewString(), Traceparent: trace}
}
