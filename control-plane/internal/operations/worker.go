package operations

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

type Sender interface {
	SendCommand(context.Context, []byte, []byte) error
}

type Worker struct {
	service *Service
	sender  Sender
	logger  *slog.Logger
	id      uuid.UUID
	workerFaults
}

const (
	defaultCommandLease    = 10 * time.Second
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
	jobs, err := w.service.Claim(ctx, w.id, dispatchBatchLimit, defaultCommandLease)
	if err != nil {
		return 0, false, err
	}
	claimedCount := len(jobs)
	if len(jobs) == 0 {
		return claimedCount, false, nil
	}
	// DispatchCandidates selects at most one command per node, so cross-node
	// dispatches are independent.
	if len(jobs) == 1 {
		w.dispatchOne(ctx, jobs[0])
		return claimedCount, false, nil
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
	return claimedCount, claimedCount == dispatchBatchLimit, nil
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

type sentDispatchError struct{ err error }

func (e *sentDispatchError) Error() string { return e.err.Error() }
func (e *sentDispatchError) Unwrap() error { return e.err }

func (w *Worker) dispatchFenced(ctx context.Context, job Dispatch) error {
	if faultInjection {
		if err := w.beforeSend(ctx, job); err != nil {
			return err
		}
	}
	if err := w.sender.SendCommand(ctx, job.NodeID[:], job.Envelope); err != nil {
		return err
	}
	if faultInjection {
		if err := w.afterSend(ctx, job); err != nil {
			return &sentDispatchError{err: err}
		}
	}
	if err := w.service.MarkSent(ctx, job); err != nil {
		return &sentDispatchError{err: err}
	}
	return nil
}
