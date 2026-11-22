package operations

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/commandlimit"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

type Sender interface {
	SendCommand(context.Context, []byte, []byte) error
}

type Worker struct {
	service           *Service
	sender            Sender
	logger            *slog.Logger
	id                uuid.UUID
	testClaimLease    time.Duration
	preSendBarrierDir string
}

const (
	defaultCommandLease    = 10 * time.Second
	maxTestCommandLease    = 60 * time.Second
	dispatchBatchLimit     = 16
	maxDispatchConcurrency = 8
	maxDrainBatchesPerTick = 8
)

func NewWorker(service *Service, sender Sender, logger *slog.Logger) (*Worker, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	return &Worker{service: service, sender: sender, logger: logger, id: id}, nil
}

// EnablePreSendBarrier configures a development-only barrier for one exact
// claimed command. Production configuration rejects the hook before startup.
func (w *Worker) EnablePreSendBarrier(directory string, claimLease time.Duration) error {
	if !filepath.IsAbs(directory) {
		return errors.New("pre-send barrier path must be absolute")
	}
	info, err := os.Stat(directory)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("pre-send barrier path is not a directory")
	}
	if claimLease < defaultCommandLease || claimLease > maxTestCommandLease {
		return errors.New("pre-send barrier command lease must be between 10s and 60s")
	}
	w.preSendBarrierDir = filepath.Clean(directory)
	w.testClaimLease = claimLease
	return nil
}

func (w *Worker) Run(ctx context.Context) error {
	poll := time.NewTicker(200 * time.Millisecond)
	maintenance := time.NewTicker(time.Second)
	defer poll.Stop()
	defer maintenance.Stop()
	wakeup := w.service.Wakeup()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-maintenance.C:
			if err := w.service.Reap(ctx, 3); err != nil {
				w.logger.ErrorContext(ctx, "reap expired command leases", "error", err)
			}
			if err := w.service.Expire(ctx); err != nil {
				w.logger.ErrorContext(ctx, "expire queued commands", "error", err)
			}
			if err := w.service.ReconcileAgentUpgrades(ctx); err != nil {
				w.logger.ErrorContext(ctx, "reconcile agent upgrades", "error", err)
			}
		case <-wakeup:
			w.drain(ctx)
		case <-poll.C:
			w.drain(ctx)
		}
	}
}

func (w *Worker) drain(ctx context.Context) {
	for range maxDrainBatchesPerTick {
		if ctx.Err() != nil {
			return
		}
		claimed, full, err := w.dispatchBatch(ctx)
		if err != nil {
			w.logger.ErrorContext(ctx, "dispatch outbox batch", "error", err)
			return
		}
		if !full || claimed == 0 {
			return
		}
	}
}

func (w *Worker) dispatch(ctx context.Context) error {
	_, _, err := w.dispatchBatch(ctx)
	return err
}

func (w *Worker) dispatchBatch(ctx context.Context) (int, bool, error) {
	_, armedBeforeClaim, err := w.readBarrierArm("arm")
	if err != nil {
		return 0, false, err
	}
	limit := dispatchBatchLimit
	if armedBeforeClaim {
		limit = 1
	}
	jobs, err := w.service.Claim(ctx, w.id, limit, defaultCommandLease)
	if err != nil {
		return 0, false, err
	}
	claimedCount := len(jobs)
	jobs, err = w.selectPreSendBarrierTarget(ctx, jobs)
	if err != nil {
		return claimedCount, false, err
	}
	if len(jobs) == 0 {
		return claimedCount, false, nil
	}
	// DispatchCandidates selects at most one command per node, so cross-node
	// dispatches are independent; keep pre-send barrier runs strictly serial.
	if len(jobs) == 1 || armedBeforeClaim || w.preSendBarrierDir != "" {
		for _, job := range jobs {
			w.dispatchOne(ctx, job)
		}
		return claimedCount, claimedCount == limit && !armedBeforeClaim, nil
	}
	workerCount := min(maxDispatchConcurrency, len(jobs))
	queue := make(chan Dispatch, len(jobs))
	for _, job := range jobs {
		queue <- job
	}
	close(queue)
	var wg sync.WaitGroup
	wg.Add(workerCount)
	for range workerCount {
		go func() {
			defer wg.Done()
			for job := range queue {
				w.dispatchOne(ctx, job)
			}
		}()
	}
	wg.Wait()
	return claimedCount, claimedCount == limit, nil
}

func (w *Worker) dispatchOne(ctx context.Context, job Dispatch) {
	jobCtx := propagation.TraceContext{}.Extract(ctx, propagation.MapCarrier{"traceparent": job.Traceparent})
	jobCtx, span := otel.Tracer("ocservia.operations").Start(jobCtx, "outbox.dispatch", trace.WithSpanKind(trace.SpanKindProducer))
	err := w.dispatchFenced(jobCtx, job)
	if err != nil {
		span.RecordError(err)
		span.End()
		var accepted *sentDispatchError
		if errors.As(err, &accepted) {
			w.logger.ErrorContext(ctx, "record command dispatch success", "command_id", job.CommandID, "error", accepted.err)
			return
		}
		if markErr := w.service.MarkFailed(ctx, job, err); markErr != nil {
			w.logger.ErrorContext(ctx, "record command dispatch failure", "command_id", job.CommandID, "error", markErr)
		}
		return
	}
	span.End()
}

func (w *Worker) readBarrierArm(name string) (uuid.UUID, bool, error) {
	if w.preSendBarrierDir == "" {
		return uuid.Nil, false, nil
	}
	armed, err := os.ReadFile(filepath.Join(w.preSendBarrierDir, name))
	if errors.Is(err, os.ErrNotExist) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("read %s command barrier: %w", name, err)
	}
	armedID := string(bytes.TrimSpace(armed))
	parsed, err := uuid.Parse(armedID)
	if err != nil || parsed.String() != armedID {
		return uuid.Nil, false, fmt.Errorf("%s command barrier must contain a canonical command UUID", name)
	}
	return parsed, true, nil
}

func (w *Worker) selectPreSendBarrierTarget(ctx context.Context, jobs []Dispatch) ([]Dispatch, error) {
	armedID, armed, err := w.readBarrierArm("arm")
	if err != nil {
		for _, job := range jobs {
			_ = w.service.MarkFailed(ctx, job, err)
		}
		return nil, err
	}
	if !armed {
		return jobs, nil
	}
	var target *Dispatch
	for index := range jobs {
		if jobs[index].CommandID == armedID {
			target = &jobs[index]
			continue
		}
		if markErr := w.service.MarkFailed(ctx, jobs[index], errors.New("pre-send barrier selected a different command")); markErr != nil {
			return nil, fmt.Errorf("release non-target pre-send claim: %w", markErr)
		}
	}
	if target == nil {
		if len(jobs) == 0 {
			return nil, nil
		}
		return nil, errors.New("pre-send barrier target was not the claimed command")
	}
	return []Dispatch{*target}, nil
}

type sentDispatchError struct{ err error }

func (e *sentDispatchError) Error() string { return e.err.Error() }
func (e *sentDispatchError) Unwrap() error { return e.err }

func (w *Worker) dispatchFenced(ctx context.Context, job Dispatch) error {
	if err := w.waitAtCommandBarrier(ctx, job.CommandID, "arm", "received", "release", func() error {
		return w.extendPreSendClaim(ctx, job)
	}); err != nil {
		return err
	}
	if err := w.sender.SendCommand(ctx, job.NodeID[:], job.Envelope); err != nil {
		return err
	}
	if err := w.waitAtPostSendBarrier(ctx, job.CommandID); err != nil {
		return &sentDispatchError{err: err}
	}
	if err := w.service.MarkSentWithEnvelope(ctx, job, job.Envelope); err != nil {
		return &sentDispatchError{err: err}
	}
	return nil
}

func (w *Worker) waitAtPostSendBarrier(ctx context.Context, commandID uuid.UUID) error {
	return w.waitAtCommandBarrier(ctx, commandID, "post-send-arm", "post-send-received", "post-send-release", nil)
}

func (w *Worker) waitAtCommandBarrier(ctx context.Context, commandID uuid.UUID, armName, receivedName, releaseName string, onArmed func() error) error {
	armedID, armed, err := w.readBarrierArm(armName)
	if err != nil {
		return err
	}
	if !armed || armedID != commandID {
		return nil
	}
	if onArmed != nil {
		if err := onArmed(); err != nil {
			return err
		}
	}
	received := []byte(commandID.String() + "\n" + time.Now().UTC().Format(time.RFC3339) + "\n")
	if err := writeCommandBarrierSignal(w.preSendBarrierDir, receivedName, received); err != nil {
		return fmt.Errorf("signal %s command barrier: %w", armName, err)
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			release, readErr := os.ReadFile(filepath.Join(w.preSendBarrierDir, releaseName))
			if readErr == nil && string(bytes.TrimSpace(release)) == commandID.String() {
				return nil
			}
			if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
				return fmt.Errorf("read %s command barrier release: %w", armName, readErr)
			}
		}
	}
}

func (w *Worker) extendPreSendClaim(ctx context.Context, dispatch Dispatch) error {
	return database.Within(ctx, w.service.backend, database.ReadCommitted, func(tx database.Tx) error {
		if err := commandlimit.Lock(ctx, tx); err != nil {
			return fmt.Errorf("serialize pre-send claim extension: %w", err)
		}
		store, err := operationstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		return store.ExtendDispatch(ctx, dispatch, w.id, w.testClaimLease)
	})
}

func writeCommandBarrierSignal(directory, name string, contents []byte) error {
	temporary, err := os.CreateTemp(directory, ".received-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Chmod(0o644); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, filepath.Join(directory, name))
}
