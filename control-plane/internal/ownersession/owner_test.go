package ownersession

import (
	"context"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
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
	observer, err := NewObserver(&recordingReader{fence: fence}, signer)
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
	observer, err := NewObserver(&recordingReader{fence: fence}, signer)
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
	observer, err := NewObserver(&recordingReader{}, signer)
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
	observer, err := NewObserver(&recordingReader{err: readerFailure}, signer)
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
	observer, err := NewObserver(&recordingReader{fence: fence}, signer)
	if err != nil {
		t.Fatalf("new observer: %v", err)
	}
	_, _, err = observer.BindOperation(context.Background(), mustUUIDv7(t), agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, mustUUIDv7(t), "ocserv.service.reload")
	if err == nil {
		t.Fatal("malformed fence accepted")
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
