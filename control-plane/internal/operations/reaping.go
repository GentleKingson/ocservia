package operations

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandlimit"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (s *Service) Reap(ctx context.Context, maxAttempts int) error {
	if maxAttempts < 1 {
		return ErrInvalidRequest
	}
	return database.WithinRetry(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		if err := commandlimit.Lock(ctx, tx); err != nil {
			return fmt.Errorf("serialize dispatch lease reaping: %w", err)
		}
		store, err := operationstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		if err := s.reconcileExpiredSending(ctx, store, maxAttempts); err != nil {
			return err
		}
		if err := s.reconcileExpiredApplies(ctx, store); err != nil {
			return err
		}
		if err := s.reconcileMissingResults(ctx, store); err != nil {
			return err
		}
		if err := s.continueReconciliations(ctx, store); err != nil {
			return err
		}
		return store.DeleteExpiredLeases(ctx)
	})
}

func recoveryEnvelope(command operationstore.RecoveryCommand) (*agentv1.CommandEnvelope, error) {
	var envelope agentv1.CommandEnvelope
	if err := proto.Unmarshal(command.Envelope, &envelope); err != nil {
		return nil, fmt.Errorf("decode recovery command %s: %w", command.CommandID, err)
	}
	if !bytes.Equal(envelope.GetCommandId(), command.CommandID[:]) || !bytes.Equal(envelope.GetOperationId(), command.OperationID[:]) || !bytes.Equal(envelope.GetNodeId(), command.NodeID[:]) {
		return nil, fmt.Errorf("recovery command %s has inconsistent identity", command.CommandID)
	}
	if envelope.GetExpiresAt() == nil || envelope.GetExpiresAt().CheckValid() != nil {
		return nil, fmt.Errorf("recovery command %s has invalid expiry", command.CommandID)
	}
	return &envelope, nil
}

func recoveryTime(ctx context.Context, store operationstore.Store) (value.Timestamp, time.Time, error) {
	at, err := store.RecoveryClock(ctx)
	if err != nil {
		return at, time.Time{}, err
	}
	clock, err := at.Time()
	return at, clock, err
}

// A sending lease can expire before or after the network write. Its outcome
// is always Unknown; automatic follow-up may only observe the Agent journal.
func (s *Service) reconcileExpiredSending(ctx context.Context, store operationstore.Store, maxAttempts int) error {
	ids, err := store.ExpiredSendingCommands(ctx, reconciliationBatchLimit)
	if err != nil || len(ids) == 0 {
		return err
	}
	if s.signer == nil {
		return errors.New("operations: reconciliation signer is unavailable")
	}
	at, now, err := recoveryTime(ctx, store)
	if err != nil {
		return err
	}
	for _, id := range ids {
		command, err := store.LockExpiredSendingCommand(ctx, id)
		if errors.Is(err, database.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		envelope, err := recoveryEnvelope(command)
		if err != nil {
			return err
		}
		update := operationstore.RecoveryUpdate{Command: command, Envelope: command.Envelope, At: at, Schedule: command.CommandState == "queued" || command.Attempts < maxAttempts}
		expires := envelope.GetExpiresAt().AsTime()
		if update.Schedule {
			if envelope.GetDeliveryMode() != agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY {
				update.Envelope, expires, err = PrepareRecoveryEnvelope(envelope, agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY, now, s.signer)
			} else {
				update.Envelope, expires, err = prepareRecoveryContinuationEnvelope(envelope, now, s.signer)
			}
			if err != nil {
				return err
			}
		}
		update.ExpiresAt, err = value.FromTime(expires)
		if err != nil {
			return err
		}
		changed, err := store.ReconcileExpiredSending(ctx, update)
		if err != nil {
			return err
		}
		if changed && command.CommandState == "queued" {
			if err := markRecoveryProjection(ctx, store, command.OperationID, envelope, at); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) reconcileExpiredApplies(ctx context.Context, store operationstore.Store) error {
	commands, err := store.ExpiredApplyCommands(ctx)
	if err != nil || len(commands) == 0 {
		return err
	}
	if s.signer == nil {
		return errors.New("operations: reconciliation signer is unavailable")
	}
	for _, command := range commands {
		envelope, err := recoveryEnvelope(command)
		if err != nil {
			return err
		}
		at, now, err := recoveryTime(ctx, store)
		if err != nil {
			return err
		}
		envelope.ExpiresAt = timestamppb.New(now.Add(recoveryCommandTTL))
		payload, expires, err := PrepareRecoveryEnvelope(envelope, agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY, now, s.signer)
		if err != nil {
			return err
		}
		expiry, err := value.FromTime(expires)
		if err != nil {
			return err
		}
		if err := store.ReconcileExpiredApply(ctx, operationstore.RecoveryUpdate{Command: command, Envelope: payload, ExpiresAt: expiry, At: at, Schedule: true}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) reconcileMissingResults(ctx context.Context, store operationstore.Store) error {
	ids, err := store.StaleSentCommands(ctx, commandResultResponseTimeout, reconciliationBatchLimit)
	if err != nil || len(ids) == 0 {
		return err
	}
	at, now, err := recoveryTime(ctx, store)
	if err != nil {
		return err
	}
	for _, id := range ids {
		command, err := store.LockStaleSentCommand(ctx, id, commandResultResponseTimeout)
		if errors.Is(err, database.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		envelope, err := recoveryEnvelope(command)
		if err != nil {
			return err
		}
		if envelope.GetAgentUpgrade() != nil {
			acked, err := store.UpgradeSchedulingAcked(ctx, command.OperationID)
			if err != nil {
				return err
			}
			if acked {
				continue
			}
		}
		update := operationstore.RecoveryUpdate{Command: command, Envelope: command.Envelope, At: at, Schedule: command.Attempts < reconciliationAttemptLimit}
		expires := envelope.GetExpiresAt().AsTime()
		if update.Schedule {
			if s.signer == nil {
				return errors.New("operations: reconciliation signer is unavailable")
			}
			priorMessageID := hex.EncodeToString(envelope.GetMessageId())
			update.Envelope, expires, err = PrepareRecoveryEnvelope(envelope, agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY, now, s.signer)
			if err != nil {
				return err
			}
			slog.InfoContext(ctx, "missing-result reconciliation prepared; transaction not yet committed",
				"event_type", "command_reconciliation_prepared", "reason", "sent_result_timeout",
				"command_id", id, "operation_id", command.OperationID, "node_id", command.NodeID,
				"outbox_id", command.OutboxID, "prior_attempt", command.Attempts,
				"prior_message_id", priorMessageID, "message_id", hex.EncodeToString(envelope.GetMessageId()),
				"semantic_payload_sha256", hex.EncodeToString(envelope.GetSemanticPayloadSha256()))
		}
		update.ExpiresAt, err = value.FromTime(expires)
		if err != nil {
			return err
		}
		changed, err := store.ReconcileMissingResult(ctx, update)
		if err != nil {
			return err
		}
		if changed {
			if err := markRecoveryProjection(ctx, store, command.OperationID, envelope, at); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) continueReconciliations(ctx context.Context, store operationstore.Store) error {
	commands, err := store.StaleReconciliations(ctx, reconciliationAttemptLimit, commandResultResponseTimeout, reconciliationCandidateScanLimit)
	if err != nil || len(commands) == 0 {
		return err
	}
	if s.signer == nil {
		return errors.New("operations: reconciliation signer is unavailable")
	}
	at, now, err := recoveryTime(ctx, store)
	if err != nil {
		return err
	}
	continued := 0
	for _, command := range commands {
		if continued == reconciliationBatchLimit {
			break
		}
		envelope, err := recoveryEnvelope(command)
		if err != nil {
			return err
		}
		if envelope.GetAgentUpgrade() != nil {
			acked, err := store.UpgradeSchedulingAcked(ctx, command.OperationID)
			if err != nil {
				return err
			}
			if acked {
				continue
			}
		}
		if envelope.GetDeliveryMode() != agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY {
			continue
		}
		payload, _, err := prepareRecoveryContinuationEnvelope(envelope, now, s.signer)
		if err != nil {
			return err
		}
		changed, err := store.ContinueReconciliation(ctx, operationstore.RecoveryUpdate{Command: command, Envelope: payload, At: at, Schedule: true})
		if err != nil {
			return err
		}
		if changed {
			continued++
		}
	}
	return nil
}
