package enrollment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/ownersession"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type TrustTransport interface {
	UpdateNodeTrust(context.Context, []byte, []byte, transportv1.NodeTrustState, string, uint64, *agentv1.FenceBindingV2) error
	CloseNode(context.Context, []byte, string, *agentv1.FenceBindingV2) error
}

type TrustConvergenceWorker struct {
	pool      *pgxpool.Pool
	transport TrustTransport
	fences    ownersession.OperationBinder
	logger    *slog.Logger
	workerID  uuid.UUID
}

type trustConvergenceJob struct {
	NodeID        uuid.UUID
	EndpointID    []byte
	State         transportv1.NodeTrustState
	Revision      uint64
	Reason        string
	UpdateApplied bool
	CloseRequired bool
	CloseApplied  bool
	Attempts      int
}

func NewTrustConvergenceWorker(pool *pgxpool.Pool, transport TrustTransport, logger *slog.Logger) (*TrustConvergenceWorker, error) {
	workerID, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	return &TrustConvergenceWorker{pool: pool, transport: transport, logger: logger, workerID: workerID}, nil
}

// NewFencedTrustConvergenceWorker signs owner-fence bindings for trust
// updates and connection closes so a stale owner cannot drive connection
// state. Nodes without a registered fence keep the unfenced compatibility
// path.
func NewFencedTrustConvergenceWorker(pool *pgxpool.Pool, transport TrustTransport, fences ownersession.OperationBinder, logger *slog.Logger) (*TrustConvergenceWorker, error) {
	worker, err := NewTrustConvergenceWorker(pool, transport, logger)
	if err != nil {
		return nil, err
	}
	worker.fences = fences
	return worker, nil
}

// operationBinding signs one administrative fence binding. Trust updates and
// connection closes are Controller-side operations, so their binding
// capability is the fencing capability itself, which every fence carries by
// construction.
func (w *TrustConvergenceWorker) operationBinding(ctx context.Context, nodeID uuid.UUID, kind agentv1.FenceOperationKind) (*agentv1.FenceBindingV2, error) {
	if w.fences == nil {
		return nil, nil
	}
	var fixed [16]byte
	copy(fixed[:], nodeID[:])
	_, binding, err := w.fences.BindOperation(ctx, fixed, kind, fixed, ownersession.FencingCapability)
	if errors.Is(err, ownersession.ErrNoFence) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return binding, nil
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
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !job.UpdateApplied {
		updateBinding, err := w.operationBinding(ctx, job.NodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_STATE_UPDATE)
		if err != nil {
			return true, w.release(ctx, job, err)
		}
		if err := w.transport.UpdateNodeTrust(ctx, job.NodeID[:], job.EndpointID, job.State, job.Reason, job.Revision, updateBinding); err != nil {
			return true, w.release(ctx, job, err)
		}
		if err := w.markUpdateApplied(ctx, job); err != nil {
			return true, err
		}
		job.UpdateApplied = true
	}
	if job.CloseRequired && !job.CloseApplied {
		closeBinding, err := w.operationBinding(ctx, job.NodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_CONNECTION_CLOSE)
		if err != nil {
			return true, w.release(ctx, job, err)
		}
		if err := w.transport.CloseNode(ctx, job.NodeID[:], "node revoked", closeBinding); err != nil {
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
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return trustConvergenceJob{}, err
	}
	defer rollback(tx)
	var job trustConvergenceJob
	var state string
	err = tx.QueryRow(ctx, `SELECT node_id,endpoint_id,desired_state,revision,reason,update_applied,close_required,close_applied,attempts
		FROM node_trust_convergence
		WHERE (NOT update_applied OR (close_required AND NOT close_applied))
		  AND available_at<=now() AND (locked_until IS NULL OR locked_until<now())
		ORDER BY available_at,node_id FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(
		&job.NodeID, &job.EndpointID, &state, &job.Revision, &job.Reason, &job.UpdateApplied,
		&job.CloseRequired, &job.CloseApplied, &job.Attempts)
	if err != nil {
		return trustConvergenceJob{}, err
	}
	if state == "active" {
		job.State = transportv1.NodeTrustState_NODE_TRUST_STATE_ACTIVE
	} else if state == "revoked" {
		job.State = transportv1.NodeTrustState_NODE_TRUST_STATE_REVOKED
	} else {
		return trustConvergenceJob{}, errors.New("stored trust convergence state is invalid")
	}
	result, err := tx.Exec(ctx, `UPDATE node_trust_convergence SET locked_by=$2,locked_until=now()+interval '10 seconds',attempts=attempts+1,updated_at=now()
		WHERE node_id=$1 AND revision=$3`, job.NodeID, w.workerID, job.Revision)
	if err != nil {
		return trustConvergenceJob{}, fmt.Errorf("claim trust convergence: %w", err)
	}
	if result.RowsAffected() != 1 {
		return trustConvergenceJob{}, errors.New("claim trust convergence changed no row")
	}
	job.Attempts++
	if err := tx.Commit(ctx); err != nil {
		return trustConvergenceJob{}, err
	}
	return job, nil
}

func (w *TrustConvergenceWorker) markUpdateApplied(ctx context.Context, job trustConvergenceJob) error {
	result, err := w.pool.Exec(ctx, `UPDATE node_trust_convergence SET update_applied=true,locked_until=now()+interval '10 seconds',last_error=NULL,updated_at=now()
		WHERE node_id=$1 AND revision=$2 AND locked_by=$3`, job.NodeID, job.Revision, w.workerID)
	if err != nil {
		return fmt.Errorf("record trust update convergence: %w", err)
	}
	if result.RowsAffected() != 1 {
		return errors.New("record trust update convergence changed no row")
	}
	return nil
}

func (w *TrustConvergenceWorker) markCloseApplied(ctx context.Context, job trustConvergenceJob) error {
	result, err := w.pool.Exec(ctx, `UPDATE node_trust_convergence SET close_applied=true,locked_by=NULL,locked_until=NULL,last_error=NULL,updated_at=now()
		WHERE node_id=$1 AND revision=$2 AND locked_by=$3`, job.NodeID, job.Revision, w.workerID)
	if err != nil {
		return fmt.Errorf("record node close convergence: %w", err)
	}
	if result.RowsAffected() != 1 {
		return errors.New("record node close convergence changed no row")
	}
	return nil
}

func (w *TrustConvergenceWorker) unlockComplete(ctx context.Context, job trustConvergenceJob) error {
	_, err := w.pool.Exec(ctx, `UPDATE node_trust_convergence SET locked_by=NULL,locked_until=NULL,last_error=NULL,updated_at=now()
		WHERE node_id=$1 AND revision=$2 AND locked_by=$3`, job.NodeID, job.Revision, w.workerID)
	return err
}

func (w *TrustConvergenceWorker) release(ctx context.Context, job trustConvergenceJob, cause error) error {
	delay := time.Duration(1<<min(job.Attempts, 6)) * time.Second
	detail := cause.Error()
	if len(detail) > 512 {
		detail = detail[:512]
	}
	_, err := w.pool.Exec(ctx, `UPDATE node_trust_convergence SET locked_by=NULL,locked_until=NULL,available_at=now()+$4::interval,last_error=$5,updated_at=now()
		WHERE node_id=$1 AND revision=$2 AND locked_by=$3`, job.NodeID, job.Revision, w.workerID, delay.String(), detail)
	if err != nil {
		return errors.Join(cause, err)
	}
	return cause
}
