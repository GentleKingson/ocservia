// Package semanticpayload computes the versioned canonical semantic hash for
// command envelopes, shared by the localslice and operations packages.
package semanticpayload

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"net"
	"strings"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
)

// ValidateVersion rejects hash algorithms that this binary cannot verify.
//
// The version is part of the durable command identity and must never silently
// fall back to the legacy algorithm when a newer value is received.
func ValidateVersion(version agentv1.SemanticPayloadHashVersion) error {
	switch version {
	case agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_UNSPECIFIED,
		agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V1,
		agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V2:
		return nil
	default:
		return errors.New("semantic payload hash version is unsupported")
	}
}

// domainSeparatorV1 is the ASCII label plus NUL terminator defined by
// docs/development/command-semantic-hash-v1.md.
var domainSeparatorV1 = []byte("ocservia.command.semantic-hash.v1\x00")
var domainSeparatorV2 = []byte("ocservia.command.semantic-hash.v2\x00")

// HashV1 computes the versioned canonical semantic hash (v1) for a reconcilable
// command envelope.
//
// The preimage is hand-specified canonical bytes, never Protobuf wire encoding,
// so it is stable across Go and Rust regardless of unknown-field retention.
// Only explicitly mapped typed payloads are reconcilable; other payload types
// (including SimulationProbe) return an error.
func HashV1(envelope *agentv1.CommandEnvelope) ([sha256.Size]byte, error) {
	return hash(envelope, agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V1)
}

// HashV2 binds the authorization revision and the independent ConfigPlan
// expected revision without changing the frozen v1 layout.
func HashV2(envelope *agentv1.CommandEnvelope) ([sha256.Size]byte, error) {
	return hash(envelope, agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V2)
}

func hash(envelope *agentv1.CommandEnvelope, version agentv1.SemanticPayloadHashVersion) ([sha256.Size]byte, error) {
	var payloadKind uint32
	var canonicalPayload []byte
	switch envelope.Payload.(type) {
	case *agentv1.CommandEnvelope_SyntheticNoop:
		payloadKind = 107
	case *agentv1.CommandEnvelope_SyntheticEcho:
		payloadKind = 108
		utf8 := []byte(envelope.GetSyntheticEcho().GetMessage())
		canonicalPayload = make([]byte, 4+len(utf8))
		binary.BigEndian.PutUint32(canonicalPayload[:4], uint32(len(utf8)))
		copy(canonicalPayload[4:], utf8)
	case *agentv1.CommandEnvelope_SessionDisconnect:
		payloadKind = 100
		canonicalPayload = canonicalStrings(envelope.GetSessionDisconnect().GetSessionId(), envelope.GetSessionDisconnect().GetBootId())
	case *agentv1.CommandEnvelope_ServiceReload:
		payloadKind = 105
	case *agentv1.CommandEnvelope_ConfigPlan:
		payloadKind = 103
		payload := envelope.GetConfigPlan()
		if len(payload.GetCandidateHash()) != sha256.Size {
			return [sha256.Size]byte{}, errors.New("candidate hash is malformed")
		}
		canonicalPayload = append(canonicalPayload, payload.GetCandidateHash()...)
		if version == agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V2 {
			var expected [8]byte
			binary.BigEndian.PutUint64(expected[:], payload.GetExpectedRevision())
			canonicalPayload = append(canonicalPayload, expected[:]...)
		}
	case *agentv1.CommandEnvelope_ConfigApply:
		payloadKind = 104
		payload := envelope.GetConfigApply()
		if len(payload.GetCandidateHash()) != sha256.Size || len(payload.GetExpectedCurrentHash()) != sha256.Size || payload.GetDesiredRevision() == 0 {
			return [sha256.Size]byte{}, errors.New("configuration apply identity is malformed")
		}
		canonicalPayload = append(canonicalPayload, payload.GetCandidateHash()...)
		canonicalPayload = append(canonicalPayload, payload.GetExpectedCurrentHash()...)
		var desired [8]byte
		binary.BigEndian.PutUint64(desired[:], payload.GetDesiredRevision())
		canonicalPayload = append(canonicalPayload, desired[:]...)
	case *agentv1.CommandEnvelope_SessionTerminate:
		payloadKind = 112
		canonicalPayload = canonicalStrings(envelope.GetSessionTerminate().GetSessionId(), envelope.GetSessionTerminate().GetBootId())
	case *agentv1.CommandEnvelope_IpBanRemove:
		payloadKind = 113
		ip := envelope.GetIpBanRemove().GetIp()
		parsed := net.ParseIP(ip)
		if parsed == nil || parsed.String() != ip {
			return [sha256.Size]byte{}, errors.New("IP address is not canonical")
		}
		canonicalPayload = canonicalStrings(ip)
	case *agentv1.CommandEnvelope_UserCreate:
		payloadKind = 101
		payload := envelope.GetUserCreate()
		secret, err := canonicalSecret(payload.GetSealedPasswordV1(), agentv1.SealedSecretPurpose_SEALED_SECRET_PURPOSE_USER_PASSWORD)
		if err != nil || len(payload.GetSealedPassword()) != 0 || payload.GetSecretKeyId() != "" {
			return [sha256.Size]byte{}, errors.New("user sealed secret is malformed")
		}
		canonicalPayload = canonicalStringsAndBytes([]string{payload.GetUsername(), payload.GetSealedPasswordV1().GetKeyId()}, secret, payload.GetDesiredRevision())
	case *agentv1.CommandEnvelope_UserDisable:
		payloadKind = 102
		payload := envelope.GetUserDisable()
		canonicalPayload = canonicalStringsAndBytes([]string{payload.GetUsername()}, nil, payload.GetDesiredRevision())
	case *agentv1.CommandEnvelope_UserEnable:
		payloadKind = 116
		payload := envelope.GetUserEnable()
		canonicalPayload = canonicalStringsAndBytes([]string{payload.GetUsername()}, nil, payload.GetDesiredRevision())
	case *agentv1.CommandEnvelope_UserPasswordRotate:
		payloadKind = 114
		payload := envelope.GetUserPasswordRotate()
		secret, err := canonicalSecret(payload.GetSealedPasswordV1(), agentv1.SealedSecretPurpose_SEALED_SECRET_PURPOSE_USER_PASSWORD)
		if err != nil || len(payload.GetSealedPassword()) != 0 || payload.GetSecretKeyId() != "" {
			return [sha256.Size]byte{}, errors.New("user sealed secret is malformed")
		}
		canonicalPayload = canonicalStringsAndBytes([]string{payload.GetUsername(), payload.GetSealedPasswordV1().GetKeyId()}, secret, payload.GetDesiredRevision())
	case *agentv1.CommandEnvelope_GroupApply:
		payloadKind = 115
		payload := envelope.GetGroupApply()
		values := append([]string{payload.GetGroupName()}, payload.GetMembers()...)
		canonicalPayload = canonicalStringsAndBytes(values, nil, payload.GetDesiredRevision())
	case *agentv1.CommandEnvelope_CertificateCsr:
		payloadKind = 117
		payload := envelope.GetCertificateCsr()
		if len(payload.GetCertificateId()) != 16 {
			return [sha256.Size]byte{}, errors.New("certificate ID is malformed")
		}
		values := append([]string{payload.GetCommonName()}, payload.GetDnsNames()...)
		canonicalPayload = canonicalStringsAndBytes(values, payload.GetCertificateId(), uint64(payload.GetKeyBits()))
	case *agentv1.CommandEnvelope_CertificateP12:
		payloadKind = 118
		payload := envelope.GetCertificateP12()
		if len(payload.GetCertificateId()) != 16 || len(payload.GetArtifactId()) != 16 {
			return [sha256.Size]byte{}, errors.New("certificate artifact identity is malformed")
		}
		secret, err := canonicalSecret(payload.GetSealedPasswordV1(), agentv1.SealedSecretPurpose_SEALED_SECRET_PURPOSE_CERTIFICATE_P12_PASSWORD)
		expires := payload.GetArtifactExpiresAt()
		if err != nil || len(payload.GetSealedPassword()) != 0 || payload.GetSecretKeyId() != "" || payload.GetCertificateVersion() == 0 || expires == nil || !expires.IsValid() {
			return [sha256.Size]byte{}, errors.New("certificate P12 binding is malformed")
		}
		data := append(append(append([]byte(nil), payload.GetCertificateId()...), payload.GetArtifactId()...), payload.GetCertificateChainPem()...)
		data = append(data, secret...)
		var expiry [12]byte
		binary.BigEndian.PutUint64(expiry[:8], uint64(expires.GetSeconds()))
		binary.BigEndian.PutUint32(expiry[8:], uint32(expires.GetNanos()))
		data = append(data, expiry[:]...)
		canonicalPayload = canonicalStringsAndBytes([]string{payload.GetSealedPasswordV1().GetKeyId()}, data, payload.GetCertificateVersion())
	case *agentv1.CommandEnvelope_CertificateRevoke:
		payloadKind = 119
		payload := envelope.GetCertificateRevoke()
		if len(payload.GetCertificateId()) != 16 || strings.TrimSpace(payload.GetReason()) == "" {
			return [sha256.Size]byte{}, errors.New("certificate revoke payload is malformed")
		}
		if payload.GetCertificateVersion() == 0 {
			return [sha256.Size]byte{}, errors.New("certificate revoke version is malformed")
		}
		canonicalPayload = canonicalStringsAndBytes([]string{payload.GetReason()}, payload.GetCertificateId(), payload.GetCertificateVersion())
	default:
		return [sha256.Size]byte{}, errors.New("command payload type is not reconcilable")
	}
	h := sha256.New()
	switch version {
	case agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V1:
		h.Write(domainSeparatorV1)
	case agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V2:
		h.Write(domainSeparatorV2)
	default:
		return [sha256.Size]byte{}, errors.New("semantic payload hash version is unsupported")
	}
	h.Write(envelope.GetNodeId())
	var rev [8]byte
	binary.BigEndian.PutUint64(rev[:], envelope.GetExpectedRevision())
	h.Write(rev[:])
	var kind [4]byte
	binary.BigEndian.PutUint32(kind[:], payloadKind)
	h.Write(kind[:])
	h.Write(canonicalPayload)
	var out [sha256.Size]byte
	copy(out[:], h.Sum(nil))
	return out, nil
}

func canonicalSecret(secret *agentv1.SealedSecretV1, purpose agentv1.SealedSecretPurpose) ([]byte, error) {
	if secret == nil || secret.GetVersion() != agentv1.SealedSecretVersion_SEALED_SECRET_VERSION_V1 || secret.GetPurpose() != purpose || strings.TrimSpace(secret.GetKeyId()) == "" || len(secret.GetCiphertext()) < 32 || len(secret.GetCiphertext()) > 16*1024 {
		return nil, errors.New("sealed secret is malformed")
	}
	out := make([]byte, 8, 8+len(secret.GetCiphertext()))
	binary.BigEndian.PutUint32(out[:4], uint32(secret.GetVersion()))
	binary.BigEndian.PutUint32(out[4:8], uint32(secret.GetPurpose()))
	return append(out, secret.GetCiphertext()...), nil
}

func canonicalStringsAndBytes(values []string, value []byte, revision uint64) []byte {
	out := canonicalStrings(values...)
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	out = append(out, length[:]...)
	out = append(out, value...)
	var encodedRevision [8]byte
	binary.BigEndian.PutUint64(encodedRevision[:], revision)
	return append(out, encodedRevision[:]...)
}

func canonicalStrings(values ...string) []byte {
	size := 4 * len(values)
	for _, value := range values {
		size += len(value)
	}
	out := make([]byte, 0, size)
	for _, value := range values {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(value)))
		out = append(out, length[:]...)
		out = append(out, value...)
	}
	return out
}

// PopulateV1 fills the versioned hash fields on a reconcilable command envelope.
//
// Non-reconcilable payloads return an error so the caller can decide whether to
// leave the envelope on the legacy/empty path or reject the command.
func PopulateV1(envelope *agentv1.CommandEnvelope) error {
	digest, err := HashV1(envelope)
	if err != nil {
		return err
	}
	envelope.SemanticPayloadHashVersion = agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V1
	envelope.SemanticPayloadSha256 = digest[:]
	return nil
}

// PopulateV2 fills the current semantic identity on a command envelope.
func PopulateV2(envelope *agentv1.CommandEnvelope) error {
	digest, err := HashV2(envelope)
	if err != nil {
		return err
	}
	envelope.SemanticPayloadHashVersion = agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V2
	envelope.SemanticPayloadSha256 = digest[:]
	return nil
}
