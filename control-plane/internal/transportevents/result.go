package transportevents

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/postgresinput"
	"github.com/GentleKingson/ocservia/control-plane/internal/privdattestation"
	resultstore "github.com/GentleKingson/ocservia/control-plane/internal/transportevents/store"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

// IngestCommandResult verifies and applies one result inside the ingress
// transaction. The caller owns node trust, the event row and commit/rollback.
func IngestCommandResult(ctx context.Context, tx database.Tx, eventID, nodeID uuid.UUID, payload []byte, occurredAt, observedAt time.Time, signer *commandauth.Signer) error {
	var result agentv1.CommandResult
	if err := proto.Unmarshal(payload, &result); err != nil {
		return invalidCommandResult("structured Agent command result protobuf is invalid")
	}
	commandID, err := uuid.FromBytes(result.GetCommandId())
	if err != nil || commandID.Version() != 7 {
		return invalidCommandResult("command result command_id must be UUIDv7")
	}
	idempotencyKey, err := uuid.FromBytes(result.GetIdempotencyKey())
	if err != nil || idempotencyKey.Version() != 7 {
		return invalidCommandResult("command result idempotency_key must be UUIDv7")
	}
	completedAt := result.GetCompletedAt()
	if completedAt == nil || completedAt.CheckValid() != nil {
		return invalidCommandResult("command result completed_at is invalid")
	}
	completedTime := completedAt.AsTime()
	if completedTime.After(observedAt.Add(5*time.Minute)) || completedTime.After(occurredAt.Add(5*time.Minute)) {
		return invalidCommandResult("command result completed_at exceeds clock skew bound")
	}
	errorCodeValue := result.GetErrorCode()
	if len(result.GetResult()) > 1<<20 || (errorCodeValue != "" && !postgresinput.ValidText(errorCodeValue, 128)) {
		return invalidCommandResult("command result field is invalid")
	}
	state := ""
	switch result.GetState() {
	case agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED:
		state = "succeeded"
	case agentv1.CommandResultState_COMMAND_RESULT_STATE_FAILED:
		state = "failed"
	case agentv1.CommandResultState_COMMAND_RESULT_STATE_UNKNOWN:
		state = "unknown"
	case agentv1.CommandResultState_COMMAND_RESULT_STATE_REJECTED:
		state = "rejected"
	default:
		return invalidCommandResult("command result state is invalid")
	}
	store, err := resultstore.Results(tx)
	if err != nil {
		return err
	}
	at, err := value.FromTime(observedAt)
	if err != nil {
		return err
	}
	completed, err := value.FromTime(completedTime)
	if err != nil {
		return err
	}
	stored, err := store.LoadCommand(ctx, commandID, nodeID)
	if errors.Is(err, database.ErrNotFound) {
		return invalidCommandResult("command result does not match a dispatched command")
	}
	if err != nil {
		return fmt.Errorf("load command envelope for result: %w", err)
	}
	if stored.Compacted {
		return invalidCommandResult("command detail is retired; terminal identity remains immutable")
	}
	envelopeBytes, currentState := stored.Envelope, stored.State
	dispatchInFlight := stored.DispatchInFlight
	inFlightAttemptID, inFlightLeaseToken := stored.AttemptID, stored.LeaseToken
	if currentState == "queued" && !dispatchInFlight {
		return invalidCommandResult("queued command result has no current sending attempt")
	}
	var envelope agentv1.CommandEnvelope
	if err := proto.Unmarshal(envelopeBytes, &envelope); err != nil {
		return invalidCommandResult("stored command envelope is invalid")
	}
	if err := validateDispatchedResult(&envelope, &result, stored, state, completedTime); err != nil {
		return err
	}
	payloadHash := resultBytesOrNull(result.GetPayloadSha256())
	hashVersion := int16(result.GetSemanticPayloadHashVersion())
	var accepted value.Timestamp
	if acceptedAt := result.GetAcceptedAt(); acceptedAt != nil {
		accepted, err = value.FromTime(acceptedAt.AsTime())
		if err != nil {
			return err
		}
	}
	errorCode := resultText(result.GetErrorCode())
	resultBytes := result.GetResult()
	if resultBytes == nil {
		resultBytes = []byte{}
	}
	verification := privdattestation.VerifyResultTransaction(ctx, tx, nodeID, &envelope, &result)
	receiptFailed := verification.Status != "not_required" && !verification.Verified()
	outcome, normalizationErr := normalizeCommandOutcome(&envelope, state, resultBytes, receiptFailed)
	effectiveState := outcome.state
	recoveryReason := result.GetErrorCode()
	if normalizationErr != nil {
		effectiveState = "unknown"
		recoveryReason = "outcome_requires_reconciliation"
	}
	terminalEvidenceOnly := false
	if (currentState == "succeeded" || currentState == "failed" || currentState == "rejected" || currentState == "rolled_back") && currentState != effectiveState {
		if normalizationErr != nil {
			// A forged duplicate cannot change an already terminal outcome, but its
			// verification evidence still has to reach the durable alert and audit
			// path below.
			terminalEvidenceOnly = true
		} else {
			return invalidCommandResult("command result contradicts a terminal state")
		}
	}
	if currentState == "expired" || currentState == "superseded" {
		return invalidCommandResult("command result contradicts a terminal state")
	}
	if verification.Verified() {
		existingCommandID, existingReceipt, err := store.Receipt(ctx, verification.KeyID, verification.EffectRecordID, verification.EffectSequence)
		if err == nil {
			if existingCommandID == commandID && bytes.Equal(existingReceipt, verification.ReceiptSHA256) {
				return nil
			}
			return invalidCommandResult("privd effect receipt was replayed across command identities")
		}
		if !errors.Is(err, database.ErrNotFound) {
			return fmt.Errorf("check privd effect receipt replay: %w", err)
		}
	}
	// Use the database clock for durable result/attempt ordering, independently
	// of the Controller observation clock carried by projections and audit.
	if err := store.InsertResult(ctx, resultstore.Result{
		EventID: eventID, CommandID: commandID, IdempotencyKey: idempotencyKey,
		PayloadHash: payloadHash, HashVersion: hashVersion, State: state, Bytes: resultBytes, ErrorCode: errorCode,
		AcceptedAt: accepted, CompletedAt: completed, Replayed: result.GetReplayed(),
		VerificationStatus: verification.Status, FailureReason: resultText(verification.FailureReason), KeyID: resultText(verification.KeyID),
		EffectRecordID: resultBytesOrNull(verification.EffectRecordID), EffectSequence: resultSequence(verification.EffectSequence),
		ReceiptHash: resultBytesOrNull(verification.ReceiptSHA256), Proof: resultBytesOrNull(verification.EncodedProof),
	}); err != nil {
		return fmt.Errorf("persist Agent command result: %w", err)
	}
	if receiptFailed {
		alertWorkspaceID, err := store.Workspace(ctx, nodeID)
		if err != nil {
			return fmt.Errorf("load privd receipt alert workspace: %w", err)
		}
		if err := store.Alert(ctx, uuid.Must(uuid.NewV7()), alertWorkspaceID, "critical", "privd.receipt_verification_failed", at); err != nil {
			return fmt.Errorf("emit privd receipt verification alert: %w", err)
		}
		if err := audit.AppendChainTx(ctx, tx, audit.ChainRecord{
			WorkspaceID: alertWorkspaceID, ActorType: "controller", ActorID: "privd-receipt-verifier",
			Action: "privd.result.verify", ResourceType: "command", ResourceID: commandID,
			NodeID: &nodeID, CommandID: &commandID, RequestID: eventID.String(), Result: "failed",
			Reason: verification.FailureReason, ErrorType: verification.FailureReason, At: observedAt,
		}); err != nil {
			return fmt.Errorf("append privd receipt verification audit: %w", err)
		}
	}
	if terminalEvidenceOnly {
		return nil
	}
	operationEventID, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("generate operation result event ID: %w", err)
	}
	terminal := effectiveState == "succeeded" || effectiveState == "failed" || effectiveState == "rejected" || effectiveState == "rolled_back"
	commandChanged, err := store.UpdateCommand(ctx, commandID, effectiveState, at)
	if err != nil {
		return fmt.Errorf("apply Agent command result: %w", err)
	}
	if !commandChanged {
		return nil
	}
	operationState := effectiveState
	if effectiveState == "rejected" {
		operationState = "failed"
	}
	operation, err := store.UpdateOperation(ctx, commandID, operationState, terminal, at)
	if err != nil && !errors.Is(err, database.ErrNotFound) {
		return fmt.Errorf("apply Agent operation result: %w", err)
	}
	if errors.Is(err, database.ErrNotFound) {
		return invalidCommandResult("command result does not match a mutable operation")
	}
	operationID, workspaceID := operation.ID, operation.WorkspaceID
	operationRequestID, operationTraceID := operation.RequestID, operation.TraceID
	if terminal {
		if err := store.CompleteOutbox(ctx, commandID, at); err != nil {
			return fmt.Errorf("complete command reconciliation outbox: %w", err)
		}
	}
	if dispatchInFlight {
		if err := store.CloseDispatch(ctx, nodeID, commandID, inFlightAttemptID, inFlightLeaseToken); err != nil {
			return fmt.Errorf("close result-observed dispatch: %w", err)
		}
	}
	if err := outcome.apply(ctx, store, &envelope, &result, verification, operation, nodeID, operationState, at); err != nil {
		return err
	}
	if err := store.AppendEvent(ctx, operationEventID, operationID, operationState, at); err != nil {
		return fmt.Errorf("append Agent operation result event: %w", err)
	}
	if terminal {
		auditResult := "failed"
		if operationState == "succeeded" {
			auditResult = "succeeded"
		}
		action := commandAuditAction(&envelope)
		if err := audit.AppendChainTx(ctx, tx, audit.ChainRecord{WorkspaceID: workspaceID, ActorType: "agent", ActorID: envelope.GetActorId(), Action: action, ResourceType: "operation", ResourceID: operationID, NodeID: &nodeID, CommandID: &commandID, RequestID: operationRequestID, TraceID: operationTraceID, Result: auditResult, Reason: envelope.GetReason(), ErrorType: result.GetErrorCode(), At: observedAt}); err != nil {
			return fmt.Errorf("append Agent audit result: %w", err)
		}
	}
	if effectiveState == "unknown" {
		if err := scheduleCommandRecovery(ctx, store, commandID, &envelope, recoveryReason, observedAt, signer); err != nil {
			return err
		}
	}
	return nil
}

func scheduleCommandRecovery(ctx context.Context, store resultstore.ResultStore, commandID uuid.UUID, envelope *agentv1.CommandEnvelope, reason string, observedAt time.Time, signer *commandauth.Signer) error {
	var mode agentv1.CommandDeliveryMode
	switch reason {
	case "effect_absent":
		if envelope.GetDeliveryMode() != agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY {
			return invalidCommandResult("effect absence was not observed during reconciliation")
		}
		// The Agent's effect_absent is unsigned. These fixed host actions cannot
		// prove absence cryptographically, so the Unknown outcome stays for
		// manual reconciliation; only a signed completed result can settle it.
		if fixedHostEffect(envelope) {
			return nil
		}
		mode = agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RETRY_IF_EFFECT_ABSENT
	case "outcome_requires_reconciliation", "result_persistence_failed", "privd_transport_unknown", "privd_outcome_unknown":
		mode = agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY
	default:
		return nil
	}
	if mode == agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RETRY_IF_EFFECT_ABSENT {
		if envelope.GetProtocolVersion() != commandauth.ProtocolVersion {
			return nil
		}
		expiresAt := envelope.GetExpiresAt()
		if expiresAt == nil || expiresAt.CheckValid() != nil || !expiresAt.AsTime().After(observedAt) {
			return nil
		}
	}
	payload, expiresAt, err := operations.PrepareRecoveryEnvelope(envelope, mode, observedAt, signer)
	if err != nil {
		return err
	}
	expiry, err := value.FromTime(expiresAt)
	if err != nil {
		return err
	}
	at, err := value.FromTime(observedAt)
	if err != nil {
		return err
	}
	return store.ScheduleRecovery(ctx, commandID, payload, expiry, at)
}

func fixedHostEffect(envelope *agentv1.CommandEnvelope) bool {
	switch envelope.GetPayload().(type) {
	case *agentv1.CommandEnvelope_SessionDisconnect, *agentv1.CommandEnvelope_SessionTerminate,
		*agentv1.CommandEnvelope_IpBanRemove, *agentv1.CommandEnvelope_ServiceReload:
		return true
	}
	return false
}

func resultText(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}
func resultBytesOrNull(v []byte) []byte {
	if len(v) == 0 {
		return nil
	}
	return v
}
func resultSequence(v uint64) *int64 {
	if v == 0 {
		return nil
	}
	n := int64(v)
	return &n
}
