//go:build faultinject

package transportevents

import (
	"context"
	"errors"
	"testing"
	"time"

	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
)

func TestResultCommitHookSignalsBeforeRelease(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	service := &Service{}
	event := &transportv1.TransportEvent{Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT}
	at := time.Unix(123, 0).UTC()
	received := make(chan bool, 1)
	release := make(chan struct{})
	injected := errors.New("commit interrupted")
	service.SetResultCommitHook(func(ctx context.Context, result *transportv1.TransportEvent, observedAt time.Time) error {
		received <- result == event && observedAt.Equal(at)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
			return injected
		}
	})
	done := make(chan error, 1)
	go func() { done <- service.beforeResultCommit(ctx, event, at) }()
	select {
	case matches := <-received:
		if !matches {
			t.Fatal("result hook received the wrong event")
		}
	case <-ctx.Done():
		t.Fatal("result hook did not receive the event")
	}
	select {
	case err := <-done:
		t.Fatalf("result hook returned before release: %v", err)
	default:
	}
	close(release)
	if err := <-done; !errors.Is(err, injected) {
		t.Fatalf("result hook returned %v, want %v", err, injected)
	}
}
