package enrollment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	enrollmentstore "github.com/GentleKingson/ocservia/control-plane/internal/enrollment/store"
	"github.com/google/uuid"
)

type TrustTransport interface {
	UpdateNodeTrust(context.Context, []byte, []byte, transportv1.NodeTrustState, string, uint64) error
	CloseNode(context.Context, []byte, string) error
}

type TrustConvergenceWorker struct {
	backend   database.Backend
	transport TrustTransport
	logger    *slog.Logger
	workerID  uuid.UUID
}

type trustConvergenceJob struct {
	enrollmentstore.TrustJob
	State transportv1.NodeTrustState
}

func NewTrustConvergenceWorker(backend database.Backend, transport TrustTransport, logger *slog.Logger) (*TrustConvergenceWorker, error) {
	workerID, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	return &TrustConvergenceWorker{backend: backend, transport: transport, logger: logger, workerID: workerID}, nil
}

// executeLeased renews the convergence claim only while the transport RPC is in flight.
func (w *TrustConvergenceWorker) executeLeased(ctx context.Context, job trustConvergenceJob, action func(context.Context) error) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	if err := w.renew(ctx, job); err != nil {
		return err
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(enrollmentstore.TrustLeaseTTL / 3)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := w.renew(ctx, job); err != nil {
					cancel(err)
					return
				}
			}
		}
	}()
	err := action(ctx)
	close(stop)
	<-done
	return errors.Join(err, context.Cause(ctx))
}

func (w *TrustConvergenceWorker) renew(ctx context.Context, job trustConvergenceJob) error {
	ctx, cancel := context.WithTimeout(ctx, enrollmentstore.TrustLeaseTTL/3)
	defer cancel()
	return w.withTrust(ctx, func(store enrollmentstore.TrustStore) error {
		changed, err := store.Renew(ctx, job.TrustJob, w.workerID)
		if err != nil {
			return err
		}
		if !changed {
			return enrollmentstore.ErrTrustLeaseLost
		}
		return nil
	})
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
		err := w.executeLeased(ctx, job, func(ctx context.Context) error {
			return w.transport.UpdateNodeTrust(ctx, job.NodeID[:], job.EndpointID, job.State, job.Reason, job.Revision)
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
		err := w.executeLeased(ctx, job, func(ctx context.Context) error {
			return w.transport.CloseNode(ctx, job.NodeID[:], "node revoked")
		})
		if err != nil {
			return true, w.release(ctx, job, err)
		}
		if err := w.markCloseApplied(ctx, job); err != nil {
			return true, err
		}
		return true, nil
	}
	if err := w.withTrust(ctx, func(store enrollmentstore.TrustStore) error {
		return store.UnlockComplete(ctx, job.TrustJob, w.workerID)
	}); err != nil {
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
		return action(enrollmentstore.Trust(tx))
	})
}
