package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"testing"
	"time"
)

type shutdownFunc func(context.Context) error

func (f shutdownFunc) Shutdown(ctx context.Context) error { return f(ctx) }

func TestLifecycleCleanup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	life := newLifecycle(ctx, time.Second, quietLogger())
	var order []string
	for _, name := range []string{"HTTP", "Trust"} {
		shutdown := shutdownFunc(func(ctx context.Context) error {
			if ctx.Err() != nil {
				t.Error("cleanup inherited cancelled parent", ctx.Err())
			}
			if life.ctx.Err() == nil {
				t.Error("workers still accepting work during shutdown")
			}
			order = append(order, name)
			return nil
		})
		if name == "HTTP" {
			life.http = shutdown
		} else {
			life.trust = shutdown
		}
	}
	workerDone := make(chan struct{})
	life.start("worker", func(ctx context.Context) error { <-ctx.Done(); close(workerDone); return ctx.Err() })
	cancel()
	if err := life.close(life.wait()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	select {
	case <-workerDone:
	default:
		t.Fatal("shared resources would close before worker")
	}
	if !reflect.DeepEqual(order, []string{"HTTP", "Trust"}) {
		t.Fatal(order)
	}
}

func TestLifecycleFailureDuringCancellation(t *testing.T) {
	for _, name := range []string{"serve HTTP", "run local slice worker", "serve trust UDS", "run telemetry maintenance"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			life := newLifecycle(ctx, time.Second, quietLogger())
			failure := errors.New("component failure")
			life.start(name, func(ctx context.Context) error { <-ctx.Done(); return failure })
			life.start("other worker", func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })
			cancel()
			err := life.close(life.wait())
			if !errors.Is(err, failure) || errors.Is(err, context.Canceled) {
				t.Fatalf("CLI would lose real failure: %v", err)
			}
			if err.Error() != name+": component failure" {
				t.Fatal(err)
			}
		})
	}
}

func TestLifecycleStartupAndCleanupFailures(t *testing.T) {
	life := newLifecycle(context.Background(), time.Second, quietLogger())
	startupErr, cleanupErr := errors.New("construction failed"), errors.New("shutdown failed")
	trustClosed := false
	life.http = shutdownFunc(func(context.Context) error { return errors.Join(cleanupErr, context.Canceled) })
	life.trust = shutdownFunc(func(context.Context) error { trustClosed = true; return nil })
	// A bound but not yet served listener still needs closing; no task to await.
	err := life.close(startupErr)
	if !errors.Is(err, startupErr) || !errors.Is(err, cleanupErr) || errors.Is(err, context.Canceled) || !trustClosed {
		t.Fatalf("partial cleanup lost original error or Trust: %v", err)
	}
}

func TestLifecycleWaitIsBounded(t *testing.T) {
	life := newLifecycle(context.Background(), 20*time.Millisecond, quietLogger())
	release, finished := make(chan struct{}), make(chan struct{})
	life.start("stalled", func(context.Context) error { <-release; close(finished); return nil })
	err := life.close(nil)
	close(release)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("late task result blocked after close")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unbounded worker was not reported: %v", err)
	}
}

func TestLifecycleSimultaneousFailures(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	life := newLifecycle(ctx, time.Second, quietLogger())
	first, second := errors.New("HTTP failed"), errors.New("worker failed")
	life.start("HTTP", func(ctx context.Context) error { <-ctx.Done(); return first })
	life.start("worker", func(ctx context.Context) error { <-ctx.Done(); return errors.Join(second, ctx.Err()) })
	cancel()
	err := life.close(life.wait())
	if !errors.Is(err, first) || !errors.Is(err, second) || errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestLifecycleFailureProcessExit(t *testing.T) {
	if os.Getenv("OCSERV_APP_EXIT_TEST") == "1" {
		ctx, cancel := context.WithCancel(context.Background())
		life := newLifecycle(ctx, time.Second, quietLogger())
		life.start("worker", func(ctx context.Context) error { <-ctx.Done(); return errors.New("real component failure") })
		cancel()
		// Match cmd/ocserv-control's unchanged exit classification.
		if err := life.close(life.wait()); err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLifecycleFailureProcessExit$")
	cmd.Env = append(os.Environ(), "OCSERV_APP_EXIT_TEST=1")
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || string(output) != "worker: real component failure\n" {
		t.Fatalf("wrong failure exit: %v %s", err, output)
	}
}
