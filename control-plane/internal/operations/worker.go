package operations

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/ownersession"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"
)

type Sender interface {
	SendCommand(context.Context, []byte, []byte) error
}

// FenceBinder signs owner fence proofs for command dispatch. A nil binder
// keeps the unfenced compatibility path for deployments whose agent sessions
// never negotiated connection fencing.
type FenceBinder interface {
	BindOperation(ctx context.Context, nodeID [16]byte, kind agentv1.FenceOperationKind, operationID [16]byte, capability string) (*agentv1.ConnectionFenceV2, *agentv1.FenceBindingV2, error)
}

type Worker struct {
	service *Service
	sender  Sender
	fences  FenceBinder
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

// NewFencedWorker attaches the per-node connection owner binder so every
// dispatch to a fenced session carries the fence and the attempt binding.
func NewFencedWorker(service *Service, sender Sender, fences FenceBinder, logger *slog.Logger) (*Worker, error) {
	worker, err := NewWorker(service, sender, logger)
	if err != nil {
		return nil, err
	}
	worker.fences = fences
	return worker, nil
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
		envelope, fenceErr := w.fenceEnvelope(jobCtx, job)
		if fenceErr != nil {
			span.RecordError(fenceErr)
			span.End()
			if markErr := w.service.MarkFailed(ctx, job, fenceErr); markErr != nil {
				w.logger.ErrorContext(ctx, "record command dispatch failure", "command_id", job.CommandID, "error", markErr)
			}
			continue
		}
		err := w.sender.SendCommand(jobCtx, job.NodeID[:], envelope)
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

// fenceEnvelope attaches the owner fence and the attempt binding to the
// dispatch envelope when the node session is fenced. Nodes without an owner
// fence keep the established unfenced wire form; ownership loss fails the
// dispatch so the command returns to the queue instead of being attributed to
// a stale owner.
func (w *Worker) fenceEnvelope(ctx context.Context, job Dispatch) ([]byte, error) {
	if w.fences == nil {
		return job.Envelope, nil
	}
	envelope := &agentv1.CommandEnvelope{}
	if err := proto.Unmarshal(job.Envelope, envelope); err != nil {
		return nil, fmt.Errorf("decode command envelope for fencing: %w", err)
	}
	var nodeID [16]byte
	copy(nodeID[:], job.NodeID[:])
	var commandID [16]byte
	copy(commandID[:], job.CommandID[:])
	fence, binding, err := w.fences.BindOperation(ctx, nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, commandID, envelope.GetRequiredCapability())
	if errors.Is(err, ownersession.ErrNoFence) {
		return job.Envelope, nil
	}
	if err != nil {
		return nil, err
	}
	envelope.ConnectionFence = fence
	envelope.FenceBinding = binding
	fenced, err := proto.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("encode fenced command envelope: %w", err)
	}
	return fenced, nil
}
