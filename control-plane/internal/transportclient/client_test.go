package transportclient

import (
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
		if _, err := New(test.path, test.deadline, test.queue); err == nil {
			t.Fatalf("New(%q, %s, %d) succeeded", test.path, test.deadline, test.queue)
		}
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
