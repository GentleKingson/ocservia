package operations

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestEnablePreSendBarrierRejectsUnsafeConfiguration(t *testing.T) {
	worker := &Worker{}
	if err := worker.EnablePreSendBarrier("relative", 10*time.Second); err == nil {
		t.Fatal("relative barrier directory was accepted")
	}
	directory := t.TempDir()
	for _, lease := range []time.Duration{9 * time.Second, 61 * time.Second} {
		if err := worker.EnablePreSendBarrier(directory, lease); err == nil {
			t.Fatalf("unsafe test lease %s was accepted", lease)
		}
	}
	if err := worker.EnablePreSendBarrier(directory, 60*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestPreSendBarrierRequiresCanonicalArm(t *testing.T) {
	directory := t.TempDir()
	worker := &Worker{preSendBarrierDir: directory}
	if _, armed, err := worker.readBarrierArm("arm"); err != nil || armed {
		t.Fatalf("unarmed read = %v, error = %v", armed, err)
	}
	if err := os.WriteFile(filepath.Join(directory, "arm"), []byte("not-a-command-id\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := worker.readBarrierArm("arm"); err == nil {
		t.Fatal("malformed pre-send arm was accepted")
	}
	commandID := uuid.Must(uuid.NewV7())
	if err := os.WriteFile(filepath.Join(directory, "arm"), []byte(commandID.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, armed, err := worker.readBarrierArm("arm")
	if err != nil || !armed || got != commandID {
		t.Fatalf("armed read = %s/%v, error = %v", got, armed, err)
	}
}

func TestCommandBarrierStopsOnCancellation(t *testing.T) {
	directory := t.TempDir()
	commandID := uuid.Must(uuid.NewV7())
	if err := os.WriteFile(filepath.Join(directory, "arm"), []byte(commandID.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	worker := &Worker{preSendBarrierDir: directory}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- worker.waitAtCommandBarrier(ctx, commandID, "arm", "received", "release", nil)
	}()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(filepath.Join(directory, "received")); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("barrier did not signal")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("barrier cancellation = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("barrier ignored cancellation")
	}
}
