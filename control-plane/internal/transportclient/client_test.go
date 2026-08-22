package transportclient

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
)

type termAwareGapHandler struct {
	called bool
}

func (*termAwareGapHandler) Ingest(context.Context, *transportv1.TransportEvent) error {
	return nil
}

func (h *termAwareGapHandler) ReconcileOwnerEventGap(_ context.Context, reader NodeConnectionReader) error {
	h.called = reader != nil
	return nil
}

type presenceGapHandler struct {
	called bool
}

func (*presenceGapHandler) Ingest(context.Context, *transportv1.TransportEvent) error {
	return nil
}

func (h *presenceGapHandler) ReconcileEventGap(_ context.Context, reader func(context.Context, []byte) (bool, error)) error {
	h.called = reader != nil
	return nil
}

func TestNewRejectsTCPAndUnboundedConfiguration(t *testing.T) {
	tests := []struct {
		path     string
		deadline time.Duration
		queue    int
	}{
		{path: "127.0.0.1:9000", deadline: time.Second, queue: 1},
		{path: "/tmp/transport.sock", deadline: 0, queue: 1},
		{path: "/tmp/transport.sock", deadline: time.Second, queue: 0},
		{path: "/tmp/transport.sock", deadline: time.Second, queue: 4097},
	}
	for _, test := range tests {
		if _, err := New(test.path, test.deadline, test.queue, uint32(os.Geteuid()), uint32(os.Getegid())); err == nil {
			t.Fatalf("New(%q, %s, %d) succeeded", test.path, test.deadline, test.queue)
		}
	}
}

func TestClientRejectsForeignOwnedUnixServer(t *testing.T) {
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	directory, err := os.MkdirTemp(filepath.Join(workingDirectory, "..", ".."), ".tc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	path := filepath.Join(directory, "transport.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(path, 0o660); err != nil {
		t.Fatal(err)
	}
	client, err := New(path, 100*time.Millisecond, 1, uint32(os.Geteuid()+1), uint32(os.Getegid()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.NodeConnected(context.Background(), make([]byte, 16)); err == nil {
		t.Fatal("foreign-owned transport socket was accepted")
	}
}

func TestFullJitterStaysWithinBound(t *testing.T) {
	for attempt := uint(0); attempt < 32; attempt++ {
		delay := fullJitter(attempt, time.Millisecond, 10*time.Millisecond)
		if delay < 0 || delay > 10*time.Millisecond {
			t.Fatalf("attempt %d produced %s", attempt, delay)
		}
	}
}

func TestClientSelectsTermAwareAndPresenceGapReconcilers(t *testing.T) {
	client := &Client{}
	termAware := &termAwareGapHandler{}
	if err := client.reconcileEventGap(context.Background(), termAware); err != nil {
		t.Fatalf("term-aware reconciliation: %v", err)
	}
	if !termAware.called {
		t.Fatal("term-aware reconciliation did not receive the connection metadata reader")
	}
	presence := &presenceGapHandler{}
	if err := client.reconcileEventGap(context.Background(), presence); err != nil {
		t.Fatalf("presence reconciliation: %v", err)
	}
	if !presence.called {
		t.Fatal("legacy presence reconciliation did not receive the bool reader")
	}
}
