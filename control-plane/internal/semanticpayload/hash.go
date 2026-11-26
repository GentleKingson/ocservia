// Package semanticpayload computes the canonical RFC 8949 Core Deterministic CBOR
// semantic hash for command envelopes, shared by transportevents and operations.
package semanticpayload

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"net"
	"slices"
	"strings"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/canonicalcbor"
	"github.com/GentleKingson/ocservia/control-plane/internal/configprofile"
	"golang.org/x/mod/semver"
)

const domainSeparatorV2 = "ocservia.command.semantic-hash.cbor.v2\x00"

// ValidateVersion rejects hash algorithms that this binary cannot verify.
func ValidateVersion(version agentv1.SemanticPayloadHashVersion) error {
	switch version {
	case agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_UNSPECIFIED,
		agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V2:
		return nil
	default:
		return errors.New("semantic payload hash version is unsupported")
	}
}

type cborEnvelope struct {
	NodeID           []byte `cbor:"node_id"`
	ExpectedRevision uint64 `cbor:"expected_revision"`
	PayloadKind      uint32 `cbor:"payload_kind"`
	Payload          any    `cbor:"payload"`
}

type cborEmpty struct{}

type cborEcho struct {
	Message string `cbor:"message"`
}

type cborSession struct {
	SessionID string `cbor:"session_id"`
	BootID    string `cbor:"boot_id"`
}

type cborIPBan struct {
	IP string `cbor:"ip"`
}

type cborSealedSecret struct {
	Version    uint32 `cbor:"version"`
	Purpose    uint32 `cbor:"purpose"`
	KeyID      string `cbor:"key_id"`
	Ciphertext []byte `cbor:"ciphertext"`
}

type cborUserWithSecret struct {
	Username        string           `cbor:"username"`
	DesiredRevision uint64           `cbor:"desired_revision"`
	SealedPassword  cborSealedSecret `cbor:"sealed_password"`
}

type cborUserToggle struct {
	Username        string `cbor:"username"`
	DesiredRevision uint64 `cbor:"desired_revision"`
}

type cborGroupApply struct {
	GroupName       string   `cbor:"group_name"`
	Members         []string `cbor:"members"`
	DesiredRevision uint64   `cbor:"desired_revision"`
}

type cborCompleteConfigPlan struct {
	CandidateHash []byte `cbor:"candidate_hash"`
}

type cborCompleteConfigApply struct {
	CandidateHash        []byte `cbor:"candidate_hash"`
	MaterializedHash     []byte `cbor:"materialized_hash"`
	ExpectedCurrentHash  []byte `cbor:"expected_current_hash"`
	DesiredRevision      uint64 `cbor:"desired_revision"`
	PlanID               []byte `cbor:"plan_id"`
	PlanExpiresAtSeconds int64  `cbor:"plan_expires_at_seconds"`
	PlanExpiresAtNanos   uint32 `cbor:"plan_expires_at_nanos"`
}

type cborCertificateCSR struct {
	CertificateID []byte   `cbor:"certificate_id"`
	CommonName    string   `cbor:"common_name"`
	DNSNames      []string `cbor:"dns_names"`
	KeyBits       uint32   `cbor:"key_bits"`
}

type cborCertificateP12 struct {
	CertificateID            []byte           `cbor:"certificate_id"`
	ArtifactID               []byte           `cbor:"artifact_id"`
	CertificateChainPEM      []byte           `cbor:"certificate_chain_pem"`
	SealedPassword           cborSealedSecret `cbor:"sealed_password"`
	CertificateVersion       uint64           `cbor:"certificate_version"`
	ArtifactExpiresAtSeconds int64            `cbor:"artifact_expires_at_seconds"`
	ArtifactExpiresAtNanos   uint32           `cbor:"artifact_expires_at_nanos"`
}

type cborCertificateRevoke struct {
	CertificateID      []byte `cbor:"certificate_id"`
	Reason             string `cbor:"reason"`
	CertificateVersion uint64 `cbor:"certificate_version"`
}

type cborAgentUpgrade struct {
	TargetVersion string `cbor:"target_version"`
	Architecture  string `cbor:"architecture"`
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

// HashV2 computes the canonical RFC 8949 Core Deterministic CBOR semantic hash
// for a reconcilable command envelope.
func HashV2(envelope *agentv1.CommandEnvelope) ([sha256.Size]byte, error) {
	return hashV2(envelope, false)
}

func hashV2(envelope *agentv1.CommandEnvelope, normalizedConfig bool) ([sha256.Size]byte, error) {
	if envelope == nil {
		return [sha256.Size]byte{}, errors.New("command envelope is missing")
	}
	var payloadKind uint32
	var payloadValue any
	switch envelope.Payload.(type) {
	case *agentv1.CommandEnvelope_SyntheticNoop:
		payloadKind = 107
		payloadValue = cborEmpty{}
	case *agentv1.CommandEnvelope_SyntheticEcho:
		payloadKind = 108
		payloadValue = cborEcho{Message: envelope.GetSyntheticEcho().GetMessage()}
	case *agentv1.CommandEnvelope_SessionDisconnect:
		payloadKind = 100
		p := envelope.GetSessionDisconnect()
		payloadValue = cborSession{SessionID: p.GetSessionId(), BootID: p.GetBootId()}
	case *agentv1.CommandEnvelope_ServiceReload:
		payloadKind = 105
		payloadValue = cborEmpty{}
	case *agentv1.CommandEnvelope_CompleteConfigPlan:
		p := envelope.GetCompleteConfigPlan()
		if !normalizedConfig {
			digest, err := configprofile.Hash(p.GetCandidate())
			if err != nil || !bytes.Equal(digest[:], p.GetCandidateHash()) || !bytes.Equal(p.GetCandidate().GetNodeId(), envelope.GetNodeId()) {
				return [sha256.Size]byte{}, errors.New("complete config plan identity is malformed")
			}
		}
		payloadKind = 129
		payloadValue = cborCompleteConfigPlan{CandidateHash: p.GetCandidateHash()}
	case *agentv1.CommandEnvelope_CompleteConfigApply:
		p := envelope.GetCompleteConfigApply()
		if !normalizedConfig {
			digest, err := configprofile.Hash(p.GetCandidate())
			if err != nil || !bytes.Equal(digest[:], p.GetCandidateHash()) || !bytes.Equal(p.GetCandidate().GetNodeId(), envelope.GetNodeId()) {
				return [sha256.Size]byte{}, errors.New("complete config apply identity is malformed")
			}
		}
		expires := p.GetPlanExpiresAt()
		if len(p.GetMaterializedHash()) != 32 || len(p.GetExpectedCurrentHash()) != 32 || p.GetDesiredRevision() <= p.GetCandidate().GetExpectedRevision() || len(p.GetPlanId()) != 16 || expires == nil || !expires.IsValid() || expires.GetNanos() < 0 {
			return [sha256.Size]byte{}, errors.New("complete config apply identity is malformed")
		}
		payloadKind = 130
		payloadValue = cborCompleteConfigApply{
			CandidateHash:        p.GetCandidateHash(),
			MaterializedHash:     p.GetMaterializedHash(),
			ExpectedCurrentHash:  p.GetExpectedCurrentHash(),
			DesiredRevision:      p.GetDesiredRevision(),
			PlanID:               p.GetPlanId(),
			PlanExpiresAtSeconds: expires.GetSeconds(),
			PlanExpiresAtNanos:   uint32(expires.GetNanos()),
		}
	case *agentv1.CommandEnvelope_SessionTerminate:
		payloadKind = 112
		p := envelope.GetSessionTerminate()
		payloadValue = cborSession{SessionID: p.GetSessionId(), BootID: p.GetBootId()}
	case *agentv1.CommandEnvelope_IpBanRemove:
		payloadKind = 113
		ip := envelope.GetIpBanRemove().GetIp()
		parsed := net.ParseIP(ip)
		if parsed == nil || parsed.String() != ip {
			return [sha256.Size]byte{}, errors.New("IP address is not canonical")
		}
		payloadValue = cborIPBan{IP: ip}
	case *agentv1.CommandEnvelope_UserCreate:
		payloadKind = 101
		p := envelope.GetUserCreate()
		secret, err := validateSealedSecret(p.GetSealedPasswordV1(), agentv1.SealedSecretPurpose_SEALED_SECRET_PURPOSE_USER_PASSWORD)
		if err != nil {
			return [sha256.Size]byte{}, errors.New("user sealed secret is malformed")
		}
		payloadValue = cborUserWithSecret{
			Username:        p.GetUsername(),
			DesiredRevision: p.GetDesiredRevision(),
			SealedPassword:  secret,
		}
	case *agentv1.CommandEnvelope_UserDisable:
		payloadKind = 102
		p := envelope.GetUserDisable()
		payloadValue = cborUserToggle{Username: p.GetUsername(), DesiredRevision: p.GetDesiredRevision()}
	case *agentv1.CommandEnvelope_UserEnable:
		payloadKind = 116
		p := envelope.GetUserEnable()
		payloadValue = cborUserToggle{Username: p.GetUsername(), DesiredRevision: p.GetDesiredRevision()}
	case *agentv1.CommandEnvelope_UserPasswordRotate:
		payloadKind = 114
		p := envelope.GetUserPasswordRotate()
		secret, err := validateSealedSecret(p.GetSealedPasswordV1(), agentv1.SealedSecretPurpose_SEALED_SECRET_PURPOSE_USER_PASSWORD)
		if err != nil {
			return [sha256.Size]byte{}, errors.New("user sealed secret is malformed")
		}
		payloadValue = cborUserWithSecret{
			Username:        p.GetUsername(),
			DesiredRevision: p.GetDesiredRevision(),
			SealedPassword:  secret,
		}
	case *agentv1.CommandEnvelope_GroupApply:
		payloadKind = 115
		p := envelope.GetGroupApply()
		payloadValue = cborGroupApply{
			GroupName:       p.GetGroupName(),
			Members:         nonNilStrings(p.GetMembers()),
			DesiredRevision: p.GetDesiredRevision(),
		}
	case *agentv1.CommandEnvelope_CertificateCsr:
		payloadKind = 117
		p := envelope.GetCertificateCsr()
		if len(p.GetCertificateId()) != 16 {
			return [sha256.Size]byte{}, errors.New("certificate ID is malformed")
		}
		payloadValue = cborCertificateCSR{
			CertificateID: p.GetCertificateId(),
			CommonName:    p.GetCommonName(),
			DNSNames:      nonNilStrings(p.GetDnsNames()),
			KeyBits:       p.GetKeyBits(),
		}
	case *agentv1.CommandEnvelope_CertificateP12:
		payloadKind = 118
		p := envelope.GetCertificateP12()
		if len(p.GetCertificateId()) != 16 || len(p.GetArtifactId()) != 16 {
			return [sha256.Size]byte{}, errors.New("certificate artifact identity is malformed")
		}
		secret, err := validateSealedSecret(p.GetSealedPasswordV1(), agentv1.SealedSecretPurpose_SEALED_SECRET_PURPOSE_CERTIFICATE_P12_PASSWORD)
		expires := p.GetArtifactExpiresAt()
		if err != nil || p.GetCertificateVersion() == 0 || expires == nil || !expires.IsValid() || expires.GetNanos() < 0 {
			return [sha256.Size]byte{}, errors.New("certificate P12 binding is malformed")
		}
		payloadValue = cborCertificateP12{
			CertificateID:            p.GetCertificateId(),
			ArtifactID:               p.GetArtifactId(),
			CertificateChainPEM:      nonNilBytes(p.GetCertificateChainPem()),
			SealedPassword:           secret,
			CertificateVersion:       p.GetCertificateVersion(),
			ArtifactExpiresAtSeconds: expires.GetSeconds(),
			ArtifactExpiresAtNanos:   uint32(expires.GetNanos()),
		}
	case *agentv1.CommandEnvelope_CertificateRevoke:
		payloadKind = 119
		p := envelope.GetCertificateRevoke()
		if len(p.GetCertificateId()) != 16 || strings.TrimSpace(p.GetReason()) == "" {
			return [sha256.Size]byte{}, errors.New("certificate revoke payload is malformed")
		}
		if p.GetCertificateVersion() == 0 {
			return [sha256.Size]byte{}, errors.New("certificate revoke version is malformed")
		}
		payloadValue = cborCertificateRevoke{
			CertificateID:      p.GetCertificateId(),
			Reason:             p.GetReason(),
			CertificateVersion: p.GetCertificateVersion(),
		}
	case *agentv1.CommandEnvelope_AgentUpgrade:
		payloadKind = 128
		p := envelope.GetAgentUpgrade()
		if !ValidAgentUpgradeTargetVersion(p.GetTargetVersion()) || len(p.GetPackageSha256()) != sha256.Size || !ValidAgentUpgradeArchitecture(p.GetArchitecture()) {
			return [sha256.Size]byte{}, errors.New("agent upgrade release identity is malformed")
		}
		payloadValue = cborAgentUpgrade{
			TargetVersion: p.GetTargetVersion(),
			Architecture:  p.GetArchitecture(),
			PackageSHA256: p.GetPackageSha256(),
		}
	default:
		return [sha256.Size]byte{}, errors.New("command payload type is not reconcilable")
	}

	return canonicalcbor.Hash(domainSeparatorV2, cborEnvelope{
		NodeID:           nonNilBytes(envelope.GetNodeId()),
		ExpectedRevision: envelope.GetExpectedRevision(),
		PayloadKind:      payloadKind,
		Payload:          payloadValue,
	})
}

func validateSealedSecret(secret *agentv1.SealedSecretV1, purpose agentv1.SealedSecretPurpose) (cborSealedSecret, error) {
	if secret == nil || secret.GetVersion() != 1 || secret.GetPurpose() != purpose || strings.TrimSpace(secret.GetKeyId()) == "" || len(secret.GetCiphertext()) < 32 || len(secret.GetCiphertext()) > 16*1024 {
		return cborSealedSecret{}, errors.New("sealed secret is malformed")
	}
	return cborSealedSecret{
		Version:    secret.GetVersion(),
		Purpose:    uint32(secret.GetPurpose()),
		KeyID:      secret.GetKeyId(),
		Ciphertext: secret.GetCiphertext(),
	}, nil
}

// AgentUpgradeArchitectures are the release package architectures of the
// published native agent packages.
var AgentUpgradeArchitectures = []string{"amd64", "arm64"}

// ValidAgentUpgradeArchitecture reports whether architecture is one of the
// supported agent package architectures.
func ValidAgentUpgradeArchitecture(architecture string) bool {
	return slices.Contains(AgentUpgradeArchitectures, architecture)
}

// ValidAgentUpgradeTargetVersion reports whether version is a strict SemVer
// 2.0.0 version without the Go module "v" prefix.
func ValidAgentUpgradeTargetVersion(version string) bool {
	if len(version) < 5 || len(version) > 128 {
		return false
	}
	core, _, _ := strings.Cut(version, "+")
	core, _, _ = strings.Cut(core, "-")
	return strings.Count(core, ".") == 2 && semver.IsValid("v"+version)
}

// PopulateV2 fills the current semantic identity on a command envelope.
func PopulateV2(envelope *agentv1.CommandEnvelope) error {
	digest, err := HashV2(envelope)
	return populate(envelope, digest, err)
}

// PopulateConfigV2 binds a validated candidate to an outgoing config command.
func PopulateConfigV2(envelope *agentv1.CommandEnvelope, candidate configprofile.Normalized) error {
	profile := candidate.Proto()
	if !bytes.Equal(profile.GetNodeId(), envelope.GetNodeId()) || len(envelope.GetNodeId()) != 16 {
		return errors.New("complete config identity is malformed")
	}
	switch p := envelope.Payload.(type) {
	case *agentv1.CommandEnvelope_CompleteConfigPlan:
		p.CompleteConfigPlan.Candidate, p.CompleteConfigPlan.CandidateHash = profile, candidate.Digest()
	case *agentv1.CommandEnvelope_CompleteConfigApply:
		p.CompleteConfigApply.Candidate, p.CompleteConfigApply.CandidateHash = profile, candidate.Digest()
	default:
		return errors.New("complete config command required")
	}
	digest, err := hashV2(envelope, true)
	return populate(envelope, digest, err)
}

func populate(envelope *agentv1.CommandEnvelope, digest [sha256.Size]byte, err error) error {
	if err != nil {
		return err
	}
	envelope.SemanticPayloadHashVersion = agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V2
	envelope.SemanticPayloadSha256 = digest[:]
	return nil
}
