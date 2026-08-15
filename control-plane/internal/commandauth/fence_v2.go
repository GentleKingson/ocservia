package commandauth

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var connectionFenceV2Domain = []byte("ocservia/connection-fence/v2\x00")
var fenceBindingV2Domain = []byte("ocservia/fence-binding/v2\x00")

// ConnectionFenceClaimsV2 is the independent, non-Protobuf signing input for
// the per-node connection-owner fence. It binds the exact ownership term
// recorded by the database authority: owner instance, process incarnation,
// connection identity, and the monotonic fencing epoch. V1 session grants
// cannot represent these claims and are never reinterpreted as fences.
type ConnectionFenceClaimsV2 struct {
	SignatureVersion      uint32
	KeyID                 string
	FenceID               [16]byte
	NodeID                [16]byte
	EndpointID            [32]byte
	OwnerInstanceID       [16]byte
	OwnerIncarnation      uint64
	OwnerEpoch            uint64
	ConnectionID          [16]byte
	AuthorizationRevision uint64
	Capabilities          []string
	LeaseUntilSeconds     int64
	LeaseUntilNanos       uint32
	IssuedAtSeconds       int64
	IssuedAtNanos         uint32
	ExpiresAtSeconds      int64
	ExpiresAtNanos        uint32
}

// FenceBindingClaimsV2 binds exactly one operation identity (command,
// artifact acknowledgement, connection close, or state update) to the fence
// identity tuple. A binding signed for one epoch never authorizes work
// recorded under a different epoch or connection.
type FenceBindingClaimsV2 struct {
	SignatureVersion      uint32
	KeyID                 string
	OperationKind         uint32
	OperationID           [16]byte
	FenceID               [16]byte
	NodeID                [16]byte
	EndpointID            [32]byte
	OwnerInstanceID       [16]byte
	OwnerIncarnation      uint64
	OwnerEpoch            uint64
	ConnectionID          [16]byte
	AuthorizationRevision uint64
	Capability            string
	IssuedAtSeconds       int64
	IssuedAtNanos         uint32
	ExpiresAtSeconds      int64
	ExpiresAtNanos        uint32
}

// ConnectionFenceV2SignatureVersion is the only signature version defined by
// the frozen V2 contract: Ed25519 with the sha256 key identifier scheme.
const ConnectionFenceV2SignatureVersion = uint32(agentv1.FenceSignatureVersion_FENCE_SIGNATURE_VERSION_ED25519_V1)

// IssueConnectionFenceV2 signs the ownership term exactly as the database
// authority recorded it. The lease deadline must be anchored to PostgreSQL
// time by the caller and must not outlive the proof expiry.
func (s *Signer) IssueConnectionFenceV2(fenceID, nodeID [16]byte, endpointID [32]byte, ownerInstanceID [16]byte, ownerIncarnation, ownerEpoch uint64, connectionID [16]byte, authorizationRevision uint64, capabilities []string, leaseUntil, issuedAt, expiresAt time.Time) (*agentv1.ConnectionFenceV2, error) {
	if s == nil || len(s.privateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("controller command signing key is unavailable")
	}
	claims := ConnectionFenceClaimsV2{
		SignatureVersion: ConnectionFenceV2SignatureVersion, KeyID: s.keyID,
		FenceID: fenceID, NodeID: nodeID, EndpointID: endpointID,
		OwnerInstanceID: ownerInstanceID, OwnerIncarnation: ownerIncarnation, OwnerEpoch: ownerEpoch,
		ConnectionID: connectionID, AuthorizationRevision: authorizationRevision,
		Capabilities:      append([]string(nil), capabilities...),
		LeaseUntilSeconds: leaseUntil.Unix(), LeaseUntilNanos: uint32(leaseUntil.Nanosecond()),
		IssuedAtSeconds: issuedAt.Unix(), IssuedAtNanos: uint32(issuedAt.Nanosecond()),
		ExpiresAtSeconds: expiresAt.Unix(), ExpiresAtNanos: uint32(expiresAt.Nanosecond()),
	}
	canonical, err := CanonicalConnectionFenceV2(claims)
	if err != nil {
		return nil, err
	}
	return &agentv1.ConnectionFenceV2{
		SignatureVersion: agentv1.FenceSignatureVersion_FENCE_SIGNATURE_VERSION_ED25519_V1,
		KeyId:            s.keyID, FenceId: fenceID[:], NodeId: nodeID[:], EndpointId: endpointID[:],
		OwnerInstanceId: ownerInstanceID[:], OwnerIncarnation: ownerIncarnation, OwnerEpoch: ownerEpoch,
		ConnectionId: connectionID[:], AuthorizationRevision: authorizationRevision,
		Capabilities: append([]string(nil), capabilities...),
		LeaseUntil:   timestamppb.New(leaseUntil), IssuedAt: timestamppb.New(issuedAt), ExpiresAt: timestamppb.New(expiresAt),
		Signature: ed25519.Sign(s.privateKey, canonical),
	}, nil
}

// IssueFenceBindingV2 signs one operation identity against the exact fence
// term of the dispatch attempt.
func (s *Signer) IssueFenceBindingV2(operationKind agentv1.FenceOperationKind, operationID, fenceID, nodeID [16]byte, endpointID [32]byte, ownerInstanceID [16]byte, ownerIncarnation, ownerEpoch uint64, connectionID [16]byte, authorizationRevision uint64, capability string, issuedAt, expiresAt time.Time) (*agentv1.FenceBindingV2, error) {
	if s == nil || len(s.privateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("controller command signing key is unavailable")
	}
	claims := FenceBindingClaimsV2{
		SignatureVersion: ConnectionFenceV2SignatureVersion, KeyID: s.keyID,
		OperationKind: uint32(operationKind), OperationID: operationID, FenceID: fenceID,
		NodeID: nodeID, EndpointID: endpointID, OwnerInstanceID: ownerInstanceID,
		OwnerIncarnation: ownerIncarnation, OwnerEpoch: ownerEpoch, ConnectionID: connectionID,
		AuthorizationRevision: authorizationRevision, Capability: capability,
		IssuedAtSeconds: issuedAt.Unix(), IssuedAtNanos: uint32(issuedAt.Nanosecond()),
		ExpiresAtSeconds: expiresAt.Unix(), ExpiresAtNanos: uint32(expiresAt.Nanosecond()),
	}
	canonical, err := CanonicalFenceBindingV2(claims)
	if err != nil {
		return nil, err
	}
	return &agentv1.FenceBindingV2{
		SignatureVersion: agentv1.FenceSignatureVersion_FENCE_SIGNATURE_VERSION_ED25519_V1,
		KeyId:            s.keyID, OperationKind: operationKind, OperationId: operationID[:],
		FenceId: fenceID[:], NodeId: nodeID[:], EndpointId: endpointID[:],
		OwnerInstanceId: ownerInstanceID[:], OwnerIncarnation: ownerIncarnation, OwnerEpoch: ownerEpoch,
		ConnectionId: connectionID[:], AuthorizationRevision: authorizationRevision,
		Capability: capability, IssuedAt: timestamppb.New(issuedAt), ExpiresAt: timestamppb.New(expiresAt),
		Signature: ed25519.Sign(s.privateKey, canonical),
	}, nil
}

// MatchesFence reports whether this binding was signed for exactly the fence
// term of the recorded dispatch attempt: owner instance, incarnation,
// connection, epoch, fence identity, and authorization revision. A late
// legitimate result still matches its own attempt's fence; a result recorded
// under any other term does not.
func (c FenceBindingClaimsV2) MatchesFence(fence ConnectionFenceClaimsV2) bool {
	return c.FenceID == fence.FenceID &&
		c.OwnerInstanceID == fence.OwnerInstanceID &&
		c.OwnerIncarnation == fence.OwnerIncarnation &&
		c.OwnerEpoch == fence.OwnerEpoch &&
		c.ConnectionID == fence.ConnectionID &&
		c.AuthorizationRevision == fence.AuthorizationRevision
}

// CanonicalConnectionFenceV2 returns the exact domain-separated connection
// fence signing bytes. It never serializes Protobuf and shares no domain with
// any V1 contract, so V1 and V2 proofs can never cross-validate.
func CanonicalConnectionFenceV2(claims ConnectionFenceClaimsV2) ([]byte, error) {
	if claims.SignatureVersion != ConnectionFenceV2SignatureVersion || claims.KeyID == "" || len(claims.KeyID) > 128 ||
		claims.OwnerEpoch == 0 || claims.AuthorizationRevision == 0 ||
		claims.LeaseUntilNanos >= 1_000_000_000 || claims.IssuedAtNanos >= 1_000_000_000 || claims.ExpiresAtNanos >= 1_000_000_000 ||
		!(leaseDeadlineAfter(claims.LeaseUntilSeconds, claims.LeaseUntilNanos, claims.IssuedAtSeconds, claims.IssuedAtNanos)) ||
		!deadlineNotAfter(claims.LeaseUntilSeconds, claims.LeaseUntilNanos, claims.ExpiresAtSeconds, claims.ExpiresAtNanos) ||
		claims.ExpiresAtSeconds < claims.IssuedAtSeconds ||
		(claims.ExpiresAtSeconds == claims.IssuedAtSeconds && claims.ExpiresAtNanos <= claims.IssuedAtNanos) {
		return nil, errors.New("connection fence v2 claims are invalid")
	}
	if len(claims.Capabilities) > 128 || !validateSortedCapabilities(claims.Capabilities) {
		return nil, errors.New("connection fence v2 capabilities are invalid")
	}
	var encoded bytes.Buffer
	encoded.Grow(640)
	encoded.Write(connectionFenceV2Domain)
	writeUint32(&encoded, claims.SignatureVersion)
	if err := writeString(&encoded, claims.KeyID); err != nil {
		return nil, err
	}
	encoded.Write(claims.FenceID[:])
	encoded.Write(claims.NodeID[:])
	encoded.Write(claims.EndpointID[:])
	encoded.Write(claims.OwnerInstanceID[:])
	writeUint64(&encoded, claims.OwnerIncarnation)
	writeUint64(&encoded, claims.OwnerEpoch)
	encoded.Write(claims.ConnectionID[:])
	writeUint64(&encoded, claims.AuthorizationRevision)
	writeUint32(&encoded, uint32(len(claims.Capabilities)))
	for _, capability := range claims.Capabilities {
		if err := writeString(&encoded, capability); err != nil {
			return nil, err
		}
	}
	writeInt64(&encoded, claims.LeaseUntilSeconds)
	writeUint32(&encoded, claims.LeaseUntilNanos)
	writeInt64(&encoded, claims.IssuedAtSeconds)
	writeUint32(&encoded, claims.IssuedAtNanos)
	writeInt64(&encoded, claims.ExpiresAtSeconds)
	writeUint32(&encoded, claims.ExpiresAtNanos)
	return encoded.Bytes(), nil
}

// CanonicalFenceBindingV2 returns the exact domain-separated operation
// binding signing bytes. It never serializes Protobuf.
func CanonicalFenceBindingV2(claims FenceBindingClaimsV2) ([]byte, error) {
	if claims.SignatureVersion != ConnectionFenceV2SignatureVersion || claims.KeyID == "" || len(claims.KeyID) > 128 ||
		claims.OperationKind == 0 || claims.OperationKind > 4 || claims.OwnerEpoch == 0 || claims.AuthorizationRevision == 0 ||
		claims.Capability == "" || len(claims.Capability) > 128 ||
		claims.IssuedAtNanos >= 1_000_000_000 || claims.ExpiresAtNanos >= 1_000_000_000 ||
		claims.ExpiresAtSeconds < claims.IssuedAtSeconds ||
		(claims.ExpiresAtSeconds == claims.IssuedAtSeconds && claims.ExpiresAtNanos <= claims.IssuedAtNanos) {
		return nil, errors.New("fence binding v2 claims are invalid")
	}
	var encoded bytes.Buffer
	encoded.Grow(512)
	encoded.Write(fenceBindingV2Domain)
	writeUint32(&encoded, claims.SignatureVersion)
	if err := writeString(&encoded, claims.KeyID); err != nil {
		return nil, err
	}
	writeUint32(&encoded, claims.OperationKind)
	encoded.Write(claims.OperationID[:])
	encoded.Write(claims.FenceID[:])
	encoded.Write(claims.NodeID[:])
	encoded.Write(claims.EndpointID[:])
	encoded.Write(claims.OwnerInstanceID[:])
	writeUint64(&encoded, claims.OwnerIncarnation)
	writeUint64(&encoded, claims.OwnerEpoch)
	encoded.Write(claims.ConnectionID[:])
	writeUint64(&encoded, claims.AuthorizationRevision)
	if err := writeString(&encoded, claims.Capability); err != nil {
		return nil, err
	}
	writeInt64(&encoded, claims.IssuedAtSeconds)
	writeUint32(&encoded, claims.IssuedAtNanos)
	writeInt64(&encoded, claims.ExpiresAtSeconds)
	writeUint32(&encoded, claims.ExpiresAtNanos)
	return encoded.Bytes(), nil
}

// leaseDeadlineAfter reports whether the lease deadline is strictly after the
// issuance instant.
func leaseDeadlineAfter(untilSeconds int64, untilNanos uint32, fromSeconds int64, fromNanos uint32) bool {
	return untilSeconds > fromSeconds || (untilSeconds == fromSeconds && untilNanos > fromNanos)
}

// deadlineNotAfter reports whether the lease deadline never outlives the
// proof expiry.
func deadlineNotAfter(untilSeconds int64, untilNanos uint32, limitSeconds int64, limitNanos uint32) bool {
	return untilSeconds < limitSeconds || (untilSeconds == limitSeconds && untilNanos <= limitNanos)
}

// validateSortedCapabilities validates the frozen capability set encoding rules:
// sorted, non-empty, bounded, and free of duplicates.
func validateSortedCapabilities(capabilities []string) bool {
	previous := ""
	for index, capability := range capabilities {
		if capability == "" || len(capability) > 128 || (index > 0 && previous >= capability) {
			return false
		}
		previous = capability
	}
	return true
}
