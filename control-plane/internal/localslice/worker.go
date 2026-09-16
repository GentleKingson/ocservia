package localslice

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/transportclient"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

type Worker struct {
	service   *Service
	transport *transportclient.Client
	logger    *slog.Logger
}

func NewWorker(service *Service, transport *transportclient.Client, logger *slog.Logger) *Worker {
	return &Worker{service: service, transport: transport, logger: logger}
}

func (w *Worker) Run(ctx context.Context) error {
	return runWorker(ctx, func(ctx context.Context) error {
		return w.transport.RunWatch(ctx, w.service, w.service)
	}, w.dispatch)
}

func runWorker(ctx context.Context, watch, dispatch func(context.Context) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, 2)
	go func() { errCh <- watch(ctx) }()
	go func() { errCh <- dispatch(ctx) }()
	var failures []error
	for range 2 {
		err := <-errCh
		cancel()
		if err != nil && !errors.Is(err, context.Canceled) {
			failures = append(failures, err)
		}
	}
	if err := errors.Join(failures...); err != nil {
		return err
	}
	return ctx.Err()
}

func (w *Worker) dispatch(ctx context.Context) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := w.service.ExpireJobs(ctx); err != nil {
				w.logger.ErrorContext(ctx, "expire simulator jobs failed", "error", err)
				continue
			}
			jobs, err := w.service.ClaimJobs(ctx, 16)
			if err != nil {
				w.logger.ErrorContext(ctx, "claim simulator jobs failed", "error", err)
				continue
			}
			for _, job := range jobs {
				jobCtx := propagation.TraceContext{}.Extract(ctx, propagation.MapCarrier{"traceparent": job.Traceparent})
				jobCtx, span := otel.Tracer("ocservia.localslice").Start(jobCtx, "local_slice.dispatch", trace.WithSpanKind(trace.SpanKindProducer))
				if err := w.service.MarkDispatchStarted(ctx, job.OperationID); err != nil {
					span.RecordError(err)
					span.End()
					w.logger.ErrorContext(ctx, "mark simulator dispatch started", "operation_id", job.OperationID, "error", err)
					continue
				}
				if err := w.transport.SendCommand(jobCtx, job.NodeID[:], job.Envelope); err != nil {
					span.RecordError(err)
					span.End()
					w.logger.WarnContext(ctx, "simulator dispatch failed", "operation_id", job.OperationID, "error", err)
					if markErr := w.service.MarkDispatchError(ctx, job.OperationID, err.Error()); markErr != nil {
						w.logger.ErrorContext(ctx, "record simulator dispatch failure", "operation_id", job.OperationID, "error", markErr)
					}
					continue
				}
				span.End()
			}
		}
	}
}
