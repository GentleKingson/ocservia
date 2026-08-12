package localslice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"math"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/transportclient"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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

type legacyTransportCursor struct {
	pool *pgxpool.Pool
}

func (cursor legacyTransportCursor) LastEventID(ctx context.Context) ([]byte, error) {
	var eventID uuid.UUID
	err := cursor.pool.QueryRow(ctx, `SELECT event_id FROM transport_events
		WHERE transport_cursor_valid ORDER BY ingest_sequence DESC LIMIT 1`).Scan(&eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return eventID[:], nil
}

type captureEventHandler struct {
	mu          sync.Mutex
	eventIDs    [][]byte
	cancel      context.CancelFunc
	cancelAfter int
	reject      bool
}

func (handler *captureEventHandler) Ingest(_ context.Context, event *transportv1.TransportEvent) error {
	handler.mu.Lock()
	handler.eventIDs = append(handler.eventIDs, bytes.Clone(event.GetEventId()))
	count := len(handler.eventIDs)
	handler.mu.Unlock()
	if handler.reject {
		if count >= handler.cancelAfter {
			handler.cancel()
		}
		return errors.New("legacy Controller rejected permanent-invalid event")
	}
	if count >= handler.cancelAfter {
		go handler.cancel()
	}
	return nil
}

func (handler *captureEventHandler) captured() [][]byte {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	result := make([][]byte, len(handler.eventIDs))
	for index := range handler.eventIDs {
		result[index] = bytes.Clone(handler.eventIDs[index])
	}
	return result
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
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketRoot) })
	socketRoot, err = filepath.Abs(socketRoot)
	if err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(socketRoot, "transportd.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
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
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestRunWatchAdvancesPastRevokedTerminalDisconnectIntegration(t *testing.T) {
	databaseURL := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL is not set")
	}
	testCtx, testCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer testCancel()
	pool, err := pgxpool.New(testCtx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(testCtx, `UPDATE transport_event_cursor SET valid=false,updated_at=now() WHERE singleton`); err != nil {
		t.Fatal(err)
	}

	workspaceID := uuid.Must(uuid.NewV7())
	revokedNodeID, activeNodeID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	revokedEndpoint, activeEndpoint := integrationEndpoint(revokedNodeID), integrationEndpoint(activeNodeID)
	if _, err := pool.Exec(testCtx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'Revocation cursor test',$2,now(),now())`, workspaceID, "revocation-cursor-"+workspaceID.String()); err != nil {
		t.Fatal(err)
	}
	for _, node := range []struct {
		id       uuid.UUID
		endpoint [32]byte
	}{
		{id: revokedNodeID, endpoint: revokedEndpoint},
		{id: activeNodeID, endpoint: activeEndpoint},
	} {
		if _, err := pool.Exec(testCtx, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES($1,$2,$3,'active',now(),now())`, node.id, workspaceID, "node-"+node.id.String()); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(testCtx, `INSERT INTO node_endpoint_keys(node_id,endpoint_id,state,bound_at) VALUES($1,$2,'active',now())`, node.id, node.endpoint[:]); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		cleanup := context.Background()
		_, _ = pool.Exec(cleanup, `DELETE FROM transport_events WHERE node_id IN($1,$2)`, revokedNodeID, activeNodeID)
		_, _ = pool.Exec(cleanup, `DELETE FROM node_endpoint_keys WHERE node_id IN($1,$2)`, revokedNodeID, activeNodeID)
		_, _ = pool.Exec(cleanup, `DELETE FROM nodes WHERE id IN($1,$2)`, revokedNodeID, activeNodeID)
		_, _ = pool.Exec(cleanup, `DELETE FROM workspaces WHERE id=$1`, workspaceID)
		pool.Close()
	})
	tx, err := pool.Begin(testCtx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(testCtx, `UPDATE nodes SET status='revoked',updated_at=now() WHERE id=$1`, revokedNodeID); err != nil {
		_ = tx.Rollback(testCtx)
		t.Fatal(err)
	}
	if _, err := tx.Exec(testCtx, `UPDATE node_endpoint_keys SET state='revoked',revoked_at=now() WHERE node_id=$1`, revokedNodeID); err != nil {
		_ = tx.Rollback(testCtx)
		t.Fatal(err)
	}
	if err := tx.Commit(testCtx); err != nil {
		t.Fatal(err)
	}

	disconnectID, heartbeatID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	events := []*transportv1.TransportEvent{
		{EventId: disconnectID[:], NodeId: revokedNodeID[:], EndpointId: revokedEndpoint[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_DISCONNECTED, OccurredAt: timestamppb.Now(), Traceparent: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01", Payload: []byte("revoked")},
		{EventId: heartbeatID[:], NodeId: activeNodeID[:], EndpointId: activeEndpoint[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_HEARTBEAT, OccurredAt: timestamppb.Now(), Traceparent: "00-1123456789abcdef0123456789abcdef-1123456789abcdef-01", Payload: []byte("active")},
	}
	serverImpl := &retainedEventServer{
		events:          events,
		eventsDelivered: make(chan struct{}),
		releaseStream:   make(chan struct{}),
		finalCursorSeen: make(chan struct{}),
	}
	client := newRetainedEventClient(t, serverImpl)
	service := NewWithSigner(pool, integrationCommandSigner())
	watchCtx, watchCancel := context.WithCancel(testCtx)
	watchResult := make(chan error, 1)
	go func() { watchResult <- client.RunWatch(watchCtx, service, service) }()
	select {
	case <-serverImpl.eventsDelivered:
	case <-testCtx.Done():
		t.Fatal("transport stream did not deliver the test events")
	}
	for {
		cursor, err := service.LastEventID(testCtx)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(cursor, heartbeatID[:]) {
			break
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-testCtx.Done():
			t.Fatal("watch handler did not commit the revoked disconnect and following heartbeat")
		}
	}
	close(serverImpl.releaseStream)
	select {
	case <-serverImpl.finalCursorSeen:
	case <-testCtx.Done():
		t.Fatal("watch cursor did not advance past the revoked disconnect and following heartbeat")
	}
	watchCancel()
	if err := <-watchResult; err != context.Canceled {
		t.Fatalf("RunWatch returned %v after cancellation", err)
	}

	cursor, err := service.LastEventID(testCtx)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cursor, heartbeatID[:]) {
		t.Fatalf("last cursor = %x, want %x", cursor, heartbeatID[:])
	}
	var eventCount int
	if err := pool.QueryRow(testCtx, `SELECT count(*) FROM transport_events WHERE node_id IN($1,$2)`, revokedNodeID, activeNodeID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 2 {
		t.Fatalf("ingested event count = %d, want 2", eventCount)
	}
	var nodeStatus, endpointState string
	if err := pool.QueryRow(testCtx, `SELECT n.status,k.state FROM nodes n JOIN node_endpoint_keys k ON k.node_id=n.id WHERE n.id=$1`, revokedNodeID).Scan(&nodeStatus, &endpointState); err != nil {
		t.Fatal(err)
	}
	if nodeStatus != "revoked" || endpointState != "revoked" {
		t.Fatalf("revoked tombstone changed to node=%q endpoint=%q", nodeStatus, endpointState)
	}
	for _, eventType := range []transportv1.TransportEventType{
		transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_TELEMETRY,
		transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT,
	} {
		eventID := uuid.Must(uuid.NewV7())
		err := service.Ingest(testCtx, &transportv1.TransportEvent{EventId: eventID[:], NodeId: revokedNodeID[:], EndpointId: revokedEndpoint[:], Type: eventType, OccurredAt: timestamppb.Now(), Traceparent: "00-2123456789abcdef0123456789abcdef-2123456789abcdef-01", Payload: []byte("must remain rejected")})
		if err != nil {
			t.Fatalf("quarantine revoked node event type %s: %v", eventType, err)
		}
		assertEventQuarantined(t, pool, eventID, revokedNodeID, "node_endpoint_not_active")
	}
	if err := pool.QueryRow(testCtx, `SELECT count(*) FROM transport_events WHERE node_id IN($1,$2)`, revokedNodeID, activeNodeID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 2 {
		t.Fatalf("rejected revoked events changed ingested event count to %d", eventCount)
	}
}

func TestRunWatchQuarantinesPermanentEventAndContinuesIntegration(t *testing.T) {
	fixture := newCommandResultFixture(t)
	testCtx, testCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer testCancel()
	if _, err := fixture.pool.Exec(testCtx, `UPDATE transport_event_cursor SET valid=false,updated_at=now() WHERE singleton`); err != nil {
		t.Fatal(err)
	}

	healthyNodeID := uuid.Must(uuid.NewV7())
	healthyEndpoint := integrationEndpoint(healthyNodeID)
	if _, err := fixture.pool.Exec(testCtx, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES($1,$2,$3,'active',now(),now())`, healthyNodeID, fixture.workspaceID, "healthy-"+healthyNodeID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(testCtx, `INSERT INTO node_endpoint_keys(node_id,endpoint_id,state,bound_at) VALUES($1,$2,'active',now())`, healthyNodeID, healthyEndpoint[:]); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup := context.Background()
		_, _ = fixture.pool.Exec(cleanup, `DELETE FROM transport_events WHERE node_id=$1`, healthyNodeID)
		_, _ = fixture.pool.Exec(cleanup, `DELETE FROM node_endpoint_keys WHERE node_id=$1`, healthyNodeID)
		_, _ = fixture.pool.Exec(cleanup, `DELETE FROM nodes WHERE id=$1`, healthyNodeID)
	})

	invalidBatchID := uuid.Must(uuid.NewV7())
	invalidPayload, err := proto.Marshal(&agentv1.TelemetryBatch{
		BatchId: invalidBatchID[:], NodeId: fixture.nodeID[:], Sequence: 1,
		Priority: agentv1.TelemetryPriority_TELEMETRY_PRIORITY_CURRENT_HEALTH,
		// A missing snapshot is structurally valid protobuf but permanently invalid telemetry.
	})
	if err != nil {
		t.Fatal(err)
	}
	validResultPayload, err := proto.Marshal(fixture.validResult())
	if err != nil {
		t.Fatal(err)
	}
	invalidID, heartbeatID, resultID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	now := time.Now().UTC()
	events := []*transportv1.TransportEvent{
		{EventId: invalidID[:], NodeId: fixture.nodeID[:], EndpointId: fixture.endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_TELEMETRY, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: invalidPayload},
		{EventId: heartbeatID[:], NodeId: healthyNodeID[:], EndpointId: healthyEndpoint[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_HEARTBEAT, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: []byte("healthy")},
		{EventId: resultID[:], NodeId: fixture.nodeID[:], EndpointId: fixture.endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: validResultPayload},
	}
	serverImpl := &retainedEventServer{
		events:          events,
		eventsDelivered: make(chan struct{}),
		releaseStream:   make(chan struct{}),
		finalCursorSeen: make(chan struct{}),
	}
	client := newRetainedEventClient(t, serverImpl)

	firstCtx, firstCancel := context.WithCancel(testCtx)
	firstResult := make(chan error, 1)
	go func() { firstResult <- client.RunWatch(firstCtx, fixture.service, fixture.service) }()
	select {
	case <-serverImpl.eventsDelivered:
	case <-testCtx.Done():
		t.Fatal("transport stream did not deliver poison/heartbeat/result sequence")
	}
	for {
		cursor, err := fixture.service.LastEventID(testCtx)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(cursor, resultID[:]) {
			break
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-testCtx.Done():
			t.Fatal("permanent event blocked the following heartbeat or command result")
		}
	}
	firstCancel()
	if err := <-firstResult; err != context.Canceled {
		t.Fatalf("first RunWatch returned %v after cancellation", err)
	}

	secondCtx, secondCancel := context.WithCancel(testCtx)
	secondResult := make(chan error, 1)
	go func() { secondResult <- client.RunWatch(secondCtx, fixture.service, fixture.service) }()
	select {
	case <-serverImpl.finalCursorSeen:
	case <-testCtx.Done():
		t.Fatal("restarted watch did not resume after the final durable cursor")
	}
	secondCancel()
	if err := <-secondResult; err != context.Canceled {
		t.Fatalf("second RunWatch returned %v after cancellation", err)
	}

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
	if _, err := fixture.pool.Exec(testCtx, `UPDATE transport_event_cursor SET valid=false,updated_at=now() WHERE singleton`); err != nil {
		t.Fatal(err)
	}

	healthyNodeID := uuid.Must(uuid.NewV7())
	healthyEndpoint := integrationEndpoint(healthyNodeID)
	if _, err := fixture.pool.Exec(testCtx, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES($1,$2,$3,'active',now(),now())`, healthyNodeID, fixture.workspaceID, "healthy-command-result-"+healthyNodeID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(testCtx, `INSERT INTO node_endpoint_keys(node_id,endpoint_id,state,bound_at) VALUES($1,$2,'active',now())`, healthyNodeID, healthyEndpoint[:]); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup := context.Background()
		_, _ = fixture.pool.Exec(cleanup, `DELETE FROM transport_events WHERE node_id=$1`, healthyNodeID)
		_, _ = fixture.pool.Exec(cleanup, `DELETE FROM node_endpoint_keys WHERE node_id=$1`, healthyNodeID)
		_, _ = fixture.pool.Exec(cleanup, `DELETE FROM nodes WHERE id=$1`, healthyNodeID)
	})

	poisonResult := fixture.validResult()
	poisonResult.State = agentv1.CommandResultState_COMMAND_RESULT_STATE_FAILED
	poisonResult.ErrorCode = "poison\x00code"
	poisonPayload, err := proto.Marshal(poisonResult)
	if err != nil {
		t.Fatal(err)
	}
	validResultPayload, err := proto.Marshal(fixture.validResult())
	if err != nil {
		t.Fatal(err)
	}
	poisonID, heartbeatID, resultID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	now := time.Now().UTC()
	events := []*transportv1.TransportEvent{
		{EventId: poisonID[:], NodeId: fixture.nodeID[:], EndpointId: fixture.endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: poisonPayload},
		{EventId: heartbeatID[:], NodeId: healthyNodeID[:], EndpointId: healthyEndpoint[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_HEARTBEAT, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: []byte("healthy after command-result poison")},
		{EventId: resultID[:], NodeId: fixture.nodeID[:], EndpointId: fixture.endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: validResultPayload},
	}
	serverImpl := &retainedEventServer{
		events:          events,
		eventsDelivered: make(chan struct{}),
		releaseStream:   make(chan struct{}),
		finalCursorSeen: make(chan struct{}),
	}
	client := newRetainedEventClient(t, serverImpl)

	firstCtx, firstCancel := context.WithCancel(testCtx)
	firstResult := make(chan error, 1)
	go func() { firstResult <- client.RunWatch(firstCtx, fixture.service, fixture.service) }()
	select {
	case <-serverImpl.eventsDelivered:
	case <-testCtx.Done():
		t.Fatal("transport stream did not deliver poison/heartbeat/result sequence")
	}
	for {
		cursor, err := fixture.service.LastEventID(testCtx)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(cursor, resultID[:]) {
			break
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-testCtx.Done():
			t.Fatal("invalid command result blocked the following heartbeat or command result")
		}
	}
	firstCancel()
	if err := <-firstResult; err != context.Canceled {
		t.Fatalf("first RunWatch returned %v after cancellation", err)
	}

	secondCtx, secondCancel := context.WithCancel(testCtx)
	secondResult := make(chan error, 1)
	go func() { secondResult <- client.RunWatch(secondCtx, fixture.service, fixture.service) }()
	select {
	case <-serverImpl.finalCursorSeen:
	case <-testCtx.Done():
		t.Fatal("restarted watch did not resume after the final durable cursor")
	}
	secondCancel()
	if err := <-secondResult; err != context.Canceled {
		t.Fatalf("second RunWatch returned %v after cancellation", err)
	}

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

func TestLegacyCursorRollbackRequiresAcceptedTailIntegration(t *testing.T) {
	fixture := newCommandResultFixture(t)
	testCtx, testCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer testCancel()
	if _, err := fixture.pool.Exec(testCtx, `UPDATE transport_event_cursor SET valid=false,updated_at=now() WHERE singleton;
		UPDATE transport_events SET transport_cursor_valid=false WHERE transport_cursor_valid`); err != nil {
		t.Fatal(err)
	}

	invalidBatchID := uuid.Must(uuid.NewV7())
	invalidPayload, err := proto.Marshal(&agentv1.TelemetryBatch{
		BatchId: invalidBatchID[:], NodeId: fixture.nodeID[:], Sequence: 1,
		Priority: agentv1.TelemetryPriority_TELEMETRY_PRIORITY_CURRENT_HEALTH,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	acceptedBeforeID, quarantinedID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	acceptedAfterID, followingID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	acceptedBefore := &transportv1.TransportEvent{EventId: acceptedBeforeID[:], NodeId: fixture.nodeID[:], EndpointId: fixture.endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_HEARTBEAT, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: []byte("accepted before quarantine")}
	quarantined := &transportv1.TransportEvent{EventId: quarantinedID[:], NodeId: fixture.nodeID[:], EndpointId: fixture.endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_TELEMETRY, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: invalidPayload}
	acceptedAfter := &transportv1.TransportEvent{EventId: acceptedAfterID[:], NodeId: fixture.nodeID[:], EndpointId: fixture.endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_HEARTBEAT, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: []byte("accepted after quarantine")}
	following := &transportv1.TransportEvent{EventId: followingID[:], NodeId: fixture.nodeID[:], EndpointId: fixture.endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_HEARTBEAT, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: []byte("following legacy reconnect")}

	if err := fixture.service.Ingest(testCtx, acceptedBefore); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.Ingest(testCtx, quarantined); err != nil {
		t.Fatal(err)
	}
	assertEventQuarantined(t, fixture.pool, quarantinedID, fixture.nodeID, "invalid_telemetry")

	legacyCursor := legacyTransportCursor{pool: fixture.pool}
	legacyID, err := legacyCursor.LastEventID(testCtx)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(legacyID, acceptedBeforeID[:]) {
		t.Fatalf("legacy cursor=%x, want accepted event %x", legacyID, acceptedBeforeID[:])
	}

	poisonServer := &retainedEventServer{
		events:          []*transportv1.TransportEvent{acceptedBefore, quarantined},
		eventsDelivered: make(chan struct{}), releaseStream: make(chan struct{}), finalCursorSeen: make(chan struct{}),
	}
	poisonClient := newRetainedEventClient(t, poisonServer)
	poisonCtx, poisonCancel := context.WithCancel(testCtx)
	poisonHandler := &captureEventHandler{cancel: poisonCancel, cancelAfter: 2, reject: true}
	if err := poisonClient.RunWatch(poisonCtx, legacyCursor, poisonHandler); !errors.Is(err, context.Canceled) {
		t.Fatalf("legacy poison watch returned %v", err)
	}
	poisonedIDs := poisonHandler.captured()
	if len(poisonedIDs) != 2 || !bytes.Equal(poisonedIDs[0], quarantinedID[:]) || !bytes.Equal(poisonedIDs[1], quarantinedID[:]) {
		t.Fatalf("legacy cursor did not repeatedly receive quarantine tail: %x", poisonedIDs)
	}

	if err := fixture.service.Ingest(testCtx, acceptedAfter); err != nil {
		t.Fatal(err)
	}
	durableID, err := fixture.service.LastEventID(testCtx)
	if err != nil {
		t.Fatal(err)
	}
	legacyID, err = legacyCursor.LastEventID(testCtx)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(durableID, acceptedAfterID[:]) || !bytes.Equal(legacyID, acceptedAfterID[:]) {
		t.Fatalf("rollback cursor compatibility durable=%x legacy=%x want=%x", durableID, legacyID, acceptedAfterID[:])
	}

	safeServer := &retainedEventServer{
		events:          []*transportv1.TransportEvent{acceptedBefore, quarantined, acceptedAfter, following},
		eventsDelivered: make(chan struct{}), releaseStream: make(chan struct{}), finalCursorSeen: make(chan struct{}),
	}
	safeClient := newRetainedEventClient(t, safeServer)
	safeCtx, safeCancel := context.WithCancel(testCtx)
	safeHandler := &captureEventHandler{cancel: safeCancel, cancelAfter: 1}
	if err := safeClient.RunWatch(safeCtx, legacyCursor, safeHandler); !errors.Is(err, context.Canceled) {
		t.Fatalf("legacy safe watch returned %v", err)
	}
	safeIDs := safeHandler.captured()
	if len(safeIDs) != 1 || !bytes.Equal(safeIDs[0], followingID[:]) {
		t.Fatalf("legacy reconnect replayed the quarantine tail: %x", safeIDs)
	}
}

func TestRunWatchQuarantinesTelemetryBigintOverflowAndContinuesIntegration(t *testing.T) {
	fixture := newCommandResultFixture(t)
	testCtx, testCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer testCancel()
	if _, err := fixture.pool.Exec(testCtx, `UPDATE transport_event_cursor SET valid=false,updated_at=now() WHERE singleton`); err != nil {
		t.Fatal(err)
	}

	healthyNodeID := uuid.Must(uuid.NewV7())
	healthyEndpoint := integrationEndpoint(healthyNodeID)
	if _, err := fixture.pool.Exec(testCtx, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES($1,$2,$3,'active',now(),now())`, healthyNodeID, fixture.workspaceID, "bigint-healthy-"+healthyNodeID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(testCtx, `INSERT INTO node_endpoint_keys(node_id,endpoint_id,state,bound_at) VALUES($1,$2,'active',now())`, healthyNodeID, healthyEndpoint[:]); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup := context.Background()
		_, _ = fixture.pool.Exec(cleanup, `DELETE FROM transport_events WHERE node_id=$1`, healthyNodeID)
		_, _ = fixture.pool.Exec(cleanup, `DELETE FROM node_endpoint_keys WHERE node_id=$1`, healthyNodeID)
		_, _ = fixture.pool.Exec(cleanup, `DELETE FROM nodes WHERE id=$1`, healthyNodeID)
	})

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
	if err != nil {
		t.Fatal(err)
	}
	banPayload, err := proto.Marshal(banBatch)
	if err != nil {
		t.Fatal(err)
	}
	resultPayload, err := proto.Marshal(fixture.validResult())
	if err != nil {
		t.Fatal(err)
	}
	dropID, heartbeatID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	banID, resultID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	events := []*transportv1.TransportEvent{
		{EventId: dropID[:], NodeId: fixture.nodeID[:], EndpointId: fixture.endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_TELEMETRY, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: dropPayload},
		{EventId: heartbeatID[:], NodeId: healthyNodeID[:], EndpointId: healthyEndpoint[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_HEARTBEAT, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: []byte("healthy after drop overflow")},
		{EventId: banID[:], NodeId: fixture.nodeID[:], EndpointId: fixture.endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_TELEMETRY, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: banPayload},
		{EventId: resultID[:], NodeId: fixture.nodeID[:], EndpointId: fixture.endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: resultPayload},
	}
	serverImpl := &retainedEventServer{
		events:          events,
		eventsDelivered: make(chan struct{}),
		releaseStream:   make(chan struct{}),
		finalCursorSeen: make(chan struct{}),
	}
	client := newRetainedEventClient(t, serverImpl)

	firstCtx, firstCancel := context.WithCancel(testCtx)
	firstResult := make(chan error, 1)
	go func() { firstResult <- client.RunWatch(firstCtx, fixture.service, fixture.service) }()
	select {
	case <-serverImpl.eventsDelivered:
	case <-testCtx.Done():
		t.Fatal("transport stream did not deliver bigint overflow sequence")
	}
	for {
		cursor, err := fixture.service.LastEventID(testCtx)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(cursor, resultID[:]) {
			break
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-testCtx.Done():
			t.Fatal("bigint overflow blocked the following heartbeat or command result")
		}
	}
	firstCancel()
	if err := <-firstResult; err != context.Canceled {
		t.Fatalf("first RunWatch returned %v after cancellation", err)
	}

	secondCtx, secondCancel := context.WithCancel(testCtx)
	secondResult := make(chan error, 1)
	go func() { secondResult <- client.RunWatch(secondCtx, fixture.service, fixture.service) }()
	select {
	case <-serverImpl.finalCursorSeen:
	case <-testCtx.Done():
		t.Fatal("restarted watch did not resume after bigint overflow quarantine")
	}
	secondCancel()
	if err := <-secondResult; err != context.Canceled {
		t.Fatalf("second RunWatch returned %v after cancellation", err)
	}

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
	if _, err := fixture.pool.Exec(testCtx, `UPDATE transport_event_cursor SET valid=false,updated_at=now() WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = fixture.pool.Exec(context.Background(), `DELETE FROM telemetry_ingest_batches WHERE node_id=$1`, fixture.nodeID)
	})

	healthyNodeID := uuid.Must(uuid.NewV7())
	healthyEndpoint := integrationEndpoint(healthyNodeID)
	if _, err := fixture.pool.Exec(testCtx, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES($1,$2,$3,'active',now(),now())`, healthyNodeID, fixture.workspaceID, "duplicate-healthy-"+healthyNodeID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(testCtx, `INSERT INTO node_endpoint_keys(node_id,endpoint_id,state,bound_at) VALUES($1,$2,'active',now())`, healthyNodeID, healthyEndpoint[:]); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup := context.Background()
		_, _ = fixture.pool.Exec(cleanup, `DELETE FROM transport_events WHERE node_id=$1`, healthyNodeID)
		_, _ = fixture.pool.Exec(cleanup, `DELETE FROM node_endpoint_keys WHERE node_id=$1`, healthyNodeID)
		_, _ = fixture.pool.Exec(cleanup, `DELETE FROM nodes WHERE id=$1`, healthyNodeID)
	})

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
	if err != nil {
		t.Fatal(err)
	}
	validPayload, err := proto.Marshal(validBatch)
	if err != nil {
		t.Fatal(err)
	}
	conflictPayload, err := proto.Marshal(conflictBatch)
	if err != nil {
		t.Fatal(err)
	}
	resultPayload, err := proto.Marshal(fixture.validResult())
	if err != nil {
		t.Fatal(err)
	}
	duplicateID, heartbeatID, validID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	conflictID, resultID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	events := []*transportv1.TransportEvent{
		{EventId: duplicateID[:], NodeId: fixture.nodeID[:], EndpointId: fixture.endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_TELEMETRY, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: duplicatePayload},
		{EventId: heartbeatID[:], NodeId: healthyNodeID[:], EndpointId: healthyEndpoint[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_HEARTBEAT, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: []byte("healthy after duplicate session")},
		{EventId: validID[:], NodeId: fixture.nodeID[:], EndpointId: fixture.endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_TELEMETRY, OccurredAt: timestamppb.New(validObservedAt), Traceparent: fixture.traceparent, Payload: validPayload},
		{EventId: conflictID[:], NodeId: fixture.nodeID[:], EndpointId: fixture.endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_TELEMETRY, OccurredAt: timestamppb.New(now.Add(2 * time.Second)), Traceparent: fixture.traceparent, Payload: conflictPayload},
		{EventId: resultID[:], NodeId: fixture.nodeID[:], EndpointId: fixture.endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: resultPayload},
	}
	serverImpl := &retainedEventServer{
		events:          events,
		eventsDelivered: make(chan struct{}),
		releaseStream:   make(chan struct{}),
		finalCursorSeen: make(chan struct{}),
	}
	client := newRetainedEventClient(t, serverImpl)

	firstCtx, firstCancel := context.WithCancel(testCtx)
	firstResult := make(chan error, 1)
	go func() { firstResult <- client.RunWatch(firstCtx, fixture.service, fixture.service) }()
	select {
	case <-serverImpl.eventsDelivered:
	case <-testCtx.Done():
		t.Fatal("transport stream did not deliver duplicate/conflict sequence")
	}
	for {
		cursor, err := fixture.service.LastEventID(testCtx)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(cursor, resultID[:]) {
			break
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-testCtx.Done():
			t.Fatal("invalid session telemetry blocked following events")
		}
	}
	firstCancel()
	if err := <-firstResult; err != context.Canceled {
		t.Fatalf("first RunWatch returned %v after cancellation", err)
	}

	secondCtx, secondCancel := context.WithCancel(testCtx)
	secondResult := make(chan error, 1)
	go func() { secondResult <- client.RunWatch(secondCtx, fixture.service, fixture.service) }()
	select {
	case <-serverImpl.finalCursorSeen:
	case <-testCtx.Done():
		t.Fatal("restarted watch did not resume after session telemetry quarantine")
	}
	secondCancel()
	if err := <-secondResult; err != context.Canceled {
		t.Fatalf("second RunWatch returned %v after cancellation", err)
	}

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
	if _, err := fixture.pool.Exec(testCtx, `UPDATE transport_event_cursor SET valid=false,updated_at=now() WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = fixture.pool.Exec(context.Background(), `DELETE FROM telemetry_ingest_batches WHERE node_id=$1`, fixture.nodeID)
	})

	healthyNodeID := uuid.Must(uuid.NewV7())
	healthyEndpoint := integrationEndpoint(healthyNodeID)
	if _, err := fixture.pool.Exec(testCtx, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES($1,$2,$3,'active',now(),now())`, healthyNodeID, fixture.workspaceID, "postgres-safe-"+healthyNodeID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(testCtx, `INSERT INTO node_endpoint_keys(node_id,endpoint_id,state,bound_at) VALUES($1,$2,'active',now())`, healthyNodeID, healthyEndpoint[:]); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup := context.Background()
		_, _ = fixture.pool.Exec(cleanup, `DELETE FROM transport_events WHERE node_id=$1`, healthyNodeID)
		_, _ = fixture.pool.Exec(cleanup, `DELETE FROM node_endpoint_keys WHERE node_id=$1`, healthyNodeID)
		_, _ = fixture.pool.Exec(cleanup, `DELETE FROM nodes WHERE id=$1`, healthyNodeID)
	})

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
	if err != nil {
		t.Fatal(err)
	}
	documentPayload, err := proto.Marshal(documentBatch)
	if err != nil {
		t.Fatal(err)
	}
	numericPayload, err := proto.Marshal(numericBatch)
	if err != nil {
		t.Fatal(err)
	}
	resultPayload, err := proto.Marshal(fixture.validResult())
	if err != nil {
		t.Fatal(err)
	}
	bootID, heartbeatID, documentID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	numericID, resultID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	events := []*transportv1.TransportEvent{
		{EventId: bootID[:], NodeId: fixture.nodeID[:], EndpointId: fixture.endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_TELEMETRY, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: bootPayload},
		{EventId: heartbeatID[:], NodeId: healthyNodeID[:], EndpointId: healthyEndpoint[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_HEARTBEAT, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: []byte("healthy after PostgreSQL-unsafe text")},
		{EventId: documentID[:], NodeId: fixture.nodeID[:], EndpointId: fixture.endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_TELEMETRY, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: documentPayload},
		{EventId: numericID[:], NodeId: fixture.nodeID[:], EndpointId: fixture.endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_TELEMETRY, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: numericPayload},
		{EventId: resultID[:], NodeId: fixture.nodeID[:], EndpointId: fixture.endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: resultPayload},
	}
	serverImpl := &retainedEventServer{
		events:          events,
		eventsDelivered: make(chan struct{}),
		releaseStream:   make(chan struct{}),
		finalCursorSeen: make(chan struct{}),
	}
	client := newRetainedEventClient(t, serverImpl)

	firstCtx, firstCancel := context.WithCancel(testCtx)
	firstResult := make(chan error, 1)
	go func() { firstResult <- client.RunWatch(firstCtx, fixture.service, fixture.service) }()
	select {
	case <-serverImpl.eventsDelivered:
	case <-testCtx.Done():
		t.Fatal("transport stream did not deliver PostgreSQL-unsafe telemetry sequence")
	}
	for {
		cursor, err := fixture.service.LastEventID(testCtx)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(cursor, resultID[:]) {
			break
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-testCtx.Done():
			t.Fatal("PostgreSQL-unsafe telemetry blocked following events")
		}
	}
	firstCancel()
	if err := <-firstResult; err != context.Canceled {
		t.Fatalf("first RunWatch returned %v after cancellation", err)
	}

	secondCtx, secondCancel := context.WithCancel(testCtx)
	secondResult := make(chan error, 1)
	go func() { secondResult <- client.RunWatch(secondCtx, fixture.service, fixture.service) }()
	select {
	case <-serverImpl.finalCursorSeen:
	case <-testCtx.Done():
		t.Fatal("restarted watch did not resume after PostgreSQL-unsafe telemetry quarantine")
	}
	secondCancel()
	if err := <-secondResult; err != context.Canceled {
		t.Fatalf("second RunWatch returned %v after cancellation", err)
	}

	var quarantineCount, invalidBusinessEvents, validBusinessEvents int
	var invalidBatches, snapshotCount, securityCount, resultCount, alertCount int
	if err := fixture.pool.QueryRow(testCtx, `SELECT
		(SELECT count(*) FROM transport_event_quarantine WHERE event_id IN($1,$2,$3) AND reason_code='invalid_telemetry'),
		(SELECT count(*) FROM transport_events WHERE event_id IN($1,$2,$3)),
		(SELECT count(*) FROM transport_events WHERE event_id IN($4,$5)),
		(SELECT count(*) FROM telemetry_ingest_batches WHERE batch_id IN($6,$7,$8)),
		(SELECT count(*) FROM node_observed_snapshots WHERE node_id=$9),
		(SELECT count(*) FROM telemetry_security_events WHERE node_id=$9),
		(SELECT count(*) FROM agent_command_results WHERE event_id=$5),
		(SELECT count(*) FROM security_alerts WHERE kind='transport_event.permanent_invalid' AND node_id=$9 AND resource_id IN($1,$2,$3))`,
		bootID, documentID, numericID, heartbeatID, resultID, bootBatchID, documentBatchID, numericBatchID, fixture.nodeID).Scan(
		&quarantineCount, &invalidBusinessEvents, &validBusinessEvents, &invalidBatches,
		&snapshotCount, &securityCount, &resultCount, &alertCount); err != nil {
		t.Fatal(err)
	}
	if quarantineCount != 3 || invalidBusinessEvents != 0 || validBusinessEvents != 2 || invalidBatches != 0 || snapshotCount != 0 || securityCount != 0 || resultCount != 1 || alertCount != 3 {
		t.Fatalf("PostgreSQL-unsafe telemetry state quarantine=%d invalid=%d valid=%d invalid_batches=%d snapshots=%d security=%d results=%d alerts=%d",
			quarantineCount, invalidBusinessEvents, validBusinessEvents, invalidBatches, snapshotCount, securityCount, resultCount, alertCount)
	}
}

func TestTransientDatabaseFailureDoesNotAdvanceTransportCursorIntegration(t *testing.T) {
	fixture := newCommandResultFixture(t)
	testCtx, testCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer testCancel()
	if _, err := fixture.pool.Exec(testCtx, `UPDATE transport_event_cursor SET valid=false,updated_at=now() WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	baselineID, retryID, resultID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	baseline := &transportv1.TransportEvent{EventId: baselineID[:], NodeId: fixture.nodeID[:], EndpointId: fixture.endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_HEARTBEAT, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: []byte("baseline")}
	retry := &transportv1.TransportEvent{EventId: retryID[:], NodeId: fixture.nodeID[:], EndpointId: fixture.endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_HEARTBEAT, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: []byte("retry")}
	resultPayload, err := proto.Marshal(fixture.validResult())
	if err != nil {
		t.Fatal(err)
	}
	result := &transportv1.TransportEvent{EventId: resultID[:], NodeId: fixture.nodeID[:], EndpointId: fixture.endpointID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT, OccurredAt: timestamppb.New(now), Traceparent: fixture.traceparent, Payload: resultPayload}
	if err := fixture.service.Ingest(testCtx, baseline); err != nil {
		t.Fatal(err)
	}

	lockTx, err := fixture.pool.Begin(testCtx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockTx.Exec(testCtx, `SELECT event_id FROM transport_event_cursor WHERE singleton FOR UPDATE`); err != nil {
		_ = lockTx.Rollback(testCtx)
		t.Fatal(err)
	}
	retryCtx, retryCancel := context.WithTimeout(testCtx, 150*time.Millisecond)
	err = fixture.service.Ingest(retryCtx, retry)
	retryCancel()
	if err == nil {
		_ = lockTx.Rollback(testCtx)
		t.Fatal("cursor lock did not surface as a transient ingest failure")
	}
	if rollbackErr := lockTx.Rollback(testCtx); rollbackErr != nil {
		t.Fatal(rollbackErr)
	}

	cursor, err := fixture.service.LastEventID(testCtx)
	if err != nil {
		t.Fatal(err)
	}
	var retryBusinessEvents, retryQuarantineEvents int
	if err := fixture.pool.QueryRow(testCtx, `SELECT
		(SELECT count(*) FROM transport_events WHERE event_id=$1),
		(SELECT count(*) FROM transport_event_quarantine WHERE event_id=$1)`, retryID).Scan(&retryBusinessEvents, &retryQuarantineEvents); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cursor, baselineID[:]) || retryBusinessEvents != 0 || retryQuarantineEvents != 0 {
		t.Fatalf("transient failure advanced/skipped event: cursor=%x business=%d quarantine=%d", cursor, retryBusinessEvents, retryQuarantineEvents)
	}

	if err := fixture.service.Ingest(testCtx, retry); err != nil {
		t.Fatalf("retry valid event after transient failure: %v", err)
	}
	if err := fixture.service.Ingest(testCtx, result); err != nil {
		t.Fatalf("ingest following command result: %v", err)
	}
	cursor, err = fixture.service.LastEventID(testCtx)
	if err != nil {
		t.Fatal(err)
	}
	var acceptedCount int
	if err := fixture.pool.QueryRow(testCtx, `SELECT count(*) FROM transport_events WHERE event_id IN($1,$2,$3)`, baselineID, retryID, resultID).Scan(&acceptedCount); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cursor, resultID[:]) || acceptedCount != 3 {
		t.Fatalf("retry sequence cursor=%x accepted=%d", cursor, acceptedCount)
	}
}
