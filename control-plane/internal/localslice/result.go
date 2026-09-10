package localslice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	resultstore "github.com/GentleKingson/ocservia/control-plane/internal/localslice/store"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/postgresinput"
	"github.com/GentleKingson/ocservia/control-plane/internal/privdattestation"
	"github.com/GentleKingson/ocservia/control-plane/internal/semanticpayload"
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
	if !bytes.Equal(envelope.GetIdempotencyKey(), result.GetIdempotencyKey()) {
		return invalidCommandResult("command result idempotency key mismatch")
	}
	storedHashVersion := envelope.GetSemanticPayloadHashVersion()
	if err := semanticpayload.ValidateVersion(storedHashVersion); err != nil {
		return invalidCommandResult("stored command semantic payload hash version is invalid")
	}
	resultHashVersion := result.GetSemanticPayloadHashVersion()
	if err := semanticpayload.ValidateVersion(resultHashVersion); err != nil {
		return invalidCommandResult("command result semantic payload hash version is invalid")
	}
	issuedAt := envelope.GetIssuedAt()
	if issuedAt == nil || issuedAt.CheckValid() != nil {
		return invalidCommandResult("stored command issued_at is invalid")
	}
	issuedTime := issuedAt.AsTime()
	lowerBound := issuedTime
	if !stored.CreatedAt.Valid || stored.CreatedAt.Micros == value.PositiveInfinity {
		return invalidCommandResult("stored command creation is later than the result")
	}
	if stored.CreatedAt.Micros != value.NegativeInfinity {
		commandCreatedAt, err := stored.CreatedAt.Time()
		if err != nil {
			return err
		}
		if commandCreatedAt.After(lowerBound) {
			lowerBound = commandCreatedAt
		}
	}
	if completedTime.Before(lowerBound.Add(-5 * time.Minute)) {
		return invalidCommandResult("command result completed_at precedes issued_at")
	}
	acceptedAt := result.GetAcceptedAt()
	if state == "rejected" {
		if acceptedAt != nil || result.GetErrorCode() == "" || len(result.GetResult()) != 0 || (len(result.GetPayloadSha256()) != 0 && len(result.GetPayloadSha256()) != sha256.Size) {
			return invalidCommandResult("rejected command result fields are invalid")
		}
	} else {
		if acceptedAt == nil || acceptedAt.CheckValid() != nil || len(result.GetPayloadSha256()) != sha256.Size {
			return invalidCommandResult("accepted command result fields are invalid")
		}
		if acceptedAt.AsTime().Before(lowerBound.Add(-5*time.Minute)) || acceptedAt.AsTime().After(completedTime) {
			return invalidCommandResult("command result accepted_at is invalid")
		}
		if state == "succeeded" && result.GetErrorCode() != "" {
			return invalidCommandResult("succeeded command result must not contain an error code")
		}
		if (state == "failed" || state == "unknown") && result.GetErrorCode() == "" {
			return invalidCommandResult("non-success command result requires an error code")
		}
		if state == "unknown" && len(result.GetResult()) != 0 {
			return invalidCommandResult("unknown command result must not contain result bytes")
		}
	}
	if state == "rejected" && len(result.GetPayloadSha256()) == 0 {
		if resultHashVersion != agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_UNSPECIFIED {
			return invalidCommandResult("rejected command result must not declare a payload hash version")
		}
	} else if resultHashVersion != storedHashVersion {
		return invalidCommandResult("command result semantic payload hash version mismatch")
	}
	if len(result.GetPayloadSha256()) == sha256.Size {
		var expectedHash [sha256.Size]byte
		var err error
		switch resultHashVersion {
		case agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V1:
			expectedHash, err = semanticpayload.HashV1(&envelope)
		case agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V2:
			expectedHash, err = semanticpayload.HashV2(&envelope)
		case agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_UNSPECIFIED:
			expectedHash, err = agentPayloadHash(&envelope)
		}
		if err != nil {
			return invalidCommandResult("stored command semantic payload cannot be hashed")
		}
		if !bytes.Equal(result.GetPayloadSha256(), expectedHash[:]) {
			return invalidCommandResult("command result payload hash mismatch")
		}
	}
	payloadHash := resultBytesOrNull(result.GetPayloadSha256())
	hashVersion := int16(result.GetSemanticPayloadHashVersion())
	var accepted value.Timestamp
	if acceptedAt != nil {
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
	normalizationState := state
	if verification.Status != "not_required" && !verification.Verified() {
		normalizationState = "unknown"
	}
	effectiveState, applyResult, normalizationErr := normalizeConfigApplyResult(&envelope, normalizationState, resultBytes)
	csrResult, csrErr := normalizeCertificateCSRResult(&envelope, normalizationState, resultBytes)
	revokeResult, revokeErr := normalizeCertificateRevokeResult(&envelope, normalizationState, resultBytes)
	artifactResult, artifactErr := normalizeCertificateArtifactResult(&envelope, normalizationState, resultBytes)
	if normalizationErr == nil && csrErr != nil {
		normalizationErr = csrErr
	}
	if normalizationErr == nil && revokeErr != nil {
		normalizationErr = revokeErr
	}
	if normalizationErr == nil && artifactErr != nil {
		normalizationErr = artifactErr
	}
	// An acknowledged agent upgrade is only scheduled: the expected restart
	// and target-version verification follow, so the operation must not
	// become terminal here. Any other state (explicit failure, rejection, or
	// an unverified receipt) keeps the generic semantics.
	upgradeScheduled := false
	if envelope.GetAgentUpgrade() != nil && normalizationErr == nil && normalizationState == "succeeded" {
		if err := validateAgentUpgradeScheduledResult(&envelope, resultBytes); err != nil {
			normalizationErr = err
		} else {
			effectiveState = "accepted"
			upgradeScheduled = true
		}
	}
	recoveryReason := result.GetErrorCode()
	if verification.Status != "not_required" && !verification.Verified() {
		normalizationErr = errors.New("privileged result receipt verification failed")
		recoveryReason = verification.FailureReason
	}
	if normalizationErr != nil {
		effectiveState = "unknown"
		applyResult = nil
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
	if verification.Status != "not_required" && !verification.Verified() {
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
	if apply := envelope.GetConfigApply(); apply != nil {
		applyState := operationState
		failureCode := resultText("")
		if result.GetErrorCode() != "" {
			failureCode = resultText(result.GetErrorCode())
		}
		if applyResult != nil {
			if applyResult.GetFailureCode() != "" {
				failureCode = resultText(applyResult.GetFailureCode())
			}
			if applyResult.GetFailedCritical() {
				applyState = "failed_critical"
			} else if applyResult.GetRolledBack() {
				applyState = "rolled_back"
			}
		}
		matched, err := store.ConfigOutcome(ctx, resultstore.ConfigOutcome{
			OperationID: operationID, NodeID: nodeID, WorkspaceID: workspaceID, AlertID: uuid.Must(uuid.NewV7()),
			State: applyState, FailureCode: failureCode, Revision: apply.GetDesiredRevision(), CandidateHash: apply.GetCandidateHash(), At: at,
		})
		if err != nil {
			return fmt.Errorf("update configuration apply outcome: %w", err)
		}
		if !matched {
			return invalidCommandResult("configuration apply result has no immutable plan")
		}
	}
	if csr := envelope.GetCertificateCsr(); csr != nil {
		certificateID, parseErr := uuid.FromBytes(csr.GetCertificateId())
		if parseErr != nil {
			return invalidCommandResult("certificate command ID is invalid")
		}
		certificateState := "failed"
		if effectiveState == "unknown" {
			certificateState = "unknown"
		} else if effectiveState == "succeeded" && csrResult != nil {
			certificateState = "csr_ready"
		}
		outcome := resultstore.CSROutcome{CertificateID: certificateID, OperationID: operationID, State: certificateState, At: at}
		if csrResult != nil {
			outcome.DER, outcome.PublicHash = csrResult.GetCsrDer(), csrResult.GetPublicKeySha256()
		}
		if certificateState == "csr_ready" && verification.Verified() && verification.Certificate != nil {
			outcome.VerifiedAt, outcome.ReceiptHash, outcome.KeyID = at, verification.ReceiptSHA256, resultText(verification.KeyID)
			outcome.EffectRecordID, outcome.CSRHash, outcome.SubjectHash = verification.EffectRecordID, verification.Certificate.GetCsrDerSha256(), verification.Certificate.GetRequestedSubjectSha256()
		}
		if err := store.CSROutcome(ctx, outcome); err != nil {
			return fmt.Errorf("update certificate CSR outcome: %w", err)
		}
	}
	if revoke := envelope.GetCertificateRevoke(); revoke != nil {
		certificateID, parseErr := uuid.FromBytes(revoke.GetCertificateId())
		if parseErr != nil {
			return invalidCommandResult("certificate revoke ID is invalid")
		}
		certificateState := "revoking"
		var revokedAt value.Timestamp
		if effectiveState == "unknown" {
			certificateState = "unknown"
		} else if effectiveState == "succeeded" && revokeResult != nil {
			certificateState, revokedAt = "revoked", at
		}
		if err := store.RevokeOutcome(ctx, resultstore.RevokeOutcome{CertificateID: certificateID, NodeID: nodeID, State: certificateState, RevokedAt: revokedAt, Reason: revoke.GetReason(), At: at}); err != nil {
			return fmt.Errorf("update certificate revocation outcome: %w", err)
		}
	}
	if artifact := envelope.GetCertificateP12(); artifact != nil {
		artifactID, parseErr := uuid.FromBytes(artifact.GetArtifactId())
		if parseErr != nil {
			return invalidCommandResult("certificate artifact ID is invalid")
		}
		artifactState := "failed"
		var digest []byte
		var size *int64
		if effectiveState == "unknown" {
			artifactState = "pending"
		} else if effectiveState == "succeeded" && artifactResult != nil {
			artifactState, digest = "ready", artifactResult.GetArtifactSha256()
			bytes := int64(artifactResult.GetArtifactSize())
			size = &bytes
		}
		if err := store.ArtifactOutcome(ctx, resultstore.ArtifactOutcome{ArtifactID: artifactID, OperationID: operationID, State: artifactState, Digest: digest, Size: size, At: at}); err != nil {
			return fmt.Errorf("update certificate artifact outcome: %w", err)
		}
	}
	if envelope.GetAgentUpgrade() != nil {
		var scheduledAt value.Timestamp
		if upgradeScheduled {
			scheduledAt = at
		}
		if err := store.UpgradeOutcome(ctx, operationID, operationState, scheduledAt, at); err != nil {
			return fmt.Errorf("update agent upgrade outcome: %w", err)
		}
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
		mode = agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RETRY_IF_EFFECT_ABSENT
	case "outcome_requires_reconciliation", "result_persistence_failed", "privd_transport_unknown", "privd_outcome_unknown":
		mode = agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY
	default:
		return nil
	}
	if mode == agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RETRY_IF_EFFECT_ABSENT {
		expiresAt := envelope.GetExpiresAt()
		if expiresAt == nil || expiresAt.CheckValid() != nil || !expiresAt.AsTime().After(observedAt) {
			return nil
		}
	}
	payload, expiresAt, err := operationstore.PrepareRecoveryEnvelope(envelope, mode, observedAt, signer)
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
