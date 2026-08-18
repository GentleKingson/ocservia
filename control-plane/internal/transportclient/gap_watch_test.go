package transportclient

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type fixedEventCursor []byte

func (c fixedEventCursor) LastEventID(context.Context) ([]byte, error) {
	return bytes.Clone(c), nil
}

type recordingEventCursor struct {
	mu    sync.Mutex
	value []byte
}

func (c *recordingEventCursor) LastEventID(context.Context) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.Clone(c.value), nil
}

func (c *recordingEventCursor) set(value []byte) {
	c.mu.Lock()
	c.value = bytes.Clone(value)
	c.mu.Unlock()
}

type protectedGapServer struct {
	transportv1.UnimplementedTransportServiceServer
	events        chan *transportv1.TransportEvent
	subscribed    chan struct{}
	allSent       chan struct{}
	total         int
	watchCalls    atomic.Int64
	subscribeOnce sync.Once
	sentOnce      sync.Once
}

func (s *protectedGapServer) WatchEvents(request *transportv1.WatchEventsRequest, stream transportv1.TransportService_WatchEventsServer) error {
	s.watchCalls.Add(1)
	if len(request.GetAfterEventId()) != 0 {
		return status.Error(codes.OutOfRange, "cursor is outside retention")
	}
	if err := stream.SendHeader(metadata.Pairs("x-transport-subscription", "ready")); err != nil {
		return err
	}
	s.subscribeOnce.Do(func() { close(s.subscribed) })
	for sent := 0; sent < s.total; sent++ {
		select {
		case event := <-s.events:
			if err := stream.Send(event); err != nil {
				return err
			}
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	}
	s.sentOnce.Do(func() { close(s.allSent) })
	<-stream.Context().Done()
	return stream.Context().Err()
}

type bufferedGapHandler struct {
	server     *protectedGapServer
	events     []*transportv1.TransportEvent
	cursor     *recordingEventCursor
	cancel     context.CancelFunc
	reconciled atomic.Bool
	mu         sync.Mutex
	ingested   [][]byte
}

func (h *bufferedGapHandler) ReconcileOwnerEventGap(ctx context.Context, reader NodeConnectionReader) error {
	if reader == nil {
		return errors.New("connection reader is missing")
	}
	select {
	case <-h.server.subscribed:
	case <-ctx.Done():
		return ctx.Err()
	}
	for _, event := range h.events {
		select {
		case h.server.events <- event:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	select {
	case <-h.server.allSent:
	case <-ctx.Done():
		return ctx.Err()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.ingested) != 0 {
		return errors.New("event ingestion raced inventory reconciliation")
	}
	h.cursor.set(nil)
	h.reconciled.Store(true)
	return nil
}

func (h *bufferedGapHandler) Ingest(_ context.Context, event *transportv1.TransportEvent) error {
	if !h.reconciled.Load() {
		return errors.New("event was ingested before inventory reconciliation")
	}
	h.mu.Lock()
	h.ingested = append(h.ingested, bytes.Clone(event.GetEventId()))
	complete := len(h.ingested) == len(h.events)
	h.mu.Unlock()
	h.cursor.set(event.GetEventId())
	if complete {
		h.cancel()
	}
	return nil
}

func (h *bufferedGapHandler) captured() [][]byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	result := make([][]byte, len(h.ingested))
	for index := range h.ingested {
		result[index] = bytes.Clone(h.ingested[index])
	}
	return result
}

func newGapWatchClient(t *testing.T, server transportv1.TransportServiceServer, queueCapacity int) *Client {
	t.Helper()
	directory, err := os.MkdirTemp(filepath.Join("..", ".."), ".gap-watch-")
	if err != nil {
		t.Fatal(err)
	}
	directory, err = filepath.Abs(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socketPath := filepath.Join(directory, "transportd.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socketPath, 0o660); err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	grpcServer := grpc.NewServer()
	transportv1.RegisterTransportServiceServer(grpcServer, server)
	healthServer := health.NewServer()
	healthServer.SetServingStatus("ocserv.platform.transport.v1.TransportService", healthv1.HealthCheckResponse_SERVING)
	healthv1.RegisterHealthServer(grpcServer, healthServer)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(func() {
		grpcServer.Stop()
		_ = listener.Close()
	})
	client, err := New(socketPath, 3*time.Second, queueCapacity, uint32(os.Geteuid()), uint32(os.Getegid()))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestRunWatchBuffersBeyondRetentionWhileReconcilingGap(t *testing.T) {
	const eventCount = 500
	events := make([]*transportv1.TransportEvent, eventCount)
	for index := range events {
		eventID := make([]byte, 16)
		binary.BigEndian.PutUint32(eventID[12:], uint32(index+1))
		eventType := transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_HEARTBEAT
		if index == 0 {
			eventType = transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_DISCONNECTED
		}
		events[index] = &transportv1.TransportEvent{EventId: eventID, NodeId: make([]byte, 16), Type: eventType}
	}
	server := &protectedGapServer{
		events:     make(chan *transportv1.TransportEvent, eventCount),
		subscribed: make(chan struct{}),
		allSent:    make(chan struct{}),
		total:      eventCount,
	}
	client := newGapWatchClient(t, server, 8)
	watchCtx, stopWatch := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopWatch()
	staleCursor := make([]byte, 16)
	staleCursor[0] = 1
	cursor := &recordingEventCursor{value: staleCursor}
	handler := &bufferedGapHandler{server: server, events: events, cursor: cursor, cancel: stopWatch}
	err := client.RunWatch(watchCtx, cursor, handler)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("protected gap watch = %v, want context canceled", err)
	}
	captured := handler.captured()
	if len(captured) != eventCount {
		t.Fatalf("ingested events = %d, want %d", len(captured), eventCount)
	}
	for index := range events {
		if !bytes.Equal(captured[index], events[index].GetEventId()) {
			t.Fatalf("ingested event %d = %x, want %x", index, captured[index], events[index].GetEventId())
		}
	}
	last, err := cursor.LastEventID(context.Background())
	if err != nil {
		t.Fatalf("read final cursor: %v", err)
	}
	if !bytes.Equal(last, events[len(events)-1].GetEventId()) {
		t.Fatalf("final cursor = %x, want %x", last, events[len(events)-1].GetEventId())
	}
	if calls := server.watchCalls.Load(); calls != 2 {
		t.Fatalf("watch calls = %d, want one stale attempt and one protected stream", calls)
	}
}

type overflowingGapServer struct {
	transportv1.UnimplementedTransportServiceServer
	watchCalls atomic.Int64
	sent       atomic.Int64
}

func (s *overflowingGapServer) WatchEvents(request *transportv1.WatchEventsRequest, stream transportv1.TransportService_WatchEventsServer) error {
	call := s.watchCalls.Add(1)
	if len(request.GetAfterEventId()) != 0 {
		return status.Error(codes.OutOfRange, "cursor is outside retention")
	}
	if err := stream.SendHeader(metadata.Pairs("x-transport-subscription", "ready")); err != nil {
		return err
	}
	if call == 2 {
		event := &transportv1.TransportEvent{EventId: make([]byte, 16), NodeId: make([]byte, 16)}
		for range maxGapBufferedEvents + 1 {
			if err := stream.Send(event); err != nil {
				return err
			}
			s.sent.Add(1)
		}
	}
	<-stream.Context().Done()
	return stream.Context().Err()
}

type retryingGapHandler struct {
	cancel   context.CancelFunc
	attempts atomic.Int64
	ingested atomic.Int64
}

func (h *retryingGapHandler) ReconcileOwnerEventGap(ctx context.Context, _ NodeConnectionReader) error {
	if h.attempts.Add(1) == 1 {
		<-ctx.Done()
		return ctx.Err()
	}
	h.cancel()
	return context.Canceled
}

func (h *retryingGapHandler) Ingest(context.Context, *transportv1.TransportEvent) error {
	h.ingested.Add(1)
	return nil
}

func TestRunWatchRetriesProtectedGapAfterBufferOverflow(t *testing.T) {
	server := &overflowingGapServer{}
	client := newGapWatchClient(t, server, 1)
	watchCtx, stopWatch := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopWatch()
	handler := &retryingGapHandler{cancel: stopWatch}
	staleCursor := make([]byte, 16)
	staleCursor[0] = 1
	err := client.RunWatch(watchCtx, fixedEventCursor(staleCursor), handler)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("overflowing protected gap watch = %v, want context canceled", err)
	}
	if attempts := handler.attempts.Load(); attempts != 2 {
		t.Fatalf("reconciliation attempts = %d, want 2", attempts)
	}
	if ingested := handler.ingested.Load(); ingested != 0 {
		t.Fatalf("events ingested from failed reconciliation = %d, want 0", ingested)
	}
	if sent := server.sent.Load(); sent < maxGapBufferedEvents {
		t.Fatalf("overflow stream sent %d events, want at least %d", sent, maxGapBufferedEvents)
	}
	if calls := server.watchCalls.Load(); calls != 3 {
		t.Fatalf("watch calls = %d, want stale, overflow, and protected retry", calls)
	}
}
