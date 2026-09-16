package transportclient

import (
	"context"
	"errors"
	"testing"
	"time"

	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
)

type shutdownWatchServer struct {
	transportv1.UnimplementedTransportServiceServer
}

func (shutdownWatchServer) WatchEvents(_ *transportv1.WatchEventsRequest, stream transportv1.TransportService_WatchEventsServer) error {
	if err := stream.Send(&transportv1.TransportEvent{}); err != nil {
		return err
	}
	<-stream.Context().Done()
	return stream.Context().Err()
}

type shutdownIngestHandler struct{ started, release, finished chan struct{} }

func (h *shutdownIngestHandler) Ingest(ctx context.Context, _ *transportv1.TransportEvent) error {
	close(h.started)
	<-ctx.Done()
	<-h.release
	close(h.finished)
	return ctx.Err()
}

func TestWatchShutdownJoinsIngest(t *testing.T) {
	client := newGapWatchClient(t, shutdownWatchServer{}, 8)
	handler := &shutdownIngestHandler{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.RunWatch(ctx, fixedEventCursor(nil), handler) }()
	select {
	case <-handler.started:
	case <-time.After(time.Second):
		t.Fatal("ingest never started")
	}
	cancel()
	select {
	case err := <-done:
		t.Fatalf("watch returned before database ingestion: %v", err)
	default:
	}
	close(handler.release)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("watch shutdown blocked")
	}
	select {
	case <-handler.finished:
	default:
		t.Fatal("ingestion still using database after watch exit")
	}
}
