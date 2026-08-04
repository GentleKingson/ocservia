package localslice

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/semanticpayload"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestAgentPayloadHashMatchesRustGoldenVectors(t *testing.T) {
	node := make([]byte, 16)
	for index := range node {
		node[index] = byte(index)
	}
	envelope := agentv1.CommandEnvelope{NodeId: node, ExpectedRevision: 1}
	cases := []struct {
		message  string
		expected string
	}{
		{"", "2d6daaae892285c786fba378aff37d4d0436dae76699061500b548e939782433"},
		{"hello", "a7299856cb7fa4e266b1234614361677e1d1b2466608d014e71b4a547c804397"},
		{"你好", "10f1ae9994bde9ea69de8fb965bf5029e97ad8b1bf1f00834d47831f60adf38d"},
		{strings.Repeat("x", 4096), "d6f3a8f8d0fc7ccbbfe6fec9f93bcad544d98fec0f3501aeef8be2a5d2b78daa"},
	}
	for _, test := range cases {
		message := test.message
		envelope.Payload = &agentv1.CommandEnvelope_SyntheticEcho{SyntheticEcho: &agentv1.SyntheticEcho{Message: message}}
		hash, err := agentPayloadHash(&envelope)
		if err != nil {
			t.Fatal(err)
		}
		if hex.EncodeToString(hash[:]) != test.expected {
			t.Fatalf("hash for %d-byte echo = %x", len(message), hash)
		}
	}
	envelope.Payload = &agentv1.CommandEnvelope_SyntheticNoop{SyntheticNoop: &agentv1.SyntheticNoop{}}
	hash, err := agentPayloadHash(&envelope)
	if err != nil || hex.EncodeToString(hash[:]) != "2e5b198f3c3a2718113a4dbf2a552c730ddede13567ee448c6118459ccfa0d98" {
		t.Fatalf("noop hash = %x, %v", hash, err)
	}
	envelope.Payload = &agentv1.CommandEnvelope_SyntheticEcho{SyntheticEcho: &agentv1.SyntheticEcho{Message: "hello"}}
	envelope.ExpectedRevision = 2
	hash, err = agentPayloadHash(&envelope)
	if err != nil || hex.EncodeToString(hash[:]) != "79659b4e1819080191867174096d8aa5d01a43cb634cab9c51b113391643c343" {
		t.Fatalf("revision hash = %x, %v", hash, err)
	}
	envelope.ExpectedRevision = 1
	for index := range envelope.NodeId {
		envelope.NodeId[index]++
	}
	hash, err = agentPayloadHash(&envelope)
	if err != nil || hex.EncodeToString(hash[:]) != "a45222e4babe147a02b9274937f09c337ac56764a56c61a9ebfd901d4fec7afe" {
		t.Fatalf("node hash = %x, %v", hash, err)
	}
}

func TestDisconnectedEventPreservesUntrustedNodeStatesIntegration(t *testing.T) {
	databaseURL := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	workspaceID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id,name,slug,created_at,updated_at) VALUES ($1,'Revoked event test',$2,now(),now())`, workspaceID, "revoked-event-"+workspaceID.String()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM workspaces WHERE id=$1`, workspaceID) })
	for _, initialStatus := range []string{"pending", "revoked"} {
		nodeID := uuid.Must(uuid.NewV7())
		if _, err := pool.Exec(ctx, `INSERT INTO nodes (id,workspace_id,name,status,created_at,updated_at) VALUES ($1,$2,$3,$4,now(),now())`, nodeID, workspaceID, initialStatus+"-node", initialStatus); err != nil {
			t.Fatal(err)
		}
		eventID := uuid.Must(uuid.NewV7())
		err = New(pool).Ingest(ctx, &transportv1.TransportEvent{EventId: eventID[:], NodeId: nodeID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_DISCONNECTED, OccurredAt: timestamppb.Now(), Traceparent: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01", Payload: []byte(initialStatus + " disconnect")})
		if err != nil {
			t.Fatal(err)
		}
		var status string
		if err := pool.QueryRow(ctx, `SELECT status FROM nodes WHERE id=$1`, nodeID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != initialStatus {
			t.Fatalf("%s node status after disconnect = %q", initialStatus, status)
		}
	}
}

func TestStructuredAgentResultPersistsUnknownBeforeReconciledSuccessIntegration(t *testing.T) {
	databaseURL := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	workspaceID, nodeID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'I10 test',$2,now(),now())`, workspaceID, "i10-"+workspaceID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at) VALUES($1,$2,$3,'active',1,now(),now())`, nodeID, workspaceID, "node-"+nodeID.String()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, statement := range []string{
			`DELETE FROM agent_command_results WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
			`DELETE FROM transport_events WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`,
			`DELETE FROM node_command_leases WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
			`DELETE FROM command_attempts WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
			`DELETE FROM outbox_events WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
			`DELETE FROM commands WHERE workspace_id=$1`, `DELETE FROM operations WHERE workspace_id=$1`,
			`DELETE FROM audit_events WHERE workspace_id=$1`, `DELETE FROM nodes WHERE workspace_id=$1`,
			`DELETE FROM workspaces WHERE id=$1`,
		} {
			_, _ = pool.Exec(context.Background(), statement, workspaceID)
		}
	})
	operationID, commandID, idempotencyKey := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	messageID := uuid.Must(uuid.NewV7())
	now := time.Now().UTC()
	envelope := agentv1.CommandEnvelope{ProtocolVersion: "1.0", MessageId: messageID[:], CommandId: commandID[:], IdempotencyKey: idempotencyKey[:], NodeId: nodeID[:], Sequence: 1, IssuedAt: timestamppb.New(now), ExpiresAt: timestamppb.New(now.Add(time.Minute)), ExpectedRevision: 1, Traceparent: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01", ActorId: "test", Reason: "I10 result ingestion", DeliveryMode: agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_EXECUTE_OR_REPLAY, Payload: &agentv1.CommandEnvelope_SyntheticEcho{SyntheticEcho: &agentv1.SyntheticEcho{Message: "hello"}}}
	envelopeBytes, err := proto.Marshal(&envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO operations(id,workspace_id,node_id,command_id,state,request_id,trace_id,created_at,updated_at,idempotency_key,request_hash,expires_at) VALUES($1,$2,$3,$4,'dispatched','i10-result','0123456789abcdef0123456789abcdef',now(),now(),'journal-result',$5,$6)`, operationID, workspaceID, nodeID, commandID, make([]byte, 32), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO commands(id,operation_id,workspace_id,node_id,state,payload_type,envelope,idempotency_key,expected_version,sequence,traceparent,expires_at,created_at,updated_at) VALUES($1,$2,$3,$4,'dispatched','synthetic_echo',$5,'journal-result',1,1,$6,$7,$8,$8)`, commandID, operationID, workspaceID, nodeID, envelopeBytes, envelope.GetTraceparent(), now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	payloadHash, err := agentPayloadHash(&envelope)
	if err != nil {
		t.Fatal(err)
	}
	ingest := func(state agentv1.CommandResultState, replayed bool) {
		t.Helper()
		result := &agentv1.CommandResult{CommandId: envelope.GetCommandId(), IdempotencyKey: envelope.GetIdempotencyKey(), PayloadSha256: payloadHash[:], State: state, Result: []byte("hello"), AcceptedAt: timestamppb.Now(), CompletedAt: timestamppb.Now(), Replayed: replayed}
		if state == agentv1.CommandResultState_COMMAND_RESULT_STATE_UNKNOWN {
			result.Result = nil
			result.ErrorCode = "outcome_requires_reconciliation"
		}
		payload, err := proto.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		eventID := uuid.Must(uuid.NewV7())
		if err := New(pool).Ingest(ctx, &transportv1.TransportEvent{EventId: eventID[:], NodeId: nodeID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT, OccurredAt: timestamppb.Now(), Traceparent: envelope.GetTraceparent(), Payload: payload}); err != nil {
			t.Fatal(err)
		}
	}
	ingest(agentv1.CommandResultState_COMMAND_RESULT_STATE_UNKNOWN, false)
	var state string
	if err := pool.QueryRow(ctx, `SELECT state FROM operations WHERE id=$1`, operationID).Scan(&state); err != nil || state != "unknown" {
		t.Fatalf("unknown state = %q, %v", state, err)
	}
	ingest(agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED, true)
	var resultCount int
	if err := pool.QueryRow(ctx, `SELECT operation.state,count(result.event_id) FROM operations AS operation JOIN commands AS command ON command.operation_id=operation.id JOIN agent_command_results AS result ON result.command_id=command.id WHERE operation.id=$1 GROUP BY operation.state`, operationID).Scan(&state, &resultCount); err != nil {
		t.Fatal(err)
	}
	if state != "succeeded" || resultCount != 2 {
		t.Fatalf("reconciled state/results = %q/%d", state, resultCount)
	}
}

type commandResultFixture struct {
	pool         *pgxpool.Pool
	service      *Service
	workspaceID  uuid.UUID
	nodeID       uuid.UUID
	operationID  uuid.UUID
	commandID    uuid.UUID
	envelope     *agentv1.CommandEnvelope
	payloadHash  [32]byte
	traceparent  string
	issuedAt     time.Time
	originalTime time.Time
}

func newCommandResultFixture(t *testing.T) commandResultFixture {
	t.Helper()
	databaseURL := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	workspaceID, nodeID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	operationID, commandID, key := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	messageID := uuid.Must(uuid.NewV7())
	now := time.Now().UTC().Truncate(time.Microsecond)
	traceparent := "00-1123456789abcdef0123456789abcdef-1123456789abcdef-01"
	envelope := agentv1.CommandEnvelope{
		ProtocolVersion: "1.0", MessageId: messageID[:], CommandId: commandID[:],
		IdempotencyKey: key[:], NodeId: nodeID[:], Sequence: 1,
		IssuedAt: timestamppb.New(now), ExpiresAt: timestamppb.New(now.Add(time.Hour)),
		ExpectedRevision: 1, Traceparent: traceparent, ActorId: "test", Reason: "strict result validation",
		DeliveryMode: agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_EXECUTE_OR_REPLAY,
		Payload:      &agentv1.CommandEnvelope_SyntheticEcho{SyntheticEcho: &agentv1.SyntheticEcho{Message: "hello"}},
	}
	envelopeBytes, err := proto.Marshal(&envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'strict result',$2,$3,$3)`, workspaceID, "strict-result-"+workspaceID.String(), now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at) VALUES($1,$2,$3,'active',1,$4,$4)`, nodeID, workspaceID, "node-"+nodeID.String(), now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO operations(id,workspace_id,node_id,command_id,state,request_id,trace_id,created_at,updated_at,idempotency_key,request_hash,expires_at) VALUES($1,$2,$3,$4,'dispatched','strict-result','1123456789abcdef0123456789abcdef',$5,$5,'strict-result',$6,$7)`, operationID, workspaceID, nodeID, commandID, now, make([]byte, 32), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO commands(id,operation_id,workspace_id,node_id,state,payload_type,envelope,idempotency_key,expected_version,sequence,traceparent,expires_at,created_at,updated_at) VALUES($1,$2,$3,$4,'dispatched','synthetic_echo',$5,'strict-result',1,1,$6,$7,$8,$8)`, commandID, operationID, workspaceID, nodeID, envelopeBytes, traceparent, now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	payloadHash, err := agentPayloadHash(&envelope)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, statement := range []string{
			`DELETE FROM operation_events WHERE operation_id=$1`,
			`DELETE FROM agent_command_results WHERE command_id=$2`,
			`DELETE FROM transport_events WHERE node_id=$3`,
			`DELETE FROM outbox_events WHERE command_id=$2`,
			`DELETE FROM commands WHERE id=$2`, `DELETE FROM operations WHERE id=$1`,
			`DELETE FROM nodes WHERE id=$3`, `DELETE FROM workspaces WHERE id=$4`,
		} {
			_, _ = pool.Exec(context.Background(), statement, operationID, commandID, nodeID, workspaceID)
		}
		pool.Close()
	})
	return commandResultFixture{pool: pool, service: New(pool), workspaceID: workspaceID, nodeID: nodeID, operationID: operationID, commandID: commandID, envelope: &envelope, payloadHash: payloadHash, traceparent: traceparent, issuedAt: now, originalTime: now}
}

func (fixture *commandResultFixture) validResult() *agentv1.CommandResult {
	return &agentv1.CommandResult{
		CommandId: fixture.commandID[:], IdempotencyKey: fixture.envelope.GetIdempotencyKey(),
		PayloadSha256: fixture.payloadHash[:], State: agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED,
		Result: []byte("hello"), AcceptedAt: timestamppb.New(fixture.issuedAt), CompletedAt: timestamppb.New(fixture.issuedAt),
	}
}

func (fixture *commandResultFixture) ingestResult(t *testing.T, result *agentv1.CommandResult) (uuid.UUID, error) {
	t.Helper()
	payload, err := proto.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	eventID := uuid.Must(uuid.NewV7())
	err = fixture.service.Ingest(context.Background(), &transportv1.TransportEvent{
		EventId: eventID[:], NodeId: fixture.nodeID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT,
		OccurredAt: timestamppb.Now(), Traceparent: fixture.traceparent, Payload: payload,
	})
	return eventID, err
}

func TestMalformedCommandResultRollsBackTransactionIntegration(t *testing.T) {
	fixture := newCommandResultFixture(t)
	eventID := uuid.Must(uuid.NewV7())
	err := fixture.service.Ingest(context.Background(), &transportv1.TransportEvent{
		EventId: eventID[:], NodeId: fixture.nodeID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT,
		OccurredAt: timestamppb.Now(), Traceparent: fixture.traceparent, Payload: []byte{0x80},
	})
	if err == nil {
		t.Fatal("malformed command_result was accepted")
	}
	var transportCount, resultCount, operationEventCount int
	var operationState, commandState string
	if err := fixture.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM transport_events WHERE event_id=$1),(SELECT count(*) FROM agent_command_results WHERE command_id=$2),(SELECT count(*) FROM operation_events WHERE operation_id=$3),(SELECT state FROM operations WHERE id=$3),(SELECT state FROM commands WHERE id=$2)`, eventID, fixture.commandID, fixture.operationID).Scan(&transportCount, &resultCount, &operationEventCount, &operationState, &commandState); err != nil {
		t.Fatal(err)
	}
	if transportCount != 0 || resultCount != 0 || operationEventCount != 0 || operationState != "dispatched" || commandState != "dispatched" {
		t.Fatalf("partial malformed result transaction: transport=%d result=%d operation_events=%d operation=%s command=%s", transportCount, resultCount, operationEventCount, operationState, commandState)
	}
}

func TestInvalidCommandResultStatesAndTimesFailClosedIntegration(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*agentv1.CommandResult, time.Time)
	}{
		{"succeeded_with_error", func(result *agentv1.CommandResult, _ time.Time) { result.ErrorCode = "unexpected" }},
		{"failed_without_error", func(result *agentv1.CommandResult, _ time.Time) {
			result.State = agentv1.CommandResultState_COMMAND_RESULT_STATE_FAILED
		}},
		{"unknown_without_reason", func(result *agentv1.CommandResult, _ time.Time) {
			result.State = agentv1.CommandResultState_COMMAND_RESULT_STATE_UNKNOWN
			result.Result = nil
		}},
		{"rejected_with_accepted_at", func(result *agentv1.CommandResult, _ time.Time) {
			result.State = agentv1.CommandResultState_COMMAND_RESULT_STATE_REJECTED
			result.ErrorCode = "rejected"
			result.Result = nil
		}},
		{"completed_before_accepted", func(result *agentv1.CommandResult, now time.Time) {
			result.AcceptedAt = timestamppb.New(now)
			result.CompletedAt = timestamppb.New(now.Add(-time.Second))
		}},
		{"completed_before_issued", func(result *agentv1.CommandResult, now time.Time) {
			result.AcceptedAt = timestamppb.New(now.Add(-10 * time.Minute))
			result.CompletedAt = timestamppb.New(now.Add(-10 * time.Minute))
		}},
		{"future_completed", func(result *agentv1.CommandResult, now time.Time) {
			result.AcceptedAt = timestamppb.New(now)
			result.CompletedAt = timestamppb.New(now.Add(10 * time.Minute))
		}},
		{"unknown_enum", func(result *agentv1.CommandResult, _ time.Time) { result.State = agentv1.CommandResultState(99) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newCommandResultFixture(t)
			result := fixture.validResult()
			test.mutate(result, fixture.issuedAt)
			eventID, err := fixture.ingestResult(t, result)
			if err == nil {
				t.Fatal("invalid result was accepted")
			}
			var count int
			if err := fixture.pool.QueryRow(context.Background(), `SELECT count(*) FROM transport_events WHERE event_id=$1`, eventID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("invalid result was partially persisted: count=%d err=%v", count, err)
			}
		})
	}
}

func TestAgentResultCannotRegressAuthorityTimestampsIntegration(t *testing.T) {
	fixture := newCommandResultFixture(t)
	authorityTime := time.Now().UTC().Add(2 * time.Minute).Truncate(time.Microsecond)
	if _, err := fixture.pool.Exec(context.Background(), `UPDATE commands SET updated_at=$2 WHERE id=$1`, fixture.commandID, authorityTime); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(context.Background(), `UPDATE operations SET updated_at=$2 WHERE id=$1`, fixture.operationID, authorityTime); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.ingestResult(t, fixture.validResult()); err != nil {
		t.Fatal(err)
	}
	var commandUpdatedAt, operationUpdatedAt time.Time
	if err := fixture.pool.QueryRow(context.Background(), `SELECT command.updated_at,operation.updated_at FROM commands command JOIN operations operation ON operation.id=command.operation_id WHERE command.id=$1`, fixture.commandID).Scan(&commandUpdatedAt, &operationUpdatedAt); err != nil {
		t.Fatal(err)
	}
	if commandUpdatedAt.Before(authorityTime) || operationUpdatedAt.Before(authorityTime) {
		t.Fatalf("authority timestamp regressed: command=%s operation=%s baseline=%s", commandUpdatedAt, operationUpdatedAt, authorityTime)
	}
}

func TestContradictoryTerminalResultRollsBackIntegration(t *testing.T) {
	fixture := newCommandResultFixture(t)
	if _, err := fixture.ingestResult(t, fixture.validResult()); err != nil {
		t.Fatal(err)
	}
	contradiction := fixture.validResult()
	contradiction.State = agentv1.CommandResultState_COMMAND_RESULT_STATE_FAILED
	contradiction.ErrorCode = "late_failure"
	eventID, err := fixture.ingestResult(t, contradiction)
	if err == nil {
		t.Fatal("contradictory terminal result was accepted")
	}
	var transportCount, resultCount int
	var operationState, commandState string
	if err := fixture.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM transport_events WHERE event_id=$1),(SELECT count(*) FROM agent_command_results WHERE command_id=$2),(SELECT state FROM operations WHERE id=$3),(SELECT state FROM commands WHERE id=$2)`, eventID, fixture.commandID, fixture.operationID).Scan(&transportCount, &resultCount, &operationState, &commandState); err != nil {
		t.Fatal(err)
	}
	if transportCount != 0 || resultCount != 1 || operationState != "succeeded" || commandState != "succeeded" {
		t.Fatalf("terminal contradiction left partial state: transport=%d results=%d operation=%s command=%s", transportCount, resultCount, operationState, commandState)
	}
}

func TestUnknownSchedulesExplicitReconcileThenSafeRetryIntegration(t *testing.T) {
	fixture := newCommandResultFixture(t)
	outboxID := uuid.Must(uuid.NewV7())
	envelopeBytes, err := proto.Marshal(fixture.envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(context.Background(), `INSERT INTO outbox_events(id,command_id,event_type,payload,available_at,published_at,created_at) VALUES($1,$2,'command.dispatch',$3,$4,$4,$4)`, outboxID, fixture.commandID, envelopeBytes, fixture.issuedAt); err != nil {
		t.Fatal(err)
	}
	unauthorizedRetry := fixture.validResult()
	unauthorizedRetry.State = agentv1.CommandResultState_COMMAND_RESULT_STATE_UNKNOWN
	unauthorizedRetry.Result = nil
	unauthorizedRetry.ErrorCode = "effect_absent"
	unauthorizedEventID, err := fixture.ingestResult(t, unauthorizedRetry)
	if err == nil {
		t.Fatal("effect absence without a prior reconcile was accepted")
	}
	var unauthorizedCount int
	if err := fixture.pool.QueryRow(context.Background(), `SELECT count(*) FROM transport_events WHERE event_id=$1`, unauthorizedEventID).Scan(&unauthorizedCount); err != nil || unauthorizedCount != 0 {
		t.Fatalf("unauthorized safe retry was partially persisted: count=%d err=%v", unauthorizedCount, err)
	}
	unknown := fixture.validResult()
	unknown.State = agentv1.CommandResultState_COMMAND_RESULT_STATE_UNKNOWN
	unknown.Result = nil
	unknown.ErrorCode = "outcome_requires_reconciliation"
	if _, err := fixture.ingestResult(t, unknown); err != nil {
		t.Fatal(err)
	}
	assertOutboxMode := func(expected agentv1.CommandDeliveryMode) {
		t.Helper()
		var payload []byte
		var unpublished bool
		if err := fixture.pool.QueryRow(context.Background(), `SELECT payload,published_at IS NULL FROM outbox_events WHERE id=$1`, outboxID).Scan(&payload, &unpublished); err != nil {
			t.Fatal(err)
		}
		var envelope agentv1.CommandEnvelope
		if err := proto.Unmarshal(payload, &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.GetDeliveryMode() != expected || !unpublished {
			t.Fatalf("recovery dispatch = %s, unpublished=%v", envelope.GetDeliveryMode(), unpublished)
		}
	}
	assertOutboxMode(agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY)

	effectAbsent := fixture.validResult()
	effectAbsent.State = agentv1.CommandResultState_COMMAND_RESULT_STATE_UNKNOWN
	effectAbsent.Result = nil
	effectAbsent.ErrorCode = "effect_absent"
	effectAbsent.Replayed = true
	if _, err := fixture.ingestResult(t, effectAbsent); err != nil {
		t.Fatal(err)
	}
	assertOutboxMode(agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RETRY_IF_EFFECT_ABSENT)
}

// ---- Canonical Semantic Payload Hash v1 ----
//
// The tests below pin the v1 algorithm defined in
// docs/development/command-semantic-hash-v1.md against the shared golden fixture
// testdata/semantic-payload-hash-v1.json. The inlined computeV1Canonical mirror
// is an independent reference; the production semanticpayload.HashV1 function
// must produce identical bytes.

// v1DomainSeparator is the ASCII label plus a NUL terminator.
var v1DomainSeparator = []byte("ocservia.command.semantic-hash.v1\x00")

func computeV1Canonical(nodeID []byte, expectedRevision uint64, payloadKind uint32, canonicalPayload []byte) [sha256.Size]byte {
	h := sha256.New()
	h.Write(v1DomainSeparator)
	h.Write(nodeID)
	var rev [8]byte
	binary.BigEndian.PutUint64(rev[:], expectedRevision)
	h.Write(rev[:])
	var kind [4]byte
	binary.BigEndian.PutUint32(kind[:], payloadKind)
	h.Write(kind[:])
	h.Write(canonicalPayload)
	var out [sha256.Size]byte
	copy(out[:], h.Sum(nil))
	return out
}

func v1CanonicalPayload(payloadKind uint32, message string) []byte {
	switch payloadKind {
	case 107: // SyntheticNoop
		return nil
	case 108: // SyntheticEcho: u32_be(len(utf8)) || utf8
		utf8 := []byte(message)
		out := make([]byte, 4+len(utf8))
		binary.BigEndian.PutUint32(out[:4], uint32(len(utf8)))
		copy(out[4:], utf8)
		return out
	default:
		panic("unsupported payload_kind in test")
	}
}

func v1FixturePath(t *testing.T) string {
	t.Helper()
	// go test runs with the package directory as CWD, so the repo-rooted
	// testdata file is three levels up.
	candidates := []string{
		filepath.Join("..", "..", "..", "testdata", "semantic-payload-hash-v1.json"),
	}
	// Fallback: resolve from this source file's location so the test still
	// works under tools that change the working directory.
	if _, file, _, ok := runtime.Caller(0); ok {
		candidates = append(candidates, filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "testdata", "semantic-payload-hash-v1.json"))
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c
		}
	}
	t.Fatalf("semantic payload hash v1 fixture not found relative to %s", candidates[0])
	return ""
}

type v1Vector struct {
	Name             string `json:"name"`
	NodeIDHex        string `json:"node_id_hex"`
	ExpectedRevision uint64 `json:"expected_revision"`
	PayloadKind      uint64 `json:"payload_kind"`
	Payload          struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"payload"`
	CanonicalPreimageHex string `json:"canonical_preimage_hex"`
	ExpectedSHA256       string `json:"expected_sha256"`
}

func loadV1Fixture(t *testing.T) []v1Vector {
	t.Helper()
	raw, err := os.ReadFile(v1FixturePath(t))
	if err != nil {
		t.Fatalf("read v1 fixture: %v", err)
	}
	var doc struct {
		Version int        `json:"version"`
		Vectors []v1Vector `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse v1 fixture: %v", err)
	}
	if doc.Version != 1 {
		t.Fatalf("unexpected fixture version %d", doc.Version)
	}
	return doc.Vectors
}

// v1EnvelopeFromVector builds a CommandEnvelope from a fixture vector so the
// production hash function can be exercised against the golden vectors.
func v1EnvelopeFromVector(nodeID []byte, expectedRevision uint64, payloadKind uint32, message string) *agentv1.CommandEnvelope {
	envelope := &agentv1.CommandEnvelope{NodeId: nodeID, ExpectedRevision: expectedRevision}
	switch payloadKind {
	case 107:
		envelope.Payload = &agentv1.CommandEnvelope_SyntheticNoop{SyntheticNoop: &agentv1.SyntheticNoop{}}
	case 108:
		envelope.Payload = &agentv1.CommandEnvelope_SyntheticEcho{SyntheticEcho: &agentv1.SyntheticEcho{Message: message}}
	default:
		panic("unsupported payload_kind in test")
	}
	return envelope
}

func TestCanonicalSemanticHashV1MatchesSharedFixture(t *testing.T) {
	for _, vector := range loadV1Fixture(t) {
		nodeID, err := hex.DecodeString(vector.NodeIDHex)
		if err != nil {
			t.Fatalf("%s: decode node_id: %v", vector.Name, err)
		}
		// Cross-check: the test-only mirror must agree with the fixture.
		payload := v1CanonicalPayload(uint32(vector.PayloadKind), vector.Payload.Message)
		mirror := computeV1Canonical(nodeID, vector.ExpectedRevision, uint32(vector.PayloadKind), payload)
		if hex.EncodeToString(mirror[:]) != vector.ExpectedSHA256 {
			t.Fatalf("%s: mirror digest mismatch\ngot  %x\nwant %s", vector.Name, mirror, vector.ExpectedSHA256)
		}
		// Production function must agree with the mirror.
		envelope := v1EnvelopeFromVector(nodeID, vector.ExpectedRevision, uint32(vector.PayloadKind), vector.Payload.Message)
		got, gErr := semanticpayload.HashV1(envelope)
		if gErr != nil {
			t.Fatalf("%s: production hash error: %v", vector.Name, gErr)
		}
		if hex.EncodeToString(got[:]) != vector.ExpectedSHA256 {
			t.Fatalf("%s: production digest mismatch\ngot  %x\nwant %s", vector.Name, got, vector.ExpectedSHA256)
		}
	}
}

func TestCanonicalSemanticHashV1ExcludesDeliveryMetadata(t *testing.T) {
	node := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	// Build two envelopes that differ only in delivery/audit metadata.
	a := v1EnvelopeFromVector(node, 1, 108, "hello")
	a.MessageId = []byte{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1}
	a.CommandId = []byte{2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2}
	a.IdempotencyKey = []byte{3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3}
	a.Sequence = 42
	a.Traceparent = "00-aaa"
	a.ActorId = "alice"
	a.Reason = "ticket-1"
	a.DeliveryMode = agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_EXECUTE_OR_REPLAY
	b := v1EnvelopeFromVector(node, 1, 108, "hello")
	b.MessageId = []byte{9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9}
	b.CommandId = []byte{8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8}
	b.IdempotencyKey = []byte{7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7}
	b.Sequence = 99
	b.Traceparent = "00-bbb"
	b.ActorId = "bob"
	b.Reason = "ticket-2"
	b.DeliveryMode = agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY
	// Only node_id, revision, kind, and payload bytes enter the preimage,
	// so changing delivery/audit fields cannot affect the digest.
	hashA, errA := semanticpayload.HashV1(a)
	if errA != nil {
		t.Fatalf("hash a: %v", errA)
	}
	hashB, errB := semanticpayload.HashV1(b)
	if errB != nil {
		t.Fatalf("hash b: %v", errB)
	}
	if hashA != hashB {
		t.Fatal("identical semantic inputs must hash equally")
	}
}

func TestCanonicalSemanticHashV1ChangesOnSemanticInput(t *testing.T) {
	node := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	nodeShifted := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}

	hello, err := semanticpayload.HashV1(v1EnvelopeFromVector(node, 1, 108, "hello"))
	if err != nil {
		t.Fatalf("hash hello: %v", err)
	}
	if world, err := semanticpayload.HashV1(v1EnvelopeFromVector(node, 1, 108, "world")); err != nil || world == hello {
		t.Fatal("message change must change the hash")
	}
	if shifted, err := semanticpayload.HashV1(v1EnvelopeFromVector(nodeShifted, 1, 108, "hello")); err != nil || shifted == hello {
		t.Fatal("node_id change must change the hash")
	}
	if rev2, err := semanticpayload.HashV1(v1EnvelopeFromVector(node, 2, 108, "hello")); err != nil || rev2 == hello {
		t.Fatal("revision change must change the hash")
	}
	if noop, err := semanticpayload.HashV1(v1EnvelopeFromVector(node, 1, 107, "")); err != nil || noop == hello {
		t.Fatal("payload kind change must change the hash")
	}
}

func TestCanonicalSemanticHashV1RejectsUnicodeNormalization(t *testing.T) {
	node := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	composed, err := semanticpayload.HashV1(v1EnvelopeFromVector(node, 1, 108, "\u00e9"))
	if err != nil {
		t.Fatalf("hash composed: %v", err)
	}
	decomposed, err := semanticpayload.HashV1(v1EnvelopeFromVector(node, 1, 108, "e\u0301"))
	if err != nil {
		t.Fatalf("hash decomposed: %v", err)
	}
	if composed == decomposed {
		t.Fatal("no Unicode normalization: different bytes must hash differently")
	}
}
