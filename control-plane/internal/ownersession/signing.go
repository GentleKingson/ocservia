package ownersession

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/connectionowner"
)

// bindingTerm is an immutable snapshot of the session's current term. It is
// copied under the node mutex and signed outside it, so renewal can never
// race a concurrent binding issuance on the same fields.
type bindingTerm struct {
	fence                 *agentv1.ConnectionFenceV2
	fenceID               [16]byte
	nodeID                [16]byte
	endpointID            [32]byte
	identity              connectionowner.Identity
	epoch                 int64
	connectionID          [16]byte
	authorizationRevision uint64
	capabilities          []string
}

// issueBinding signs one binding for the term's current fence. The fence
// travels with the binding so enforcing peers never need to guess the term.
func (m *Manager) issueBinding(term *bindingTerm, kind agentv1.FenceOperationKind, operationID [16]byte, capability string) (*agentv1.ConnectionFenceV2, *agentv1.FenceBindingV2, error) {
	if !slices.Contains(term.capabilities, capability) {
		return nil, nil, ErrCapabilityNotFenced
	}
	now := m.now().UTC()
	binding, err := m.signer.IssueFenceBindingV2(kind, operationID, term.fenceID, term.nodeID, term.endpointID,
		term.identity.InstanceID, uint64(term.identity.Incarnation), uint64(term.epoch),
		term.connectionID, term.authorizationRevision, capability, now, now.Add(m.bindingTTL))
	if err != nil {
		return nil, nil, fmt.Errorf("ownersession: sign fence binding: %w", err)
	}
	return term.fence, binding, nil
}

// signFence signs the session term with its exact PostgreSQL lease deadline.
// The fence expiry deliberately outlives the lease: the lease authorizes
// current mutations while the proof remains verifiable for audit and late
// result matching.
func (m *Manager) signFence(session *nodeSession) (*agentv1.ConnectionFenceV2, error) {
	issued := m.now().UTC()
	if !session.leaseUntil.After(issued) {
		return nil, errors.New("ownersession: lease deadline is not in the future")
	}
	fence, err := m.signer.IssueConnectionFenceV2(session.fenceID, session.nodeID, session.endpointID,
		session.term.Identity().InstanceID, uint64(session.term.Identity().Incarnation), uint64(session.term.Epoch()),
		session.term.ConnectionID(), session.authorizationRevision, session.capabilities,
		session.leaseUntil, issued, session.leaseUntil.Add(m.fenceGrace))
	if err != nil {
		return nil, fmt.Errorf("ownersession: sign connection fence: %w", err)
	}
	return fence, nil
}

// StateUpdateOperationID derives the canonical operation identity of one node
// trust update from its exact carrier: node, endpoint, state, revision, and
// reason. The Controller signs fence bindings over this identity and
// transportd derives it again independently, so one binding cannot authorize
// a different update.
func StateUpdateOperationID(nodeID [16]byte, endpointID []byte, state int32, revision uint64, reason string) [16]byte {
	digest := sha256.New()
	digest.Write(stateUpdateOperationDomain)
	digest.Write(nodeID[:])
	digest.Write(endpointID)
	var stateBytes [4]byte
	binary.BigEndian.PutUint32(stateBytes[:], uint32(state))
	digest.Write(stateBytes[:])
	var revisionBytes [8]byte
	binary.BigEndian.PutUint64(revisionBytes[:], revision)
	digest.Write(revisionBytes[:])
	reasonBytes := []byte(reason)
	var lengthBytes [4]byte
	binary.BigEndian.PutUint32(lengthBytes[:], uint32(len(reasonBytes)))
	digest.Write(lengthBytes[:])
	digest.Write(reasonBytes)
	var operationID [16]byte
	copy(operationID[:], digest.Sum(nil)[:16])
	return operationID
}

// fenceCapabilities normalizes a negotiated capability set into the frozen
// fence encoding: sorted, unique, and bounded.
func fenceCapabilities(capabilities []string) []string {
	normalized := make([]string, 0, len(capabilities))
	seen := make(map[string]struct{}, len(capabilities))
	for _, capability := range capabilities {
		if capability == "" || len(capability) > 128 {
			continue
		}
		if _, duplicate := seen[capability]; duplicate {
			continue
		}
		seen[capability] = struct{}{}
		normalized = append(normalized, capability)
	}
	slices.Sort(normalized)
	return normalized
}

func fixed16(value []byte) ([16]byte, error) {
	if len(value) != 16 {
		return [16]byte{}, errors.New("ownersession: value must be 16 bytes")
	}
	var fixed [16]byte
	copy(fixed[:], value)
	return fixed, nil
}

func fixed32(value []byte) ([32]byte, error) {
	if len(value) != 32 {
		return [32]byte{}, errors.New("ownersession: value must be 32 bytes")
	}
	var fixed [32]byte
	copy(fixed[:], value)
	return fixed, nil
}
