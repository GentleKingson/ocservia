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
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const canonicalReceiptGoldenV1 = "6f637365727669612f70726976642d726573756c742d726563656970742f7631000000000100000010018f2a3b4c5d700080000000000000010000004f656432353531392d7368613235363a6665383132633132663361623463653661633564623639616333353266393036636231623131656634336662333365323532656637666635353232363338383900000010018f2a3b4c5d7000800000000000000200000010018f2a3b4c5d7000800000000000000300000010018f2a3b4c5d70008000000000000004000000020000002011111111111111111111111111111111111111111111111111111111111111110000000c000000040000000100000020222222222222222222222222222222222222222222222222222222222222222200000020e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b85500000010018f2a3b4c5d700080000000000000050000000000000009000000006553f1000000007b000000006553f101000001c8000100000010018f2a3b4c5d7000800000000000000100000020333333333333333333333333333333333333333333333333333333333333333300000020444444444444444444444444444444444444444444444444444444444444444400000020555555555555555555555555555555555555555555555555555555555555555500000010018f2a3b4c5d70008000000000000005"

func TestCanonicalReceiptV1MatchesRustGoldenVector(t *testing.T) {
	id := mustHex(t, "018f2a3b4c5d70008000000000000001")
	effect := mustHex(t, "018f2a3b4c5d70008000000000000005")
	receipt := &agentv1.PrivdResultReceiptV1{
		ReceiptVersion: agentv1.PrivdReceiptVersion_PRIVD_RECEIPT_VERSION_V1,
		NodeId:         id, PrivdAttestationKeyId: "ed25519-sha256:fe812c12f3ab4ce6ac5db69ac352f906cb1b11ef43fb33e252ef7ff552263889",
		CommandId: mustHex(t, "018f2a3b4c5d70008000000000000002"), OperationId: mustHex(t, "018f2a3b4c5d70008000000000000003"), IdempotencyKey: mustHex(t, "018f2a3b4c5d70008000000000000004"),
		SemanticPayloadHashVersion: agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V2,
		SemanticPayloadSha256:      makeRepeated(0x11, 32), CommandKind: agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_CERTIFICATE_CSR,
		ResultKind: agentv1.PrivilegedResultKind_PRIVILEGED_RESULT_KIND_CERTIFICATE_CSR, TerminalState: agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED,
		ResultBytesSha256: makeRepeated(0x22, 32), ErrorCodeSha256: mustHex(t, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"),
		EffectRecordId: effect, EffectSequence: 9, AcceptedAt: &timestamppb.Timestamp{Seconds: 1_700_000_000, Nanos: 123}, CompletedAt: &timestamppb.Timestamp{Seconds: 1_700_000_001, Nanos: 456},
		Certificate: &agentv1.PrivdCertificateReceiptBindingV1{CertificateId: id, CsrDerSha256: makeRepeated(0x33, 32), PublicKeySha256: makeRepeated(0x44, 32), RequestedSubjectSha256: makeRepeated(0x55, 32), RootEffectRecordId: effect},
	}
	canonical, err := CanonicalReceiptV1(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(canonical) != canonicalReceiptGoldenV1 {
		t.Fatal("Go canonical transcript differs from the shared Rust golden vector")
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
		{name: "result byte", mutate: func(_ *agentv1.CommandEnvelope, result *agentv1.CommandResult) { result.Result[0] ^= 1 }, status: "invalid", reason: "receipt_claim_mismatch"},
		{name: "node", mutate: func(envelope *agentv1.CommandEnvelope, _ *agentv1.CommandResult) { envelope.NodeId[15] ^= 1 }, status: "invalid", reason: "receipt_claim_mismatch"},
		{name: "command", mutate: func(envelope *agentv1.CommandEnvelope, _ *agentv1.CommandResult) { envelope.CommandId[15] ^= 1 }, status: "invalid", reason: "receipt_claim_mismatch"},
		{name: "operation", mutate: func(envelope *agentv1.CommandEnvelope, _ *agentv1.CommandResult) { envelope.OperationId[15] ^= 1 }, status: "invalid", reason: "receipt_claim_mismatch"},
		{name: "idempotency", mutate: func(_ *agentv1.CommandEnvelope, result *agentv1.CommandResult) { result.IdempotencyKey[15] ^= 1 }, status: "invalid", reason: "receipt_claim_mismatch"},
		{name: "semantic version", mutate: func(_ *agentv1.CommandEnvelope, result *agentv1.CommandResult) {
			result.SemanticPayloadHashVersion = agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V2
		}, status: "invalid", reason: "receipt_claim_mismatch"},
		{name: "semantic digest", mutate: func(_ *agentv1.CommandEnvelope, result *agentv1.CommandResult) { result.PayloadSha256[0] ^= 1 }, status: "invalid", reason: "receipt_claim_mismatch"},
		{name: "receipt before command issuance", mutate: func(envelope *agentv1.CommandEnvelope, _ *agentv1.CommandResult) {
			envelope.IssuedAt = timestamppb.New(time.Unix(1_700_000_002, 0))
			envelope.ExpiresAt = timestamppb.New(time.Unix(1_700_000_062, 0))
		}, status: "invalid", reason: "receipt_claim_mismatch"},
		{name: "terminal state", mutate: func(_ *agentv1.CommandEnvelope, result *agentv1.CommandResult) {
			result.State = agentv1.CommandResultState_COMMAND_RESULT_STATE_FAILED
		}, status: "invalid", reason: "receipt_claim_mismatch"},
		{name: "effect identity", mutate: func(_ *agentv1.CommandEnvelope, result *agentv1.CommandResult) {
			result.PrivilegedResultProof.ReceiptV1.EffectRecordId[0] ^= 1
		}, status: "invalid", reason: "receipt_signature_invalid"},
		{name: "signature", mutate: func(_ *agentv1.CommandEnvelope, result *agentv1.CommandResult) {
			result.PrivilegedResultProof.Signature[0] ^= 1
		}, status: "invalid", reason: "receipt_signature_invalid"},
		{name: "unknown key", lookup: func(context.Context, uuid.UUID, string) (attestationKeyRecord, error) {
			return attestationKeyRecord{}, pgx.ErrNoRows
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
	if err != nil {
		t.Fatal(err)
	}
	attackerResult.PrivilegedResultProof.Signature = ed25519.Sign(attackerPrivate, canonical)
	verification := verifyResult(context.Background(), func(context.Context, uuid.UUID, string) (attestationKeyRecord, error) {
		return attestationKeyRecord{}, pgx.ErrNoRows
	}, nodeID, envelope, attackerResult)
	if verification.FailureReason != "receipt_key_unknown" {
		t.Fatalf("attacker-selected key verification=%+v", verification)
	}
}

func TestReceiptUnknownProtobufFieldsDoNotChangeSignatureSemantics(t *testing.T) {
	nodeID, envelope, result, key := receiptVerificationFixture(t)
	proofBytes, err := proto.Marshal(result.GetPrivilegedResultProof())
	if err != nil {
		t.Fatal(err)
	}
	proofBytes = protowire.AppendTag(proofBytes, 1000, protowire.BytesType)
	proofBytes = protowire.AppendBytes(proofBytes, []byte("ignored by canonical v1"))
	var decoded agentv1.PrivilegedResultProof
	if err := proto.Unmarshal(proofBytes, &decoded); err != nil {
		t.Fatal(err)
	}
	result.PrivilegedResultProof = &decoded
	verification := verifyResult(context.Background(), lookupFor(key.Public().(ed25519.PublicKey), "active", time.Unix(1_699_999_999, 0), nil), nodeID, envelope, result)
	if !verification.Verified() {
		t.Fatalf("unknown Protobuf field changed canonical signature semantics: %+v", verification)
	}
}

func TestReceiptUnknownVersionAndOversizeFailClosed(t *testing.T) {
	nodeID, envelope, result, key := receiptVerificationFixture(t)
	lookup := lookupFor(key.Public().(ed25519.PublicKey), "active", time.Unix(1_699_999_999, 0), nil)
	result.PrivilegedResultProof.Version = agentv1.PrivdReceiptVersion_PRIVD_RECEIPT_VERSION_UNSPECIFIED
	if verification := verifyResult(context.Background(), lookup, nodeID, envelope, result); verification.FailureReason != "receipt_version_unsupported" {
		t.Fatalf("unknown version verification=%+v", verification)
	}
	result.PrivilegedResultProof.Version = agentv1.PrivdReceiptVersion_PRIVD_RECEIPT_VERSION_V1
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
				envelope.Payload = &agentv1.CommandEnvelope_CertificateP12{CertificateP12: &agentv1.CertificateP12{CertificateId: certificateID[:]}}
			} else {
				envelope.Payload = &agentv1.CommandEnvelope_CertificateRevoke{CertificateRevoke: &agentv1.CertificateRevoke{CertificateId: certificateID[:]}}
			}
			receipt := result.GetPrivilegedResultProof().GetReceiptV1()
			receipt.CommandKind, receipt.ResultKind = test.commandKind, test.resultKind
			receipt.Certificate = &agentv1.PrivdCertificateReceiptBindingV1{CertificateId: bytes.Clone(certificateID[:]), RootEffectRecordId: bytes.Clone(receipt.GetEffectRecordId())}
			canonical, err := CanonicalReceiptV1(receipt)
			if err != nil {
				t.Fatal(err)
			}
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
		SemanticPayloadHashVersion: agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V1,
		SemanticPayloadSha256:      semantic,
		Payload:                    &agentv1.CommandEnvelope_SessionDisconnect{SessionDisconnect: &agentv1.SessionDisconnect{SessionId: "session", BootId: "boot"}},
	}
	resultBytes := []byte{0x08, 0x01}
	result := &agentv1.CommandResult{
		CommandId: commandID[:], IdempotencyKey: idempotencyID[:], PayloadSha256: semantic,
		SemanticPayloadHashVersion: agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V1,
		State:                      agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED, Result: resultBytes,
		AcceptedAt: timestamppb.New(accepted), CompletedAt: timestamppb.New(completed),
	}
	resultDigest, errorDigest := sha256.Sum256(resultBytes), sha256.Sum256(nil)
	receipt := &agentv1.PrivdResultReceiptV1{
		ReceiptVersion: agentv1.PrivdReceiptVersion_PRIVD_RECEIPT_VERSION_V1,
		NodeId:         nodeID[:], PrivdAttestationKeyId: PublicKeyID(publicKey), CommandId: commandID[:], OperationId: operationID[:], IdempotencyKey: idempotencyID[:],
		SemanticPayloadHashVersion: agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V1, SemanticPayloadSha256: semantic,
		CommandKind: agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_SESSION_DISCONNECT, ResultKind: agentv1.PrivilegedResultKind_PRIVILEGED_RESULT_KIND_MUTATION,
		TerminalState: agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED, ResultBytesSha256: resultDigest[:], ErrorCodeSha256: errorDigest[:],
		EffectRecordId: makeRepeated(0x22, 32), EffectSequence: 1, AcceptedAt: timestamppb.New(accepted), CompletedAt: timestamppb.New(completed),
	}
	canonical, err := CanonicalReceiptV1(receipt)
	if err != nil {
		t.Fatal(err)
	}
	result.PrivilegedResultProof = &agentv1.PrivilegedResultProof{Version: agentv1.PrivdReceiptVersion_PRIVD_RECEIPT_VERSION_V1, ReceiptV1: receipt, Signature: ed25519.Sign(privateKey, canonical)}
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
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

func makeRepeated(value byte, length int) []byte {
	result := make([]byte, length)
	for i := range result {
		result[i] = value
	}
	return result
}
