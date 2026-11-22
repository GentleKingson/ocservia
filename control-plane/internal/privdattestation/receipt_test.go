package privdattestation

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestCanonicalReceiptV1DeterministicCBOR(t *testing.T) {
	id := mustHex(t, "018f2a3b4c5d70008000000000000001")
	receipt := &agentv1.PrivdResultReceiptV1{
		ReceiptVersion: 1,
		NodeId:         id, PrivdAttestationKeyId: "ed25519-sha256:fe812c12f3ab4ce6ac5db69ac352f906cb1b11ef43fb33e252ef7ff552263889",
		CommandId: mustHex(t, "018f2a3b4c5d70008000000000000002"), OperationId: mustHex(t, "018f2a3b4c5d70008000000000000003"), IdempotencyKey: mustHex(t, "018f2a3b4c5d70008000000000000004"),
		SemanticPayloadHashVersion: agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V2,
		SemanticPayloadSha256:      makeRepeated(0x11, 32), CommandKind: agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_CERTIFICATE_CSR,
		ResultKind: agentv1.PrivilegedResultKind_PRIVILEGED_RESULT_KIND_CERTIFICATE_CSR, TerminalState: agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED,
		ResultCborSha256: makeRepeated(0x22, 32), ErrorCode: "",
		EffectSequence: 9, AcceptedAt: &timestamppb.Timestamp{Seconds: 1_700_000_000, Nanos: 123}, CompletedAt: &timestamppb.Timestamp{Seconds: 1_700_000_001, Nanos: 456},
		Certificate: &agentv1.PrivdCertificateReceiptBindingV1{CertificateId: id, CsrDerSha256: makeRepeated(0x33, 32), PublicKeySha256: makeRepeated(0x44, 32)},
	}
	canonical, err := CanonicalReceiptV1(receipt)
	testutil.NoError(t, err)
	if !bytes.HasPrefix(canonical, []byte(receiptDomain)) {
		t.Fatalf("canonical transcript missing CBOR domain prefix: %x", canonical)
	}
	// Repeat to verify strict determinism.
	again, err := CanonicalReceiptV1(receipt)
	testutil.NoError(t, err)
	if !bytes.Equal(canonical, again) {
		t.Fatal("canonical CBOR receipt transcript is not deterministic")
	}
}

func TestPrivilegedReceiptVerificationRejectsTamperingAndUntrustedKeys(t *testing.T) {
	nodeID, envelope, result, trustedKey := receiptVerificationFixture(t)
	trustedLookup := lookupFor(trustedKey.Public().(ed25519.PublicKey), "active", time.Unix(1_699_999_999, 0), nil)
	if verification := verifyResult(context.Background(), trustedLookup, nodeID, envelope, result); !verification.Verified() {
		t.Fatalf("valid root receipt was rejected: %+v", verification)
	}
	if verification := verifyResult(context.Background(), trustedLookup, nodeID, envelope, proto.Clone(result).(*agentv1.CommandResult)); !verification.Verified() {
		t.Fatalf("exact idempotent receipt replay was rejected: %+v", verification)
	}

	tests := []struct {
		name   string
		mutate func(*agentv1.CommandEnvelope, *agentv1.CommandResult)
		lookup keyLookup
		status string
		reason string
	}{
		{name: "missing receipt", mutate: func(_ *agentv1.CommandEnvelope, result *agentv1.CommandResult) { result.PrivilegedResultProof = nil }, status: "missing", reason: "receipt_missing"},
		{name: "result byte", mutate: func(_ *agentv1.CommandEnvelope, result *agentv1.CommandResult) { result.Result = []byte{0x01} }, status: "invalid", reason: "receipt_claim_mismatch"},
		{name: "node", mutate: func(envelope *agentv1.CommandEnvelope, _ *agentv1.CommandResult) { envelope.NodeId[15] ^= 1 }, status: "invalid", reason: "receipt_claim_mismatch"},
		{name: "command", mutate: func(envelope *agentv1.CommandEnvelope, _ *agentv1.CommandResult) { envelope.CommandId[15] ^= 1 }, status: "invalid", reason: "receipt_claim_mismatch"},
		{name: "operation", mutate: func(envelope *agentv1.CommandEnvelope, _ *agentv1.CommandResult) { envelope.OperationId[15] ^= 1 }, status: "invalid", reason: "receipt_claim_mismatch"},
		{name: "idempotency", mutate: func(_ *agentv1.CommandEnvelope, result *agentv1.CommandResult) { result.IdempotencyKey[15] ^= 1 }, status: "invalid", reason: "receipt_claim_mismatch"},
		{name: "semantic version", mutate: func(_ *agentv1.CommandEnvelope, result *agentv1.CommandResult) {
			result.SemanticPayloadHashVersion = agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_UNSPECIFIED
		}, status: "invalid", reason: "receipt_claim_mismatch"},
		{name: "semantic digest", mutate: func(_ *agentv1.CommandEnvelope, result *agentv1.CommandResult) { result.PayloadSha256[0] ^= 1 }, status: "invalid", reason: "receipt_claim_mismatch"},
		{name: "receipt before command issuance", mutate: func(envelope *agentv1.CommandEnvelope, _ *agentv1.CommandResult) {
			envelope.IssuedAt = timestamppb.New(time.Unix(1_700_000_002, 0))
			envelope.ExpiresAt = timestamppb.New(time.Unix(1_700_000_062, 0))
		}, status: "invalid", reason: "receipt_claim_mismatch"},
		{name: "terminal state", mutate: func(_ *agentv1.CommandEnvelope, result *agentv1.CommandResult) {
			result.State = agentv1.CommandResultState_COMMAND_RESULT_STATE_FAILED
		}, status: "invalid", reason: "receipt_claim_mismatch"},
		{name: "effect sequence", mutate: func(_ *agentv1.CommandEnvelope, result *agentv1.CommandResult) {
			result.PrivilegedResultProof.ReceiptV1.EffectSequence++
		}, status: "invalid", reason: "receipt_signature_invalid"},
		{name: "signature", mutate: func(_ *agentv1.CommandEnvelope, result *agentv1.CommandResult) {
			result.PrivilegedResultProof.Signature[0] ^= 1
		}, status: "invalid", reason: "receipt_signature_invalid"},
		{name: "unknown key", lookup: func(context.Context, uuid.UUID, string) (attestationKeyRecord, error) {
			return attestationKeyRecord{}, database.ErrNotFound
		}, status: "unknown_key", reason: "receipt_key_unknown"},
		{name: "revoked key", lookup: lookupFor(trustedKey.Public().(ed25519.PublicKey), "revoked", time.Unix(1_699_999_999, 0), nil), status: "revoked_key", reason: "receipt_key_revoked"},
		{name: "expired rotation overlap", lookup: lookupFor(trustedKey.Public().(ed25519.PublicKey), "active", time.Unix(1_699_999_999, 0), timePointer(time.Unix(1_700_000_000, 500))), status: "invalid", reason: "receipt_key_outside_validity"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testEnvelope := proto.Clone(envelope).(*agentv1.CommandEnvelope)
			testResult := proto.Clone(result).(*agentv1.CommandResult)
			if test.mutate != nil {
				test.mutate(testEnvelope, testResult)
			}
			lookup := test.lookup
			if lookup == nil {
				lookup = trustedLookup
			}
			verification := verifyResult(context.Background(), lookup, nodeID, testEnvelope, testResult)
			if verification.Status != test.status || verification.FailureReason != test.reason {
				t.Fatalf("verification=%+v, want status=%s reason=%s", verification, test.status, test.reason)
			}
		})
	}

	attackerPrivate := ed25519.NewKeyFromSeed(makeRepeated(0x42, ed25519.SeedSize))
	attackerPublic := attackerPrivate.Public().(ed25519.PublicKey)
	attackerResult := proto.Clone(result).(*agentv1.CommandResult)
	attackerResult.PrivilegedResultProof.ReceiptV1.PrivdAttestationKeyId = PublicKeyID(attackerPublic)
	canonical, err := CanonicalReceiptV1(attackerResult.GetPrivilegedResultProof().GetReceiptV1())
	testutil.NoError(t, err)
	attackerResult.PrivilegedResultProof.Signature = ed25519.Sign(attackerPrivate, canonical)
	verification := verifyResult(context.Background(), func(context.Context, uuid.UUID, string) (attestationKeyRecord, error) {
		return attestationKeyRecord{}, database.ErrNotFound
	}, nodeID, envelope, attackerResult)
	if verification.FailureReason != "receipt_key_unknown" {
		t.Fatalf("attacker-selected key verification=%+v", verification)
	}
}

func TestReceiptUnknownProtobufFieldsDoNotChangeSignatureSemantics(t *testing.T) {
	nodeID, envelope, result, key := receiptVerificationFixture(t)
	proofBytes, err := proto.Marshal(result.GetPrivilegedResultProof())
	testutil.NoError(t, err)
	proofBytes = protowire.AppendTag(proofBytes, 1000, protowire.BytesType)
	proofBytes = protowire.AppendBytes(proofBytes, []byte("ignored by canonical v1"))
	var decoded agentv1.PrivilegedResultProof
	testutil.NoError(t, proto.Unmarshal(proofBytes, &decoded))
	result.PrivilegedResultProof = &decoded
	verification := verifyResult(context.Background(), lookupFor(key.Public().(ed25519.PublicKey), "active", time.Unix(1_699_999_999, 0), nil), nodeID, envelope, result)
	if !verification.Verified() {
		t.Fatalf("unknown Protobuf field changed canonical signature semantics: %+v", verification)
	}
}

func TestReceiptMissingTransactionFailsClosed(t *testing.T) {
	node, envelope, result, _ := receiptVerificationFixture(t)
	for _, verification := range []Verification{
		VerifyResultTransaction(context.Background(), nil, node, envelope, result),
		VerifyResultTransaction(context.Background(), nil, node, envelope, result),
	} {
		if verification.Status != "invalid" || verification.FailureReason != "receipt_key_lookup_failed" {
			t.Fatalf("missing transaction verification = %+v", verification)
		}
	}
}

func TestRejectedPrivilegedResultNeverVerifiesAsTerminalEvidence(t *testing.T) {
	nodeID, envelope, result, key := receiptVerificationFixture(t)
	lookup := lookupFor(key.Public().(ed25519.PublicKey), "active", time.Unix(1_699_999_999, 0), nil)
	rejected := func() *agentv1.CommandResult {
		return &agentv1.CommandResult{CommandId: result.CommandId, IdempotencyKey: result.IdempotencyKey, State: agentv1.CommandResultState_COMMAND_RESULT_STATE_REJECTED, ErrorCode: "rejected", CompletedAt: result.CompletedAt}
	}
	if verification := verifyResult(context.Background(), lookup, nodeID, envelope, rejected()); verification.Status != "missing" || verification.Verified() {
		t.Fatalf("unsigned rejection of a root command = %+v", verification)
	}
	// A signed success receipt cannot be reused to vouch for a rejection.
	reused := rejected()
	reused.PrivilegedResultProof = result.PrivilegedResultProof
	if verification := verifyResult(context.Background(), lookup, nodeID, envelope, reused); verification.Verified() || verification.Status != "invalid" {
		t.Fatalf("receipt reused for a rejection = %+v", verification)
	}
	// Commands without a root effect keep their ordinary rejection semantics.
	envelope.Payload = &agentv1.CommandEnvelope_SyntheticEcho{SyntheticEcho: &agentv1.SyntheticEcho{Message: "x"}}
	if verification := verifyResult(context.Background(), lookup, nodeID, envelope, rejected()); verification.Status != "not_required" {
		t.Fatalf("non-privileged rejection = %+v", verification)
	}
}

func TestReceiptUnknownVersionAndOversizeFailClosed(t *testing.T) {
	nodeID, envelope, result, key := receiptVerificationFixture(t)
	lookup := lookupFor(key.Public().(ed25519.PublicKey), "active", time.Unix(1_699_999_999, 0), nil)
	result.PrivilegedResultProof.Version = 0
	if verification := verifyResult(context.Background(), lookup, nodeID, envelope, result); verification.FailureReason != "receipt_version_unsupported" {
		t.Fatalf("unknown version verification=%+v", verification)
	}
	result.PrivilegedResultProof.Version = 1
	result.PrivilegedResultProof.Signature = make([]byte, 65*1024)
	if verification := verifyResult(context.Background(), lookup, nodeID, envelope, result); verification.FailureReason != "receipt_malformed" {
		t.Fatalf("oversized proof verification=%+v", verification)
	}
}

func TestCertificateP12AndRevokeRequireBoundReceipts(t *testing.T) {
	for _, test := range []struct {
		name        string
		commandKind agentv1.PrivilegedCommandKind
		resultKind  agentv1.PrivilegedResultKind
	}{
		{name: "p12", commandKind: agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_CERTIFICATE_P12, resultKind: agentv1.PrivilegedResultKind_PRIVILEGED_RESULT_KIND_CERTIFICATE_P12},
		{name: "revoke", commandKind: agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_CERTIFICATE_REVOKE, resultKind: agentv1.PrivilegedResultKind_PRIVILEGED_RESULT_KIND_CERTIFICATE_REVOKE},
	} {
		t.Run(test.name, func(t *testing.T) {
			nodeID, envelope, result, key := receiptVerificationFixture(t)
			certificateID := uuid.Must(uuid.NewV7())
			if test.name == "p12" {
				artifactID := uuid.Must(uuid.NewV7())
				envelope.Payload = &agentv1.CommandEnvelope_CertificateP12{CertificateP12: &agentv1.CertificateP12{CertificateId: certificateID[:], ArtifactId: artifactID[:]}}
				raw, err := proto.Marshal(&agentv1.CertificateArtifactResult{CertificateId: certificateID[:], ArtifactId: artifactID[:], ArtifactSha256: makeRepeated(0x33, 32), ArtifactSize: 128})
				testutil.NoError(t, err)
				result.Result = raw
			} else {
				envelope.Payload = &agentv1.CommandEnvelope_CertificateRevoke{CertificateRevoke: &agentv1.CertificateRevoke{CertificateId: certificateID[:]}}
				raw, err := proto.Marshal(&agentv1.CertificateRevokeResult{CertificateId: certificateID[:], KeyRemoved: true})
				testutil.NoError(t, err)
				result.Result = raw
			}
			resultDigest, err := ResultPayloadCBORHash(test.resultKind, result.Result)
			testutil.NoError(t, err)
			receipt := result.GetPrivilegedResultProof().GetReceiptV1()
			receipt.CommandKind, receipt.ResultKind, receipt.ResultCborSha256 = test.commandKind, test.resultKind, resultDigest[:]
			receipt.Certificate = &agentv1.PrivdCertificateReceiptBindingV1{CertificateId: bytes.Clone(certificateID[:])}
			canonical, err := CanonicalReceiptV1(receipt)
			testutil.NoError(t, err)
			result.PrivilegedResultProof.Signature = ed25519.Sign(key, canonical)
			lookup := lookupFor(key.Public().(ed25519.PublicKey), "active", time.Unix(1_699_999_999, 0), nil)
			if verification := verifyResult(context.Background(), lookup, nodeID, envelope, result); !verification.Verified() {
				t.Fatalf("valid certificate root receipt = %+v", verification)
			}
			result.PrivilegedResultProof.ReceiptV1.Certificate.CertificateId[15] ^= 1
			if verification := verifyResult(context.Background(), lookup, nodeID, envelope, result); verification.FailureReason != "receipt_claim_mismatch" {
				t.Fatalf("substituted certificate binding = %+v", verification)
			}
		})
	}
}

func receiptVerificationFixture(t *testing.T) (uuid.UUID, *agentv1.CommandEnvelope, *agentv1.CommandResult, ed25519.PrivateKey) {
	t.Helper()
	nodeID := uuid.MustParse("018f2a3b-4c5d-7000-8000-000000000001")
	commandID := uuid.MustParse("018f2a3b-4c5d-7000-8000-000000000002")
	operationID := uuid.MustParse("018f2a3b-4c5d-7000-8000-000000000003")
	idempotencyID := uuid.MustParse("018f2a3b-4c5d-7000-8000-000000000004")
	semantic := makeRepeated(0x11, sha256.Size)
	accepted, completed := time.Unix(1_700_000_000, 0).UTC(), time.Unix(1_700_000_001, 0).UTC()
	seed := makeRepeated(0x29, ed25519.SeedSize)
	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	envelope := &agentv1.CommandEnvelope{
		CommandId: commandID[:], OperationId: operationID[:], IdempotencyKey: idempotencyID[:], NodeId: nodeID[:],
		IssuedAt: timestamppb.New(accepted), ExpiresAt: timestamppb.New(completed.Add(time.Minute)),
		SemanticPayloadHashVersion: agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V2,
		SemanticPayloadSha256:      semantic,
		Payload:                    &agentv1.CommandEnvelope_SessionDisconnect{SessionDisconnect: &agentv1.SessionDisconnect{SessionId: "session", BootId: "boot"}},
	}
	result := &agentv1.CommandResult{
		CommandId: commandID[:], IdempotencyKey: idempotencyID[:], PayloadSha256: semantic,
		SemanticPayloadHashVersion: agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V2,
		State:                      agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED,
		AcceptedAt:                 timestamppb.New(accepted), CompletedAt: timestamppb.New(completed),
	}
	resultDigest, err := ResultPayloadCBORHash(agentv1.PrivilegedResultKind_PRIVILEGED_RESULT_KIND_MUTATION, nil)
	testutil.NoError(t, err)
	receipt := &agentv1.PrivdResultReceiptV1{
		ReceiptVersion: 1,
		NodeId:         nodeID[:], PrivdAttestationKeyId: PublicKeyID(publicKey), CommandId: commandID[:], OperationId: operationID[:], IdempotencyKey: idempotencyID[:],
		SemanticPayloadHashVersion: agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V2, SemanticPayloadSha256: semantic,
		CommandKind: agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_SESSION_DISCONNECT, ResultKind: agentv1.PrivilegedResultKind_PRIVILEGED_RESULT_KIND_MUTATION,
		TerminalState: agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED, ResultCborSha256: resultDigest[:], ErrorCode: "",
		EffectSequence: 1, AcceptedAt: timestamppb.New(accepted), CompletedAt: timestamppb.New(completed),
	}
	canonical, err := CanonicalReceiptV1(receipt)
	testutil.NoError(t, err)
	result.PrivilegedResultProof = &agentv1.PrivilegedResultProof{Version: 1, ReceiptV1: receipt, Signature: ed25519.Sign(privateKey, canonical)}
	return nodeID, envelope, result, privateKey
}

func lookupFor(publicKey ed25519.PublicKey, state string, activatedAt time.Time, validUntil *time.Time) keyLookup {
	return func(context.Context, uuid.UUID, string) (attestationKeyRecord, error) {
		if len(publicKey) == 0 {
			return attestationKeyRecord{}, errors.New("missing test key")
		}
		return attestationKeyRecord{PublicKey: bytes.Clone(publicKey), State: state, ActivatedAt: activatedAt, ValidUntil: validUntil}, nil
	}
}

func timePointer(value time.Time) *time.Time { return &value }

func mustHex(t *testing.T, value string) []byte {
	t.Helper()
	decoded, err := hex.DecodeString(value)
	testutil.NoError(t, err)
	return decoded
}

func makeRepeated(value byte, length int) []byte {
	result := make([]byte, length)
	for i := range result {
		result[i] = value
	}
	return result
}

func TestAgentUpgradeReceiptMapsToScheduledIntentKind(t *testing.T) {
	envelope := &agentv1.CommandEnvelope{Payload: &agentv1.CommandEnvelope_AgentUpgrade{AgentUpgrade: &agentv1.AgentUpgrade{TargetVersion: "1.2.3", PackageSha256: make([]byte, sha256.Size), Architecture: "arm64"}}}
	if commandKind(envelope) != agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_AGENT_UPGRADE {
		t.Fatal("agent upgrade envelope did not map to its privileged command kind")
	}
	succeeded := &agentv1.CommandResult{State: agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED}
	if resultKind(envelope, succeeded) != agentv1.PrivilegedResultKind_PRIVILEGED_RESULT_KIND_AGENT_UPGRADE_SCHEDULED {
		t.Fatal("successful upgrade result must attest a scheduled intent, never a running version")
	}
	failed := &agentv1.CommandResult{State: agentv1.CommandResultState_COMMAND_RESULT_STATE_FAILED}
	if resultKind(envelope, failed) != agentv1.PrivilegedResultKind_PRIVILEGED_RESULT_KIND_ERROR {
		t.Fatal("failed upgrade result must attest an error outcome")
	}
}
