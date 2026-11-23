package transportevents

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/privdattestation"
	"github.com/GentleKingson/ocservia/control-plane/internal/semanticpayload"
	resultstore "github.com/GentleKingson/ocservia/control-plane/internal/transportevents/store"
	"github.com/google/uuid"
)

func validateDispatchedResult(envelope *agentv1.CommandEnvelope, result *agentv1.CommandResult, stored resultstore.ResultCommand, state string, completedTime time.Time) error {
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
	if stored.CreatedAt.IsZero() {
		return invalidCommandResult("stored command creation is later than the result")
	}
	if stored.CreatedAt.After(lowerBound) {
		lowerBound = stored.CreatedAt
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
		if resultHashVersion != agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V2 {
			return invalidCommandResult("command result payload hash requires version 2")
		}
		expectedHash, err := semanticpayload.HashV2(envelope)
		if err != nil {
			return invalidCommandResult("stored command semantic payload cannot be hashed")
		}
		if !bytes.Equal(result.GetPayloadSha256(), expectedHash[:]) {
			return invalidCommandResult("command result payload hash mismatch")
		}
	}
	return nil
}

func applyConfigOutcome(ctx context.Context, store resultstore.ResultStore, apply *configApplyIdentity, result *agentv1.CommandResult, applyResult *agentv1.ConfigApplyResult, operationState string, operationID, nodeID, workspaceID uuid.UUID, at time.Time) error {
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
		State: applyState, FailureCode: failureCode, Revision: apply.desiredRevision, CandidateHash: apply.candidateHash, At: at,
	})
	if err != nil {
		return fmt.Errorf("update configuration apply outcome: %w", err)
	}
	if !matched {
		return invalidCommandResult("configuration apply result has no immutable plan")
	}
	return nil
}

func applyCSROutcome(ctx context.Context, store resultstore.ResultStore, csr *agentv1.CertificateCsr, csrResult *agentv1.CertificateCsrResult, verification privdattestation.Verification, effectiveState string, operationID uuid.UUID, at time.Time) error {
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
		verifiedAt := at
		outcome.VerifiedAt, outcome.ReceiptHash, outcome.KeyID = &verifiedAt, verification.ReceiptSHA256, resultText(verification.KeyID)
		outcome.CSRHash = verification.Certificate.GetCsrDerSha256()
	}
	if err := store.CSROutcome(ctx, outcome); err != nil {
		return fmt.Errorf("update certificate CSR outcome: %w", err)
	}
	return nil
}

func applyRevokeOutcome(ctx context.Context, store resultstore.ResultStore, revoke *agentv1.CertificateRevoke, revokeResult *agentv1.CertificateRevokeResult, effectiveState string, nodeID uuid.UUID, at time.Time) error {
	certificateID, parseErr := uuid.FromBytes(revoke.GetCertificateId())
	if parseErr != nil {
		return invalidCommandResult("certificate revoke ID is invalid")
	}
	certificateState := "revoking"
	var revokedAt *time.Time
	if effectiveState == "unknown" {
		certificateState = "unknown"
	} else if effectiveState == "succeeded" && revokeResult != nil {
		t := at
		certificateState, revokedAt = "revoked", &t
	}
	if err := store.RevokeOutcome(ctx, resultstore.RevokeOutcome{CertificateID: certificateID, NodeID: nodeID, State: certificateState, RevokedAt: revokedAt, Reason: revoke.GetReason(), At: at}); err != nil {
		return fmt.Errorf("update certificate revocation outcome: %w", err)
	}
	return nil
}

func applyArtifactOutcome(ctx context.Context, store resultstore.ResultStore, artifact *agentv1.CertificateP12, artifactResult *agentv1.CertificateArtifactResult, effectiveState string, operationID uuid.UUID, at time.Time) error {
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
	return nil
}

type commandOutcome struct {
	state    string
	config   *agentv1.ConfigApplyResult
	csr      *agentv1.CertificateCsrResult
	revoke   *agentv1.CertificateRevokeResult
	artifact *agentv1.CertificateArtifactResult
}

func normalizeCommandOutcome(envelope *agentv1.CommandEnvelope, state string, data []byte, receiptFailed bool) (commandOutcome, error) {
	if receiptFailed {
		return commandOutcome{}, errors.New("privileged result receipt verification failed")
	}
	outcome := commandOutcome{state: state}
	var err error
	switch {
	case configApplyBinding(envelope) != nil:
		outcome.state, outcome.config, err = normalizeConfigApplyResult(envelope, state, data)
	case envelope.GetCertificateCsr() != nil:
		outcome.csr, err = normalizeCertificateCSRResult(envelope, state, data)
	case envelope.GetCertificateRevoke() != nil:
		outcome.revoke, err = normalizeCertificateRevokeResult(envelope, state, data)
	case envelope.GetCertificateP12() != nil:
		outcome.artifact, err = normalizeCertificateArtifactResult(envelope, state, data)
	case envelope.GetAgentUpgrade() != nil && state == "succeeded":
		// The restart and target-version check follow the scheduling receipt.
		err = validateAgentUpgradeScheduledResult(envelope, data)
		outcome.state = "accepted"
	}
	return outcome, err
}

func (outcome commandOutcome) apply(ctx context.Context, store resultstore.ResultStore, envelope *agentv1.CommandEnvelope, result *agentv1.CommandResult, verification privdattestation.Verification, operation resultstore.ResultOperation, nodeID uuid.UUID, operationState string, at time.Time) error {
	switch apply := configApplyBinding(envelope); {
	case apply != nil:
		return applyConfigOutcome(ctx, store, apply, result, outcome.config, operationState, operation.ID, nodeID, operation.WorkspaceID, at)
	case envelope.GetCertificateCsr() != nil:
		return applyCSROutcome(ctx, store, envelope.GetCertificateCsr(), outcome.csr, verification, operationState, operation.ID, at)
	case envelope.GetCertificateRevoke() != nil:
		return applyRevokeOutcome(ctx, store, envelope.GetCertificateRevoke(), outcome.revoke, operationState, nodeID, at)
	case envelope.GetCertificateP12() != nil:
		return applyArtifactOutcome(ctx, store, envelope.GetCertificateP12(), outcome.artifact, operationState, operation.ID, at)
	case envelope.GetAgentUpgrade() != nil:
		var scheduledAt *time.Time
		if operationState == "accepted" {
			t := at
			scheduledAt = &t
		}
		if err := store.UpgradeOutcome(ctx, operation.ID, operationState, scheduledAt, at); err != nil {
			return fmt.Errorf("update agent upgrade outcome: %w", err)
		}
	}
	return nil
}
