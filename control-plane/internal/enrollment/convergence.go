package enrollment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	enrollmentstore "github.com/GentleKingson/ocservia/control-plane/internal/enrollment/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/ownersession"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type TrustTransport interface {
	UpdateNodeTrust(context.Context, []byte, []byte, transportv1.NodeTrustState, string, uint64, []byte, *agentv1.FenceBindingV2) error
	CloseNode(context.Context, []byte, string, *agentv1.FenceBindingV2) error
}

type TrustConvergenceWorker struct {
	backend   database.Backend
	transport TrustTransport
	fences    ownersession.FencedExecutor
	logger    *slog.Logger
	workerID  uuid.UUID
}

type trustConvergenceJob struct {
	enrollmentstore.TrustJob
	State transportv1.NodeTrustState
}

func NewTrustConvergenceWorker(pool *pgxpool.Pool, transport TrustTransport, logger *slog.Logger) (*TrustConvergenceWorker, error) {
	return NewTrustConvergenceWorkerBackend(postgres.WrapPool(pool), transport, logger)
}

func NewTrustConvergenceWorkerBackend(backend database.Backend, transport TrustTransport, logger *slog.Logger) (*TrustConvergenceWorker, error) {
	workerID, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	return &TrustConvergenceWorker{backend: backend, transport: transport, logger: logger, workerID: workerID}, nil
}

// NewFencedTrustConvergenceWorker runs trust updates and connection closes
// inside the connection owner's fencing interval, so a stale owner cannot
// drive connection state and no binding outlives the term the ownership
// authority backed at mutation time. Nodes without a registered fence keep
// the unfenced compatibility path.
func NewFencedTrustConvergenceWorker(pool *pgxpool.Pool, transport TrustTransport, fences ownersession.FencedExecutor, logger *slog.Logger) (*TrustConvergenceWorker, error) {
	return NewFencedTrustConvergenceWorkerBackend(postgres.WrapPool(pool), transport, fences, logger)
}

func NewFencedTrustConvergenceWorkerBackend(backend database.Backend, transport TrustTransport, fences ownersession.FencedExecutor, logger *slog.Logger) (*TrustConvergenceWorker, error) {
	worker, err := NewTrustConvergenceWorkerBackend(backend, transport, logger)
	if err != nil {
		return nil, err
	}
	worker.fences = fences
	return worker, nil
}

// executeFenced runs one administrative transport mutation inside the owner
// fencing interval. Trust updates and connection closes are Controller-side
// operations, so their binding capability is the fencing capability itself,
// which every fence carries by construction.
func (w *TrustConvergenceWorker) executeFenced(ctx context.Context, nodeID uuid.UUID, kind agentv1.FenceOperationKind, operationID [16]byte, action ownersession.FencedAction) error {
	if w.fences == nil {
		return action(ctx, nil, nil)
	}
	var fixed [16]byte
	copy(fixed[:], nodeID[:])
	return w.fences.ExecuteFenced(ctx, fixed, kind, operationID, ownersession.FencingCapability, action)
}

func (w *TrustConvergenceWorker) Run(ctx context.Context) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			for range 16 {
				worked, err := w.RunOnce(ctx)
				if err != nil {
					w.logger.ErrorContext(ctx, "converge node trust", "error", err)
					break
				}
				if !worked {
					break
				}
			}
		}
	}
}

func (w *TrustConvergenceWorker) RunOnce(ctx context.Context) (bool, error) {
	job, err := w.claim(ctx)
	if errors.Is(err, database.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !job.UpdateApplied {
		operationID := ownersession.StateUpdateOperationID([16]byte(job.NodeID), job.EndpointID, int32(job.State), job.Revision, job.Reason)
		err := w.executeFenced(ctx, job.NodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_STATE_UPDATE, operationID,
			func(ctx context.Context, _ *agentv1.ConnectionFenceV2, binding *agentv1.FenceBindingV2) error {
				return w.transport.UpdateNodeTrust(ctx, job.NodeID[:], job.EndpointID, job.State, job.Reason, job.Revision, operationID[:], binding)
			})
		if err != nil {
			return true, w.release(ctx, job, err)
		}
		if err := w.markUpdateApplied(ctx, job); err != nil {
			return true, err
		}
		job.UpdateApplied = true
	}
	if job.CloseRequired && !job.CloseApplied {
		err := w.executeFenced(ctx, job.NodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_CONNECTION_CLOSE, [16]byte(job.NodeID),
			func(ctx context.Context, _ *agentv1.ConnectionFenceV2, binding *agentv1.FenceBindingV2) error {
				return w.transport.CloseNode(ctx, job.NodeID[:], "node revoked", binding)
			})
		if err != nil {
			return true, w.release(ctx, job, err)
		}
		if err := w.markCloseApplied(ctx, job); err != nil {
			return true, err
		}
		return true, nil
	}
	if err := w.unlockComplete(ctx, job); err != nil {
		return true, err
	}
	return true, nil
}

func (w *TrustConvergenceWorker) claim(ctx context.Context) (trustConvergenceJob, error) {
	var job trustConvergenceJob
	err := w.withTrust(ctx, func(store enrollmentstore.TrustStore) error {
		var err error
		job.TrustJob, err = store.Claim(ctx, w.workerID)
		if err != nil {
			return err
		}
		switch job.DesiredState {
		case "active":
			job.State = transportv1.NodeTrustState_NODE_TRUST_STATE_ACTIVE
		case "revoked":
			job.State = transportv1.NodeTrustState_NODE_TRUST_STATE_REVOKED
		default:
			return errors.New("stored trust convergence state is invalid")
		}
		return nil
	})
	return job, err
}

func (w *TrustConvergenceWorker) markUpdateApplied(ctx context.Context, job trustConvergenceJob) error {
	return w.withTrust(ctx, func(store enrollmentstore.TrustStore) error {
		changed, err := store.MarkUpdateApplied(ctx, job.TrustJob, w.workerID)
		if err != nil {
			return fmt.Errorf("record trust update convergence: %w", err)
		}
		if !changed {
			return errors.New("record trust update convergence changed no row")
		}
		return nil
	})
}

func (w *TrustConvergenceWorker) markCloseApplied(ctx context.Context, job trustConvergenceJob) error {
	return w.withTrust(ctx, func(store enrollmentstore.TrustStore) error {
		changed, err := store.MarkCloseApplied(ctx, job.TrustJob, w.workerID)
		if err != nil {
			return fmt.Errorf("record node close convergence: %w", err)
		}
		if !changed {
			return errors.New("record node close convergence changed no row")
		}
		return nil
	})
}

func (w *TrustConvergenceWorker) unlockComplete(ctx context.Context, job trustConvergenceJob) error {
	return w.withTrust(ctx, func(store enrollmentstore.TrustStore) error {
		return store.UnlockComplete(ctx, job.TrustJob, w.workerID)
	})
}

func (w *TrustConvergenceWorker) release(ctx context.Context, job trustConvergenceJob, cause error) error {
	delay := time.Duration(1<<min(job.Attempts, 6)) * time.Second
	detail := cause.Error()
	if len(detail) > 512 {
		detail = detail[:512]
	}
	err := w.withTrust(ctx, func(store enrollmentstore.TrustStore) error {
		return store.Release(ctx, job.TrustJob, w.workerID, delay, detail)
	})
	if err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func (w *TrustConvergenceWorker) withTrust(ctx context.Context, action func(enrollmentstore.TrustStore) error) error {
	return database.Within(ctx, w.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := enrollmentstore.Trust(tx)
		if err != nil {
			return err
		}
		return action(store)
	})
}
