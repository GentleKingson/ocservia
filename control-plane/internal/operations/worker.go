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

// FenceExecutor runs dispatch mutations inside the connection owner's
// fencing interval. A nil executor keeps the unfenced compatibility path for
// deployments whose agent sessions never negotiated connection fencing.
type FenceExecutor interface {
	ExecuteFenced(ctx context.Context, nodeID [16]byte, kind agentv1.FenceOperationKind, operationID [16]byte, capability string, action ownersession.FencedAction) error
}

type Worker struct {
	service *Service
	sender  Sender
	fences  FenceExecutor
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

// NewFencedWorker runs every dispatch to a fenced session inside the
// connection owner's fencing interval, so the fence and attempt binding on
// the wire were backed by the ownership authority at mutation time.
func NewFencedWorker(service *Service, sender Sender, fences FenceExecutor, logger *slog.Logger) (*Worker, error) {
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
		err := w.dispatchFenced(jobCtx, job)
		if err != nil {
			span.RecordError(err)
			span.End()
			var accepted *sentDispatchError
			if errors.As(err, &accepted) {
				w.logger.ErrorContext(ctx, "record command dispatch success", "command_id", job.CommandID, "error", accepted.err)
				continue
			}
			if markErr := w.service.MarkFailed(ctx, job, err); markErr != nil {
				w.logger.ErrorContext(ctx, "record command dispatch failure", "command_id", job.CommandID, "error", markErr)
			}
			continue
		}
		span.End()
	}
	return nil
}

type sentDispatchError struct{ err error }

func (e *sentDispatchError) Error() string { return e.err.Error() }
func (e *sentDispatchError) Unwrap() error { return e.err }

// dispatchFenced sends and records one dispatch inside the owner's fencing
// interval. Keeping MarkSent inside the action prevents a same-process reopen
// from advancing the owner epoch between transport acceptance and persistence
// of the exact sent frame.
func (w *Worker) dispatchFenced(ctx context.Context, job Dispatch) error {
	if w.fences == nil {
		if err := w.sender.SendCommand(ctx, job.NodeID[:], job.Envelope); err != nil {
			return err
		}
		if err := w.service.MarkSentWithEnvelope(ctx, job, job.Envelope); err != nil {
			return &sentDispatchError{err: err}
		}
		return nil
	}
	envelope := &agentv1.CommandEnvelope{}
	if err := proto.Unmarshal(job.Envelope, envelope); err != nil {
		return fmt.Errorf("decode command envelope for fencing: %w", err)
	}
	var nodeID [16]byte
	copy(nodeID[:], job.NodeID[:])
	var commandID [16]byte
	copy(commandID[:], job.CommandID[:])
	return w.fences.ExecuteFenced(ctx, nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, commandID, envelope.GetRequiredCapability(),
		func(ctx context.Context, fence *agentv1.ConnectionFenceV2, binding *agentv1.FenceBindingV2) error {
			envelope.ConnectionFence = fence
			envelope.FenceBinding = binding
			fenced, err := proto.Marshal(envelope)
			if err != nil {
				return fmt.Errorf("encode fenced command envelope: %w", err)
			}
			if err := w.sender.SendCommand(ctx, job.NodeID[:], fenced); err != nil {
				return err
			}
			if err := w.service.MarkSentWithEnvelope(ctx, job, fenced); err != nil {
				return &sentDispatchError{err: err}
			}
			return nil
		})
}
