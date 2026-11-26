package devsim

import (
	"context"
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
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := w.service.ExpireSimulationJobs(ctx); err != nil {
				w.logger.ErrorContext(ctx, "expire simulator jobs failed", "error", err)
				continue
			}
			jobs, err := w.service.ClaimSimulationJobs(ctx, 16)
			if err != nil {
				w.logger.ErrorContext(ctx, "claim simulator jobs failed", "error", err)
				continue
			}
			for _, job := range jobs {
				jobCtx := propagation.TraceContext{}.Extract(ctx, propagation.MapCarrier{"traceparent": job.Traceparent})
				jobCtx, span := otel.Tracer("ocservia.devsim").Start(jobCtx, "development.simulation.dispatch", trace.WithSpanKind(trace.SpanKindProducer))
				if err := w.service.MarkSimulationDispatchStarted(ctx, job.OperationID); err != nil {
					span.RecordError(err)
					span.End()
					w.logger.ErrorContext(ctx, "mark simulator dispatch started", "operation_id", job.OperationID, "error", err)
					continue
				}
				if err := w.transport.SendCommand(jobCtx, job.NodeID[:], job.Envelope); err != nil {
					span.RecordError(err)
					span.End()
					w.logger.WarnContext(ctx, "simulator dispatch failed", "operation_id", job.OperationID, "error", err)
					if markErr := w.service.MarkSimulationDispatchError(ctx, job.OperationID, err.Error()); markErr != nil {
						w.logger.ErrorContext(ctx, "record simulator dispatch failure", "operation_id", job.OperationID, "error", markErr)
					}
					continue
				}
				span.End()
			}
		}
	}
}
