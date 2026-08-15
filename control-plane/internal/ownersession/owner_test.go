package ownersession

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/connectionowner"
)

type recordingReader struct {
	fence  *agentv1.ConnectionFenceV2
	err    error
	calls  int
	lastID []byte
}

func (r *recordingReader) GetOwnerFence(_ context.Context, nodeID []byte) (*agentv1.ConnectionFenceV2, error) {
	r.calls++
	r.lastID = append([]byte(nil), nodeID...)
	if r.err != nil {
		return nil, r.err
	}
	return r.fence, nil
}

// ownerStateForFence builds the PostgreSQL row that exactly backs a fence's
// term, so tests can mutate one dimension at a time.
func ownerStateForFence(fence *agentv1.ConnectionFenceV2) connectionowner.OwnerState {
	instance, _ := fixed16(fence.GetOwnerInstanceId())
	connection, _ := fixed16(fence.GetConnectionId())
	return connectionowner.OwnerState{
		InstanceID:      instance,
		Incarnation:     int64(fence.GetOwnerIncarnation()),
		ConnectionID:    connection,
		Epoch:           int64(fence.GetOwnerEpoch()),
		LeaseUntilValid: true,
	}
}

func authorityReturning(state connectionowner.OwnerState, err error) ownerStateReader {
	return func(context.Context, [16]byte) (connectionowner.OwnerState, error) { return state, err }
}

func testSigner(t *testing.T) (*commandauth.Signer, ed25519.PublicKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	var seed [ed25519.SeedSize]byte
	copy(seed[:], privateKey.Seed())
	return commandauth.NewSignerFromSeed(seed), publicKey
}

func testFence(t *testing.T, signer *commandauth.Signer) (*agentv1.ConnectionFenceV2, [16]byte, [16]byte, [32]byte) {
	t.Helper()
	nodeID := mustUUIDv7(t)
	fenceID := mustUUIDv7(t)
	ownerInstanceID := mustUUIDv7(t)
	endpointID := [32]byte{}
	copy(endpointID[:], "observer-endpoint-32-bytes-test!")
	now := time.Now().UTC()
	fence, err := signer.IssueConnectionFenceV2(fenceID, nodeID, endpointID, ownerInstanceID, 7, 4, mustUUIDv7(t), 9,
		[]string{"ocserv.fencing.v2", "ocserv.service.reload"}, now.Add(30*time.Second), now, now.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("sign fence: %v", err)
	}
	var node [16]byte
	copy(node[:], nodeID[:])
	var instance [16]byte
	copy(instance[:], ownerInstanceID[:])
	return fence, node, instance, endpointID
}

func fenceClaimsFromProto(t *testing.T, fence *agentv1.ConnectionFenceV2) commandauth.ConnectionFenceClaimsV2 {
	t.Helper()
	fenceID, err := fixed16(fence.GetFenceId())
	if err != nil {
		t.Fatalf("fence id: %v", err)
	}
	nodeID, err := fixed16(fence.GetNodeId())
	if err != nil {
		t.Fatalf("node id: %v", err)
	}
	endpointID, err := fixed32(fence.GetEndpointId())
	if err != nil {
		t.Fatalf("endpoint id: %v", err)
	}
	ownerInstanceID, err := fixed16(fence.GetOwnerInstanceId())
	if err != nil {
		t.Fatalf("owner instance id: %v", err)
	}
	connectionID, err := fixed16(fence.GetConnectionId())
	if err != nil {
		t.Fatalf("connection id: %v", err)
	}
	leaseUntil := fence.GetLeaseUntil().AsTime().UTC()
	issuedAt := fence.GetIssuedAt().AsTime().UTC()
	expiresAt := fence.GetExpiresAt().AsTime().UTC()
	return commandauth.ConnectionFenceClaimsV2{
		SignatureVersion:      commandauth.ConnectionFenceV2SignatureVersion,
		KeyID:                 fence.GetKeyId(),
		FenceID:               fenceID,
		NodeID:                nodeID,
		EndpointID:            endpointID,
		OwnerInstanceID:       ownerInstanceID,
		OwnerIncarnation:      fence.GetOwnerIncarnation(),
		OwnerEpoch:            fence.GetOwnerEpoch(),
		ConnectionID:          connectionID,
		AuthorizationRevision: fence.GetAuthorizationRevision(),
		Capabilities:          fence.GetCapabilities(),
		LeaseUntilSeconds:     leaseUntil.Unix(),
		LeaseUntilNanos:       uint32(leaseUntil.Nanosecond()),
		IssuedAtSeconds:       issuedAt.Unix(),
		IssuedAtNanos:         uint32(issuedAt.Nanosecond()),
		ExpiresAtSeconds:      expiresAt.Unix(),
		ExpiresAtNanos:        uint32(expiresAt.Nanosecond()),
	}
}

func TestFenceCapabilitiesNormalization(t *testing.T) {
	normalized := fenceCapabilities([]string{"ocserv.service.reload", "", "ocserv.fencing.v2", "ocserv.service.reload", string(make([]byte, 129))})
	want := []string{"ocserv.fencing.v2", "ocserv.service.reload"}
	if len(normalized) != len(want) {
		t.Fatalf("normalized capabilities = %v, want %v", normalized, want)
	}
	for index := range want {
		if normalized[index] != want[index] {
			t.Fatalf("normalized capabilities = %v, want %v", normalized, want)
		}
	}
}

func TestObserverBindOperationSignsTheRegisteredTerm(t *testing.T) {
	signer, publicKey := testSigner(t)
	fence, nodeID, ownerInstanceID, endpointID := testFence(t, signer)
	observer, err := newObserver(authorityReturning(ownerStateForFence(fence), nil), &recordingReader{fence: fence}, signer)
	if err != nil {
		t.Fatalf("new observer: %v", err)
	}
	operationID := mustUUIDv7(t)
	_, binding, err := observer.BindOperation(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, operationID, "ocserv.service.reload")
	if err != nil {
		t.Fatalf("bind operation: %v", err)
	}
	if binding.GetOperationKind() != agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND {
		t.Fatalf("binding kind = %v", binding.GetOperationKind())
	}
	operationBytes, err := fixed16(binding.GetOperationId())
	if err != nil || operationBytes != operationID {
		t.Fatalf("binding operation id mismatch: %v %v", operationBytes, err)
	}
	fixedNode, _ := fixed16(binding.GetNodeId())
	fixedEndpoint, _ := fixed32(binding.GetEndpointId())
	fixedInstance, _ := fixed16(binding.GetOwnerInstanceId())
	if fixedNode != nodeID || fixedEndpoint != endpointID || fixedInstance != ownerInstanceID {
		t.Fatal("binding term does not match the registered fence")
	}
	if binding.GetOwnerEpoch() != fence.GetOwnerEpoch() || binding.GetAuthorizationRevision() != fence.GetAuthorizationRevision() {
		t.Fatal("binding epoch or revision does not match the registered fence")
	}
	claims := commandauth.FenceBindingClaimsV2{
		SignatureVersion:      commandauth.ConnectionFenceV2SignatureVersion,
		KeyID:                 binding.GetKeyId(),
		OperationKind:         uint32(binding.GetOperationKind()),
		OperationID:           operationBytes,
		FenceID:               mustFixed16(t, binding.GetFenceId()),
		NodeID:                fixedNode,
		EndpointID:            fixedEndpoint,
		OwnerInstanceID:       fixedInstance,
		OwnerIncarnation:      binding.GetOwnerIncarnation(),
		OwnerEpoch:            binding.GetOwnerEpoch(),
		ConnectionID:          mustFixed16(t, binding.GetConnectionId()),
		AuthorizationRevision: binding.GetAuthorizationRevision(),
		Capability:            binding.GetCapability(),
		IssuedAtSeconds:       binding.GetIssuedAt().AsTime().UTC().Unix(),
		IssuedAtNanos:         uint32(binding.GetIssuedAt().AsTime().UTC().Nanosecond()),
		ExpiresAtSeconds:      binding.GetExpiresAt().AsTime().UTC().Unix(),
		ExpiresAtNanos:        uint32(binding.GetExpiresAt().AsTime().UTC().Nanosecond()),
	}
	canonical, err := commandauth.CanonicalFenceBindingV2(claims)
	if err != nil {
		t.Fatalf("canonical binding: %v", err)
	}
	if !ed25519.Verify(publicKey, canonical, binding.GetSignature()) {
		t.Fatal("binding signature does not verify")
	}
	if !claims.MatchesFence(fenceClaimsFromProto(t, fence)) {
		t.Fatal("binding does not match the fence term")
	}
}

func TestObserverBindOperationRejectsUnheldCapability(t *testing.T) {
	signer, _ := testSigner(t)
	fence, nodeID, _, _ := testFence(t, signer)
	observer, err := newObserver(authorityReturning(ownerStateForFence(fence), nil), &recordingReader{fence: fence}, signer)
	if err != nil {
		t.Fatalf("new observer: %v", err)
	}
	_, _, err = observer.BindOperation(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_STATE_UPDATE, mustUUIDv7(t), "ocserv.tenant.admin")
	if !errors.Is(err, ErrCapabilityNotFenced) {
		t.Fatalf("error = %v, want ErrCapabilityNotFenced", err)
	}
}

func TestObserverBindOperationWithoutRegisteredFence(t *testing.T) {
	signer, _ := testSigner(t)
	observer, err := newObserver(authorityReturning(connectionowner.OwnerState{}, errors.New("authority must not be consulted")), &recordingReader{}, signer)
	if err != nil {
		t.Fatalf("new observer: %v", err)
	}
	_, _, err = observer.BindOperation(context.Background(), mustUUIDv7(t), agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, mustUUIDv7(t), "ocserv.service.reload")
	if !errors.Is(err, ErrNoFence) {
		t.Fatalf("error = %v, want ErrNoFence", err)
	}
}

func TestObserverBindOperationPropagatesReaderErrors(t *testing.T) {
	signer, _ := testSigner(t)
	readerFailure := errors.New("transport unavailable")
	observer, err := newObserver(authorityReturning(connectionowner.OwnerState{}, errors.New("authority must not be consulted")), &recordingReader{err: readerFailure}, signer)
	if err != nil {
		t.Fatalf("new observer: %v", err)
	}
	_, _, err = observer.BindOperation(context.Background(), mustUUIDv7(t), agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, mustUUIDv7(t), "ocserv.service.reload")
	if !errors.Is(err, readerFailure) {
		t.Fatalf("error = %v, want reader failure", err)
	}
}

func TestObserverRejectsMalformedRegisteredFence(t *testing.T) {
	signer, _ := testSigner(t)
	fence, _, _, _ := testFence(t, signer)
	fence.FenceId = []byte{1, 2, 3}
	observer, err := newObserver(authorityReturning(connectionowner.OwnerState{}, errors.New("authority must not be consulted")), &recordingReader{fence: fence}, signer)
	if err != nil {
		t.Fatalf("new observer: %v", err)
	}
	_, _, err = observer.BindOperation(context.Background(), mustUUIDv7(t), agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, mustUUIDv7(t), "ocserv.service.reload")
	if err == nil {
		t.Fatal("malformed fence accepted")
	}
}

// TestObserverBindOperationFailsClosedWhenTheAuthorityDisagrees pins the
// authority gate: a fence that transportd still serves may only be signed
// while the PostgreSQL ownership row names its exact owner instance,
// incarnation, connection, and epoch under an unexpired lease. Any
// disagreement — including a successor's higher epoch with a live lease —
// fails closed as ErrNotOwner and never degrades to the unfenced
// compatibility path.
func TestObserverBindOperationFailsClosedWhenTheAuthorityDisagrees(t *testing.T) {
	signer, _ := testSigner(t)
	fence, nodeID, _, _ := testFence(t, signer)
	backing := ownerStateForFence(fence)
	otherConnection := backing.ConnectionID
	otherConnection[0] ^= 1
	otherInstance := backing.InstanceID
	otherInstance[0] ^= 1
	successorEpoch := backing
	successorEpoch.Epoch++
	expiredLease := backing
	expiredLease.LeaseUntilValid = false
	otherConnectionState := backing
	otherConnectionState.ConnectionID = otherConnection
	otherInstanceState := backing
	otherInstanceState.InstanceID = otherInstance
	otherIncarnation := backing
	otherIncarnation.Incarnation++
	authorityBehind := backing
	authorityBehind.Epoch--
	disagreements := []struct {
		name  string
		state connectionowner.OwnerState
	}{
		{"successor epoch with a live lease", successorEpoch},
		{"expired lease", expiredLease},
		{"different connection", otherConnectionState},
		{"different owner instance", otherInstanceState},
		{"different incarnation", otherIncarnation},
		{"fence ahead of the authority row", authorityBehind},
	}
	for _, disagreement := range disagreements {
		t.Run(disagreement.name, func(t *testing.T) {
			observer, err := newObserver(authorityReturning(disagreement.state, nil), &recordingReader{fence: fence}, signer)
			if err != nil {
				t.Fatalf("new observer: %v", err)
			}
			returnedFence, binding, err := observer.BindOperation(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, mustUUIDv7(t), "ocserv.service.reload")
			if !errors.Is(err, ErrNotOwner) {
				t.Fatalf("error = %v, want ErrNotOwner", err)
			}
			if binding != nil || returnedFence != nil {
				t.Fatal("observer produced fence material for a term the authority does not back")
			}
		})
	}
}

// TestObserverBindOperationFailsClosedWithoutAnOwnershipRow covers a
// registered fence whose node has no PostgreSQL ownership row at all, and an
// authority that cannot be read: both must fail closed and neither may select
// the unfenced compatibility path.
func TestObserverBindOperationFailsClosedWithoutAnOwnershipRow(t *testing.T) {
	signer, _ := testSigner(t)
	fence, nodeID, _, _ := testFence(t, signer)
	observer, err := newObserver(authorityReturning(connectionowner.OwnerState{}, fmt.Errorf("gone: %w", connectionowner.ErrNoOwnerRow)), &recordingReader{fence: fence}, signer)
	if err != nil {
		t.Fatalf("new observer: %v", err)
	}
	_, binding, err := observer.BindOperation(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, mustUUIDv7(t), "ocserv.service.reload")
	if !errors.Is(err, ErrNotOwner) {
		t.Fatalf("error = %v, want ErrNotOwner", err)
	}
	if binding != nil {
		t.Fatal("observer signed a binding without an ownership row")
	}
}

func TestObserverBindOperationFailsClosedWhenTheAuthorityIsUnreadable(t *testing.T) {
	signer, _ := testSigner(t)
	fence, nodeID, _, _ := testFence(t, signer)
	authorityFailure := errors.New("database unavailable")
	observer, err := newObserver(authorityReturning(connectionowner.OwnerState{}, authorityFailure), &recordingReader{fence: fence}, signer)
	if err != nil {
		t.Fatalf("new observer: %v", err)
	}
	_, binding, err := observer.BindOperation(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, mustUUIDv7(t), "ocserv.service.reload")
	if !errors.Is(err, authorityFailure) {
		t.Fatalf("error = %v, want the authority failure", err)
	}
	if errors.Is(err, ErrNoFence) || errors.Is(err, ErrNotOwner) {
		t.Fatalf("authority failure = %v must stay a distinct fail-closed error", err)
	}
	if binding != nil {
		t.Fatal("observer signed a binding while the authority was unreadable")
	}
}

func TestObserverRequiresTheOwnershipAuthority(t *testing.T) {
	signer, _ := testSigner(t)
	if _, err := NewObserver(nil, &recordingReader{}, signer); err == nil {
		t.Fatal("observer accepted a nil authority pool")
	}
}

func TestFixedWidthHelpers(t *testing.T) {
	if _, err := fixed16(make([]byte, 15)); err == nil {
		t.Fatal("fixed16 accepted a short value")
	}
	if _, err := fixed32(make([]byte, 31)); err == nil {
		t.Fatal("fixed32 accepted a short value")
	}
}

func mustFixed16(t *testing.T, value []byte) [16]byte {
	t.Helper()
	fixed, err := fixed16(value)
	if err != nil {
		t.Fatalf("fixed16: %v", err)
	}
	return fixed
}

// TestStateUpdateOperationIDMatchesSharedVector pins the domain-separated
// derivation against the same vector the Rust command-authorization crate
// asserts, so Controller and transportd always derive one identity for one
// update carrier.
func TestStateUpdateOperationIDMatchesSharedVector(t *testing.T) {
	var nodeID [16]byte
	for index := range nodeID {
		nodeID[index] = byte(index + 1)
	}
	var endpointID [32]byte
	for index := range endpointID {
		endpointID[index] = byte(index)
	}
	operationID := StateUpdateOperationID(nodeID, endpointID[:], 2, 7, "review fixture")
	want := [16]byte{0xf0, 0x22, 0x9e, 0xca, 0xcf, 0x9b, 0xb6, 0x55, 0x89, 0xe1, 0x89, 0x7c, 0x66, 0x8a, 0x48, 0xf7}
	if operationID != want {
		t.Fatalf("operation id = %x, want %x", operationID, want)
	}
	if StateUpdateOperationID(nodeID, endpointID[:], 1, 7, "review fixture") == operationID {
		t.Fatal("state change kept the operation identity")
	}
	if StateUpdateOperationID(nodeID, endpointID[:], 2, 8, "review fixture") == operationID {
		t.Fatal("revision change kept the operation identity")
	}
	if StateUpdateOperationID(nodeID, endpointID[:], 2, 7, "review fixture ") == operationID {
		t.Fatal("reason change kept the operation identity")
	}
	otherEndpoint := endpointID
	otherEndpoint[0] ^= 1
	if StateUpdateOperationID(nodeID, otherEndpoint[:], 2, 7, "review fixture") == operationID {
		t.Fatal("endpoint change kept the operation identity")
	}
}
