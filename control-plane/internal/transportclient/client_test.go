package transportclient

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
