package operations

import (
	"context"
	"errors"
	"fmt"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/canonicalcbor"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/configprofile"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	tracecontext "github.com/GentleKingson/ocservia/control-plane/internal/platform/telemetry"
	"github.com/GentleKingson/ocservia/control-plane/internal/semanticpayload"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ReplayTx runs after the caller locks the target and authorizes the mutation.
func ReplayTx(ctx context.Context, tx database.Tx, store operationstore.Store, workspace uuid.UUID, key string, digest canonicalcbor.IntentDigest) (Operation, error) {
	op, stored, err := store.FindIdempotent(ctx, workspace, key)
	if err != nil || op.ID == "" {
		return op, err
	}
	if !digest.Matches(stored) {
		return Operation{}, ErrIdempotencyConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return Operation{}, fmt.Errorf("commit idempotent lookup: %w", err)
	}
	return op, nil
}

func RequireAttestation(ctx context.Context, store operationstore.Store, nodeID uuid.UUID) error {
	ready, err := store.AttestationReady(ctx, nodeID)
	if err != nil {
		return fmt.Errorf("recheck privd result attestation: %w", err)
	}
	if !ready {
		return ErrCapabilityMissing
	}
	return nil
}

type QueueIntent struct {
	WorkspaceID, NodeID         uuid.UUID
	Key, RequestID, Traceparent string
	Digest                      canonicalcbor.IntentDigest
	Now, Expires, Available     time.Time
	ExpectedVersion             int64
	ResourceType, ResourceKey   *string
}

type QueueIDs struct {
	Operation, Command, Audit uuid.UUID
}

// EnqueueTx persists the authorized command and its audit event, then resolves
// an uncertain commit by reading the exact operation's idempotency receipt.
func (s *Service) EnqueueTx(ctx context.Context, tx database.Tx, store operationstore.Store, intent QueueIntent,
	prepare func(QueueIDs) ([]byte, string, error), writeDomain func(QueueIDs) error, writeAudit func(QueueIDs) error,
) (Operation, error) {
	operationID, commandID, outboxID, auditID, eventID, err := newIDs()
	if err != nil {
		return Operation{}, err
	}
	ids := QueueIDs{Operation: operationID, Command: commandID, Audit: auditID}
	envelope, payloadType, err := prepare(ids)
	if err != nil {
		return Operation{}, err
	}
	if err := operationstore.ReserveCommandBacklog(ctx, tx, intent.WorkspaceID, intent.NodeID); err != nil {
		return Operation{}, err
	}
	hash := intent.Digest.Current
	if err := store.InsertIntent(ctx, operationstore.QueuedIntent{
		ID: ids.Operation, WorkspaceID: intent.WorkspaceID, NodeID: intent.NodeID, CommandID: ids.Command,
		RequestID: intent.RequestID, TraceID: tracecontext.TraceID(intent.Traceparent), IdempotencyKey: intent.Key,
		RequestHash: hash[:], ExpiresAt: intent.Expires, CreatedAt: intent.Now,
	}); err != nil {
		return Operation{}, fmt.Errorf("insert operation intent: %w", err)
	}
	if err := writeDomain(ids); err != nil {
		return Operation{}, err
	}
	if err := store.EnqueueCommand(ctx, operationstore.QueuedCommand{
		ID: ids.Command, OperationID: ids.Operation, WorkspaceID: intent.WorkspaceID, NodeID: intent.NodeID,
		OutboxID: outboxID, EventID: eventID, PayloadType: payloadType, Envelope: envelope,
		IdempotencyKey: intent.Key, ExpectedVersion: intent.ExpectedVersion, Traceparent: intent.Traceparent,
		ExpiresAt: intent.Expires, CreatedAt: intent.Now, AvailableAt: intent.Available,
		ResourceType: intent.ResourceType, ResourceKey: intent.ResourceKey,
	}); err != nil {
		return Operation{}, err
	}
	if err := writeAudit(ids); err != nil {
		return Operation{}, err
	}
	nodeText, commandText := intent.NodeID.String(), commandID.String()
	op := Operation{ID: operationID.String(), State: "queued", NodeID: &nodeText, CommandID: &commandText, Version: 1, CreatedAt: intent.Now, UpdatedAt: intent.Now, ExpiresAt: &intent.Expires}
	if err := tx.Commit(ctx); err != nil {
		if errors.Is(err, database.ErrTxAborted) || errors.Is(err, database.ErrSerialization) || errors.Is(err, database.ErrDeadlock) {
			return Operation{}, fmt.Errorf("commit operation transaction: %w", err)
		}
		check, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		confirmed := database.Within(check, s.backend, database.ReadCommitted, func(read database.Tx) error {
			existing, stored, err := operationstore.FromTransaction(read).FindIdempotent(check, intent.WorkspaceID, intent.Key)
			if err != nil {
				return err
			}
			if existing.ID != op.ID || !intent.Digest.Matches(stored) {
				return database.ErrNotFound
			}
			op = existing
			return nil
		})
		if confirmed != nil {
			return Operation{}, fmt.Errorf("commit operation transaction: %w", errors.Join(database.ErrCommitUnknown, err, confirmed))
		}
	}
	if intent.Available.Equal(intent.Now) {
		s.Notify()
	}
	return op, nil
}

func NewCommandEnvelope(node, operation, command uuid.UUID, revision uint64, now, expires time.Time, traceparent, actor, action, reason string) (*agentv1.CommandEnvelope, error) {
	message, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	return &agentv1.CommandEnvelope{
		ProtocolVersion: commandauth.ProtocolVersion, MessageId: message[:], CommandId: command[:], IdempotencyKey: operation[:],
		NodeId: node[:], Sequence: 1, IssuedAt: timestamppb.New(now), ExpiresAt: timestamppb.New(expires),
		ExpectedRevision: revision, Traceparent: traceparent, ActorId: actor, Reason: reason, OperationId: operation[:],
		Action: action, DeliveryMode: agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_EXECUTE_OR_REPLAY,
	}, nil
}

func SignCommandEnvelope(envelope *agentv1.CommandEnvelope, signer *commandauth.Signer) ([]byte, error) {
	return signCommandEnvelope(envelope, signer, configprofile.Normalized{})
}

func signCommandEnvelope(envelope *agentv1.CommandEnvelope, signer *commandauth.Signer, candidate configprofile.Normalized) ([]byte, error) {
	var err error
	if envelope.GetCompleteConfigPlan() != nil || envelope.GetCompleteConfigApply() != nil {
		err = semanticpayload.PopulateConfigV2(envelope, candidate)
	} else {
		err = semanticpayload.PopulateV2(envelope)
	}
	if err != nil {
		return nil, fmt.Errorf("compute semantic payload hash: %w", err)
	}
	if err := signer.Authorize(envelope); err != nil {
		return nil, fmt.Errorf("authorize typed command: %w", err)
	}
	return proto.Marshal(envelope)
}
