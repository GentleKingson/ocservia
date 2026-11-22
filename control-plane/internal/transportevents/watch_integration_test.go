package transportevents

import (
	"bytes"
	"context"
	"crypto/sha256"
	"math"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"github.com/GentleKingson/ocservia/control-plane/internal/transportclient"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type retainedEventServer struct {
	transportv1.UnimplementedTransportServiceServer
	events          []*transportv1.TransportEvent
	eventsDelivered chan struct{}
	releaseStream   chan struct{}
	deliveryOnce    sync.Once
	finalCursorSeen chan struct{}
	finalOnce       sync.Once
}

func (s *retainedEventServer) WatchEvents(request *transportv1.WatchEventsRequest, stream transportv1.TransportService_WatchEventsServer) error {
	start := 0
	if after := request.GetAfterEventId(); len(after) != 0 {
		start = 0
		for index, event := range s.events {
			if bytes.Equal(after, event.GetEventId()) {
				start = index + 1
				break
			}
		}
	}
	for _, event := range s.events[start:] {
		if err := stream.Send(event); err != nil {
			return err
		}
	}
	if bytes.Equal(request.GetAfterEventId(), s.events[len(s.events)-1].GetEventId()) {
		s.finalOnce.Do(func() { close(s.finalCursorSeen) })
		<-stream.Context().Done()
		return stream.Context().Err()
	}
	s.deliveryOnce.Do(func() { close(s.eventsDelivered) })
	select {
	case <-s.releaseStream:
		return nil
	case <-stream.Context().Done():
		return stream.Context().Err()
	}
}

func newRetainedEventClient(t *testing.T, serverImpl *retainedEventServer) *transportclient.Client {
	t.Helper()
	socketRoot, err := os.MkdirTemp(filepath.Join("..", "..", ".."), ".watch-")
	testutil.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(socketRoot) })
	socketRoot, err = filepath.Abs(socketRoot)
	testutil.NoError(t, err)
	socketPath := filepath.Join(socketRoot, "transportd.sock")
	listener, err := net.Listen("unix", socketPath)
	testutil.NoError(t, err)
	if err := os.Chmod(socketPath, 0o660); err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	grpcServer := grpc.NewServer()
	transportv1.RegisterTransportServiceServer(grpcServer, serverImpl)
	healthServer := health.NewServer()
	healthServer.SetServingStatus("ocserv.platform.transport.v1.TransportService", healthv1.HealthCheckResponse_SERVING)
	healthv1.RegisterHealthServer(grpcServer, healthServer)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(func() {
		grpcServer.Stop()
		_ = listener.Close()
	})

	client, err := transportclient.New(socketPath, 2*time.Second, 8, uint32(os.Geteuid()), uint32(os.Getegid()))
	testutil.NoError(t, err)
	return client
}

func prepareWatchNodes(t *testing.T, ctx context.Context, fixture commandResultFixture) (uuid.UUID, [32]byte) {
	t.Helper()
	if _, err := fixture.pool.Exec(ctx, `UPDATE transport_event_cursor SET valid=false,updated_at=now() WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	healthyNodeID := uuid.Must(uuid.NewV7())
	healthyEndpoint := integrationEndpoint(healthyNodeID)
	if _, err := fixture.pool.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES($1,$2,$3,'active',now(),now())`, healthyNodeID, fixture.workspaceID, "healthy-"+healthyNodeID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(ctx, `INSERT INTO node_endpoint_keys(node_id,endpoint_id,state,bound_at) VALUES($1,$2,'active',now())`, healthyNodeID, healthyEndpoint[:]); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup := context.Background()
		_, _ = fixture.pool.Exec(cleanup, `DELETE FROM transport_events WHERE node_id=$1`, healthyNodeID)
		_, _ = fixture.pool.Exec(cleanup, `DELETE FROM node_endpoint_keys WHERE node_id=$1`, healthyNodeID)
		_, _ = fixture.pool.Exec(cleanup, `DELETE FROM nodes WHERE id=$1`, healthyNodeID)
	})
	return healthyNodeID, healthyEndpoint
}

func assertWatchResumes(t *testing.T, ctx context.Context, service *Service, events []*transportv1.TransportEvent, finalID uuid.UUID) {
	t.Helper()
	serverImpl := &retainedEventServer{
		events:          events,
		eventsDelivered: make(chan struct{}),
		releaseStream:   make(chan struct{}),
		finalCursorSeen: make(chan struct{}),
	}
	client := newRetainedEventClient(t, serverImpl)

	firstCtx, firstCancel := context.WithCancel(ctx)
	defer firstCancel()
	firstResult := make(chan error, 1)
	go func() { firstResult <- client.RunWatch(firstCtx, service, service) }()
	select {
	case <-serverImpl.eventsDelivered:
	case <-ctx.Done():
		t.Fatal("transport stream did not deliver event sequence")
	}
	for {
		cursor, err := service.LastEventID(ctx)
		testutil.NoError(t, err)
		if bytes.Equal(cursor, finalID[:]) {
			break
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal("watch did not persist the final event cursor")
		}
	}
	firstCancel()
	if err := <-firstResult; err != context.Canceled {
		t.Fatalf("first RunWatch returned %v after cancellation", err)
	}

	secondCtx, secondCancel := context.WithCancel(ctx)
	defer secondCancel()
	secondResult := make(chan error, 1)
	go func() { secondResult <- client.RunWatch(secondCtx, service, service) }()
	select {
	case <-serverImpl.finalCursorSeen:
	case <-ctx.Done():
		t.Fatal("restarted watch did not resume after the final durable cursor")
	}
	secondCancel()
	if err := <-secondResult; err != context.Canceled {
		t.Fatalf("second RunWatch returned %v after cancellation", err)
	}
}

func TestRunWatchQuarantinesPermanentEventAndContinuesIntegration(t *testing.T) {
	fixture := newCommandResultFixture(t)
	testCtx, testCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer testCancel()

	healthyNodeID, healthyEndpoint := prepareWatchNodes(t, testCtx, fixture)

	invalidBatchID := uuid.Must(uuid.NewV7())
	invalidPayload, err := proto.Marshal(&agentv1.TelemetryBatch{
		BatchId: invalidBatchID[:], NodeId: fixture.nodeID[:], Sequence: 1,
		Priority: agentv1.TelemetryPriority_TELEMETRY_PRIORITY_CURRENT_HEALTH,
		// A missing snapshot is structurally valid protobuf but permanently invalid telemetry.
	})
	testutil.NoError(t, err)
	validResultPayload, err := proto.Marshal(fixture.validResult())
	testutil.NoError(t, err)
	invalidID, heartbeatID, resultID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	now := time.Now().UTC()
	events := []*transportv1.TransportEvent{
		{EventId: invalidID[:], NodeId: fixture.nodeID[:], EndpointId: fixture.endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_TELEMETRY, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: invalidPayload},
		{EventId: heartbeatID[:], NodeId: healthyNodeID[:], EndpointId: healthyEndpoint[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_HEARTBEAT, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: []byte("healthy")},
		{EventId: resultID[:], NodeId: fixture.nodeID[:], EndpointId: fixture.endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: validResultPayload},
	}
	assertWatchResumes(t, testCtx, fixture.service, events, resultID)

	expectedPayloadHash := sha256.Sum256(invalidPayload)
	var quarantineNode uuid.UUID
	var quarantineType int32
	var quarantineHash []byte
	var reasonCode, reasonDetail string
	if err := fixture.pool.QueryRow(testCtx, `SELECT node_id,event_type,payload_sha256,reason_code,reason_detail FROM transport_event_quarantine WHERE event_id=$1`, invalidID).Scan(&quarantineNode, &quarantineType, &quarantineHash, &reasonCode, &reasonDetail); err != nil {
		t.Fatal(err)
	}
	if quarantineNode != fixture.nodeID || quarantineType != int32(transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_TELEMETRY) || !bytes.Equal(quarantineHash, expectedPayloadHash[:]) || reasonCode != "invalid_telemetry" || len(reasonDetail) == 0 || len(reasonDetail) > maxQuarantineDetail {
		t.Fatalf("unexpected quarantine evidence node=%s type=%d hash=%x reason=%q detail_bytes=%d", quarantineNode, quarantineType, quarantineHash, reasonCode, len(reasonDetail))
	}
	var invalidBusinessEvents, validBusinessEvents, resultCount, alertCount int
	var operationState, commandState, healthyStatus string
	if err := fixture.pool.QueryRow(testCtx, `SELECT
		(SELECT count(*) FROM transport_events WHERE event_id=$1),
		(SELECT count(*) FROM transport_events WHERE event_id IN($2,$3)),
		(SELECT count(*) FROM agent_command_results WHERE event_id=$3),
		(SELECT count(*) FROM security_alerts WHERE kind='transport_event.permanent_invalid' AND node_id=$4 AND resource_id=$1),
		(SELECT state FROM operations WHERE id=$5),
		(SELECT state FROM commands WHERE id=$6),
		(SELECT status FROM nodes WHERE id=$7)`, invalidID, heartbeatID, resultID, fixture.nodeID, fixture.operationID, fixture.commandID, healthyNodeID).Scan(&invalidBusinessEvents, &validBusinessEvents, &resultCount, &alertCount, &operationState, &commandState, &healthyStatus); err != nil {
		t.Fatal(err)
	}
	if invalidBusinessEvents != 0 || validBusinessEvents != 2 || resultCount != 1 || alertCount != 1 || operationState != "succeeded" || commandState != "succeeded" || healthyStatus != "active" {
		t.Fatalf("post-watch state invalid=%d valid=%d results=%d alerts=%d operation=%s command=%s healthy=%s", invalidBusinessEvents, validBusinessEvents, resultCount, alertCount, operationState, commandState, healthyStatus)
	}
	var quarantineCount int
	if err := fixture.pool.QueryRow(testCtx, `SELECT count(*) FROM transport_event_quarantine WHERE event_id=$1`, invalidID).Scan(&quarantineCount); err != nil || quarantineCount != 1 {
		t.Fatalf("restart duplicated quarantine evidence: count=%d err=%v", quarantineCount, err)
	}
}

func TestRunWatchQuarantinesCommandResultNULAndContinuesIntegration(t *testing.T) {
	fixture := newCommandResultFixture(t)
	testCtx, testCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer testCancel()

	healthyNodeID, healthyEndpoint := prepareWatchNodes(t, testCtx, fixture)

	poisonResult := fixture.validResult()
	poisonResult.State = agentv1.CommandResultState_COMMAND_RESULT_STATE_FAILED
	poisonResult.ErrorCode = "poison\x00code"
	poisonPayload, err := proto.Marshal(poisonResult)
	testutil.NoError(t, err)
	validResultPayload, err := proto.Marshal(fixture.validResult())
	testutil.NoError(t, err)
	poisonID, heartbeatID, resultID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	now := time.Now().UTC()
	events := []*transportv1.TransportEvent{
		{EventId: poisonID[:], NodeId: fixture.nodeID[:], EndpointId: fixture.endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: poisonPayload},
		{EventId: heartbeatID[:], NodeId: healthyNodeID[:], EndpointId: healthyEndpoint[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_HEARTBEAT, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: []byte("healthy after command-result poison")},
		{EventId: resultID[:], NodeId: fixture.nodeID[:], EndpointId: fixture.endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: validResultPayload},
	}
	assertWatchResumes(t, testCtx, fixture.service, events, resultID)

	expectedPayloadHash := sha256.Sum256(poisonPayload)
	var quarantineNode uuid.UUID
	var quarantineType int32
	var quarantineHash []byte
	var reasonCode, reasonDetail string
	if err := fixture.pool.QueryRow(testCtx, `SELECT node_id,event_type,payload_sha256,reason_code,reason_detail FROM transport_event_quarantine WHERE event_id=$1`, poisonID).Scan(&quarantineNode, &quarantineType, &quarantineHash, &reasonCode, &reasonDetail); err != nil {
		t.Fatal(err)
	}
	if quarantineNode != fixture.nodeID || quarantineType != int32(transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT) || !bytes.Equal(quarantineHash, expectedPayloadHash[:]) || reasonCode != "invalid_command_result" || len(reasonDetail) == 0 || len(reasonDetail) > maxQuarantineDetail {
		t.Fatalf("unexpected quarantine evidence node=%s type=%d hash=%x reason=%q detail_bytes=%d", quarantineNode, quarantineType, quarantineHash, reasonCode, len(reasonDetail))
	}
	var poisonBusinessEvents, validBusinessEvents, poisonResultCount, validResultCount, alertCount int
	var operationState, commandState, healthyStatus string
	if err := fixture.pool.QueryRow(testCtx, `SELECT
		(SELECT count(*) FROM transport_events WHERE event_id=$1),
		(SELECT count(*) FROM transport_events WHERE event_id IN($2,$3)),
		(SELECT count(*) FROM agent_command_results WHERE event_id=$1),
		(SELECT count(*) FROM agent_command_results WHERE event_id=$3),
		(SELECT count(*) FROM security_alerts WHERE kind='transport_event.permanent_invalid' AND node_id=$4 AND resource_id=$1),
		(SELECT state FROM operations WHERE id=$5),
		(SELECT state FROM commands WHERE id=$6),
		(SELECT status FROM nodes WHERE id=$7)`, poisonID, heartbeatID, resultID, fixture.nodeID, fixture.operationID, fixture.commandID, healthyNodeID).Scan(&poisonBusinessEvents, &validBusinessEvents, &poisonResultCount, &validResultCount, &alertCount, &operationState, &commandState, &healthyStatus); err != nil {
		t.Fatal(err)
	}
	if poisonBusinessEvents != 0 || validBusinessEvents != 2 || poisonResultCount != 0 || validResultCount != 1 || alertCount != 1 || operationState != "succeeded" || commandState != "succeeded" || healthyStatus != "active" {
		t.Fatalf("post-watch state poison=%d valid=%d poison_results=%d valid_results=%d alerts=%d operation=%s command=%s healthy=%s", poisonBusinessEvents, validBusinessEvents, poisonResultCount, validResultCount, alertCount, operationState, commandState, healthyStatus)
	}
	var quarantineCount int
	if err := fixture.pool.QueryRow(testCtx, `SELECT count(*) FROM transport_event_quarantine WHERE event_id=$1`, poisonID).Scan(&quarantineCount); err != nil || quarantineCount != 1 {
		t.Fatalf("restart duplicated quarantine evidence: count=%d err=%v", quarantineCount, err)
	}
}

func TestRunWatchQuarantinesTelemetryBigintOverflowAndContinuesIntegration(t *testing.T) {
	fixture := newCommandResultFixture(t)
	testCtx, testCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer testCancel()

	healthyNodeID, healthyEndpoint := prepareWatchNodes(t, testCtx, fixture)

	now := time.Now().UTC()
	agentInstanceID := uuid.Must(uuid.NewV7())
	buildTelemetry := func(batchID uuid.UUID) *agentv1.TelemetryBatch {
		return &agentv1.TelemetryBatch{
			BatchId: batchID[:], NodeId: fixture.nodeID[:], Sequence: 1,
			Priority: agentv1.TelemetryPriority_TELEMETRY_PRIORITY_CURRENT_HEALTH,
			Snapshot: &agentv1.ObservedSnapshot{
				ObservedAt: timestamppb.New(now), BootId: "boot", AgentInstanceId: agentInstanceID[:],
				AgentVersion: "test", OcservVersion: "test", OsRelease: "test",
				OcservJson: []byte(`{}`), SystemJson: []byte(`{}`), PathJson: []byte(`{}`),
			},
		}
	}
	dropBatch := buildTelemetry(uuid.Must(uuid.NewV7()))
	dropBatch.Snapshot.Dropped = &agentv1.TelemetryDropCounters{Security: uint64(math.MaxInt64) + 1}
	remaining := uint64(math.MaxUint64)
	banBatch := buildTelemetry(uuid.Must(uuid.NewV7()))
	banBatch.IpBans = []*agentv1.IpBanObservation{{Ip: "192.0.2.9", SecondsRemaining: &remaining}}
	dropPayload, err := proto.Marshal(dropBatch)
	testutil.NoError(t, err)
	banPayload, err := proto.Marshal(banBatch)
	testutil.NoError(t, err)
	resultPayload, err := proto.Marshal(fixture.validResult())
	testutil.NoError(t, err)
	dropID, heartbeatID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	banID, resultID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	events := []*transportv1.TransportEvent{
		{EventId: dropID[:], NodeId: fixture.nodeID[:], EndpointId: fixture.endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_TELEMETRY, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: dropPayload},
		{EventId: heartbeatID[:], NodeId: healthyNodeID[:], EndpointId: healthyEndpoint[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_HEARTBEAT, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: []byte("healthy after drop overflow")},
		{EventId: banID[:], NodeId: fixture.nodeID[:], EndpointId: fixture.endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_TELEMETRY, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: banPayload},
		{EventId: resultID[:], NodeId: fixture.nodeID[:], EndpointId: fixture.endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: resultPayload},
	}
	assertWatchResumes(t, testCtx, fixture.service, events, resultID)

	var quarantineCount, invalidBusinessEvents, validBusinessEvents int
	var snapshotCount, banCount, resultCount, alertCount int
	if err := fixture.pool.QueryRow(testCtx, `SELECT
		(SELECT count(*) FROM transport_event_quarantine WHERE event_id IN($1,$2) AND reason_code='invalid_telemetry'),
		(SELECT count(*) FROM transport_events WHERE event_id IN($1,$2)),
		(SELECT count(*) FROM transport_events WHERE event_id IN($3,$4)),
		(SELECT count(*) FROM node_observed_snapshots WHERE node_id=$5),
		(SELECT count(*) FROM node_ip_bans WHERE node_id=$5),
		(SELECT count(*) FROM agent_command_results WHERE event_id=$4),
		(SELECT count(*) FROM security_alerts WHERE kind='transport_event.permanent_invalid' AND node_id=$5 AND resource_id IN($1,$2))`,
		dropID, banID, heartbeatID, resultID, fixture.nodeID).Scan(&quarantineCount, &invalidBusinessEvents, &validBusinessEvents, &snapshotCount, &banCount, &resultCount, &alertCount); err != nil {
		t.Fatal(err)
	}
	if quarantineCount != 2 || invalidBusinessEvents != 0 || validBusinessEvents != 2 || snapshotCount != 0 || banCount != 0 || resultCount != 1 || alertCount != 2 {
		t.Fatalf("bigint overflow state quarantine=%d invalid=%d valid=%d snapshots=%d bans=%d results=%d alerts=%d", quarantineCount, invalidBusinessEvents, validBusinessEvents, snapshotCount, banCount, resultCount, alertCount)
	}
}

func TestRunWatchQuarantinesDuplicateAndConflictingSessionsIntegration(t *testing.T) {
	fixture := newCommandResultFixture(t)
	testCtx, testCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer testCancel()

	t.Cleanup(func() {
		_, _ = fixture.pool.Exec(context.Background(), `DELETE FROM telemetry_ingest_batches WHERE node_id=$1`, fixture.nodeID)
	})

	healthyNodeID, healthyEndpoint := prepareWatchNodes(t, testCtx, fixture)

	now := time.Now().UTC().Truncate(time.Microsecond)
	connectedAt := now.Add(-time.Hour)
	agentInstanceID := uuid.Must(uuid.NewV7())
	buildTelemetry := func(batchID uuid.UUID, sequence uint64, observedAt time.Time, username string) *agentv1.TelemetryBatch {
		return &agentv1.TelemetryBatch{
			BatchId: batchID[:], NodeId: fixture.nodeID[:], Sequence: sequence,
			Priority: agentv1.TelemetryPriority_TELEMETRY_PRIORITY_CURRENT_HEALTH,
			Snapshot: &agentv1.ObservedSnapshot{
				ObservedAt: timestamppb.New(observedAt), BootId: "boot", AgentInstanceId: agentInstanceID[:],
				AgentVersion: "test", OcservVersion: "test", OsRelease: "test",
				OcservJson: []byte(`{}`), SystemJson: []byte(`{}`), PathJson: []byte(`{}`),
			},
			Sessions: []*agentv1.SessionObservation{{
				SessionId: "session-identity", Username: username, ClientIp: "192.0.2.10",
				ConnectedAt: timestamppb.New(connectedAt), BytesIn: 10, BytesOut: 20,
			}},
		}
	}
	duplicateBatchID, validBatchID, conflictBatchID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	duplicateBatch := buildTelemetry(duplicateBatchID, 1, now, "alice")
	duplicateBatch.Sessions = append(duplicateBatch.Sessions, duplicateBatch.Sessions[0])
	validObservedAt := now.Add(time.Second)
	validBatch := buildTelemetry(validBatchID, 2, validObservedAt, "alice")
	conflictBatch := buildTelemetry(conflictBatchID, 3, now.Add(2*time.Second), "bob")
	duplicatePayload, err := proto.Marshal(duplicateBatch)
	testutil.NoError(t, err)
	validPayload, err := proto.Marshal(validBatch)
	testutil.NoError(t, err)
	conflictPayload, err := proto.Marshal(conflictBatch)
	testutil.NoError(t, err)
	resultPayload, err := proto.Marshal(fixture.validResult())
	testutil.NoError(t, err)
	duplicateID, heartbeatID, validID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	conflictID, resultID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	events := []*transportv1.TransportEvent{
		{EventId: duplicateID[:], NodeId: fixture.nodeID[:], EndpointId: fixture.endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_TELEMETRY, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: duplicatePayload},
		{EventId: heartbeatID[:], NodeId: healthyNodeID[:], EndpointId: healthyEndpoint[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_HEARTBEAT, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: []byte("healthy after duplicate session")},
		{EventId: validID[:], NodeId: fixture.nodeID[:], EndpointId: fixture.endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_TELEMETRY, OccurredAt: timestamppb.New(validObservedAt), Traceparent: fixture.traceparent, Payload: validPayload},
		{EventId: conflictID[:], NodeId: fixture.nodeID[:], EndpointId: fixture.endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_TELEMETRY, OccurredAt: timestamppb.New(now.Add(2 * time.Second)), Traceparent: fixture.traceparent, Payload: conflictPayload},
		{EventId: resultID[:], NodeId: fixture.nodeID[:], EndpointId: fixture.endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: resultPayload},
	}
	assertWatchResumes(t, testCtx, fixture.service, events, resultID)

	var quarantineCount, invalidBusinessEvents, validBusinessEvents int
	var invalidBatches, validBatches, sessionCount, cursorCount, resultCount, alertCount int
	var snapshotObservedAt time.Time
	var sessionUsername, cursorUsername string
	if err := fixture.pool.QueryRow(testCtx, `SELECT
		(SELECT count(*) FROM transport_event_quarantine WHERE event_id IN($1,$2) AND reason_code='invalid_telemetry'),
		(SELECT count(*) FROM transport_events WHERE event_id IN($1,$2)),
		(SELECT count(*) FROM transport_events WHERE event_id IN($3,$4,$5)),
		(SELECT count(*) FROM telemetry_ingest_batches WHERE batch_id IN($6,$7)),
		(SELECT count(*) FROM telemetry_ingest_batches WHERE batch_id=$8),
		(SELECT count(*) FROM node_sessions WHERE node_id=$9),
		(SELECT count(*) FROM user_usage_cursors WHERE node_id=$9),
		(SELECT count(*) FROM agent_command_results WHERE event_id=$5),
		(SELECT count(*) FROM security_alerts WHERE kind='transport_event.permanent_invalid' AND node_id=$9 AND resource_id IN($1,$2)),
		(SELECT observed_at FROM node_observed_snapshots WHERE node_id=$9),
		(SELECT username FROM node_sessions WHERE node_id=$9 AND session_id='session-identity'),
		(SELECT username FROM user_usage_cursors WHERE node_id=$9 AND session_id='session-identity' AND connected_at=$10)`,
		duplicateID, conflictID, heartbeatID, validID, resultID, duplicateBatchID, conflictBatchID, validBatchID, fixture.nodeID, connectedAt).Scan(
		&quarantineCount, &invalidBusinessEvents, &validBusinessEvents, &invalidBatches, &validBatches,
		&sessionCount, &cursorCount, &resultCount, &alertCount, &snapshotObservedAt, &sessionUsername, &cursorUsername); err != nil {
		t.Fatal(err)
	}
	if quarantineCount != 2 || invalidBusinessEvents != 0 || validBusinessEvents != 3 || invalidBatches != 0 || validBatches != 1 || sessionCount != 1 || cursorCount != 1 || resultCount != 1 || alertCount != 2 || !snapshotObservedAt.Equal(validObservedAt) || sessionUsername != "alice" || cursorUsername != "alice" {
		t.Fatalf("session telemetry state quarantine=%d invalid=%d valid=%d invalid_batches=%d valid_batches=%d sessions=%d cursors=%d results=%d alerts=%d snapshot=%s session_user=%q cursor_user=%q",
			quarantineCount, invalidBusinessEvents, validBusinessEvents, invalidBatches, validBatches, sessionCount, cursorCount, resultCount, alertCount, snapshotObservedAt, sessionUsername, cursorUsername)
	}
}

func TestRunWatchQuarantinesPostgresUnsafeTelemetryIntegration(t *testing.T) {
	fixture := newCommandResultFixture(t)
	testCtx, testCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer testCancel()

	t.Cleanup(func() {
		_, _ = fixture.pool.Exec(context.Background(), `DELETE FROM telemetry_ingest_batches WHERE node_id=$1`, fixture.nodeID)
	})

	healthyNodeID, healthyEndpoint := prepareWatchNodes(t, testCtx, fixture)

	now := time.Now().UTC().Truncate(time.Microsecond)
	agentInstanceID := uuid.Must(uuid.NewV7())
	buildTelemetry := func(batchID uuid.UUID, sequence uint64) *agentv1.TelemetryBatch {
		return &agentv1.TelemetryBatch{
			BatchId: batchID[:], NodeId: fixture.nodeID[:], Sequence: sequence,
			Priority: agentv1.TelemetryPriority_TELEMETRY_PRIORITY_CURRENT_HEALTH,
			Snapshot: &agentv1.ObservedSnapshot{
				ObservedAt: timestamppb.New(now), BootId: "boot", AgentInstanceId: agentInstanceID[:],
				AgentVersion: "test", OcservVersion: "test", OsRelease: "test",
				OcservJson: []byte(`{}`), SystemJson: []byte(`{}`), PathJson: []byte(`{}`),
			},
		}
	}
	bootBatchID, documentBatchID, numericBatchID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	bootBatch := buildTelemetry(bootBatchID, 1)
	bootBatch.Snapshot.BootId = "boot\x00poison"
	documentBatch := buildTelemetry(documentBatchID, 2)
	documentBatch.Snapshot.OcservJson = []byte(`{"label":"\u0000"}`)
	numericBatch := buildTelemetry(numericBatchID, 3)
	numericSecurityID := uuid.Must(uuid.NewV7())
	numericBatch.SecurityEvents = []*agentv1.SecurityObservation{{
		EventId: numericSecurityID[:], ObservedAt: timestamppb.New(now), Severity: "warning", EventType: "numeric-range",
		DetailJson: []byte(`{"outside_postgres_numeric":1e131072}`),
	}}
	bootPayload, err := proto.Marshal(bootBatch)
	testutil.NoError(t, err)
	documentPayload, err := proto.Marshal(documentBatch)
	testutil.NoError(t, err)
	numericPayload, err := proto.Marshal(numericBatch)
	testutil.NoError(t, err)
	resultPayload, err := proto.Marshal(fixture.validResult())
	testutil.NoError(t, err)
	bootID, heartbeatID, documentID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	numericID, resultID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	acceptedBatchID, acceptedID, malformedReplayID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	acceptedBatch := buildTelemetry(acceptedBatchID, 4)
	acceptedPayload, err := proto.Marshal(acceptedBatch)
	testutil.NoError(t, err)
	acceptedBatch.SecurityEvents = numericBatch.SecurityEvents
	malformedReplayPayload, err := proto.Marshal(acceptedBatch)
	testutil.NoError(t, err)
	events := []*transportv1.TransportEvent{
		{EventId: bootID[:], NodeId: fixture.nodeID[:], EndpointId: fixture.endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_TELEMETRY, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: bootPayload},
		{EventId: heartbeatID[:], NodeId: healthyNodeID[:], EndpointId: healthyEndpoint[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_HEARTBEAT, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: []byte("healthy after PostgreSQL-unsafe text")},
		{EventId: documentID[:], NodeId: fixture.nodeID[:], EndpointId: fixture.endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_TELEMETRY, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: documentPayload},
		{EventId: numericID[:], NodeId: fixture.nodeID[:], EndpointId: fixture.endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_TELEMETRY, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: numericPayload},
		{EventId: acceptedID[:], NodeId: fixture.nodeID[:], EndpointId: fixture.endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_TELEMETRY, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: acceptedPayload},
		{EventId: malformedReplayID[:], NodeId: fixture.nodeID[:], EndpointId: fixture.endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_TELEMETRY, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: malformedReplayPayload},
		{EventId: resultID[:], NodeId: fixture.nodeID[:], EndpointId: fixture.endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: resultPayload},
	}
	assertWatchResumes(t, testCtx, fixture.service, events, resultID)

	var quarantineCount, invalidBusinessEvents, validBusinessEvents int
	var invalidBatches, snapshotCount, securityCount, resultCount, alertCount int
	if err := fixture.pool.QueryRow(testCtx, `SELECT
		(SELECT count(*) FROM transport_event_quarantine WHERE event_id IN($1,$2,$3,$10) AND reason_code='invalid_telemetry'),
		(SELECT count(*) FROM transport_events WHERE event_id IN($1,$2,$3,$10)),
		(SELECT count(*) FROM transport_events WHERE event_id IN($4,$5,$11)),
		(SELECT count(*) FROM telemetry_ingest_batches WHERE batch_id IN($6,$7,$8)),
		(SELECT count(*) FROM node_observed_snapshots WHERE node_id=$9),
		(SELECT count(*) FROM telemetry_security_events WHERE node_id=$9),
		(SELECT count(*) FROM agent_command_results WHERE event_id=$5),
		(SELECT count(*) FROM security_alerts WHERE kind='transport_event.permanent_invalid' AND node_id=$9 AND resource_id IN($1,$2,$3,$10))`,
		bootID, documentID, numericID, heartbeatID, resultID, bootBatchID, documentBatchID, numericBatchID, fixture.nodeID, malformedReplayID, acceptedID).Scan(
		&quarantineCount, &invalidBusinessEvents, &validBusinessEvents, &invalidBatches,
		&snapshotCount, &securityCount, &resultCount, &alertCount); err != nil {
		t.Fatal(err)
	}
	if quarantineCount != 4 || invalidBusinessEvents != 0 || validBusinessEvents != 3 || invalidBatches != 0 || snapshotCount != 1 || securityCount != 0 || resultCount != 1 || alertCount != 4 {
		t.Fatalf("PostgreSQL-unsafe telemetry state quarantine=%d invalid=%d valid=%d invalid_batches=%d snapshots=%d security=%d results=%d alerts=%d",
			quarantineCount, invalidBusinessEvents, validBusinessEvents, invalidBatches, snapshotCount, securityCount, resultCount, alertCount)
	}
}
