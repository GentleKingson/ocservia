package operations

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandlimit"
	"github.com/GentleKingson/ocservia/control-plane/internal/connectionowner"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

func (s *Service) Claim(ctx context.Context, workerID uuid.UUID, limit int, lease time.Duration) ([]Dispatch, error) {
	if workerID == uuid.Nil || limit < 1 || limit > 100 || lease <= 0 {
		return nil, ErrInvalidRequest
	}
	var claimed []Dispatch
	err := database.WithinRetry(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		claimed = nil
		available, err := commandlimit.Available(ctx, tx, s.commandLimit)
		if err != nil {
			return fmt.Errorf("reserve global dispatch capacity: %w", err)
		}
		store, err := operationstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		at, err := database.WallTime(ctx, tx)
		if err != nil {
			return err
		}
		until, err := at.Add(lease)
		if err != nil {
			return err
		}
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
			if err := commandlimit.Lock(check, tx); err != nil {
				return err
			}
			store, err := operationstore.FromTransaction(tx)
			if err != nil {
				return err
			}
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

func (s *Service) MarkSent(ctx context.Context, dispatch Dispatch) error {
	return s.MarkSentWithEnvelope(ctx, dispatch, dispatch.Envelope)
}

// MarkSentWithEnvelope persists the exact frame accepted by transport, including
// the owner proofs added after Claim. Reconnect recovery needs that precise term.
func (s *Service) MarkSentWithEnvelope(ctx context.Context, dispatch Dispatch, sentEnvelope []byte) error {
	if err := validateSentEnvelope(dispatch, sentEnvelope); err != nil {
		return err
	}
	return s.finishDispatch(ctx, dispatch, true, "", sentEnvelope)
}

func (s *Service) MarkFailed(ctx context.Context, dispatch Dispatch, cause error) error {
	message := "transport unavailable"
	if cause != nil {
		message = cause.Error()
	}
	if len(message) > 512 {
		message = message[:512]
	}
	return s.finishDispatch(ctx, dispatch, false, message, nil)
}

func (s *Service) finishDispatch(ctx context.Context, d Dispatch, sent bool, message string, sentEnvelope []byte) error {
	var envelope agentv1.CommandEnvelope
	if sent {
		if err := proto.Unmarshal(sentEnvelope, &envelope); err != nil {
			return fmt.Errorf("decode sent command delivery mode: %w", err)
		}
	}
	err := database.WithinRetry(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		if err := commandlimit.Lock(ctx, tx); err != nil {
			return fmt.Errorf("serialize dispatch completion: %w", err)
		}
		store, err := operationstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		if !sent {
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
			at, err := database.WallTime(ctx, tx)
			if err != nil {
				return err
			}
			return store.FailDispatch(ctx, d, message, at)
		}
		// Hold the exact owner term through commit, before locking any outbox.
		if err := guardDispatchAuthority(ctx, store, d, &envelope); err != nil {
			return err
		}
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
				if bytes.Equal(status.Envelope, sentEnvelope) {
					return nil
				}
				return errors.New("dispatch lease is no longer valid")
			}
			return preserveResultEnvelope(ctx, store, d, status.CommandState, sentEnvelope)
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
		at, err := database.WallTime(ctx, tx)
		if err != nil {
			return err
		}
		if status.ResultAfterAttempt {
			if err := preserveResultEnvelope(ctx, store, d, status.CommandState, sentEnvelope); err != nil {
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
			if err := store.RecordDispatched(ctx, d, sentEnvelope, unknown, at); err != nil {
				return err
			}
		}
		return store.CloseDispatch(ctx, d, at)
	})
	if sent && errors.Is(err, database.ErrCommitUnknown) {
		check, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if confirmed := database.Within(check, s.backend, database.ReadCommitted, func(tx database.Tx) error {
			store, err := operationstore.FromTransaction(tx)
			if err != nil {
				return err
			}
			status, err := store.CompletedDispatch(check, d)
			if err != nil {
				return err
			}
			if !bytes.Equal(status.Envelope, sentEnvelope) {
				return database.ErrNotFound
			}
			return nil
		}); confirmed == nil {
			return nil
		}
	}
	return err
}

func preserveResultEnvelope(ctx context.Context, store operationstore.Store, d Dispatch, state string, encoded []byte) error {
	switch state {
	case "succeeded", "failed", "rejected", "rolled_back":
		return store.SaveTerminalEnvelope(ctx, d.CommandID, encoded)
	case "unknown":
		// Result ingestion may have already scheduled a reconcile-only envelope.
		return nil
	default:
		return fmt.Errorf("agent result did not advance command from %s", state)
	}
}

func guardDispatchAuthority(ctx context.Context, store operationstore.Store, d Dispatch, envelope *agentv1.CommandEnvelope) error {
	fence := envelope.GetConnectionFence()
	if fence == nil {
		return nil
	}
	owner, err := uuid.FromBytes(fence.GetOwnerInstanceId())
	if err != nil || owner.Version() != 7 {
		return errors.New("operations: sent command owner instance is not UUIDv7")
	}
	connection, err := uuid.FromBytes(fence.GetConnectionId())
	if err != nil || connection.Version() != 7 {
		return errors.New("operations: sent command connection is not UUIDv7")
	}
	err = store.GuardDispatchAuthority(ctx, operationstore.DispatchAuthority{NodeID: d.NodeID, OwnerID: owner, ConnectionID: connection, Incarnation: int64(fence.GetOwnerIncarnation()), Epoch: int64(fence.GetOwnerEpoch())})
	if errors.Is(err, database.ErrNotFound) {
		return connectionowner.ErrNotOwner
	}
	return err
}

func (s *Service) Metrics(ctx context.Context) (m QueueMetrics, err error) {
	err = database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := operationstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		m, err = store.QueueMetrics(ctx)
		return err
	})
	return
}
