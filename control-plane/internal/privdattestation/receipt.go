package privdattestation

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	receiptDomain      = "ocservia/privd-result-receipt/v1\x00"
	registrationDomain = "ocservia/privd-attestation-registration/v1\x00"
	keyIDPrefix        = "ed25519-sha256:"
	maxCanonicalBytes  = 2048
)

type Verification struct {
	Status         string
	FailureReason  string
	KeyID          string
	EffectRecordID []byte
	EffectSequence uint64
	ReceiptSHA256  []byte
	EncodedProof   []byte
	Certificate    *agentv1.PrivdCertificateReceiptBindingV1
}

func (v Verification) Verified() bool { return v.Status == "verified" }

type attestationKeyRecord struct {
	PublicKey   []byte
	State       string
	ActivatedAt time.Time
	ValidUntil  *time.Time
}

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

func VerifyResult(ctx context.Context, tx pgx.Tx, nodeID uuid.UUID, envelope *agentv1.CommandEnvelope, result *agentv1.CommandResult) Verification {
	return verifyResult(ctx, func(ctx context.Context, nodeID uuid.UUID, keyID string) (attestationKeyRecord, error) {
		if tx == nil {
			return attestationKeyRecord{}, errors.New("privd receipt key transaction is unavailable")
		}
		var record attestationKeyRecord
		err := tx.QueryRow(ctx, `SELECT public_key,state,activated_at,valid_until FROM node_privd_attestation_keys WHERE node_id=$1 AND key_id=$2`, nodeID, keyID).Scan(&record.PublicKey, &record.State, &record.ActivatedAt, &record.ValidUntil)
		return record, err
	}, nodeID, envelope, result)
}

func verifyResult(ctx context.Context, lookup keyLookup, nodeID uuid.UUID, envelope *agentv1.CommandEnvelope, result *agentv1.CommandResult) (verification Verification) {
	defer func() { recordVerificationMetric(verification) }()
	commandKind := commandKind(envelope)
	if commandKind == agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_UNSPECIFIED ||
		(result.GetState() != agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED && result.GetState() != agentv1.CommandResultState_COMMAND_RESULT_STATE_FAILED) {
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
	if proof.GetVersion() != agentv1.PrivdReceiptVersion_PRIVD_RECEIPT_VERSION_V1 || receipt == nil || receipt.GetReceiptVersion() != agentv1.PrivdReceiptVersion_PRIVD_RECEIPT_VERSION_V1 {
		return Verification{Status: "invalid", FailureReason: "receipt_version_unsupported", EncodedProof: encoded}
	}
	canonical, err := CanonicalReceiptV1(receipt)
	if err != nil {
		return Verification{Status: "invalid", FailureReason: "receipt_malformed", EncodedProof: encoded}
	}
	verification = Verification{
		Status: "invalid", FailureReason: "receipt_claim_mismatch", KeyID: receipt.GetPrivdAttestationKeyId(),
		EffectRecordID: slices.Clone(receipt.GetEffectRecordId()), EffectSequence: receipt.GetEffectSequence(), EncodedProof: encoded,
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
	if errors.Is(err, pgx.ErrNoRows) {
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
	if completedAt.Before(record.ActivatedAt) || record.ValidUntil != nil && completedAt.After(*record.ValidUntil) {
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

func CanonicalReceiptV1(receipt *agentv1.PrivdResultReceiptV1) ([]byte, error) {
	if !validReceipt(receipt) {
		return nil, errors.New("privd receipt is malformed")
	}
	output := make([]byte, 0, 512)
	output = append(output, receiptDomain...)
	output = appendU32(output, uint32(receipt.GetReceiptVersion()))
	output = appendBytes(output, receipt.GetNodeId())
	output = appendBytes(output, []byte(receipt.GetPrivdAttestationKeyId()))
	output = appendBytes(output, receipt.GetCommandId())
	output = appendBytes(output, receipt.GetOperationId())
	output = appendBytes(output, receipt.GetIdempotencyKey())
	output = appendU32(output, uint32(receipt.GetSemanticPayloadHashVersion()))
	output = appendBytes(output, receipt.GetSemanticPayloadSha256())
	output = appendU32(output, uint32(receipt.GetCommandKind()))
	output = appendU32(output, uint32(receipt.GetResultKind()))
	output = appendU32(output, uint32(receipt.GetTerminalState()))
	output = appendBytes(output, receipt.GetResultBytesSha256())
	output = appendBytes(output, receipt.GetErrorCodeSha256())
	output = appendBytes(output, receipt.GetEffectRecordId())
	output = appendU64(output, receipt.GetEffectSequence())
	output = appendTimestamp(output, receipt.GetAcceptedAt())
	output = appendTimestamp(output, receipt.GetCompletedAt())
	if receipt.GetReplayed() {
		output = append(output, 1)
	} else {
		output = append(output, 0)
	}
	if certificate := receipt.GetCertificate(); certificate != nil {
		output = append(output, 1)
		output = appendBytes(output, certificate.GetCertificateId())
		output = appendBytes(output, certificate.GetCsrDerSha256())
		output = appendBytes(output, certificate.GetPublicKeySha256())
		output = appendBytes(output, certificate.GetRequestedSubjectSha256())
		output = appendBytes(output, certificate.GetRootEffectRecordId())
	} else {
		output = append(output, 0)
	}
	if len(output) > maxCanonicalBytes {
		return nil, errors.New("privd receipt canonical form is oversized")
	}
	return output, nil
}

func CanonicalRegistrationV1(registration *agentv1.PrivdAttestationRegistrationV1) ([]byte, error) {
	if registration.GetVersion() != agentv1.PrivdReceiptVersion_PRIVD_RECEIPT_VERSION_V1 || !validUUIDv7(registration.GetNodeId()) || len(registration.GetPublicKey()) != ed25519.PublicKeySize || len(registration.GetControllerNonce()) != 32 || len(registration.GetCredentialContextSha256()) != 32 || !validKeyID(registration.GetPrivdAttestationKeyId()) || registration.GetPrivdAttestationKeyId() != PublicKeyID(registration.GetPublicKey()) {
		return nil, errors.New("privd key registration is malformed")
	}
	output := []byte(registrationDomain)
	output = appendU32(output, uint32(registration.GetVersion()))
	output = appendBytes(output, registration.GetNodeId())
	output = appendBytes(output, []byte(registration.GetPrivdAttestationKeyId()))
	output = appendBytes(output, registration.GetPublicKey())
	output = appendBytes(output, registration.GetControllerNonce())
	output = appendBytes(output, registration.GetCredentialContextSha256())
	return output, nil
}

func PublicKeyID(publicKey []byte) string {
	digest := sha256.Sum256(publicKey)
	return fmt.Sprintf("%s%x", keyIDPrefix, digest)
}

func RequestedSubjectDigest(request *agentv1.CertificateCsr) ([]byte, error) {
	if request == nil || !validUUIDv7(request.GetCertificateId()) || request.GetCommonName() == "" || len(request.GetCommonName()) > 253 || request.GetKeyBits() < 2048 || request.GetKeyBits() > 8192 || len(request.GetDnsNames()) > 64 {
		return nil, errors.New("certificate requested subject is malformed")
	}
	names := slices.Clone(request.GetDnsNames())
	slices.Sort(names)
	for index := 1; index < len(names); index++ {
		if names[index] == names[index-1] {
			return nil, errors.New("certificate requested subject is malformed")
		}
	}
	output := []byte("ocservia/certificate-requested-subject/v1\x00")
	output = appendBytes(output, request.GetCertificateId())
	output = appendBytes(output, []byte(request.GetCommonName()))
	output = appendU32(output, request.GetKeyBits())
	output = appendU32(output, uint32(len(names)))
	for _, name := range names {
		if name == "" || len(name) > 253 || name != strings.ToLower(name) {
			return nil, errors.New("certificate requested subject is malformed")
		}
		output = appendBytes(output, []byte(name))
	}
	digest := sha256.Sum256(output)
	return digest[:], nil
}

func receiptClaimsMatch(nodeID uuid.UUID, envelope *agentv1.CommandEnvelope, result *agentv1.CommandResult, receipt *agentv1.PrivdResultReceiptV1) bool {
	resultDigest := sha256.Sum256(result.GetResult())
	errorDigest := sha256.Sum256([]byte(result.GetErrorCode()))
	if !bytes.Equal(receipt.GetNodeId(), nodeID[:]) || !bytes.Equal(receipt.GetNodeId(), envelope.GetNodeId()) ||
		!bytes.Equal(receipt.GetCommandId(), result.GetCommandId()) || !bytes.Equal(receipt.GetCommandId(), envelope.GetCommandId()) ||
		!bytes.Equal(receipt.GetOperationId(), envelope.GetOperationId()) ||
		!bytes.Equal(receipt.GetIdempotencyKey(), result.GetIdempotencyKey()) || !bytes.Equal(receipt.GetIdempotencyKey(), envelope.GetIdempotencyKey()) ||
		receipt.GetSemanticPayloadHashVersion() != result.GetSemanticPayloadHashVersion() || receipt.GetSemanticPayloadHashVersion() != envelope.GetSemanticPayloadHashVersion() ||
		!bytes.Equal(receipt.GetSemanticPayloadSha256(), result.GetPayloadSha256()) || !bytes.Equal(receipt.GetSemanticPayloadSha256(), envelope.GetSemanticPayloadSha256()) ||
		receipt.GetCommandKind() != commandKind(envelope) || receipt.GetResultKind() != resultKind(envelope, result) ||
		receipt.GetTerminalState() != result.GetState() || !bytes.Equal(receipt.GetResultBytesSha256(), resultDigest[:]) || !bytes.Equal(receipt.GetErrorCodeSha256(), errorDigest[:]) ||
		!proto.Equal(receipt.GetAcceptedAt(), result.GetAcceptedAt()) || !proto.Equal(receipt.GetCompletedAt(), result.GetCompletedAt()) || receipt.GetReplayed() != result.GetReplayed() ||
		!receiptWithinCommandWindow(envelope, receipt) {
		return false
	}
	return certificateClaimsMatch(envelope, result, receipt)
}

func receiptWithinCommandWindow(envelope *agentv1.CommandEnvelope, receipt *agentv1.PrivdResultReceiptV1) bool {
	issued, expires := envelope.GetIssuedAt(), envelope.GetExpiresAt()
	if !validTimestamp(issued) || !validTimestamp(expires) || issued.AsTime().After(expires.AsTime()) {
		return false
	}
	// Agent journal timestamps have whole-second precision. Privd independently
	// enforces the same lower bound before execution; the expiry bound remains
	// nanosecond-exact so a retired overlap key cannot backdate a new receipt.
	return receipt.GetAcceptedAt().GetSeconds() >= issued.GetSeconds() && !receipt.GetCompletedAt().AsTime().After(expires.AsTime())
}

func certificateClaimsMatch(envelope *agentv1.CommandEnvelope, result *agentv1.CommandResult, receipt *agentv1.PrivdResultReceiptV1) bool {
	binding := receipt.GetCertificate()
	switch request := envelope.GetPayload().(type) {
	case *agentv1.CommandEnvelope_CertificateCsr:
		if result.GetState() == agentv1.CommandResultState_COMMAND_RESULT_STATE_FAILED {
			return binding != nil && bytes.Equal(binding.GetCertificateId(), request.CertificateCsr.GetCertificateId()) && bytes.Equal(binding.GetRootEffectRecordId(), receipt.GetEffectRecordId()) && certificateBindingEmpty(binding)
		}
		if binding == nil || !bytes.Equal(binding.GetCertificateId(), request.CertificateCsr.GetCertificateId()) || !bytes.Equal(binding.GetRootEffectRecordId(), result.GetPrivilegedResultProof().GetReceiptV1().GetEffectRecordId()) {
			return false
		}
		var csrResult agentv1.CertificateCsrResult
		if proto.Unmarshal(result.GetResult(), &csrResult) != nil {
			return false
		}
		csrDigest := sha256.Sum256(csrResult.GetCsrDer())
		subjectDigest, err := RequestedSubjectDigest(request.CertificateCsr)
		return err == nil && bytes.Equal(binding.GetCsrDerSha256(), csrDigest[:]) && bytes.Equal(binding.GetPublicKeySha256(), csrResult.GetPublicKeySha256()) && bytes.Equal(binding.GetRequestedSubjectSha256(), subjectDigest)
	case *agentv1.CommandEnvelope_CertificateP12:
		return binding != nil && bytes.Equal(binding.GetCertificateId(), request.CertificateP12.GetCertificateId()) && bytes.Equal(binding.GetRootEffectRecordId(), receipt.GetEffectRecordId()) && certificateBindingEmpty(binding)
	case *agentv1.CommandEnvelope_CertificateRevoke:
		return binding != nil && bytes.Equal(binding.GetCertificateId(), request.CertificateRevoke.GetCertificateId()) && bytes.Equal(binding.GetRootEffectRecordId(), receipt.GetEffectRecordId()) && certificateBindingEmpty(binding)
	default:
		return binding == nil
	}
}

func certificateBindingEmpty(binding *agentv1.PrivdCertificateReceiptBindingV1) bool {
	return len(binding.GetCsrDerSha256()) == 0 && len(binding.GetPublicKeySha256()) == 0 && len(binding.GetRequestedSubjectSha256()) == 0
}

func validReceipt(receipt *agentv1.PrivdResultReceiptV1) bool {
	if receipt == nil || receipt.GetReceiptVersion() != agentv1.PrivdReceiptVersion_PRIVD_RECEIPT_VERSION_V1 || !validUUIDv7(receipt.GetNodeId()) || !validUUIDv7(receipt.GetCommandId()) || !validUUIDv7(receipt.GetOperationId()) || !validUUIDv7(receipt.GetIdempotencyKey()) || !validKeyID(receipt.GetPrivdAttestationKeyId()) || len(receipt.GetSemanticPayloadSha256()) != 32 || len(receipt.GetResultBytesSha256()) != 32 || len(receipt.GetErrorCodeSha256()) != 32 || len(receipt.GetEffectRecordId()) < 16 || len(receipt.GetEffectRecordId()) > 32 || receipt.GetEffectSequence() == 0 || receipt.GetEffectSequence() > math.MaxInt64 || !validTimestamp(receipt.GetAcceptedAt()) || !validTimestamp(receipt.GetCompletedAt()) || receipt.GetAcceptedAt().AsTime().After(receipt.GetCompletedAt().AsTime()) {
		return false
	}
	if receipt.GetSemanticPayloadHashVersion() != agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V1 && receipt.GetSemanticPayloadHashVersion() != agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V2 || receipt.GetCommandKind() == agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_UNSPECIFIED || receipt.GetResultKind() == agentv1.PrivilegedResultKind_PRIVILEGED_RESULT_KIND_UNSPECIFIED || receipt.GetTerminalState() != agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED && receipt.GetTerminalState() != agentv1.CommandResultState_COMMAND_RESULT_STATE_FAILED {
		return false
	}
	certificate := receipt.GetCertificate()
	certificateCommand := receipt.GetCommandKind() == agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_CERTIFICATE_CSR || receipt.GetCommandKind() == agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_CERTIFICATE_P12 || receipt.GetCommandKind() == agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_CERTIFICATE_REVOKE
	if certificate == nil {
		return !certificateCommand
	}
	if !certificateCommand || !validUUIDv7(certificate.GetCertificateId()) || len(certificate.GetRootEffectRecordId()) < 16 || len(certificate.GetRootEffectRecordId()) > 32 {
		return false
	}
	if receipt.GetResultKind() == agentv1.PrivilegedResultKind_PRIVILEGED_RESULT_KIND_CERTIFICATE_CSR {
		return len(certificate.GetCsrDerSha256()) == 32 && len(certificate.GetPublicKeySha256()) == 32 && len(certificate.GetRequestedSubjectSha256()) == 32
	}
	return len(certificate.GetCsrDerSha256()) == 0 && len(certificate.GetPublicKeySha256()) == 0 && len(certificate.GetRequestedSubjectSha256()) == 0
}

func commandKind(envelope *agentv1.CommandEnvelope) agentv1.PrivilegedCommandKind {
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
	case *agentv1.CommandEnvelope_ConfigPlan:
		return agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_CONFIG_PLAN
	case *agentv1.CommandEnvelope_ConfigApply:
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

func resultKind(envelope *agentv1.CommandEnvelope, result *agentv1.CommandResult) agentv1.PrivilegedResultKind {
	if result.GetState() == agentv1.CommandResultState_COMMAND_RESULT_STATE_FAILED {
		return agentv1.PrivilegedResultKind_PRIVILEGED_RESULT_KIND_ERROR
	}
	switch commandKind(envelope) {
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
	for _, value := range value[len(keyIDPrefix):] {
		if value < '0' || value > '9' && value < 'a' || value > 'f' {
			return false
		}
	}
	return true
}
func appendU32(output []byte, value uint32) []byte {
	return binary.BigEndian.AppendUint32(output, value)
}
func appendU64(output []byte, value uint64) []byte {
	return binary.BigEndian.AppendUint64(output, value)
}
func appendBytes(output, value []byte) []byte {
	output = appendU32(output, uint32(len(value)))
	return append(output, value...)
}
func appendTimestamp(output []byte, value *timestamppb.Timestamp) []byte {
	output = binary.BigEndian.AppendUint64(output, uint64(value.GetSeconds()))
	return appendU32(output, uint32(value.GetNanos()))
}
