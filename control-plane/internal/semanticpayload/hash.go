// Package semanticpayload computes the versioned canonical semantic hash for
// command envelopes, shared by the localslice and operations packages.
package semanticpayload

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"net"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
)

// ValidateVersion rejects hash algorithms that this binary cannot verify.
//
// The version is part of the durable command identity and must never silently
// fall back to the legacy algorithm when a newer value is received.
func ValidateVersion(version agentv1.SemanticPayloadHashVersion) error {
	switch version {
	case agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_UNSPECIFIED,
		agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V1:
		return nil
	default:
		return errors.New("semantic payload hash version is unsupported")
	}
}

// domainSeparatorV1 is the ASCII label plus NUL terminator defined by
// docs/development/command-semantic-hash-v1.md.
var domainSeparatorV1 = []byte("ocservia.command.semantic-hash.v1\x00")

// HashV1 computes the versioned canonical semantic hash (v1) for a reconcilable
// command envelope.
//
// The preimage is hand-specified canonical bytes, never Protobuf wire encoding,
// so it is stable across Go and Rust regardless of unknown-field retention.
// Only explicitly mapped typed payloads are reconcilable; other payload types
// (including SimulationProbe) return an error.
func HashV1(envelope *agentv1.CommandEnvelope) ([sha256.Size]byte, error) {
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
		canonicalPayload = canonicalStringsAndBytes([]string{payload.GetUsername(), payload.GetSecretKeyId()}, payload.GetSealedPassword(), payload.GetDesiredRevision())
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
		canonicalPayload = canonicalStringsAndBytes([]string{payload.GetUsername(), payload.GetSecretKeyId()}, payload.GetSealedPassword(), payload.GetDesiredRevision())
	case *agentv1.CommandEnvelope_GroupApply:
		payloadKind = 115
		payload := envelope.GetGroupApply()
		values := append([]string{payload.GetGroupName()}, payload.GetMembers()...)
		canonicalPayload = canonicalStringsAndBytes(values, nil, payload.GetDesiredRevision())
	default:
		return [sha256.Size]byte{}, errors.New("command payload type is not reconcilable")
	}
	h := sha256.New()
	h.Write(domainSeparatorV1)
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
