package privdattestation

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/canonicalcbor"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/semanticpayload"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	receiptDomain       = "ocservia/privd-result-receipt/cbor.v1\x00"
	resultPayloadDomain = "ocservia/privd-result-payload/cbor.v1\x00"
	registrationDomain  = "ocservia/privd-attestation-registration/cbor.v1\x00"
	upgradeResultDomain = "ocservia/privd-upgrade-result/cbor.v1\x00"
	keyIDPrefix         = "ed25519-sha256:"
	maxCanonicalBytes   = 2048
	maxErrorCodeBytes   = 128
)

type Verification struct {
	Status         string
	FailureReason  string
	KeyID          string
	EffectSequence uint64
	ReceiptSHA256  []byte
	EncodedProof   []byte
	Certificate    *agentv1.PrivdCertificateReceiptBindingV1
}

func (v Verification) Verified() bool { return v.Status == "verified" }

// UpgradeResultVerification is the Controller-side verification result for a
// root-owned durable upgrader outcome.
type UpgradeResultVerification struct {
	Status        string
	FailureReason string
	KeyID         string
	EncodedProof  []byte
	PackageSHA256 []byte
	ResultSHA256  []byte
	CompletedAt   time.Time
}

func (v UpgradeResultVerification) Verified() bool { return v.Status == "verified" }

type attestationKeyRecord = VerificationKey

type keyLookup func(context.Context, uuid.UUID, string) (attestationKeyRecord, error)

type VerificationMetric struct {
	Outcome string `json:"outcome"`
	Reason  string `json:"reason"`
	Total   uint64 `json:"total"`
}

var verificationMetricDefinitions = [...]struct {
	status, reason string
	counter        atomic.Uint64
}{
	{status: "not_required", reason: "not_required"},
	{status: "verified", reason: "verified"},
	{status: "missing", reason: "receipt_missing"},
	{status: "invalid", reason: "receipt_malformed"},
	{status: "invalid", reason: "receipt_version_unsupported"},
	{status: "invalid", reason: "receipt_claim_mismatch"},
	{status: "invalid", reason: "receipt_key_lookup_failed"},
	{status: "invalid", reason: "receipt_key_outside_validity"},
	{status: "invalid", reason: "receipt_signature_invalid"},
	{status: "unknown_key", reason: "receipt_key_unknown"},
	{status: "revoked_key", reason: "receipt_key_revoked"},
}

type cborCertificateBinding struct {
	CertificateID   []byte `cbor:"certificate_id"`
	CSRDerSHA256    []byte `cbor:"csr_der_sha256"`
	PublicKeySHA256 []byte `cbor:"public_key_sha256"`
}

type cborReceiptV1 struct {
	ReceiptVersion             uint32                  `cbor:"receipt_version"`
	NodeID                     []byte                  `cbor:"node_id"`
	PrivdAttestationKeyID      string                  `cbor:"privd_attestation_key_id"`
	CommandID                  []byte                  `cbor:"command_id"`
	OperationID                []byte                  `cbor:"operation_id"`
	IdempotencyKey             []byte                  `cbor:"idempotency_key"`
	SemanticPayloadHashVersion uint32                  `cbor:"semantic_payload_hash_version"`
	SemanticPayloadSHA256      []byte                  `cbor:"semantic_payload_sha256"`
	CommandKind                uint32                  `cbor:"command_kind"`
	ResultKind                 uint32                  `cbor:"result_kind"`
	TerminalState              uint32                  `cbor:"terminal_state"`
	ResultCBORSHA256           []byte                  `cbor:"result_cbor_sha256"`
	ErrorCode                  string                  `cbor:"error_code"`
	EffectSequence             uint64                  `cbor:"effect_sequence"`
	AcceptedAtSeconds          int64                   `cbor:"accepted_at_seconds"`
	AcceptedAtNanos            uint32                  `cbor:"accepted_at_nanos"`
	CompletedAtSeconds         int64                   `cbor:"completed_at_seconds"`
	CompletedAtNanos           uint32                  `cbor:"completed_at_nanos"`
	Replayed                   bool                    `cbor:"replayed"`
	Certificate                *cborCertificateBinding `cbor:"certificate"`
}

type cborRegistrationV1 struct {
	Version                 uint32 `cbor:"version"`
	NodeID                  []byte `cbor:"node_id"`
	PrivdAttestationKeyID   string `cbor:"privd_attestation_key_id"`
	PublicKey               []byte `cbor:"public_key"`
	ControllerNonce         []byte `cbor:"controller_nonce"`
	CredentialContextSHA256 []byte `cbor:"credential_context_sha256"`
}

type cborUpgradeResultProofV1 struct {
	Version               uint32 `cbor:"version"`
	NodeID                []byte `cbor:"node_id"`
	PrivdAttestationKeyID string `cbor:"privd_attestation_key_id"`
	OperationID           []byte `cbor:"operation_id"`
	TargetVersion         string `cbor:"target_version"`
	PackageSHA256         []byte `cbor:"package_sha256"`
	State                 uint32 `cbor:"state"`
	CompletedUnixMs       uint64 `cbor:"completed_unix_ms"`
	ResultSHA256          []byte `cbor:"result_sha256"`
	AttestedUnixMs        uint64 `cbor:"attested_unix_ms"`
}

type cborResultPayloadEnvelope struct {
	ResultKind uint32 `cbor:"result_kind"`
	Payload    any    `cbor:"payload"`
}

type cborEmptyResult struct{}

type cborConfigPlanResult struct {
	CandidateHash    []byte   `cbor:"candidate_hash"`
	DiffRedacted     string   `cbor:"diff_redacted"`
	Warnings         []string `cbor:"warnings"`
	CurrentUnchanged bool     `cbor:"current_unchanged"`
	StagingCleaned   bool     `cbor:"staging_cleaned"`
	CurrentHash      []byte   `cbor:"current_hash"`
	MaterializedHash []byte   `cbor:"materialized_hash"`
}

type cborConfigApplyResult struct {
	CandidateHash   []byte `cbor:"candidate_hash"`
	PreviousHash    []byte `cbor:"previous_hash"`
	ObservedHash    []byte `cbor:"observed_hash"`
	AppliedRevision uint64 `cbor:"applied_revision"`
	Healthy         bool   `cbor:"healthy"`
	RolledBack      bool   `cbor:"rolled_back"`
	FailedCritical  bool   `cbor:"failed_critical"`
	FailureCode     string `cbor:"failure_code"`
}

type cborCertificateCSRResult struct {
	CertificateID   []byte `cbor:"certificate_id"`
	CSRDer          []byte `cbor:"csr_der"`
	PublicKeySHA256 []byte `cbor:"public_key_sha256"`
}

type cborCertificateArtifactResult struct {
	CertificateID  []byte `cbor:"certificate_id"`
	ArtifactID     []byte `cbor:"artifact_id"`
	ArtifactSHA256 []byte `cbor:"artifact_sha256"`
	ArtifactSize   uint64 `cbor:"artifact_size"`
}

type cborCertificateRevokeResult struct {
	CertificateID []byte `cbor:"certificate_id"`
	KeyRemoved    bool   `cbor:"key_removed"`
}

type cborAgentUpgradeScheduledResult struct {
	OperationID   []byte `cbor:"operation_id"`
	TargetVersion string `cbor:"target_version"`
	PackageSHA256 []byte `cbor:"package_sha256"`
}

func nonNilBytes(b []byte) []byte {
	if len(b) == 0 {
		return []byte{}
	}
	return b
}

func nonNilStrings(s []string) []string {
	if len(s) == 0 {
		return []string{}
	}
	return s
}

// ResultPayloadCBORHash computes the canonical CBOR SHA-256 over the structured
// command result message, independent of Protobuf wire serialization.
func ResultPayloadCBORHash(kind agentv1.PrivilegedResultKind, raw []byte) ([sha256.Size]byte, error) {
	var structured any
	switch kind {
	case agentv1.PrivilegedResultKind_PRIVILEGED_RESULT_KIND_MUTATION,
		agentv1.PrivilegedResultKind_PRIVILEGED_RESULT_KIND_ERROR:
		if len(raw) != 0 {
			return [sha256.Size]byte{}, errors.New("unexpected result bytes for empty result kind")
		}
		structured = cborEmptyResult{}
	case agentv1.PrivilegedResultKind_PRIVILEGED_RESULT_KIND_CONFIG_PLAN:
		var msg agentv1.ConfigPlanResult
		if err := proto.Unmarshal(raw, &msg); err != nil {
			return [sha256.Size]byte{}, err
		}
		structured = cborConfigPlanResult{
			CandidateHash:    nonNilBytes(msg.GetCandidateHash()),
			DiffRedacted:     msg.GetDiffRedacted(),
			Warnings:         nonNilStrings(msg.GetWarnings()),
			CurrentUnchanged: msg.GetCurrentUnchanged(),
			StagingCleaned:   msg.GetStagingCleaned(),
			CurrentHash:      nonNilBytes(msg.GetCurrentHash()),
			MaterializedHash: nonNilBytes(msg.GetMaterializedHash()),
		}
	case agentv1.PrivilegedResultKind_PRIVILEGED_RESULT_KIND_CONFIG_APPLY:
		var msg agentv1.ConfigApplyResult
		if err := proto.Unmarshal(raw, &msg); err != nil {
			return [sha256.Size]byte{}, err
		}
		structured = cborConfigApplyResult{
			CandidateHash:   nonNilBytes(msg.GetCandidateHash()),
			PreviousHash:    nonNilBytes(msg.GetPreviousHash()),
			ObservedHash:    nonNilBytes(msg.GetObservedHash()),
			AppliedRevision: msg.GetAppliedRevision(),
			Healthy:         msg.GetHealthy(),
			RolledBack:      msg.GetRolledBack(),
			FailedCritical:  msg.GetFailedCritical(),
			FailureCode:     msg.GetFailureCode(),
		}
	case agentv1.PrivilegedResultKind_PRIVILEGED_RESULT_KIND_CERTIFICATE_CSR:
		var msg agentv1.CertificateCsrResult
		if err := proto.Unmarshal(raw, &msg); err != nil {
			return [sha256.Size]byte{}, err
		}
		structured = cborCertificateCSRResult{
			CertificateID:   nonNilBytes(msg.GetCertificateId()),
			CSRDer:          nonNilBytes(msg.GetCsrDer()),
			PublicKeySHA256: nonNilBytes(msg.GetPublicKeySha256()),
		}
	case agentv1.PrivilegedResultKind_PRIVILEGED_RESULT_KIND_CERTIFICATE_P12:
		var msg agentv1.CertificateArtifactResult
		if err := proto.Unmarshal(raw, &msg); err != nil {
			return [sha256.Size]byte{}, err
		}
		structured = cborCertificateArtifactResult{
			CertificateID:  nonNilBytes(msg.GetCertificateId()),
			ArtifactID:     nonNilBytes(msg.GetArtifactId()),
			ArtifactSHA256: nonNilBytes(msg.GetArtifactSha256()),
			ArtifactSize:   msg.GetArtifactSize(),
		}
	case agentv1.PrivilegedResultKind_PRIVILEGED_RESULT_KIND_CERTIFICATE_REVOKE:
		var msg agentv1.CertificateRevokeResult
		if err := proto.Unmarshal(raw, &msg); err != nil {
			return [sha256.Size]byte{}, err
		}
		structured = cborCertificateRevokeResult{
			CertificateID: nonNilBytes(msg.GetCertificateId()),
			KeyRemoved:    msg.GetKeyRemoved(),
		}
	case agentv1.PrivilegedResultKind_PRIVILEGED_RESULT_KIND_AGENT_UPGRADE_SCHEDULED:
		var msg agentv1.AgentUpgradeScheduledResult
		if err := proto.Unmarshal(raw, &msg); err != nil {
			return [sha256.Size]byte{}, err
		}
		structured = cborAgentUpgradeScheduledResult{
			OperationID:   nonNilBytes(msg.GetOperationId()),
			TargetVersion: msg.GetTargetVersion(),
			PackageSHA256: nonNilBytes(msg.GetPackageSha256()),
		}
	default:
		return [sha256.Size]byte{}, errors.New("unsupported privileged result kind")
	}
	return canonicalcbor.Hash(resultPayloadDomain, cborResultPayloadEnvelope{
		ResultKind: uint32(kind),
		Payload:    structured,
	})
}

func VerificationMetrics() []VerificationMetric {
	metrics := make([]VerificationMetric, 0, len(verificationMetricDefinitions))
	for index := range verificationMetricDefinitions {
		definition := &verificationMetricDefinitions[index]
		metrics = append(metrics, VerificationMetric{Outcome: definition.status, Reason: definition.reason, Total: definition.counter.Load()})
	}
	return metrics
}

func recordVerificationMetric(verification Verification) {
	reason := verification.FailureReason
	if reason == "" {
		reason = verification.Status
	}
	for index := range verificationMetricDefinitions {
		definition := &verificationMetricDefinitions[index]
		if definition.status == verification.Status && definition.reason == reason {
			definition.counter.Add(1)
			return
		}
	}
}

// VerifyResultTransaction reads the key inside the caller-owned result transaction.
func VerifyResultTransaction(ctx context.Context, tx database.Tx, nodeID uuid.UUID, envelope *agentv1.CommandEnvelope, result *agentv1.CommandResult) Verification {
	return verifyResult(ctx, transactionKeyLookup(tx), nodeID, envelope, result)
}

func transactionKeyLookup(tx database.Tx) keyLookup {
	return func(ctx context.Context, node uuid.UUID, key string) (attestationKeyRecord, error) {
		if tx == nil {
			return attestationKeyRecord{}, database.ErrUnsupported
		}
		return StoreFrom(tx).VerificationKey(ctx, node, key)
	}
}

func verifyResult(ctx context.Context, lookup keyLookup, nodeID uuid.UUID, envelope *agentv1.CommandEnvelope, result *agentv1.CommandResult) (verification Verification) {
	defer func() { recordVerificationMetric(verification) }()
	if CommandKind(envelope) == agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_UNSPECIFIED {
		return Verification{Status: "not_required"}
	}
	switch result.GetState() {
	case agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED, agentv1.CommandResultState_COMMAND_RESULT_STATE_FAILED:
		// Privd signs these terminal results.
	case agentv1.CommandResultState_COMMAND_RESULT_STATE_REJECTED:
		// Rejection is only an Agent assertion. It cannot settle a privileged command.
		if result.GetPrivilegedResultProof() == nil {
			return Verification{Status: "missing", FailureReason: "receipt_missing"}
		}
		return Verification{Status: "invalid", FailureReason: "receipt_claim_mismatch"}
	case agentv1.CommandResultState_COMMAND_RESULT_STATE_UNKNOWN:
		// Agent and transport can normalize malformed terminal frames to Unknown.
		switch result.GetErrorCode() {
		case "receipt_missing":
			return Verification{Status: "missing", FailureReason: "receipt_missing"}
		case "receipt_invalid", "privd_receipt_missing_or_malformed":
			return Verification{Status: "invalid", FailureReason: "receipt_malformed"}
		}
		return Verification{Status: "not_required"}
	default:
		return Verification{Status: "not_required"}
	}
	proof := result.GetPrivilegedResultProof()
	if proof == nil {
		return Verification{Status: "missing", FailureReason: "receipt_missing"}
	}
	encoded, err := proto.Marshal(proof)
	if err != nil || len(encoded) == 0 || len(encoded) > 64*1024 {
		return Verification{Status: "invalid", FailureReason: "receipt_malformed"}
	}
	receipt := proof.GetReceiptV1()
	if proof.GetVersion() != 1 || receipt == nil || receipt.GetReceiptVersion() != 1 {
		return Verification{Status: "invalid", FailureReason: "receipt_version_unsupported", EncodedProof: encoded}
	}
	canonical, err := CanonicalReceiptV1(receipt)
	if err != nil {
		return Verification{Status: "invalid", FailureReason: "receipt_malformed", EncodedProof: encoded}
	}
	verification = Verification{
		Status:         "invalid",
		FailureReason:  "receipt_claim_mismatch",
		KeyID:          receipt.GetPrivdAttestationKeyId(),
		EffectSequence: receipt.GetEffectSequence(),
		EncodedProof:   encoded,
	}
	if receipt.GetCertificate() != nil {
		verification.Certificate = proto.Clone(receipt.GetCertificate()).(*agentv1.PrivdCertificateReceiptBindingV1)
	}
	digest := sha256.Sum256(canonical)
	verification.ReceiptSHA256 = digest[:]
	if !receiptClaimsMatch(nodeID, envelope, result, receipt) {
		return verification
	}
	record, err := lookup(ctx, nodeID, receipt.GetPrivdAttestationKeyId())
	if errors.Is(err, database.ErrNotFound) {
		verification.Status, verification.FailureReason = "unknown_key", "receipt_key_unknown"
		return verification
	}
	if err != nil {
		verification.FailureReason = "receipt_key_lookup_failed"
		return verification
	}
	completedAt := receipt.GetCompletedAt().AsTime()
	if record.State != "active" || len(record.PublicKey) != ed25519.PublicKeySize {
		verification.Status, verification.FailureReason = "revoked_key", "receipt_key_revoked"
		return verification
	}
	if completedAt.Before(record.ActivatedAt.Add(-receiptClockSkew)) || record.ValidUntil != nil && completedAt.After(*record.ValidUntil) {
		verification.FailureReason = "receipt_key_outside_validity"
		return verification
	}
	if len(proof.GetSignature()) != ed25519.SignatureSize || !ed25519.Verify(ed25519.PublicKey(record.PublicKey), canonical, proof.GetSignature()) {
		verification.FailureReason = "receipt_signature_invalid"
		return verification
	}
	verification.Status, verification.FailureReason = "verified", ""
	return verification
}

func VerifyUpgradeResultTransaction(ctx context.Context, tx database.Tx, nodeID, operationID uuid.UUID, state agentv1.AgentUpgradeOutcomeState, targetVersion string, completedAt time.Time, proof *agentv1.AgentUpgradeResultProof) (UpgradeResultVerification, error) {
	return VerifyUpgradeResultWithLookup(ctx, transactionKeyLookup(tx), nodeID, operationID, state, targetVersion, completedAt, proof)
}

// UpgradeKey is the read-only key material needed by upgrade proof verification.
type UpgradeKey = attestationKeyRecord

// VerifyUpgradeResultWithLookup verifies the same proof using the enclosing transaction's key store.
func VerifyUpgradeResultWithLookup(ctx context.Context, lookup func(context.Context, uuid.UUID, string) (UpgradeKey, error), nodeID, operationID uuid.UUID, state agentv1.AgentUpgradeOutcomeState, targetVersion string, completedAt time.Time, proof *agentv1.AgentUpgradeResultProof) (verification UpgradeResultVerification, err error) {
	encoded, marshalErr := proto.MarshalOptions{Deterministic: true}.Marshal(proof)
	if marshalErr != nil || len(encoded) == 0 || len(encoded) > 64*1024 {
		return UpgradeResultVerification{Status: "invalid", FailureReason: "upgrade_result_proof_malformed"}, nil
	}
	verification = UpgradeResultVerification{Status: "invalid", FailureReason: "upgrade_result_claim_mismatch", EncodedProof: encoded}
	canonical, canonicalErr := CanonicalAgentUpgradeResultProofV1(proof)
	if canonicalErr != nil {
		verification.FailureReason = "upgrade_result_proof_malformed"
		return verification, nil
	}
	verification.KeyID = proof.GetPrivdAttestationKeyId()
	verification.PackageSHA256 = slices.Clone(proof.GetPackageSha256())
	verification.ResultSHA256 = slices.Clone(proof.GetResultSha256())
	verification.CompletedAt = time.UnixMilli(int64(proof.GetCompletedUnixMs())).UTC()
	attestedAt := time.UnixMilli(int64(proof.GetAttestedUnixMs())).UTC()
	if completedAt.IsZero() || completedAt.UnixMilli() < 0 || uint64(completedAt.UnixMilli()) != proof.GetCompletedUnixMs() ||
		!bytes.Equal(proof.GetNodeId(), nodeID[:]) || !bytes.Equal(proof.GetOperationId(), operationID[:]) ||
		proof.GetState() != state || proof.GetTargetVersion() != targetVersion {
		return verification, nil
	}
	if lookup == nil {
		return UpgradeResultVerification{}, errors.New("privd upgrade result key transaction is unavailable")
	}
	record, err := lookup(ctx, nodeID, proof.GetPrivdAttestationKeyId())
	if errors.Is(err, database.ErrNotFound) {
		verification.Status, verification.FailureReason = "unknown_key", "upgrade_result_key_unknown"
		return verification, nil
	}
	if err != nil {
		return UpgradeResultVerification{}, fmt.Errorf("lookup upgrade result attestation key: %w", err)
	}
	if record.State != "active" || len(record.PublicKey) != ed25519.PublicKeySize {
		verification.Status, verification.FailureReason = "revoked_key", "upgrade_result_key_revoked"
		return verification, nil
	}
	if attestedAt.Before(record.ActivatedAt) || record.ValidUntil != nil && attestedAt.After(*record.ValidUntil) {
		verification.FailureReason = "upgrade_result_key_outside_validity"
		return verification, nil
	}
	if len(proof.GetSignature()) != ed25519.SignatureSize || !ed25519.Verify(ed25519.PublicKey(record.PublicKey), canonical, proof.GetSignature()) {
		verification.FailureReason = "upgrade_result_signature_invalid"
		return verification, nil
	}
	verification.Status, verification.FailureReason = "verified", ""
	return verification, nil
}

// CanonicalAgentUpgradeResultProofV1 reconstructs the signed durable outcome
// transcript using RFC 8949 Core Deterministic CBOR.
func CanonicalAgentUpgradeResultProofV1(proof *agentv1.AgentUpgradeResultProof) ([]byte, error) {
	if !validAgentUpgradeResultProof(proof) {
		return nil, errors.New("agent upgrade result proof is malformed")
	}
	output, err := canonicalcbor.Marshal(upgradeResultDomain, cborUpgradeResultProofV1{
		Version:               proof.GetVersion(),
		NodeID:                proof.GetNodeId(),
		PrivdAttestationKeyID: proof.GetPrivdAttestationKeyId(),
		OperationID:           proof.GetOperationId(),
		TargetVersion:         proof.GetTargetVersion(),
		PackageSHA256:         proof.GetPackageSha256(),
		State:                 uint32(proof.GetState()),
		CompletedUnixMs:       proof.GetCompletedUnixMs(),
		ResultSHA256:          proof.GetResultSha256(),
		AttestedUnixMs:        proof.GetAttestedUnixMs(),
	})
	if err != nil {
		return nil, err
	}
	if len(output) > maxCanonicalBytes {
		return nil, errors.New("agent upgrade result proof is oversized")
	}
	return output, nil
}

func validAgentUpgradeResultProof(proof *agentv1.AgentUpgradeResultProof) bool {
	if proof == nil || proof.GetVersion() != 1 ||
		!validUUIDv7(proof.GetNodeId()) || !validUUIDv7(proof.GetOperationId()) ||
		!validKeyID(proof.GetPrivdAttestationKeyId()) ||
		!semanticpayload.ValidAgentUpgradeTargetVersion(proof.GetTargetVersion()) ||
		len(proof.GetPackageSha256()) != sha256.Size || len(proof.GetResultSha256()) != sha256.Size ||
		(proof.GetState() != agentv1.AgentUpgradeOutcomeState_AGENT_UPGRADE_OUTCOME_STATE_SUCCEEDED &&
			proof.GetState() != agentv1.AgentUpgradeOutcomeState_AGENT_UPGRADE_OUTCOME_STATE_FAILED &&
			proof.GetState() != agentv1.AgentUpgradeOutcomeState_AGENT_UPGRADE_OUTCOME_STATE_ROLLED_BACK) ||
		proof.GetCompletedUnixMs() == 0 || proof.GetCompletedUnixMs() > math.MaxInt64 ||
		proof.GetAttestedUnixMs() == 0 || proof.GetAttestedUnixMs() > math.MaxInt64 ||
		proof.GetAttestedUnixMs() < proof.GetCompletedUnixMs() {
		return false
	}
	return true
}

func CanonicalReceiptV1(receipt *agentv1.PrivdResultReceiptV1) ([]byte, error) {
	if !validReceipt(receipt) {
		return nil, errors.New("privd receipt is malformed")
	}
	var cert *cborCertificateBinding
	if binding := receipt.GetCertificate(); binding != nil {
		cert = &cborCertificateBinding{
			CertificateID:   binding.GetCertificateId(),
			CSRDerSHA256:    nonNilBytes(binding.GetCsrDerSha256()),
			PublicKeySHA256: nonNilBytes(binding.GetPublicKeySha256()),
		}
	}
	output, err := canonicalcbor.Marshal(receiptDomain, cborReceiptV1{
		ReceiptVersion:             receipt.GetReceiptVersion(),
		NodeID:                     receipt.GetNodeId(),
		PrivdAttestationKeyID:      receipt.GetPrivdAttestationKeyId(),
		CommandID:                  receipt.GetCommandId(),
		OperationID:                receipt.GetOperationId(),
		IdempotencyKey:             receipt.GetIdempotencyKey(),
		SemanticPayloadHashVersion: uint32(receipt.GetSemanticPayloadHashVersion()),
		SemanticPayloadSHA256:      receipt.GetSemanticPayloadSha256(),
		CommandKind:                uint32(receipt.GetCommandKind()),
		ResultKind:                 uint32(receipt.GetResultKind()),
		TerminalState:              uint32(receipt.GetTerminalState()),
		ResultCBORSHA256:           receipt.GetResultCborSha256(),
		ErrorCode:                  receipt.GetErrorCode(),
		EffectSequence:             receipt.GetEffectSequence(),
		AcceptedAtSeconds:          receipt.GetAcceptedAt().GetSeconds(),
		AcceptedAtNanos:            uint32(receipt.GetAcceptedAt().GetNanos()),
		CompletedAtSeconds:         receipt.GetCompletedAt().GetSeconds(),
		CompletedAtNanos:           uint32(receipt.GetCompletedAt().GetNanos()),
		Replayed:                   receipt.GetReplayed(),
		Certificate:                cert,
	})
	if err != nil {
		return nil, err
	}
	if len(output) > maxCanonicalBytes {
		return nil, errors.New("privd receipt canonical form is oversized")
	}
	return output, nil
}

func CanonicalRegistrationV1(registration *agentv1.PrivdAttestationRegistrationV1) ([]byte, error) {
	if registration == nil || registration.GetVersion() != 1 || !validUUIDv7(registration.GetNodeId()) || len(registration.GetPublicKey()) != ed25519.PublicKeySize || len(registration.GetControllerNonce()) != 32 || len(registration.GetCredentialContextSha256()) != 32 || !validKeyID(registration.GetPrivdAttestationKeyId()) || registration.GetPrivdAttestationKeyId() != PublicKeyID(registration.GetPublicKey()) {
		return nil, errors.New("privd key registration is malformed")
	}
	return canonicalcbor.Marshal(registrationDomain, cborRegistrationV1{
		Version:                 registration.GetVersion(),
		NodeID:                  registration.GetNodeId(),
		PrivdAttestationKeyID:   registration.GetPrivdAttestationKeyId(),
		PublicKey:               registration.GetPublicKey(),
		ControllerNonce:         registration.GetControllerNonce(),
		CredentialContextSHA256: registration.GetCredentialContextSha256(),
	})
}

func PublicKeyID(publicKey []byte) string {
	digest := sha256.Sum256(publicKey)
	return fmt.Sprintf("%s%x", keyIDPrefix, digest)
}

func receiptClaimsMatch(nodeID uuid.UUID, envelope *agentv1.CommandEnvelope, result *agentv1.CommandResult, receipt *agentv1.PrivdResultReceiptV1) bool {
	expectedResultKind := ResultKind(envelope, result)
	resultDigest, err := ResultPayloadCBORHash(expectedResultKind, result.GetResult())
	if err != nil {
		return false
	}
	if !bytes.Equal(receipt.GetNodeId(), nodeID[:]) || !bytes.Equal(receipt.GetNodeId(), envelope.GetNodeId()) ||
		!bytes.Equal(receipt.GetCommandId(), result.GetCommandId()) || !bytes.Equal(receipt.GetCommandId(), envelope.GetCommandId()) ||
		!bytes.Equal(receipt.GetOperationId(), envelope.GetOperationId()) ||
		!bytes.Equal(receipt.GetIdempotencyKey(), result.GetIdempotencyKey()) || !bytes.Equal(receipt.GetIdempotencyKey(), envelope.GetIdempotencyKey()) ||
		receipt.GetSemanticPayloadHashVersion() != result.GetSemanticPayloadHashVersion() || receipt.GetSemanticPayloadHashVersion() != envelope.GetSemanticPayloadHashVersion() ||
		!bytes.Equal(receipt.GetSemanticPayloadSha256(), result.GetPayloadSha256()) || !bytes.Equal(receipt.GetSemanticPayloadSha256(), envelope.GetSemanticPayloadSha256()) ||
		receipt.GetCommandKind() != CommandKind(envelope) || receipt.GetResultKind() != expectedResultKind ||
		receipt.GetTerminalState() != result.GetState() || !bytes.Equal(receipt.GetResultCborSha256(), resultDigest[:]) || receipt.GetErrorCode() != result.GetErrorCode() ||
		!proto.Equal(receipt.GetAcceptedAt(), result.GetAcceptedAt()) || !proto.Equal(receipt.GetCompletedAt(), result.GetCompletedAt()) || receipt.GetReplayed() != result.GetReplayed() ||
		!receiptWithinCommandWindow(envelope, receipt) {
		return false
	}
	return certificateClaimsMatch(envelope, result, receipt)
}

const receiptClockSkew = 5 * time.Minute

func receiptWithinCommandWindow(envelope *agentv1.CommandEnvelope, receipt *agentv1.PrivdResultReceiptV1) bool {
	issued, expires := envelope.GetIssuedAt(), envelope.GetExpiresAt()
	accepted, completed := receipt.GetAcceptedAt(), receipt.GetCompletedAt()
	if !validTimestamp(issued) || !validTimestamp(expires) || !validTimestamp(accepted) || !validTimestamp(completed) || issued.AsTime().After(expires.AsTime()) {
		return false
	}
	// Match the second-precision lower bound enforced by Privd. Completion
	// and expiry comparisons preserve the full timestamp precision.
	deadline := expires.AsTime().Add(receiptClockSkew)
	return accepted.GetSeconds() >= issued.GetSeconds()-int64(receiptClockSkew/time.Second) &&
		!accepted.AsTime().After(deadline) && !completed.AsTime().Before(accepted.AsTime()) && !completed.AsTime().After(deadline)
}

func certificateClaimsMatch(envelope *agentv1.CommandEnvelope, result *agentv1.CommandResult, receipt *agentv1.PrivdResultReceiptV1) bool {
	binding := receipt.GetCertificate()
	switch request := envelope.GetPayload().(type) {
	case *agentv1.CommandEnvelope_CertificateCsr:
		if result.GetState() == agentv1.CommandResultState_COMMAND_RESULT_STATE_FAILED {
			return binding != nil && bytes.Equal(binding.GetCertificateId(), request.CertificateCsr.GetCertificateId()) && certificateBindingEmpty(binding)
		}
		if binding == nil || !bytes.Equal(binding.GetCertificateId(), request.CertificateCsr.GetCertificateId()) {
			return false
		}
		var csrResult agentv1.CertificateCsrResult
		if proto.Unmarshal(result.GetResult(), &csrResult) != nil {
			return false
		}
		csrDigest := sha256.Sum256(csrResult.GetCsrDer())
		return bytes.Equal(binding.GetCsrDerSha256(), csrDigest[:]) && bytes.Equal(binding.GetPublicKeySha256(), csrResult.GetPublicKeySha256())
	case *agentv1.CommandEnvelope_CertificateP12:
		return binding != nil && bytes.Equal(binding.GetCertificateId(), request.CertificateP12.GetCertificateId()) && certificateBindingEmpty(binding)
	case *agentv1.CommandEnvelope_CertificateRevoke:
		return binding != nil && bytes.Equal(binding.GetCertificateId(), request.CertificateRevoke.GetCertificateId()) && certificateBindingEmpty(binding)
	default:
		return binding == nil
	}
}

func certificateBindingEmpty(binding *agentv1.PrivdCertificateReceiptBindingV1) bool {
	return len(binding.GetCsrDerSha256()) == 0 && len(binding.GetPublicKeySha256()) == 0
}

func validReceipt(receipt *agentv1.PrivdResultReceiptV1) bool {
	if receipt == nil || receipt.GetReceiptVersion() != 1 || !validUUIDv7(receipt.GetNodeId()) || !validUUIDv7(receipt.GetCommandId()) || !validUUIDv7(receipt.GetOperationId()) || !validUUIDv7(receipt.GetIdempotencyKey()) || !validKeyID(receipt.GetPrivdAttestationKeyId()) || len(receipt.GetSemanticPayloadSha256()) != 32 || len(receipt.GetResultCborSha256()) != 32 || len(receipt.GetErrorCode()) > maxErrorCodeBytes || receipt.GetEffectSequence() == 0 || receipt.GetEffectSequence() > math.MaxInt64 || !validTimestamp(receipt.GetAcceptedAt()) || !validTimestamp(receipt.GetCompletedAt()) || receipt.GetAcceptedAt().AsTime().After(receipt.GetCompletedAt().AsTime()) {
		return false
	}
	if receipt.GetSemanticPayloadHashVersion() != agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V2 || receipt.GetCommandKind() == agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_UNSPECIFIED || receipt.GetResultKind() == agentv1.PrivilegedResultKind_PRIVILEGED_RESULT_KIND_UNSPECIFIED || receipt.GetTerminalState() != agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED && receipt.GetTerminalState() != agentv1.CommandResultState_COMMAND_RESULT_STATE_FAILED {
		return false
	}
	certificate := receipt.GetCertificate()
	certificateCommand := receipt.GetCommandKind() == agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_CERTIFICATE_CSR || receipt.GetCommandKind() == agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_CERTIFICATE_P12 || receipt.GetCommandKind() == agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_CERTIFICATE_REVOKE
	if certificate == nil {
		return !certificateCommand
	}
	if !certificateCommand || !validUUIDv7(certificate.GetCertificateId()) {
		return false
	}
	if receipt.GetResultKind() == agentv1.PrivilegedResultKind_PRIVILEGED_RESULT_KIND_CERTIFICATE_CSR {
		return len(certificate.GetCsrDerSha256()) == 32 && len(certificate.GetPublicKeySha256()) == 32
	}
	return len(certificate.GetCsrDerSha256()) == 0 && len(certificate.GetPublicKeySha256()) == 0
}

// CommandKind maps a CommandEnvelope payload to its attested PrivilegedCommandKind.
func CommandKind(envelope *agentv1.CommandEnvelope) agentv1.PrivilegedCommandKind {
	switch envelope.GetPayload().(type) {
	case *agentv1.CommandEnvelope_SessionDisconnect:
		return agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_SESSION_DISCONNECT
	case *agentv1.CommandEnvelope_SessionTerminate:
		return agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_SESSION_TERMINATE
	case *agentv1.CommandEnvelope_IpBanRemove:
		return agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_IP_BAN_REMOVE
	case *agentv1.CommandEnvelope_ServiceReload:
		return agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_SERVICE_RELOAD
	case *agentv1.CommandEnvelope_UserCreate:
		return agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_USER_CREATE
	case *agentv1.CommandEnvelope_UserDisable:
		return agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_USER_DISABLE
	case *agentv1.CommandEnvelope_UserEnable:
		return agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_USER_ENABLE
	case *agentv1.CommandEnvelope_UserPasswordRotate:
		return agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_USER_PASSWORD_ROTATE
	case *agentv1.CommandEnvelope_GroupApply:
		return agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_GROUP_APPLY
	case *agentv1.CommandEnvelope_ConfigPlan, *agentv1.CommandEnvelope_CompleteConfigPlan:
		return agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_CONFIG_PLAN
	case *agentv1.CommandEnvelope_ConfigApply, *agentv1.CommandEnvelope_CompleteConfigApply:
		return agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_CONFIG_APPLY
	case *agentv1.CommandEnvelope_CertificateCsr:
		return agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_CERTIFICATE_CSR
	case *agentv1.CommandEnvelope_CertificateP12:
		return agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_CERTIFICATE_P12
	case *agentv1.CommandEnvelope_CertificateRevoke:
		return agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_CERTIFICATE_REVOKE
	case *agentv1.CommandEnvelope_AgentUpgrade:
		return agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_AGENT_UPGRADE
	default:
		return agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_UNSPECIFIED
	}
}

// ResultKind maps a CommandEnvelope and CommandResult state to its expected PrivilegedResultKind.
func ResultKind(envelope *agentv1.CommandEnvelope, result *agentv1.CommandResult) agentv1.PrivilegedResultKind {
	if result.GetState() == agentv1.CommandResultState_COMMAND_RESULT_STATE_FAILED {
		return agentv1.PrivilegedResultKind_PRIVILEGED_RESULT_KIND_ERROR
	}
	switch CommandKind(envelope) {
	case agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_CONFIG_PLAN:
		return agentv1.PrivilegedResultKind_PRIVILEGED_RESULT_KIND_CONFIG_PLAN
	case agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_CONFIG_APPLY:
		return agentv1.PrivilegedResultKind_PRIVILEGED_RESULT_KIND_CONFIG_APPLY
	case agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_CERTIFICATE_CSR:
		return agentv1.PrivilegedResultKind_PRIVILEGED_RESULT_KIND_CERTIFICATE_CSR
	case agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_CERTIFICATE_P12:
		return agentv1.PrivilegedResultKind_PRIVILEGED_RESULT_KIND_CERTIFICATE_P12
	case agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_CERTIFICATE_REVOKE:
		return agentv1.PrivilegedResultKind_PRIVILEGED_RESULT_KIND_CERTIFICATE_REVOKE
	case agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_AGENT_UPGRADE:
		return agentv1.PrivilegedResultKind_PRIVILEGED_RESULT_KIND_AGENT_UPGRADE_SCHEDULED
	default:
		return agentv1.PrivilegedResultKind_PRIVILEGED_RESULT_KIND_MUTATION
	}
}

func validTimestamp(value *timestamppb.Timestamp) bool {
	return value != nil && value.CheckValid() == nil && value.GetSeconds() >= 0
}
func validUUIDv7(value []byte) bool {
	id, err := uuid.FromBytes(value)
	return err == nil && id.Version() == 7
}
func validKeyID(value string) bool {
	if len(value) != len(keyIDPrefix)+64 || !strings.HasPrefix(value, keyIDPrefix) {
		return false
	}
	for _, ch := range value[len(keyIDPrefix):] {
		if ch < '0' || ch > '9' && ch < 'a' || ch > 'f' {
			return false
		}
	}
	return true
}
