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
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
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
// one node. Recovery independently checks this term against the database while
// holding the caller's transaction open.
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

// RecoverAmbiguousDispatched joins the caller's transaction. The authority
// row stays share-locked through all projections and the outbox update.
func (s *Service) RecoverAmbiguousDispatched(ctx context.Context, tx database.Tx, reconnect OwnerReconnect) (int, error) {
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
	if err := commandlimit.Lock(ctx, tx); err != nil {
		return 0, fmt.Errorf("serialize reconnect recovery: %w", err)
	}
	store, err := operationstore.FromTransaction(tx)
	if err != nil {
		return 0, err
	}
	owner, err := store.ReconnectAuthority(ctx, reconnect.NodeID, connectionID, int64(reconnect.OwnerEpoch))
	if errors.Is(err, database.ErrNotFound) {
		return 0, connectionowner.ErrNotOwner
	}
	if err != nil {
		return 0, fmt.Errorf("guard reconnect recovery authority: %w", err)
	}
	authority := recoveryAuthority{instanceID: owner.OwnerID, incarnation: owner.Incarnation, connection: reconnect.ConnectionID, epoch: reconnect.OwnerEpoch}
	candidates, err := store.AmbiguousDispatches(ctx, reconnect.NodeID, maxReconnectRecoveryCandidates)
	if err != nil {
		return 0, fmt.Errorf("select ambiguous dispatched commands: %w", err)
	}
	at, err := value.FromTime(reconnect.ObservedAt)
	if err != nil {
		return 0, err
	}
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
		if dispatchedOnAuthority(&envelope, commandID, reconnect.NodeID, authority) {
			continue
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
		deadline, err := candidate.ExpiresAt.Time()
		if err != nil {
			return 0, err
		}
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
		expires, err := value.FromTime(expiresAt)
		if err != nil {
			return 0, err
		}
		if err := store.ScheduleReconnectRecovery(ctx, commandID, candidate.OperationID, payload, expires, at); err != nil {
			return 0, err
		}
		if err := markRecoveryProjection(ctx, store, candidate.OperationID, &envelope, at); err != nil {
			return 0, err
		}
		recovered++
	}
	return recovered, nil
}

// PrepareRecoveryEnvelope preserves the logical command identity and payload,
// changes only attempt metadata, and strips the previous connection proofs so
// the outbox worker must bind the next dispatch to its then-current owner term.
// The returned deadline bounds observation dispatch, not executable authority.
func PrepareRecoveryEnvelope(envelope *agentv1.CommandEnvelope, mode agentv1.CommandDeliveryMode, observedAt time.Time, signer *commandauth.Signer) ([]byte, time.Time, error) {
	if envelope == nil || signer == nil || observedAt.IsZero() || (mode != agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY && mode != agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RETRY_IF_EFFECT_ABSENT) {
		return nil, time.Time{}, ErrInvalidRequest
	}
	expires := envelope.GetExpiresAt()
	if expires == nil || expires.CheckValid() != nil {
		return nil, time.Time{}, errors.New("operations: reconciliation command expiry is invalid")
	}
	expiresAt := expires.AsTime()
	// Only an original legacy dispatch still carries its execution deadline.
	// Legacy recovery envelopes may already have renewed it and stay observation-only.
	if envelope.GetProtocolVersion() == commandauth.LegacyProtocolVersion && envelope.GetDeliveryMode() == agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_EXECUTE_OR_REPLAY {
		envelope.ProtocolVersion = commandauth.ProtocolVersion
	}
	if mode == agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RETRY_IF_EFFECT_ABSENT && envelope.GetProtocolVersion() != commandauth.ProtocolVersion {
		return nil, time.Time{}, errors.New("operations: legacy recovery cannot authorize execution")
	}
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

const (
	envelopeCommandIDField          protowire.Number = 3
	envelopeNodeIDField             protowire.Number = 5
	envelopeDeliveryModeField       protowire.Number = 109
	envelopeOperationIDField        protowire.Number = 120
	envelopeRequiredCapabilityField protowire.Number = 122
	envelopeConnectionFenceField    protowire.Number = 126
	envelopeFenceBindingField       protowire.Number = 127
)

func validateAndDecodeSentEnvelope(dispatch Dispatch, encoded []byte) (*agentv1.CommandEnvelope, error) {
	if len(encoded) == 0 || len(encoded) > 1<<20 {
		return nil, errors.New("operations: sent command envelope size is invalid")
	}
	if env, fastOK, err := validateSentEnvelopeFast(dispatch, encoded); err != nil {
		return nil, err
	} else if fastOK {
		return env, nil
	}
	var sent agentv1.CommandEnvelope
	if err := proto.Unmarshal(encoded, &sent); err != nil {
		return nil, fmt.Errorf("decode sent command envelope: %w", err)
	}
	if !bytes.Equal(sent.GetCommandId(), dispatch.CommandID[:]) || !bytes.Equal(sent.GetOperationId(), dispatch.OperationID[:]) || !bytes.Equal(sent.GetNodeId(), dispatch.NodeID[:]) {
		return nil, errors.New("operations: sent command envelope identity is inconsistent")
	}
	var claimed agentv1.CommandEnvelope
	if err := proto.Unmarshal(dispatch.Envelope, &claimed); err != nil {
		return nil, fmt.Errorf("decode claimed command envelope: %w", err)
	}
	claimed.ConnectionFence, claimed.FenceBinding = nil, nil
	sentFence, sentBinding := sent.ConnectionFence, sent.FenceBinding
	sent.ConnectionFence, sent.FenceBinding = nil, nil
	same := proto.Equal(&claimed, &sent)
	sent.ConnectionFence, sent.FenceBinding = sentFence, sentBinding
	if !same {
		return nil, errors.New("operations: sent command changed fields outside owner proofs")
	}
	if err := validateSentOwnerProofs(dispatch, sentFence, sentBinding); err != nil {
		return nil, err
	}
	return &sent, nil
}

// validateSentEnvelopeFast walks sent and dispatch.Envelope in lockstep on
// top-level protobuf fields, skipping ConnectionFence (126) and FenceBinding
// (127) in sent. When every non-proof wire span matches byte-for-byte in order,
// it verifies identities and only unmarshals the two small owner-proof
// submessages instead of unmarshaling and deep-comparing the full payload.
// Any unexpected wire order, duplicate proof field, or malformed tag falls back
// to the full proto.Unmarshal + proto.Equal path.
func validateSentEnvelopeFast(dispatch Dispatch, sent []byte) (*agentv1.CommandEnvelope, bool, error) {
	remSent := sent
	remClaimed := dispatch.Envelope
	var (
		commandID    []byte
		operationID  []byte
		nodeID       []byte
		deliveryMode agentv1.CommandDeliveryMode
		fenceWire    []byte
		bindingWire  []byte
		hasFence     bool
		hasBinding   bool
	)
	for len(remSent) > 0 {
		num, typ, tagLen := protowire.ConsumeTag(remSent)
		if tagLen < 0 {
			return nil, false, nil
		}
		valStart := remSent[tagLen:]
		valLen := protowire.ConsumeFieldValue(num, typ, valStart)
		if valLen < 0 {
			return nil, false, nil
		}
		fieldSpan := remSent[:tagLen+valLen]
		remSent = remSent[tagLen+valLen:]

		if num == envelopeConnectionFenceField {
			if typ != protowire.BytesType || hasFence {
				return nil, false, nil
			}
			raw, n := protowire.ConsumeBytes(valStart)
			if n < 0 {
				return nil, false, nil
			}
			fenceWire = raw
			hasFence = true
			continue
		}
		if num == envelopeFenceBindingField {
			if typ != protowire.BytesType || hasBinding {
				return nil, false, nil
			}
			raw, n := protowire.ConsumeBytes(valStart)
			if n < 0 {
				return nil, false, nil
			}
			bindingWire = raw
			hasBinding = true
			continue
		}
		if !bytes.HasPrefix(remClaimed, fieldSpan) {
			return nil, false, nil
		}
		remClaimed = remClaimed[len(fieldSpan):]
		switch num {
		case envelopeCommandIDField:
			if typ != protowire.BytesType {
				return nil, false, nil
			}
			v, n := protowire.ConsumeBytes(valStart)
			if n < 0 {
				return nil, false, nil
			}
			commandID = v
		case envelopeNodeIDField:
			if typ != protowire.BytesType {
				return nil, false, nil
			}
			v, n := protowire.ConsumeBytes(valStart)
			if n < 0 {
				return nil, false, nil
			}
			nodeID = v
		case envelopeOperationIDField:
			if typ != protowire.BytesType {
				return nil, false, nil
			}
			v, n := protowire.ConsumeBytes(valStart)
			if n < 0 {
				return nil, false, nil
			}
			operationID = v
		case envelopeDeliveryModeField:
			if typ != protowire.VarintType {
				return nil, false, nil
			}
			v, n := protowire.ConsumeVarint(valStart)
			if n < 0 {
				return nil, false, nil
			}
			deliveryMode = agentv1.CommandDeliveryMode(int32(v))
		}
	}
	if len(remClaimed) != 0 {
		return nil, false, nil
	}
	if !bytes.Equal(commandID, dispatch.CommandID[:]) || !bytes.Equal(operationID, dispatch.OperationID[:]) || !bytes.Equal(nodeID, dispatch.NodeID[:]) {
		return nil, false, errors.New("operations: sent command envelope identity is inconsistent")
	}
	env := &agentv1.CommandEnvelope{
		CommandId:    commandID,
		OperationId:  operationID,
		NodeId:       nodeID,
		DeliveryMode: deliveryMode,
	}
	if hasFence {
		var fence agentv1.ConnectionFenceV2
		if err := proto.Unmarshal(fenceWire, &fence); err != nil {
			return nil, false, fmt.Errorf("decode sent command envelope: %w", err)
		}
		env.ConnectionFence = &fence
	}
	if hasBinding {
		var binding agentv1.FenceBindingV2
		if err := proto.Unmarshal(bindingWire, &binding); err != nil {
			return nil, false, fmt.Errorf("decode sent command envelope: %w", err)
		}
		env.FenceBinding = &binding
	}
	if err := validateSentOwnerProofs(dispatch, env.ConnectionFence, env.FenceBinding); err != nil {
		return nil, false, err
	}
	return env, true, nil
}

func validateSentOwnerProofs(dispatch Dispatch, sentFence *agentv1.ConnectionFenceV2, sentBinding *agentv1.FenceBindingV2) error {
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

// scanEnvelopeForFencing reads required_capability (field 122) in a single
// zero-alloc pass and reports whether the wire bytes are free of pre-existing
// owner proof fields (126/127) so owner proofs can be appended directly.
func scanEnvelopeForFencing(raw []byte) (requiredCapability string, clean bool) {
	b := raw
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return "", false
		}
		if num == envelopeConnectionFenceField || num == envelopeFenceBindingField {
			return "", false
		}
		b = b[n:]
		if num == envelopeRequiredCapabilityField && typ == protowire.BytesType {
			val, m := protowire.ConsumeString(b)
			if m < 0 {
				return "", false
			}
			requiredCapability = val
			b = b[m:]
			continue
		}
		m := protowire.ConsumeFieldValue(num, typ, b)
		if m < 0 {
			return "", false
		}
		b = b[m:]
	}
	return requiredCapability, true
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

func markRecoveryProjection(ctx context.Context, store operationstore.Store, operationID uuid.UUID, envelope *agentv1.CommandEnvelope, at value.Timestamp) error {
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
