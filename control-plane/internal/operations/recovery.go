package operations

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandlimit"
	"github.com/GentleKingson/ocservia/control-plane/internal/connectionowner"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

const (
	DefaultReconnectRecoveryLimit  = 16
	maxReconnectRecoveryLimit      = 64
	maxReconnectRecoveryCandidates = 500
	recoveryCommandTTL             = 5 * time.Minute
)

// OwnerReconnect identifies the newly active node connection session epoch.
type OwnerReconnect struct {
	NodeID       uuid.UUID
	ConnectionID [16]byte
	SessionEpoch uint64
	ObservedAt   time.Time
	Limit        int
}

// RecoverAmbiguousDispatched joins the caller's transaction. The session row
// stays share-locked through all projections and the outbox update.
func (s *Service) RecoverAmbiguousDispatched(ctx context.Context, tx database.Tx, reconnect OwnerReconnect) (int, error) {
	if s.signer == nil {
		return 0, errors.New("operations: reconciliation signer is unavailable")
	}
	if reconnect.NodeID == uuid.Nil || reconnect.NodeID.Version() != 7 || reconnect.SessionEpoch == 0 || reconnect.SessionEpoch > math.MaxInt64 || reconnect.ObservedAt.IsZero() || reconnect.Limit < 1 || reconnect.Limit > maxReconnectRecoveryLimit {
		return 0, ErrInvalidRequest
	}
	connectionID, err := uuid.FromBytes(reconnect.ConnectionID[:])
	if err != nil || connectionID.Version() != 7 {
		return 0, ErrInvalidRequest
	}
	if err := commandlimit.Lock(ctx, tx); err != nil {
		return 0, fmt.Errorf("serialize reconnect recovery: %w", err)
	}
	store := operationstore.FromTransaction(tx)
	err = store.ReconnectAuthority(ctx, reconnect.NodeID, connectionID, int64(reconnect.SessionEpoch))
	if errors.Is(err, database.ErrNotFound) {
		return 0, connectionowner.ErrNotOwner
	}
	if err != nil {
		return 0, fmt.Errorf("guard reconnect recovery authority: %w", err)
	}
	candidates, err := store.AmbiguousDispatches(ctx, reconnect.NodeID, maxReconnectRecoveryCandidates)
	if err != nil {
		return 0, fmt.Errorf("select ambiguous dispatched commands: %w", err)
	}
	at := reconnect.ObservedAt.UTC()
	recovered := 0
	for _, commandID := range candidates {
		if recovered == reconnect.Limit {
			break
		}
		candidate, err := store.LockAmbiguousDispatch(ctx, commandID, reconnect.NodeID)
		if errors.Is(err, database.ErrNotFound) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("recheck ambiguous dispatched command: %w", err)
		}
		var envelope agentv1.CommandEnvelope
		if err := proto.Unmarshal(candidate.Envelope, &envelope); err != nil {
			return 0, fmt.Errorf("decode ambiguous dispatched command: %w", err)
		}
		if !bytes.Equal(envelope.GetCommandId(), commandID[:]) || !bytes.Equal(envelope.GetOperationId(), candidate.OperationID[:]) || !bytes.Equal(envelope.GetNodeId(), reconnect.NodeID[:]) {
			return 0, fmt.Errorf("ambiguous dispatched command %s has inconsistent identity", commandID)
		}
		if envelope.GetAgentUpgrade() != nil {
			acked, err := store.UpgradeSchedulingAcked(ctx, candidate.OperationID)
			if err != nil {
				return 0, err
			}
			if acked {
				continue
			}
		}
		deadline := candidate.ExpiresAt
		payload, expiresAt, err := PrepareRecoveryEnvelope(&envelope, agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY, reconnect.ObservedAt, s.signer)
		if err != nil {
			return 0, fmt.Errorf("prepare reconnect reconciliation for command %s: %w", commandID, err)
		}
		if deadline.After(expiresAt) {
			expiresAt = deadline
		}
		if !expiresAt.After(reconnect.ObservedAt) {
			continue
		}
		if err := store.ScheduleReconnectRecovery(ctx, commandID, candidate.OperationID, payload, expiresAt.UTC(), at); err != nil {
			return 0, err
		}
		if err := markRecoveryProjection(ctx, store, candidate.OperationID, &envelope, at); err != nil {
			return 0, err
		}
		recovered++
	}
	return recovered, nil
}

// PrepareRecoveryEnvelope preserves the logical command identity and payload
// while re-signing attempt metadata for RECONCILE_ONLY or RETRY_IF_EFFECT_ABSENT.
func PrepareRecoveryEnvelope(envelope *agentv1.CommandEnvelope, mode agentv1.CommandDeliveryMode, observedAt time.Time, signer *commandauth.Signer) ([]byte, time.Time, error) {
	if envelope == nil || signer == nil || observedAt.IsZero() || (mode != agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY && mode != agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RETRY_IF_EFFECT_ABSENT) {
		return nil, time.Time{}, ErrInvalidRequest
	}
	expires := envelope.GetExpiresAt()
	if expires == nil || expires.CheckValid() != nil {
		return nil, time.Time{}, errors.New("operations: reconciliation command expiry is invalid")
	}
	expiresAt := expires.AsTime()
	if envelope.GetDeliveryMode() == agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_EXECUTE_OR_REPLAY && mode == agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY && expiresAt.Before(observedAt.Add(recoveryCommandTTL)) {
		expiresAt = observedAt.Add(recoveryCommandTTL)
	}
	if mode != agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY && !expiresAt.After(observedAt) {
		return nil, time.Time{}, errors.New("operations: retry command has expired")
	}
	messageID, err := uuid.NewV7()
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("generate reconciliation message ID: %w", err)
	}
	envelope.MessageId = messageID[:]
	envelope.DeliveryMode = mode
	if err := signer.Authorize(envelope); err != nil {
		return nil, time.Time{}, fmt.Errorf("authorize reconciliation command: %w", err)
	}
	payload, err := proto.Marshal(envelope)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("encode reconciliation command: %w", err)
	}
	return payload, expiresAt, nil
}

func decodeClaimedEnvelope(dispatch Dispatch) (*agentv1.CommandEnvelope, error) {
	if len(dispatch.Envelope) == 0 || len(dispatch.Envelope) > 1<<20 {
		return nil, errors.New("operations: sent command envelope size is invalid")
	}
	var sent agentv1.CommandEnvelope
	if err := proto.Unmarshal(dispatch.Envelope, &sent); err != nil {
		return nil, fmt.Errorf("decode sent command envelope: %w", err)
	}
	if !bytes.Equal(sent.GetCommandId(), dispatch.CommandID[:]) || !bytes.Equal(sent.GetOperationId(), dispatch.OperationID[:]) || !bytes.Equal(sent.GetNodeId(), dispatch.NodeID[:]) {
		return nil, errors.New("operations: sent command envelope identity is inconsistent")
	}
	return &sent, nil
}

func markRecoveryProjection(ctx context.Context, store operationstore.Store, operationID uuid.UUID, envelope *agentv1.CommandEnvelope, at time.Time) error {
	if envelope.GetAgentUpgrade() != nil {
		acked, err := store.UpgradeSchedulingAcked(ctx, operationID)
		if err != nil {
			return err
		}
		if acked {
			return nil
		}
	}
	projection := operationstore.RecoveryProjection{OperationID: operationID, ConfigApply: envelope.GetConfigApply() != nil || envelope.GetCompleteConfigApply() != nil, Artifact: envelope.GetCertificateP12() != nil, At: at}
	var certificateID []byte
	if csr := envelope.GetCertificateCsr(); csr != nil {
		certificateID = csr.GetCertificateId()
	} else if revoke := envelope.GetCertificateRevoke(); revoke != nil {
		certificateID = revoke.GetCertificateId()
	}
	if len(certificateID) != 0 {
		id, err := uuid.FromBytes(certificateID)
		if err != nil || id.Version() != 7 {
			return errors.New("operations: reconnect certificate command has invalid identity")
		}
		projection.CertificateID = id
	}
	return store.MarkRecoveryProjection(ctx, projection)
}
