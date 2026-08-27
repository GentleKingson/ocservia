// Package attestationtest builds root-attestation fixtures for database tests.
// It is not linked by the Controller binary.
package attestationtest

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"fmt"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/privdattestation"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var (
	fixtureIdentityID = uuid.MustParse("00000000-0000-7000-8000-0000000000a1")
	fixtureSessionID  = uuid.MustParse("00000000-0000-7000-8000-0000000000a2")
)

// InstallKey creates a deterministic test-only root trust anchor and approves
// the capability. Production registration always goes through the one-time
// root credential service instead.
func InstallKey(ctx context.Context, pool *pgxpool.Pool, nodeID uuid.UUID) (ed25519.PrivateKey, error) {
	seed := sha256.Sum256(append([]byte("ocservia/test-only-privd-key/v1\x00"), nodeID[:]...))
	privateKey := ed25519.NewKeyFromSeed(seed[:])
	publicKey := privateKey.Public().(ed25519.PublicKey)
	keyID := privdattestation.PublicKeyID(publicKey)
	credentialID := nodeID
	credentialID[15] ^= 0x5a
	secret := sha256.Sum256(append([]byte("ocservia/test-only-credential/v1\x00"), nodeID[:]...))
	nonce := sha256.Sum256(append([]byte("ocservia/test-only-nonce/v1\x00"), nodeID[:]...))
	credentialContext := sha256.Sum256(append([]byte("ocservia/test-only-context/v1\x00"), nodeID[:]...))
	now := time.Now().UTC().Truncate(time.Microsecond)
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO identities(id,issuer,subject,created_at,updated_at) VALUES($1,'test-fixture','privd-attestation',$2,$2) ON CONFLICT(id) DO NOTHING`, []any{fixtureIdentityID, now}},
		{`INSERT INTO auth_sessions(id,identity_id,expires_at,created_at) VALUES($1,$2,$3,$4) ON CONFLICT(id) DO NOTHING`, []any{fixtureSessionID, fixtureIdentityID, now.Add(24 * time.Hour), now.Add(-time.Minute)}},
		{`INSERT INTO privd_attestation_enrollment_credentials(id,node_id,secret_sha256,controller_nonce,credential_context_sha256,expires_at,consumed_at,created_by_identity_id,created_by_session_id,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT(id) DO NOTHING`, []any{credentialID, nodeID, secret[:], nonce[:], credentialContext[:], now.Add(time.Hour), now, fixtureIdentityID, fixtureSessionID, now.Add(-time.Minute)}},
		{`INSERT INTO node_privd_attestation_keys(node_id,key_id,algorithm,public_key,state,created_at,approved_at,activated_at,registration_credential_id) VALUES($1,$2,'ed25519',$3,'active',$4,$4,$4,$5) ON CONFLICT(node_id,key_id) DO NOTHING`, []any{nodeID, keyID, publicKey, now.Add(-time.Minute), credentialID}},
		{`INSERT INTO node_capabilities(node_id,capability,approved) VALUES($1,$2,true) ON CONFLICT(node_id,capability) DO UPDATE SET approved=true`, []any{nodeID, privdattestation.AttestationCapability}},
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement.query, statement.args...); err != nil {
			return nil, fmt.Errorf("install test privd attestation: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return privateKey, nil
}

// AttachProof signs the exact CommandResult fields with an installed fixture
// key using the production canonical transcript.
func AttachProof(envelope *agentv1.CommandEnvelope, result *agentv1.CommandResult, privateKey ed25519.PrivateKey, effectSequence uint64) error {
	if envelope == nil || result == nil || effectSequence == 0 {
		return fmt.Errorf("attestation fixture is incomplete")
	}
	if result.GetAcceptedAt() == nil {
		result.AcceptedAt = timestamppb.Now()
	}
	if result.GetCompletedAt() == nil {
		result.CompletedAt = proto.Clone(result.GetAcceptedAt()).(*timestamppb.Timestamp)
	}
	commandKind := commandKind(envelope)
	resultKind := resultKind(envelope, result)
	if commandKind == agentv1.PrivilegedCommandKind_PRIVILEGED_COMMAND_KIND_UNSPECIFIED || resultKind == agentv1.PrivilegedResultKind_PRIVILEGED_RESULT_KIND_UNSPECIFIED {
		return fmt.Errorf("attestation fixture command or result kind is unsupported")
	}
	resultDigest, errorDigest := sha256.Sum256(result.GetResult()), sha256.Sum256([]byte(result.GetErrorCode()))
	effect := sha256.Sum256(append(append(append([]byte("ocservia/test-only-root-effect/v1\x00"), envelope.GetNodeId()...), envelope.GetCommandId()...), envelope.GetOperationId()...))
	receipt := &agentv1.PrivdResultReceiptV1{
		ReceiptVersion: agentv1.PrivdReceiptVersion_PRIVD_RECEIPT_VERSION_V1,
		NodeId:         envelope.GetNodeId(), PrivdAttestationKeyId: privdattestation.PublicKeyID(privateKey.Public().(ed25519.PublicKey)),
		CommandId: envelope.GetCommandId(), OperationId: envelope.GetOperationId(), IdempotencyKey: envelope.GetIdempotencyKey(),
		SemanticPayloadHashVersion: result.GetSemanticPayloadHashVersion(), SemanticPayloadSha256: result.GetPayloadSha256(),
		CommandKind: commandKind, ResultKind: resultKind, TerminalState: result.GetState(),
		ResultBytesSha256: resultDigest[:], ErrorCodeSha256: errorDigest[:], EffectRecordId: effect[:], EffectSequence: effectSequence,
		AcceptedAt: proto.Clone(result.GetAcceptedAt()).(*timestamppb.Timestamp), CompletedAt: proto.Clone(result.GetCompletedAt()).(*timestamppb.Timestamp), Replayed: result.GetReplayed(),
	}
	binding, err := certificateBinding(envelope, result, effect[:])
	if err != nil {
		return err
	}
	receipt.Certificate = binding
	canonical, err := privdattestation.CanonicalReceiptV1(receipt)
	if err != nil {
		return err
	}
	result.PrivilegedResultProof = &agentv1.PrivilegedResultProof{
		Version:   agentv1.PrivdReceiptVersion_PRIVD_RECEIPT_VERSION_V1,
		ReceiptV1: receipt, Signature: ed25519.Sign(privateKey, canonical),
	}
	return nil
}

// UpgradeResultProof signs the root-owned durable upgrade evidence used by
// telemetry integration tests.
func UpgradeResultProof(nodeID, operationID uuid.UUID, targetVersion string, packageSHA256, resultSHA256 []byte, state string, completedAt time.Time, privateKey ed25519.PrivateKey) (*agentv1.AgentUpgradeResultProof, error) {
	if nodeID.Version() != 7 || operationID.Version() != 7 || len(packageSHA256) != sha256.Size || len(resultSHA256) != sha256.Size || completedAt.IsZero() || completedAt.UnixMilli() < 0 {
		return nil, fmt.Errorf("upgrade result fixture is incomplete")
	}
	var outcome agentv1.AgentUpgradeOutcomeState
	switch state {
	case "succeeded":
		outcome = agentv1.AgentUpgradeOutcomeState_AGENT_UPGRADE_OUTCOME_STATE_SUCCEEDED
	case "failed":
		outcome = agentv1.AgentUpgradeOutcomeState_AGENT_UPGRADE_OUTCOME_STATE_FAILED
	case "rolled_back":
		outcome = agentv1.AgentUpgradeOutcomeState_AGENT_UPGRADE_OUTCOME_STATE_ROLLED_BACK
	default:
		return nil, fmt.Errorf("upgrade result fixture state is invalid")
	}
	proof := &agentv1.AgentUpgradeResultProof{
		Version: agentv1.PrivdReceiptVersion_PRIVD_RECEIPT_VERSION_V1,
		NodeId:  nodeID[:], PrivdAttestationKeyId: privdattestation.PublicKeyID(privateKey.Public().(ed25519.PublicKey)),
		OperationId: operationID[:], TargetVersion: targetVersion, PackageSha256: append([]byte(nil), packageSHA256...),
		State: outcome, CompletedUnixMs: uint64(completedAt.UnixMilli()), ResultSha256: append([]byte(nil), resultSHA256...),
	}
	canonical, err := privdattestation.CanonicalAgentUpgradeResultProofV1(proof)
	if err != nil {
		return nil, err
	}
	proof.Signature = ed25519.Sign(privateKey, canonical)
	return proof, nil
}

func certificateBinding(envelope *agentv1.CommandEnvelope, result *agentv1.CommandResult, effect []byte) (*agentv1.PrivdCertificateReceiptBindingV1, error) {
	switch request := envelope.GetPayload().(type) {
	case *agentv1.CommandEnvelope_CertificateCsr:
		binding := &agentv1.PrivdCertificateReceiptBindingV1{CertificateId: request.CertificateCsr.GetCertificateId(), RootEffectRecordId: effect}
		if result.GetState() == agentv1.CommandResultState_COMMAND_RESULT_STATE_FAILED {
			return binding, nil
		}
		var csr agentv1.CertificateCsrResult
		if err := proto.Unmarshal(result.GetResult(), &csr); err != nil {
			return nil, err
		}
		csrDigest := sha256.Sum256(csr.GetCsrDer())
		subjectDigest, err := privdattestation.RequestedSubjectDigest(request.CertificateCsr)
		if err != nil {
			return nil, err
		}
		binding.CsrDerSha256, binding.PublicKeySha256, binding.RequestedSubjectSha256 = csrDigest[:], csr.GetPublicKeySha256(), subjectDigest
		return binding, nil
	case *agentv1.CommandEnvelope_CertificateP12:
		return &agentv1.PrivdCertificateReceiptBindingV1{CertificateId: request.CertificateP12.GetCertificateId(), RootEffectRecordId: effect}, nil
	case *agentv1.CommandEnvelope_CertificateRevoke:
		return &agentv1.PrivdCertificateReceiptBindingV1{CertificateId: request.CertificateRevoke.GetCertificateId(), RootEffectRecordId: effect}, nil
	default:
		return nil, nil
	}
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
