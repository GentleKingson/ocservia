package localslice

import (
	"bytes"
	"context"
	"crypto/sha256"
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
