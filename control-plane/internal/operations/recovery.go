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
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	// DefaultReconnectRecoveryLimit bounds the work attached to one newly
	// authoritative connection. A later takeover can continue reconciliation;
	// one reconnect never scans an unbounded command history.
	DefaultReconnectRecoveryLimit = 16
	maxReconnectRecoveryLimit     = 64
	// Admission caps active commands at 500. Scanning that full bounded set
	// prevents current-term Unknown rows from hiding an older ambiguous term
	// before the recovery limit is applied in Go.
	maxReconnectRecoveryCandidates = 500
	recoveryCommandTTL             = 5 * time.Minute
)

// OwnerReconnect identifies the fenced connection that has become usable for
// one node. RecoverAmbiguousDispatchedTx independently checks this term against
// PostgreSQL while holding the caller's transaction open.
type OwnerReconnect struct {
	NodeID       uuid.UUID
	ConnectionID [16]byte
	OwnerEpoch   uint64
	ObservedAt   time.Time
	Limit        int
}

type recoveryAuthority struct {
	instanceID  uuid.UUID
	incarnation int64
	connection  [16]byte
	epoch       uint64
}

type recoveryCandidate struct {
	commandID uuid.UUID
}

// RecoverAmbiguousDispatchedTx converts previously published commands whose
// result never reached Controller into reconcile-only work. The authority row
// is held FOR SHARE through every state and outbox update, so a concurrent
// takeover cannot make a stale connected event authoritative midway through
// the sweep.
func (s *Service) RecoverAmbiguousDispatchedTx(ctx context.Context, tx pgx.Tx, reconnect OwnerReconnect) (int, error) {
	if s.signer == nil {
		return 0, errors.New("operations: reconciliation signer is unavailable")
	}
	if reconnect.NodeID == uuid.Nil || reconnect.NodeID.Version() != 7 || reconnect.OwnerEpoch == 0 || reconnect.OwnerEpoch > math.MaxInt64 || reconnect.ObservedAt.IsZero() || reconnect.Limit < 1 || reconnect.Limit > maxReconnectRecoveryLimit {
		return 0, ErrInvalidRequest
	}
	connectionID, err := uuid.FromBytes(reconnect.ConnectionID[:])
	if err != nil || connectionID.Version() != 7 {
		return 0, ErrInvalidRequest
	}
	// Match dispatch completion and lease reaping before taking authority or
	// row locks. Result ingestion does not take this advisory lock, but it uses
	// the same outbox-before-command row-lock order below.
	if err := commandlimit.Lock(ctx, tx); err != nil {
		return 0, fmt.Errorf("serialize reconnect recovery: %w", err)
	}

	authority := recoveryAuthority{connection: reconnect.ConnectionID, epoch: reconnect.OwnerEpoch}
	err = tx.QueryRow(ctx, `SELECT owner_instance_id,owner_incarnation
		FROM connection_owner_fencing
		WHERE node_id=$1 AND connection_id=$2 AND owner_epoch=$3 AND lease_until>clock_timestamp()
		FOR SHARE OF connection_owner_fencing`, reconnect.NodeID[:], reconnect.ConnectionID[:], int64(reconnect.OwnerEpoch)).
		Scan(&authority.instanceID, &authority.incarnation)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, connectionowner.ErrNotOwner
	}
	if err != nil {
		return 0, fmt.Errorf("guard reconnect recovery authority: %w", err)
	}

	scanLimit := maxReconnectRecoveryCandidates
	rows, err := tx.Query(ctx, `SELECT command.id
		FROM commands AS command
		JOIN operations AS operation ON operation.id=command.operation_id
		JOIN outbox_events AS outbox ON outbox.command_id=command.id
		WHERE command.node_id=$1
		  AND command.state IN ('dispatched','accepted','running','unknown')
		  AND operation.state IN ('dispatched','accepted','running','unknown')
		  AND outbox.published_at IS NOT NULL
		  AND outbox.locked_by IS NULL
		  AND NOT EXISTS(SELECT 1 FROM node_command_leases AS lease WHERE lease.command_id=command.id)
		ORDER BY command.created_at,command.id
		LIMIT $2
		FOR UPDATE OF outbox SKIP LOCKED`, reconnect.NodeID, scanLimit)
	if err != nil {
		return 0, fmt.Errorf("select ambiguous dispatched commands: %w", err)
	}
	candidates := make([]recoveryCandidate, 0, scanLimit)
	for rows.Next() {
		var candidate recoveryCandidate
		if err := rows.Scan(&candidate.commandID); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan ambiguous dispatched command: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("iterate ambiguous dispatched commands: %w", err)
	}
	rows.Close()

	recovered := 0
	for _, candidate := range candidates {
		if recovered == reconnect.Limit {
			break
		}
		// The outbox row is already locked. Re-read command and operation in a
		// fresh READ COMMITTED statement after any result transaction we waited
		// for, then lock both projections in outbox-to-command order.
		var operationID uuid.UUID
		var encoded []byte
		err := tx.QueryRow(ctx, `SELECT command.operation_id,command.envelope
			FROM commands AS command
			JOIN operations AS operation ON operation.id=command.operation_id
			JOIN outbox_events AS outbox ON outbox.command_id=command.id
			WHERE command.id=$1 AND command.node_id=$2
			  AND command.state IN ('dispatched','accepted','running','unknown')
			  AND operation.state IN ('dispatched','accepted','running','unknown')
			  AND outbox.published_at IS NOT NULL AND outbox.locked_by IS NULL
			  AND NOT EXISTS(SELECT 1 FROM node_command_leases AS lease WHERE lease.command_id=command.id)
			FOR UPDATE OF command,operation`, candidate.commandID, reconnect.NodeID).
			Scan(&operationID, &encoded)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("recheck ambiguous dispatched command %s: %w", candidate.commandID, err)
		}
		var envelope agentv1.CommandEnvelope
		if err := proto.Unmarshal(encoded, &envelope); err != nil {
			return 0, fmt.Errorf("decode ambiguous dispatched command %s: %w", candidate.commandID, err)
		}
		if !bytes.Equal(envelope.GetCommandId(), candidate.commandID[:]) || !bytes.Equal(envelope.GetOperationId(), operationID[:]) || !bytes.Equal(envelope.GetNodeId(), reconnect.NodeID[:]) {
			return 0, fmt.Errorf("ambiguous dispatched command %s has inconsistent identity", candidate.commandID)
		}
		// MarkSentWithEnvelope persists the exact fence actually carried by a
		// successful dispatch. A command already sent on this term is not an
		// outage ambiguity, even when its result is still in flight.
		if dispatchedOnAuthority(&envelope, candidate.commandID, reconnect.NodeID, authority) {
			continue
		}

		payload, expiresAt, err := PrepareRecoveryEnvelope(&envelope, agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY, reconnect.ObservedAt, s.signer)
		if err != nil {
			return 0, fmt.Errorf("prepare reconnect reconciliation for command %s: %w", candidate.commandID, err)
		}
		commandTag, err := tx.Exec(ctx, `UPDATE commands
			SET state='unknown',envelope=$2,expires_at=$3,updated_at=$4
			WHERE id=$1 AND state IN ('dispatched','accepted','running','unknown')`, candidate.commandID, payload, expiresAt, reconnect.ObservedAt)
		if err != nil {
			return 0, fmt.Errorf("mark reconnect command unknown: %w", err)
		}
		if commandTag.RowsAffected() != 1 {
			continue
		}
		operationTag, err := tx.Exec(ctx, `UPDATE operations
			SET state='unknown',version=version+1,expires_at=$2,updated_at=$3,completed_at=NULL
			WHERE id=$1 AND state IN ('dispatched','accepted','running','unknown')`, operationID, expiresAt, reconnect.ObservedAt)
		if err != nil {
			return 0, fmt.Errorf("mark reconnect operation unknown: %w", err)
		}
		if operationTag.RowsAffected() != 1 {
			return 0, fmt.Errorf("reconnect command %s has no mutable operation", candidate.commandID)
		}
		if err := markRecoveryProjectionUnknown(ctx, tx, operationID, &envelope, reconnect.ObservedAt); err != nil {
			return 0, err
		}
		outboxTag, err := tx.Exec(ctx, `UPDATE outbox_events
			SET payload=$2,published_at=NULL,locked_by=NULL,locked_until=NULL,available_at=$3,
				last_error='owner connection changed; reconciliation required'
			WHERE command_id=$1 AND published_at IS NOT NULL AND locked_by IS NULL`, candidate.commandID, payload, reconnect.ObservedAt)
		if err != nil {
			return 0, fmt.Errorf("schedule reconnect reconciliation: %w", err)
		}
		if outboxTag.RowsAffected() != 1 {
			return 0, fmt.Errorf("reconnect command %s lost its published outbox", candidate.commandID)
		}
		eventID, err := uuid.NewV7()
		if err != nil {
			return 0, fmt.Errorf("generate reconnect reconciliation event: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO operation_events(id,operation_id,state,occurred_at)
			VALUES($1,$2,'unknown',$3)`, eventID, operationID, reconnect.ObservedAt); err != nil {
			return 0, fmt.Errorf("record reconnect reconciliation event: %w", err)
		}
		recovered++
	}
	return recovered, nil
}

// PrepareRecoveryEnvelope preserves the logical command identity and payload,
// changes only attempt metadata, and strips the previous connection proofs so
// the outbox worker must bind the next dispatch to its then-current owner term.
func PrepareRecoveryEnvelope(envelope *agentv1.CommandEnvelope, mode agentv1.CommandDeliveryMode, observedAt time.Time, signer *commandauth.Signer) ([]byte, time.Time, error) {
	return prepareRecoveryEnvelope(envelope, mode, observedAt, signer, true)
}

// prepareRecoveryContinuationEnvelope creates a distinct reconcile-only
// transport attempt without extending the logical command deadline. It is
// used only after a sent observation attempt produced no durable result.
func prepareRecoveryContinuationEnvelope(envelope *agentv1.CommandEnvelope, observedAt time.Time, signer *commandauth.Signer) ([]byte, time.Time, error) {
	if envelope == nil || envelope.GetDeliveryMode() != agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY {
		return nil, time.Time{}, ErrInvalidRequest
	}
	return prepareRecoveryEnvelope(envelope, agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY, observedAt, signer, false)
}

func prepareRecoveryEnvelope(envelope *agentv1.CommandEnvelope, mode agentv1.CommandDeliveryMode, observedAt time.Time, signer *commandauth.Signer, extendReconcileTTL bool) ([]byte, time.Time, error) {
	if envelope == nil || signer == nil || observedAt.IsZero() || (mode != agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY && mode != agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RETRY_IF_EFFECT_ABSENT) {
		return nil, time.Time{}, ErrInvalidRequest
	}
	expires := envelope.GetExpiresAt()
	if expires == nil || expires.CheckValid() != nil {
		return nil, time.Time{}, errors.New("operations: reconciliation command expiry is invalid")
	}
	expiresAt := expires.AsTime()
	if extendReconcileTTL && mode == agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY && expiresAt.Before(observedAt.Add(recoveryCommandTTL)) {
		expiresAt = observedAt.Add(recoveryCommandTTL)
		envelope.ExpiresAt = timestamppb.New(expiresAt)
	}
	if !expiresAt.After(observedAt) {
		return nil, time.Time{}, errors.New("operations: retry command has expired")
	}
	messageID, err := uuid.NewV7()
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("generate reconciliation message ID: %w", err)
	}
	envelope.MessageId = messageID[:]
	envelope.DeliveryMode = mode
	envelope.ConnectionFence = nil
	envelope.FenceBinding = nil
	if err := signer.Authorize(envelope); err != nil {
		return nil, time.Time{}, fmt.Errorf("authorize reconciliation command: %w", err)
	}
	payload, err := proto.Marshal(envelope)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("encode reconciliation command: %w", err)
	}
	return payload, expiresAt, nil
}

func validateSentEnvelope(dispatch Dispatch, encoded []byte) error {
	if len(encoded) == 0 || len(encoded) > 1<<20 {
		return errors.New("operations: sent command envelope size is invalid")
	}
	var claimed, sent agentv1.CommandEnvelope
	if err := proto.Unmarshal(dispatch.Envelope, &claimed); err != nil {
		return fmt.Errorf("decode claimed command envelope: %w", err)
	}
	if err := proto.Unmarshal(encoded, &sent); err != nil {
		return fmt.Errorf("decode sent command envelope: %w", err)
	}
	if !bytes.Equal(sent.GetCommandId(), dispatch.CommandID[:]) || !bytes.Equal(sent.GetOperationId(), dispatch.OperationID[:]) || !bytes.Equal(sent.GetNodeId(), dispatch.NodeID[:]) {
		return errors.New("operations: sent command envelope identity is inconsistent")
	}
	claimed.ConnectionFence, claimed.FenceBinding = nil, nil
	sentFence, sentBinding := sent.GetConnectionFence(), sent.GetFenceBinding()
	sent.ConnectionFence, sent.FenceBinding = nil, nil
	if !proto.Equal(&claimed, &sent) {
		return errors.New("operations: sent command changed fields outside owner proofs")
	}
	if sentFence == nil && sentBinding == nil {
		return nil
	}
	if sentFence == nil || sentBinding == nil || sentBinding.GetOperationKind() != agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND ||
		len(sentFence.GetFenceId()) != 16 || len(sentFence.GetOwnerInstanceId()) != 16 || len(sentFence.GetConnectionId()) != 16 ||
		!bytes.Equal(sentFence.GetFenceId(), sentBinding.GetFenceId()) || !bytes.Equal(sentFence.GetNodeId(), dispatch.NodeID[:]) ||
		!bytes.Equal(sentBinding.GetNodeId(), dispatch.NodeID[:]) || !bytes.Equal(sentBinding.GetOperationId(), dispatch.CommandID[:]) ||
		!bytes.Equal(sentFence.GetOwnerInstanceId(), sentBinding.GetOwnerInstanceId()) || sentFence.GetOwnerIncarnation() != sentBinding.GetOwnerIncarnation() ||
		sentFence.GetOwnerIncarnation() > math.MaxInt64 || sentFence.GetOwnerEpoch() == 0 || sentFence.GetOwnerEpoch() > math.MaxInt64 || sentFence.GetOwnerEpoch() != sentBinding.GetOwnerEpoch() ||
		!bytes.Equal(sentFence.GetConnectionId(), sentBinding.GetConnectionId()) || sentFence.GetAuthorizationRevision() != sentBinding.GetAuthorizationRevision() {
		return errors.New("operations: sent command owner proofs are inconsistent")
	}
	return nil
}

// guardSentEnvelopeAuthority serializes a fenced dispatch commit against a
// concurrent takeover. An unfenced compatibility dispatch has no owner term
// to guard and retains its established behavior.
func guardSentEnvelopeAuthority(ctx context.Context, tx pgx.Tx, dispatch Dispatch, encoded []byte) error {
	var envelope agentv1.CommandEnvelope
	if err := proto.Unmarshal(encoded, &envelope); err != nil {
		return fmt.Errorf("decode sent command envelope for authority guard: %w", err)
	}
	fence := envelope.GetConnectionFence()
	if fence == nil {
		return nil
	}
	ownerID, err := uuid.FromBytes(fence.GetOwnerInstanceId())
	if err != nil || ownerID.Version() != 7 {
		return errors.New("operations: sent command owner instance is not UUIDv7")
	}
	connectionID, err := uuid.FromBytes(fence.GetConnectionId())
	if err != nil || connectionID.Version() != 7 {
		return errors.New("operations: sent command connection is not UUIDv7")
	}
	var one int
	err = tx.QueryRow(ctx, `SELECT 1 FROM connection_owner_fencing
		WHERE node_id=$1 AND owner_instance_id=$2 AND owner_incarnation=$3
		  AND connection_id=$4 AND owner_epoch=$5 AND lease_until>clock_timestamp()
		FOR SHARE OF connection_owner_fencing`,
		dispatch.NodeID[:], ownerID, int64(fence.GetOwnerIncarnation()), fence.GetConnectionId(), int64(fence.GetOwnerEpoch())).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return connectionowner.ErrNotOwner
	}
	if err != nil {
		return fmt.Errorf("guard sent command owner authority: %w", err)
	}
	return nil
}

func dispatchedOnAuthority(envelope *agentv1.CommandEnvelope, commandID, nodeID uuid.UUID, authority recoveryAuthority) bool {
	fence, binding := envelope.GetConnectionFence(), envelope.GetFenceBinding()
	if fence == nil || binding == nil || binding.GetOperationKind() != agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND {
		return false
	}
	return bytes.Equal(fence.GetNodeId(), nodeID[:]) &&
		bytes.Equal(binding.GetNodeId(), nodeID[:]) &&
		bytes.Equal(binding.GetOperationId(), commandID[:]) &&
		bytes.Equal(fence.GetFenceId(), binding.GetFenceId()) &&
		bytes.Equal(fence.GetOwnerInstanceId(), authority.instanceID[:]) &&
		bytes.Equal(binding.GetOwnerInstanceId(), authority.instanceID[:]) &&
		fence.GetOwnerIncarnation() == uint64(authority.incarnation) &&
		binding.GetOwnerIncarnation() == uint64(authority.incarnation) &&
		fence.GetOwnerEpoch() == authority.epoch && binding.GetOwnerEpoch() == authority.epoch &&
		bytes.Equal(fence.GetConnectionId(), authority.connection[:]) &&
		bytes.Equal(binding.GetConnectionId(), authority.connection[:])
}

func markRecoveryProjectionUnknown(ctx context.Context, tx pgx.Tx, operationID uuid.UUID, envelope *agentv1.CommandEnvelope, observedAt time.Time) error {
	if envelope.GetConfigApply() != nil {
		if _, err := tx.Exec(ctx, `UPDATE config_apply_operations SET state='unknown',updated_at=$2
			WHERE operation_id=$1 AND state IN ('queued','dispatched','accepted','running','unknown')`, operationID, observedAt); err != nil {
			return fmt.Errorf("mark reconnect configuration apply unknown: %w", err)
		}
	}
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
		if _, err := tx.Exec(ctx, `UPDATE certificates SET state='unknown',version=version+1,updated_at=$2
			WHERE id=$1 AND state NOT IN ('issued','expired','revoked','failed')`, id, observedAt); err != nil {
			return fmt.Errorf("mark reconnect certificate operation unknown: %w", err)
		}
	}
	if envelope.GetCertificateP12() != nil {
		if _, err := tx.Exec(ctx, `UPDATE artifact_operations SET state='pending',updated_at=$2
			WHERE operation_id=$1 AND state IN ('pending','leased')`, operationID, observedAt); err != nil {
			return fmt.Errorf("mark reconnect certificate artifact pending: %w", err)
		}
	}
	return nil
}
