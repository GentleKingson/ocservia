package localslice

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWorkerJoinsWatchAndDispatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	failure := errors.New("watch failed")
	dispatchDone := make(chan struct{})
	err := runWorker(ctx, func(context.Context) error { cancel(); return failure }, func(ctx context.Context) error {
		<-ctx.Done()
		close(dispatchDone)
		return ctx.Err()
	})
	if !errors.Is(err, failure) || errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	select {
	case <-dispatchDone:
	default:
		t.Fatal("dispatch still uses database after Run")
	}
}

func TestWorkerCancelsSiblingOnFailure(t *testing.T) {
	failure := errors.New("stream rejected")
	done := make(chan error, 1)
	go func() {
		done <- runWorker(context.Background(), func(context.Context) error { return failure }, func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })
	}()
	select {
	case err := <-done:
		if !errors.Is(err, failure) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("failed watch left dispatch running")
	}
}
