// Package semanticpayload computes the versioned canonical semantic hash for
// command envelopes, shared by the localslice and operations packages.
package semanticpayload

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"

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
// Only SyntheticNoop and SyntheticEcho payloads are reconcilable; other payload
// types (including SimulationProbe) return an error.
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
