package operations

import (
	"context"
	"log/slog"
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
}

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
		case <-poll.C:
			if err := w.dispatch(ctx); err != nil {
				w.logger.ErrorContext(ctx, "dispatch outbox batch", "error", err)
			}
		}
	}
}

func (w *Worker) dispatch(ctx context.Context) error {
	jobs, err := w.service.Claim(ctx, w.id, 16, 10*time.Second)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		jobCtx := propagation.TraceContext{}.Extract(ctx, propagation.MapCarrier{"traceparent": job.Traceparent})
		jobCtx, span := otel.Tracer("ocservia.operations").Start(jobCtx, "outbox.dispatch", trace.WithSpanKind(trace.SpanKindProducer))
		err := w.sender.SendCommand(jobCtx, job.NodeID[:], job.Envelope)
		if err != nil {
			span.RecordError(err)
			span.End()
			if markErr := w.service.MarkFailed(ctx, job, err); markErr != nil {
				w.logger.ErrorContext(ctx, "record command dispatch failure", "command_id", job.CommandID, "error", markErr)
			}
			continue
		}
		if err := w.service.MarkSent(ctx, job); err != nil {
			span.RecordError(err)
			w.logger.ErrorContext(ctx, "record command dispatch success", "command_id", job.CommandID, "error", err)
		}
		span.End()
	}
	return nil
}
