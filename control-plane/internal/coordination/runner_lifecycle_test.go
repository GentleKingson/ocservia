package coordination

import (
	"context"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
)

type blockedRenewalBackend struct {
	database.Backend
	started  chan struct{}
	release  chan struct{}
	finished chan struct{}
}

func (b *blockedRenewalBackend) Begin(ctx context.Context, _ database.Isolation) (database.Tx, error) {
	close(b.started)
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	close(b.finished)
	return nil, context.Canceled
}

func TestRunnerStopWaitsForRenewal(t *testing.T) {
	backend := &blockedRenewalBackend{started: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
	runner := NewRunnerBackend(backend, Identity{}, time.Second, time.Millisecond, nil)
	if _, ok := runner.installSession(context.Background(), &Session{}, time.Now().Add(time.Second)); !ok {
		t.Fatal("install")
	}
	select {
	case <-backend.started:
	case <-time.After(time.Second):
		t.Fatal("renewal not started")
	}
	done := make(chan struct{})
	go func() { runner.Stop(); close(done) }()
	// Stop must cancel the session even while a database renewal is pending.
	for {
		if _, _, ok := runner.Session(); !ok {
			break
		}
		select {
		case <-done:
			t.Fatal("Stop returned with renewal in flight")
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case <-done:
		t.Fatal("Stop returned before renewal completed")
	default:
	}
	close(backend.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop did not join renewal")
	}
	select {
	case <-backend.finished:
	default:
		t.Fatal("renewal still using database")
	}
	if _, ok := runner.installSession(context.Background(), &Session{}, time.Now().Add(time.Second)); ok {
		t.Fatal("stopped runner restarted")
	}
}
