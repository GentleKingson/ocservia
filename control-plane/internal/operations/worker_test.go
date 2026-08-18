package operations

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

type blockingWorkerSender struct {
	reached chan struct{}
}

func (s *blockingWorkerSender) SendCommand(ctx context.Context, _, _ []byte) error {
	close(s.reached)
	<-ctx.Done()
	return ctx.Err()
}

type notifyingWorkerSender struct {
	reached chan struct{}
}

func (s *notifyingWorkerSender) SendCommand(context.Context, []byte, []byte) error {
	close(s.reached)
	return nil
}

func TestPreSendBarrierIgnoresDifferentCommand(t *testing.T) {
	directory := t.TempDir()
	commandID := uuid.Must(uuid.NewV7())
	if err := os.WriteFile(filepath.Join(directory, "arm"), []byte(uuid.Must(uuid.NewV7()).String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	worker := &Worker{preSendBarrierDir: directory}
	if err := worker.waitAtCommandBarrier(context.Background(), commandID, "arm", "received", "release", nil); err != nil {
		t.Fatalf("mismatched barrier returned an error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, "received")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mismatched barrier wrote a signal: %v", err)
	}
}

func TestPreSendBarrierSignalsAndWaitsForExactRelease(t *testing.T) {
	directory := t.TempDir()
	commandID := uuid.Must(uuid.NewV7())
	if err := os.WriteFile(filepath.Join(directory, "arm"), []byte(commandID.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	worker := &Worker{preSendBarrierDir: directory}
	done := make(chan error, 1)
	go func() {
		done <- worker.waitAtCommandBarrier(context.Background(), commandID, "arm", "received", "release", nil)
	}()

	receivedPath := filepath.Join(directory, "received")
	deadline := time.Now().Add(time.Second)
	for {
		received, err := os.ReadFile(receivedPath)
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(received)), "\n")
			if len(lines) != 2 || lines[0] != commandID.String() {
				t.Fatalf("unexpected pre-send signal %q", received)
			}
			if _, err := time.Parse(time.RFC3339, lines[1]); err != nil {
				t.Fatalf("pre-send signal timestamp is invalid: %q", lines[1])
			}
			info, err := os.Stat(receivedPath)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o644 {
				t.Fatalf("pre-send signal mode = %o, want 644", info.Mode().Perm())
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("pre-send barrier did not signal receipt")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := os.WriteFile(filepath.Join(directory, "release"), []byte(uuid.Must(uuid.NewV7()).String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("barrier accepted a different command release: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	if err := os.WriteFile(filepath.Join(directory, "release"), []byte(commandID.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("pre-send barrier did not accept the exact release")
	}
}

func TestPreSendBarrierHonorsContextCancellation(t *testing.T) {
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
			t.Fatal("pre-send barrier did not signal before cancellation")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("barrier cancellation error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pre-send barrier ignored context cancellation")
	}
}

func TestPostSendBarrierUsesDistinctExactFiles(t *testing.T) {
	directory := t.TempDir()
	commandID := uuid.Must(uuid.NewV7())
	if err := os.WriteFile(filepath.Join(directory, "post-send-arm"), []byte(commandID.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	worker := &Worker{preSendBarrierDir: directory}
	done := make(chan error, 1)
	go func() { done <- worker.waitAtPostSendBarrier(context.Background(), commandID) }()

	postReceived := filepath.Join(directory, "post-send-received")
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(postReceived); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("post-send barrier did not signal")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(directory, "received")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("post-send barrier reused the pre-send signal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(directory, "release"), []byte(commandID.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("post-send barrier accepted the pre-send release: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	if err := os.WriteFile(filepath.Join(directory, "post-send-release"), []byte(commandID.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("post-send barrier did not accept its exact release")
	}
}

func TestPostSendBarrierFailureIsClassifiedAsAccepted(t *testing.T) {
	directory := t.TempDir()
	commandID := uuid.Must(uuid.NewV7())
	if err := os.WriteFile(filepath.Join(directory, "post-send-arm"), []byte(commandID.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	worker := &Worker{
		sender:            &notifyingWorkerSender{reached: make(chan struct{})},
		preSendBarrierDir: directory,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.dispatchFenced(ctx, Dispatch{CommandID: commandID}) }()

	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(filepath.Join(directory, "post-send-received")); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("post-send barrier did not signal")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		var accepted *sentDispatchError
		if !errors.As(err, &accepted) || !errors.Is(err, context.Canceled) {
			t.Fatalf("post-send cancellation = %v, want accepted context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("post-send barrier ignored cancellation")
	}
}

func TestPreSendBarrierRequiresCanonicalArm(t *testing.T) {
	directory := t.TempDir()
	worker := &Worker{preSendBarrierDir: directory}
	_, armed, err := worker.readBarrierArm("arm")
	if err != nil || armed {
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

func TestWorkerUsesTestLeaseOnlyWhenArmedIntegration(t *testing.T) {
	for _, test := range []struct {
		name  string
		armed bool
	}{
		{name: "unarmed production lease"},
		{name: "armed exact extension", armed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, pool, _, nodeID := integrationService(t)
			key := "configured-worker-lease-" + strings.ReplaceAll(test.name, " ", "-")
			operation, replayed, err := service.CreateSynthetic(context.Background(), testRequest(nodeID, key, SyntheticNoop, ""))
			if err != nil || replayed || operation.CommandID == nil {
				t.Fatalf("create command = %+v, replayed=%v, err=%v", operation, replayed, err)
			}
			commandID, err := uuid.Parse(*operation.CommandID)
			if err != nil {
				t.Fatal(err)
			}
			directory := t.TempDir()
			if test.armed {
				if err := os.WriteFile(filepath.Join(directory, "arm"), []byte(commandID.String()+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			sender := &blockingWorkerSender{reached: make(chan struct{})}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			worker, err := NewWorker(service, sender, logger)
			if err != nil {
				t.Fatal(err)
			}
			if err := worker.EnablePreSendBarrier(directory, 60*time.Second); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- worker.dispatch(ctx) }()

			readyPath := filepath.Join(directory, "received")
			deadline := time.Now().Add(time.Second)
			for {
				ready := false
				if test.armed {
					if _, err := os.Stat(readyPath); err == nil {
						ready = true
					} else if !errors.Is(err, os.ErrNotExist) {
						t.Fatal(err)
					}
				} else {
					select {
					case <-sender.reached:
						ready = true
					default:
					}
				}
				if ready {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("worker did not reach the configured dispatch point")
				}
				time.Sleep(10 * time.Millisecond)
			}
			var exactLease bool
			leaseQuery := `SELECT lease.leased_until-lease.created_at=interval '10 seconds'
				FROM node_command_leases AS lease WHERE lease.command_id=$1`
			if test.armed {
				leaseQuery = `SELECT lease.leased_until=outbox.locked_until
					AND lease.leased_until>=clock_timestamp()+interval '55 seconds'
					AND lease.leased_until<=clock_timestamp()+interval '60 seconds'
					FROM node_command_leases AS lease
					JOIN outbox_events AS outbox ON outbox.command_id=lease.command_id
					WHERE lease.command_id=$1`
			}
			if err := pool.QueryRow(context.Background(), leaseQuery, commandID).Scan(&exactLease); err != nil {
				t.Fatal(err)
			}
			if !exactLease {
				t.Fatalf("worker Claim lease did not match armed=%v semantics", test.armed)
			}
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("worker dispatch did not stop after cancellation")
			}
		})
	}
}

func TestPreSendBarrierReleasesNonTargetClaimIntegration(t *testing.T) {
	service, pool, _, nodeID := integrationService(t)
	first, _, err := service.CreateSynthetic(context.Background(), testRequest(nodeID, "pre-send-non-target", SyntheticNoop, ""))
	if err != nil || first.CommandID == nil {
		t.Fatalf("create non-target command = %+v, %v", first, err)
	}
	target, _, err := service.CreateSynthetic(context.Background(), testRequest(nodeID, "pre-send-exact-target", SyntheticNoop, ""))
	if err != nil || target.CommandID == nil {
		t.Fatalf("create target command = %+v, %v", target, err)
	}
	targetID := uuid.MustParse(*target.CommandID)
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "arm"), []byte(targetID.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sender := &blockingWorkerSender{reached: make(chan struct{})}
	worker, err := NewWorker(service, sender, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.EnablePreSendBarrier(directory, 60*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := worker.dispatch(context.Background()); err == nil {
		t.Fatal("a non-target Claim did not fail closed")
	}
	select {
	case <-sender.reached:
		t.Fatal("the non-target command reached transport")
	default:
	}
	var failedAttempts, liveLeases, targetAttempts int
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM command_attempts WHERE command_id=$1 AND state='failed'),
		(SELECT count(*) FROM node_command_leases WHERE command_id=$1),
		(SELECT count(*) FROM command_attempts WHERE command_id=$2)`, uuid.MustParse(*first.CommandID), targetID).Scan(&failedAttempts, &liveLeases, &targetAttempts); err != nil {
		t.Fatal(err)
	}
	if failedAttempts != 1 || liveLeases != 0 || targetAttempts != 0 {
		t.Fatalf("armed limit-one Claim = non-target attempts %d, leases %d, target attempts %d", failedAttempts, liveLeases, targetAttempts)
	}
}

func TestPostSendBarrierStopsBeforeMarkSentIntegration(t *testing.T) {
	service, pool, _, nodeID := integrationService(t)
	operation, _, err := service.CreateSynthetic(context.Background(), testRequest(nodeID, "post-send-before-mark-sent", SyntheticNoop, ""))
	if err != nil || operation.CommandID == nil {
		t.Fatalf("create command = %+v, %v", operation, err)
	}
	commandID := uuid.MustParse(*operation.CommandID)
	directory := t.TempDir()
	for _, name := range []string{"arm", "post-send-arm"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(commandID.String()+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sender := &notifyingWorkerSender{reached: make(chan struct{})}
	worker, err := NewWorker(service, sender, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.EnablePreSendBarrier(directory, 60*time.Second); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.dispatch(ctx) }()
	waitForFile := func(name string) {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for {
			if _, err := os.Stat(filepath.Join(directory, name)); err == nil {
				return
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			if time.Now().After(deadline) {
				t.Fatalf("worker did not create %s", name)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitForFile("received")
	if err := os.WriteFile(filepath.Join(directory, "release"), []byte(commandID.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForFile("post-send-received")
	select {
	case <-sender.reached:
	default:
		t.Fatal("post-send signal preceded transport acceptance")
	}
	var unpublished, sending bool
	if err := pool.QueryRow(context.Background(), `SELECT outbox.published_at IS NULL,
		attempt.state='sending' AND attempt.finished_at IS NULL
		FROM outbox_events AS outbox
		JOIN command_attempts AS attempt ON attempt.outbox_event_id=outbox.id
		WHERE outbox.command_id=$1`, commandID).Scan(&unpublished, &sending); err != nil {
		t.Fatal(err)
	}
	if !unpublished || !sending {
		t.Fatal("MarkSent advanced while the exact post-send barrier was held")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("post-send dispatch did not stop after cancellation")
	}
}
