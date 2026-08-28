package operations

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandlimit"
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
	service           *Service
	sender            Sender
	fences            FenceExecutor
	logger            *slog.Logger
	id                uuid.UUID
	testClaimLease    time.Duration
	preSendBarrierDir string
}

const (
	defaultCommandLease = 10 * time.Second
	maxTestCommandLease = 60 * time.Second
)

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
		case <-poll.C:
			if err := w.dispatch(ctx); err != nil {
				w.logger.ErrorContext(ctx, "dispatch outbox batch", "error", err)
			}
		}
	}
}

func (w *Worker) dispatch(ctx context.Context) error {
	_, armedBeforeClaim, err := w.readBarrierArm("arm")
	if err != nil {
		return err
	}
	limit := 16
	if armedBeforeClaim {
		limit = 1
	}
	jobs, err := w.service.Claim(ctx, w.id, limit, defaultCommandLease)
	if err != nil {
		return err
	}
	jobs, err = w.selectPreSendBarrierTarget(ctx, jobs)
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

// dispatchFenced sends and records one dispatch inside the owner's fencing
// interval. Keeping MarkSent inside the action prevents a same-process reopen
// from advancing the owner epoch between transport acceptance and persistence
// of the exact sent frame.
func (w *Worker) dispatchFenced(ctx context.Context, job Dispatch) error {
	if err := w.waitAtPreSendBarrier(ctx, job); err != nil {
		return err
	}
	if w.fences == nil {
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
			if err := w.waitAtPostSendBarrier(ctx, job.CommandID); err != nil {
				return &sentDispatchError{err: err}
			}
			if err := w.service.MarkSentWithEnvelope(ctx, job, fenced); err != nil {
				return &sentDispatchError{err: err}
			}
			return nil
		})
}

func (w *Worker) waitAtPreSendBarrier(ctx context.Context, dispatch Dispatch) error {
	return w.waitAtCommandBarrier(ctx, dispatch.CommandID, "arm", "received", "release", func() error {
		return w.extendPreSendClaim(ctx, dispatch)
	})
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
	tx, err := w.service.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin pre-send claim extension: %w", err)
	}
	defer rollback(tx)
	if err := commandlimit.Lock(ctx, tx); err != nil {
		return fmt.Errorf("serialize pre-send claim extension: %w", err)
	}
	var workerID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT lease.worker_id
		FROM outbox_events AS outbox
		JOIN node_command_leases AS lease
		  ON lease.command_id=$2 AND lease.node_id=$3
		JOIN command_attempts AS attempt
		  ON attempt.id=$4 AND attempt.command_id=$2 AND attempt.outbox_event_id=$1
		WHERE outbox.id=$1 AND outbox.command_id=$2 AND outbox.published_at IS NULL
		  AND lease.lease_token=$5 AND lease.leased_until>clock_timestamp()
		  AND outbox.locked_by=lease.worker_id AND outbox.locked_until>clock_timestamp()
		  AND attempt.worker_id=lease.worker_id AND attempt.state='sending'
		  AND attempt.finished_at IS NULL
		FOR UPDATE OF outbox,lease,attempt`, dispatch.OutboxID, dispatch.CommandID,
		dispatch.NodeID, dispatch.AttemptID, dispatch.LeaseToken).Scan(&workerID)
	if err != nil {
		return fmt.Errorf("lock exact pre-send claim: %w", err)
	}
	if workerID != w.id {
		return errors.New("pre-send claim belongs to another worker")
	}
	var extendedUntil time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()+$1::bigint*interval '1 microsecond'`, w.testClaimLease.Microseconds()).Scan(&extendedUntil); err != nil {
		return fmt.Errorf("compute pre-send claim extension: %w", err)
	}
	leaseTag, err := tx.Exec(ctx, `UPDATE node_command_leases SET leased_until=$2
		WHERE lease_token=$1 AND worker_id=$3`, dispatch.LeaseToken, extendedUntil, w.id)
	if err != nil {
		return fmt.Errorf("extend exact pre-send node lease: %w", err)
	}
	if leaseTag.RowsAffected() != 1 {
		return errors.New("extend exact pre-send node lease affected no row")
	}
	outboxTag, err := tx.Exec(ctx, `UPDATE outbox_events SET locked_until=$2
		WHERE id=$1 AND command_id=$3 AND locked_by=$4`, dispatch.OutboxID, extendedUntil, dispatch.CommandID, w.id)
	if err != nil {
		return fmt.Errorf("extend exact pre-send outbox lock: %w", err)
	}
	if outboxTag.RowsAffected() != 1 {
		return errors.New("extend exact pre-send outbox lock affected no row")
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit pre-send claim extension: %w", err)
	}
	return nil
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
