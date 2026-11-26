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
	t.Parallel()
	id := mustHex(t, "018f2a3b4c5d70008000000000000001")
	receipt := &agentv1.PrivdResultReceiptV1{
		ReceiptVersion: 1,
		NodeId:         id, PrivdAttestationKeyId: "ed25519-sha256:fe812c12f3ab4ce6ac5db69ac352f906cb1b11ef43fb33e252ef7ff552263889",
		CommandId: id, OperationId: id, IdempotencyKey: id,
		SemanticPayloadHashVersion: agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V2,
		SemanticPayloadSha256:      bytes.Repeat([]byte{0x11}, 32), CommandKind: agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_CERTIFICATE_CSR,
		ResultKind: agentv1.PrivilegedResultKind_PRIVILEGED_RESULT_KIND_CERTIFICATE_CSR, TerminalState: agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED,
		ResultCborSha256: bytes.Repeat([]byte{0x22}, 32), EffectSequence: 9,
		AcceptedAt:  &timestamppb.Timestamp{Seconds: 1_700_000_000, Nanos: 123},
		CompletedAt: &timestamppb.Timestamp{Seconds: 1_700_000_001, Nanos: 456},
		Certificate: &agentv1.PrivdCertificateReceiptBindingV1{CertificateId: id, CsrDerSha256: bytes.Repeat([]byte{0x33}, 32), PublicKeySha256: bytes.Repeat([]byte{0x44}, 32)},
	}
	canonical, err := CanonicalReceiptV1(receipt)
	testutil.NoError(t, err)
	again, err := CanonicalReceiptV1(receipt)
	testutil.NoError(t, err)
	if !bytes.HasPrefix(canonical, []byte(receiptDomain)) || !bytes.Equal(canonical, again) {
		t.Fatal("canonical CBOR receipt transcript is non-deterministic or missing domain prefix")
	}
}

func TestPrivilegedReceiptVerificationRejectsTamperingAndUntrustedKeys(t *testing.T) {
	t.Parallel()
	nodeID, envelope, result, trustedKey := receiptVerificationFixture(t)
	trustedLookup := lookupFor(trustedKey.Public().(ed25519.PublicKey), "active", time.Unix(1_699_999_999, 0), nil)
	if v := verifyResult(context.Background(), trustedLookup, nodeID, envelope, result); !v.Verified() {
		t.Fatalf("valid root receipt was rejected: %+v", v)
	}

	tests := []struct {
		name   string
		mutate func(*agentv1.CommandEnvelope, *agentv1.CommandResult)
		lookup keyLookup
		status string
		reason string
	}{
		{"missing receipt", func(_ *agentv1.CommandEnvelope, r *agentv1.CommandResult) { r.PrivilegedResultProof = nil }, nil, "missing", "receipt_missing"},
		{"result byte", func(_ *agentv1.CommandEnvelope, r *agentv1.CommandResult) { r.Result = []byte{0x01} }, nil, "invalid", "receipt_claim_mismatch"},
		{"command", func(e *agentv1.CommandEnvelope, _ *agentv1.CommandResult) { e.CommandId[15] ^= 1 }, nil, "invalid", "receipt_claim_mismatch"},
		{"semantic digest", func(_ *agentv1.CommandEnvelope, r *agentv1.CommandResult) { r.PayloadSha256[0] ^= 1 }, nil, "invalid", "receipt_claim_mismatch"},
		{"terminal state", func(_ *agentv1.CommandEnvelope, r *agentv1.CommandResult) {
			r.State = agentv1.CommandResultState_COMMAND_RESULT_STATE_FAILED
		}, nil, "invalid", "receipt_claim_mismatch"},
		{"effect sequence", func(_ *agentv1.CommandEnvelope, r *agentv1.CommandResult) {
			r.PrivilegedResultProof.ReceiptV1.EffectSequence++
		}, nil, "invalid", "receipt_signature_invalid"},
		{"signature", func(_ *agentv1.CommandEnvelope, r *agentv1.CommandResult) { r.PrivilegedResultProof.Signature[0] ^= 1 }, nil, "invalid", "receipt_signature_invalid"},
		{"unknown key", nil, func(context.Context, uuid.UUID, string) (attestationKeyRecord, error) {
			return attestationKeyRecord{}, database.ErrNotFound
		}, "unknown_key", "receipt_key_unknown"},
		{"revoked key", nil, lookupFor(trustedKey.Public().(ed25519.PublicKey), "revoked", time.Unix(1_699_999_999, 0), nil), "revoked_key", "receipt_key_revoked"},
		{"expired overlap", nil, lookupFor(trustedKey.Public().(ed25519.PublicKey), "active", time.Unix(1_699_999_999, 0), timePointer(time.Unix(1_700_000_000, 500))), "invalid", "receipt_key_outside_validity"},
	}
	for _, test := range tests {
		testEnvelope := proto.Clone(envelope).(*agentv1.CommandEnvelope)
		testResult := proto.Clone(result).(*agentv1.CommandResult)
		if test.mutate != nil {
			test.mutate(testEnvelope, testResult)
		}
		lookup := test.lookup
		if lookup == nil {
			lookup = trustedLookup
		}
		if v := verifyResult(context.Background(), lookup, nodeID, testEnvelope, testResult); v.Status != test.status || v.FailureReason != test.reason {
			t.Fatalf("%s: got %+v, want %s/%s", test.name, v, test.status, test.reason)
		}
	}
}

func TestReceiptProtocolGuardsAndCertificateBinding(t *testing.T) {
	t.Parallel()
	nodeID, envelope, result, key := receiptVerificationFixture(t)
	lookup := lookupFor(key.Public().(ed25519.PublicKey), "active", time.Unix(1_699_999_999, 0), nil)

	// Unknown Protobuf fields must not change canonical v1 signature semantics.
	proofBytes, err := proto.Marshal(result.GetPrivilegedResultProof())
	testutil.NoError(t, err)
	proofBytes = protowire.AppendBytes(protowire.AppendTag(proofBytes, 1000, protowire.BytesType), []byte("ignored"))
	var decoded agentv1.PrivilegedResultProof
	testutil.NoError(t, proto.Unmarshal(proofBytes, &decoded))
	result.PrivilegedResultProof = &decoded
	if v := verifyResult(context.Background(), lookup, nodeID, envelope, result); !v.Verified() {
		t.Fatalf("unknown Protobuf field broke verification: %+v", v)
	}

	// Missing transaction fails closed.
	if v := VerifyResultTransaction(context.Background(), nil, nodeID, envelope, result); v.Status != "invalid" || v.FailureReason != "receipt_key_lookup_failed" {
		t.Fatalf("missing transaction = %+v", v)
	}

	// Signed success receipt cannot vouch for a rejected outcome.
	rejected := &agentv1.CommandResult{
		CommandId: result.CommandId, IdempotencyKey: result.IdempotencyKey,
		State: agentv1.CommandResultState_COMMAND_RESULT_STATE_REJECTED, ErrorCode: "rejected",
		CompletedAt: result.CompletedAt, PrivilegedResultProof: result.PrivilegedResultProof,
	}
	if v := verifyResult(context.Background(), lookup, nodeID, envelope, rejected); v.Verified() || v.Status != "invalid" {
		t.Fatalf("reused receipt for rejection = %+v", v)
	}

	// Certificate P12 bound receipt.
	certID, artifactID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	envelope.Payload = &agentv1.CommandEnvelope_CertificateP12{CertificateP12: &agentv1.CertificateP12{CertificateId: certID[:], ArtifactId: artifactID[:]}}
	raw, err := proto.Marshal(&agentv1.CertificateArtifactResult{CertificateId: certID[:], ArtifactId: artifactID[:], ArtifactSha256: bytes.Repeat([]byte{0x33}, 32), ArtifactSize: 128})
	testutil.NoError(t, err)
	result.Result = raw
	resultDigest, err := ResultPayloadCBORHash(agentv1.PrivilegedResultKind_PRIVILEGED_RESULT_KIND_CERTIFICATE_P12, result.Result)
	testutil.NoError(t, err)
	receipt := result.GetPrivilegedResultProof().GetReceiptV1()
	receipt.CommandKind = agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_CERTIFICATE_P12
	receipt.ResultKind = agentv1.PrivilegedResultKind_PRIVILEGED_RESULT_KIND_CERTIFICATE_P12
	receipt.ResultCborSha256 = resultDigest[:]
	receipt.Certificate = &agentv1.PrivdCertificateReceiptBindingV1{CertificateId: bytes.Clone(certID[:])}
	canonical, err := CanonicalReceiptV1(receipt)
	testutil.NoError(t, err)
	result.PrivilegedResultProof.Signature = ed25519.Sign(key, canonical)
	if v := verifyResult(context.Background(), lookup, nodeID, envelope, result); !v.Verified() {
		t.Fatalf("valid certificate root receipt = %+v", v)
	}
	result.PrivilegedResultProof.ReceiptV1.Certificate.CertificateId[15] ^= 1
	if v := verifyResult(context.Background(), lookup, nodeID, envelope, result); v.FailureReason != "receipt_claim_mismatch" {
		t.Fatalf("substituted certificate binding = %+v", v)
	}
}

func receiptVerificationFixture(t *testing.T) (uuid.UUID, *agentv1.CommandEnvelope, *agentv1.CommandResult, ed25519.PrivateKey) {
	t.Helper()
	nodeID := uuid.MustParse("018f2a3b-4c5d-7000-8000-000000000001")
	commandID := uuid.MustParse("018f2a3b-4c5d-7000-8000-000000000002")
	operationID := uuid.MustParse("018f2a3b-4c5d-7000-8000-000000000003")
	idempotencyID := uuid.MustParse("018f2a3b-4c5d-7000-8000-000000000004")
	semantic := bytes.Repeat([]byte{0x11}, sha256.Size)
	accepted, completed := time.Unix(1_700_000_000, 0).UTC(), time.Unix(1_700_000_001, 0).UTC()
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x29}, ed25519.SeedSize))
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
		ReceiptVersion: 1, NodeId: nodeID[:], PrivdAttestationKeyId: PublicKeyID(publicKey),
		CommandId: commandID[:], OperationId: operationID[:], IdempotencyKey: idempotencyID[:],
		SemanticPayloadHashVersion: agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V2, SemanticPayloadSha256: semantic,
		CommandKind: agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_SESSION_DISCONNECT, ResultKind: agentv1.PrivilegedResultKind_PRIVILEGED_RESULT_KIND_MUTATION,
		TerminalState: agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED, ResultCborSha256: resultDigest[:],
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
