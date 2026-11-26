package operations

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	"github.com/google/uuid"
)

func (s *Service) Claim(ctx context.Context, workerID uuid.UUID, limit int, lease time.Duration) ([]Dispatch, error) {
	if workerID == uuid.Nil || limit < 1 || limit > 100 || lease <= 0 {
		return nil, ErrInvalidRequest
	}
	var claimed []Dispatch
	err := database.WithinRetry(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		claimed = nil
		available, err := operationstore.AvailableCommandSlots(ctx, tx, s.commandLimit)
		if err != nil {
			return fmt.Errorf("reserve global dispatch capacity: %w", err)
		}
		if available == 0 {
			claimed = []Dispatch{}
			return nil
		}
		store := operationstore.FromTransaction(tx)
		at, err := tx.WallTime(ctx)
		if err != nil {
			return err
		}
		until := at.Add(lease)
		candidates, err := store.DispatchCandidates(ctx, limit, available, at)
		if err != nil {
			return fmt.Errorf("select outbox candidates: %w", err)
		}
		claimed = make([]Dispatch, 0, len(candidates))
		for _, d := range candidates {
			d.AttemptID, d.LeaseToken, err = twoIDs()
			if err != nil {
				return err
			}
			ok, err := store.ClaimDispatch(ctx, d, workerID, until, at)
			if err != nil {
				return fmt.Errorf("claim dispatch: %w", err)
			}
			if ok {
				claimed = append(claimed, d)
			}
		}
		return nil
	})
	if errors.Is(err, database.ErrCommitUnknown) && len(claimed) != 0 {
		// Read back this exact attempt/lease batch, not a new claim. If any
		// evidence is missing or expired, send nothing and let reaping resolve it.
		check, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if confirmed := database.Within(check, s.backend, database.ReadCommitted, func(tx database.Tx) error {
			if err := operationstore.LockCommandAdmission(check, tx); err != nil {
				return err
			}
			store := operationstore.FromTransaction(tx)
			for _, d := range claimed {
				if err := store.LockDispatchOutbox(check, d); err != nil {
					return err
				}
				status, err := store.DispatchStatus(check, d)
				if err != nil {
					return err
				}
				if !status.LeaseValid || !status.AttemptValid || !status.OwnsOutboxLock || status.ResultAfterAttempt || (status.CommandState != "queued" && status.CommandState != "unknown") {
					return database.ErrNotFound
				}
			}
			return nil
		}); confirmed == nil {
			err = nil
		}
	}
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

// MarkSent records transport acceptance of the envelope already stored by the outbox.
func (s *Service) MarkSent(ctx context.Context, dispatch Dispatch) error {
	envelope, err := decodeClaimedEnvelope(dispatch)
	if err != nil {
		return err
	}
	return s.markSent(ctx, dispatch, envelope)
}

func (s *Service) MarkFailed(ctx context.Context, dispatch Dispatch, cause error) error {
	message := "transport unavailable"
	if cause != nil {
		message = cause.Error()
	}
	if len(message) > 512 {
		message = message[:512]
	}
	return s.markFailed(ctx, dispatch, message)
}

func (s *Service) withinDispatchTx(ctx context.Context, fn func(database.Tx, operationstore.Store) error) error {
	return database.WithinRetry(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		if err := operationstore.LockCommandAdmission(ctx, tx); err != nil {
			return fmt.Errorf("serialize dispatch completion: %w", err)
		}
		return fn(tx, operationstore.FromTransaction(tx))
	})
}

func (s *Service) markFailed(ctx context.Context, d Dispatch, message string) error {
	return s.withinDispatchTx(ctx, func(tx database.Tx, store operationstore.Store) error {
		if err := store.LockDispatchOutbox(ctx, d); err != nil {
			return err
		}
		status, err := store.DispatchStatus(ctx, d)
		if err != nil {
			return err
		}
		if !status.LeaseValid || !status.AttemptValid || !status.OwnsOutboxLock || status.ResultAfterAttempt {
			return database.ErrNotFound
		}
		at, err := tx.WallTime(ctx)
		if err != nil {
			return err
		}
		return store.FailDispatch(ctx, d, message, at)
	})
}

func (s *Service) markSent(ctx context.Context, d Dispatch, envelope *agentv1.CommandEnvelope) error {
	err := s.withinDispatchTx(ctx, func(tx database.Tx, store operationstore.Store) error {
		// Take this lock in its own statement, so the following read observes a
		// result transaction that committed while this statement was waiting.
		if err := store.LockDispatchOutbox(ctx, d); err != nil {
			return fmt.Errorf("lock dispatch completion: %w", err)
		}
		status, err := store.DispatchStatus(ctx, d)
		if errors.Is(err, database.ErrNotFound) {
			status, err = store.CompletedDispatch(ctx, d)
			if err != nil {
				return fmt.Errorf("confirm result-completed dispatch: %w", err)
			}
			if !status.ResultAfterAttempt {
				if bytes.Equal(status.Envelope, d.Envelope) {
					return nil
				}
				return errors.New("dispatch lease is no longer valid")
			}
			return validateResultDispatchState(status.CommandState)
		}
		if err != nil {
			return err
		}
		if !status.LeaseValid || !status.AttemptValid {
			return errors.New("dispatch lease is no longer valid")
		}
		if !status.OwnsOutboxLock && !status.ResultAfterAttempt {
			return errors.New("dispatch outbox lock is no longer valid")
		}
		at, err := tx.WallTime(ctx)
		if err != nil {
			return err
		}
		if status.ResultAfterAttempt {
			if err := validateResultDispatchState(status.CommandState); err != nil {
				return err
			}
		}
		if status.OwnsOutboxLock {
			if err := store.PublishDispatch(ctx, d, at); err != nil {
				return err
			}
		}
		if !status.ResultAfterAttempt {
			if status.CommandState != "queued" && status.CommandState != "unknown" {
				return fmt.Errorf("dispatch command is no longer mutable from %s", status.CommandState)
			}
			unknown := status.CommandState == "unknown" && envelope.GetDeliveryMode() == agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY
			if err := store.RecordDispatched(ctx, d, unknown, at); err != nil {
				return err
			}
		}
		return store.CloseDispatch(ctx, d, at)
	})
	if errors.Is(err, database.ErrCommitUnknown) {
		check, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if confirmed := database.Within(check, s.backend, database.ReadCommitted, func(tx database.Tx) error {
			status, err := operationstore.FromTransaction(tx).CompletedDispatch(check, d)
			if err != nil {
				return err
			}
			if !bytes.Equal(status.Envelope, d.Envelope) {
				return database.ErrNotFound
			}
			return nil
		}); confirmed == nil {
			return nil
		}
	}
	return err
}

func validateResultDispatchState(state string) error {
	switch state {
	case "accepted", "succeeded", "failed", "rejected", "rolled_back", "unknown":
		return nil
	default:
		return fmt.Errorf("agent result did not advance command from %s", state)
	}
}

// Metrics reports controller-wide queue totals for unscoped callers.
func (s *Service) Metrics(ctx context.Context) (QueueMetrics, error) {
	return s.queueMetrics(ctx, uuid.Nil, true)
}

// WorkspaceMetrics reports the same totals restricted to one workspace.
func (s *Service) WorkspaceMetrics(ctx context.Context, workspaceID uuid.UUID) (QueueMetrics, error) {
	return s.queueMetrics(ctx, workspaceID, false)
}

func (s *Service) queueMetrics(ctx context.Context, workspaceID uuid.UUID, all bool) (m QueueMetrics, err error) {
	err = database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		m, err = operationstore.FromTransaction(tx).QueueMetrics(ctx, workspaceID, all)
		return err
	})
	return
}
