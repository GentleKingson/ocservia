package localslice

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/transportclient"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type retainedEventServer struct {
	transportv1.UnimplementedTransportServiceServer
	events          []*transportv1.TransportEvent
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
	return nil
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
	serverImpl := &retainedEventServer{events: events, finalCursorSeen: make(chan struct{})}
	socketRoot, err := os.MkdirTemp(".", ".watch-integration-")
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
	service := NewWithSigner(pool, integrationCommandSigner())
	watchCtx, watchCancel := context.WithCancel(testCtx)
	watchResult := make(chan error, 1)
	go func() { watchResult <- client.RunWatch(watchCtx, service, service) }()
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
		if err == nil {
			t.Fatalf("revoked node event type %s was accepted", eventType)
		}
	}
	if err := pool.QueryRow(testCtx, `SELECT count(*) FROM transport_events WHERE node_id IN($1,$2)`, revokedNodeID, activeNodeID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 2 {
		t.Fatalf("rejected revoked events changed ingested event count to %d", eventCount)
	}
}
