package ownersession

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
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

// guardedTerm records what the observer asked the ownership authority to
// guard, so tests can pin the exact-term request.
type guardedTerm struct {
	nodeID       [16]byte
	instanceID   [16]byte
	incarnation  int64
	connectionID [16]byte
	epoch        int64
}

// recordingGuard simulates the PostgreSQL ownership guard. It records the
// guarded terms, counts releases, and can fail like the real guard with
// ErrNotOwner (term not backed) or an infrastructure error.
type recordingGuard struct {
	err      error
	terms    []guardedTerm
	released int
}

func (g *recordingGuard) Guard(_ context.Context, nodeID, instanceID [16]byte, incarnation int64, connectionID [16]byte, epoch int64) (func() error, error) {
	if g.err != nil {
		return nil, g.err
	}
	g.terms = append(g.terms, guardedTerm{nodeID: nodeID, instanceID: instanceID, incarnation: incarnation, connectionID: connectionID, epoch: epoch})
	return func() error {
		g.released++
		return nil
	}, nil
}

// termGuardedBy derives the exact term a fence claims, for assertions.
func termGuardedBy(t *testing.T, fence *agentv1.ConnectionFenceV2, nodeID [16]byte) guardedTerm {
	t.Helper()
	instanceID, err := fixed16(fence.GetOwnerInstanceId())
	if err != nil {
		t.Fatalf("fence owner instance: %v", err)
	}
	connectionID, err := fixed16(fence.GetConnectionId())
	if err != nil {
		t.Fatalf("fence connection: %v", err)
	}
	return guardedTerm{nodeID: nodeID, instanceID: instanceID, incarnation: int64(fence.GetOwnerIncarnation()), connectionID: connectionID, epoch: int64(fence.GetOwnerEpoch())}
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

func TestObserverExecuteFencedSignsAndRunsTheRegisteredTerm(t *testing.T) {
	signer, publicKey := testSigner(t)
	fence, nodeID, ownerInstanceID, endpointID := testFence(t, signer)
	guard := &recordingGuard{}
	observer, err := newObserver(guard, &recordingReader{fence: fence}, signer)
	if err != nil {
		t.Fatalf("new observer: %v", err)
	}
	operationID := mustUUIDv7(t)
	var runFence *agentv1.ConnectionFenceV2
	var binding *agentv1.FenceBindingV2
	err = observer.ExecuteFenced(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, operationID, "ocserv.service.reload",
		func(_ context.Context, fenceParam *agentv1.ConnectionFenceV2, bindingParam *agentv1.FenceBindingV2) error {
			runFence, binding = fenceParam, bindingParam
			return nil
		})
	if err != nil {
		t.Fatalf("execute fenced: %v", err)
	}
	if runFence != fence {
		t.Fatal("action did not receive the registered fence")
	}
	// The authority was asked to guard exactly the fence's term, and the
	// guard was released once the action returned.
	if len(guard.terms) != 1 || guard.terms[0] != termGuardedBy(t, fence, nodeID) {
		t.Fatalf("guarded terms = %+v, want the fence's exact term", guard.terms)
	}
	if guard.released != 1 {
		t.Fatalf("guard releases = %d, want 1", guard.released)
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

// TestObserverExecuteFencedHoldsTheGuardAcrossTheAction proves the fencing
// interval's ordering: the guard is still held while the action runs and is
// released only after it returned.
func TestObserverExecuteFencedHoldsTheGuardAcrossTheAction(t *testing.T) {
	signer, _ := testSigner(t)
	fence, nodeID, _, _ := testFence(t, signer)
	guard := &recordingGuard{}
	observer, err := newObserver(guard, &recordingReader{fence: fence}, signer)
	if err != nil {
		t.Fatalf("new observer: %v", err)
	}
	err = observer.ExecuteFenced(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, mustUUIDv7(t), "ocserv.service.reload",
		func(context.Context, *agentv1.ConnectionFenceV2, *agentv1.FenceBindingV2) error {
			if guard.released != 0 {
				t.Fatal("guard was released before the mutation completed")
			}
			return nil
		})
	if err != nil {
		t.Fatalf("execute fenced: %v", err)
	}
	if guard.released != 1 {
		t.Fatalf("guard releases = %d, want 1", guard.released)
	}
}

// TestObserverExecuteFencedPropagatesActionErrorsAndReleases proves a failed
// mutation still frees the authority guard instead of pinning the node's
// ownership row.
func TestObserverExecuteFencedPropagatesActionErrorsAndReleases(t *testing.T) {
	signer, _ := testSigner(t)
	fence, nodeID, _, _ := testFence(t, signer)
	guard := &recordingGuard{}
	observer, err := newObserver(guard, &recordingReader{fence: fence}, signer)
	if err != nil {
		t.Fatalf("new observer: %v", err)
	}
	actionFailure := errors.New("transport rejected the mutation")
	err = observer.ExecuteFenced(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, mustUUIDv7(t), "ocserv.service.reload",
		func(context.Context, *agentv1.ConnectionFenceV2, *agentv1.FenceBindingV2) error {
			return actionFailure
		})
	if !errors.Is(err, actionFailure) {
		t.Fatalf("error = %v, want the action failure", err)
	}
	if guard.released != 1 {
		t.Fatalf("guard releases = %d, want 1 after a failed action", guard.released)
	}
}

func TestObserverExecuteFencedRejectsUnheldCapability(t *testing.T) {
	signer, _ := testSigner(t)
	fence, nodeID, _, _ := testFence(t, signer)
	guard := &recordingGuard{}
	observer, err := newObserver(guard, &recordingReader{fence: fence}, signer)
	if err != nil {
		t.Fatalf("new observer: %v", err)
	}
	ran := false
	err = observer.ExecuteFenced(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_STATE_UPDATE, mustUUIDv7(t), "ocserv.tenant.admin",
		func(context.Context, *agentv1.ConnectionFenceV2, *agentv1.FenceBindingV2) error {
			ran = true
			return nil
		})
	if !errors.Is(err, ErrCapabilityNotFenced) {
		t.Fatalf("error = %v, want ErrCapabilityNotFenced", err)
	}
	if ran {
		t.Fatal("action ran for an unfenced capability")
	}
	if len(guard.terms) != 0 {
		t.Fatal("guard was acquired for an unfenced capability")
	}
}

func TestObserverExecuteFencedWithoutRegisteredFence(t *testing.T) {
	signer, _ := testSigner(t)
	guard := &recordingGuard{}
	observer, err := newObserver(guard, &recordingReader{}, signer)
	if err != nil {
		t.Fatalf("new observer: %v", err)
	}
	var runFence *agentv1.ConnectionFenceV2
	var runBinding *agentv1.FenceBindingV2
	if err := observer.ExecuteFenced(context.Background(), mustUUIDv7(t), agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, mustUUIDv7(t), "ocserv.service.reload",
		func(_ context.Context, fence *agentv1.ConnectionFenceV2, binding *agentv1.FenceBindingV2) error {
			runFence, runBinding = fence, binding
			return nil
		}); err != nil {
		t.Fatalf("execute fenced without a registered fence: %v", err)
	}
	if runFence != nil || runBinding != nil {
		t.Fatal("unfenced compatibility path carried fence material")
	}
	if len(guard.terms) != 0 {
		t.Fatal("guard was acquired without a registered fence")
	}
}

func TestObserverExecuteFencedPropagatesReaderErrors(t *testing.T) {
	signer, _ := testSigner(t)
	readerFailure := errors.New("transport unavailable")
	observer, err := newObserver(&recordingGuard{}, &recordingReader{err: readerFailure}, signer)
	if err != nil {
		t.Fatalf("new observer: %v", err)
	}
	ran := false
	err = observer.ExecuteFenced(context.Background(), mustUUIDv7(t), agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, mustUUIDv7(t), "ocserv.service.reload",
		func(context.Context, *agentv1.ConnectionFenceV2, *agentv1.FenceBindingV2) error {
			ran = true
			return nil
		})
	if !errors.Is(err, readerFailure) {
		t.Fatalf("error = %v, want reader failure", err)
	}
	if ran {
		t.Fatal("action ran despite a reader failure")
	}
}

func TestObserverRejectsMalformedRegisteredFence(t *testing.T) {
	signer, _ := testSigner(t)
	fence, _, _, _ := testFence(t, signer)
	fence.FenceId = []byte{1, 2, 3}
	observer, err := newObserver(&recordingGuard{}, &recordingReader{fence: fence}, signer)
	if err != nil {
		t.Fatalf("new observer: %v", err)
	}
	ran := false
	err = observer.ExecuteFenced(context.Background(), mustUUIDv7(t), agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, mustUUIDv7(t), "ocserv.service.reload",
		func(context.Context, *agentv1.ConnectionFenceV2, *agentv1.FenceBindingV2) error {
			ran = true
			return nil
		})
	if err == nil {
		t.Fatal("malformed fence accepted")
	}
	if ran {
		t.Fatal("action ran for a malformed fence")
	}
}

// TestObserverExecuteFencedFailsClosedWhenTheAuthorityRejectsTheTerm pins the
// authority gate: a fence that transportd still serves may only mutate while
// the PostgreSQL ownership guard accepts its exact term. The guard rejects
// the term — successor epoch, expired lease, foreign connection, instance,
// incarnation, or no row at all — with connectionowner.ErrNotOwner, which the
// observer surfaces as its own ErrNotOwner without running the action and
// without ever selecting the unfenced compatibility path.
func TestObserverExecuteFencedFailsClosedWhenTheAuthorityRejectsTheTerm(t *testing.T) {
	signer, _ := testSigner(t)
	fence, nodeID, _, _ := testFence(t, signer)
	observer, err := newObserver(&recordingGuard{err: fmt.Errorf("stale term: %w", connectionowner.ErrNotOwner)}, &recordingReader{fence: fence}, signer)
	if err != nil {
		t.Fatalf("new observer: %v", err)
	}
	ran := false
	err = observer.ExecuteFenced(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, mustUUIDv7(t), "ocserv.service.reload",
		func(context.Context, *agentv1.ConnectionFenceV2, *agentv1.FenceBindingV2) error {
			ran = true
			return nil
		})
	if !errors.Is(err, ErrNotOwner) {
		t.Fatalf("error = %v, want ErrNotOwner", err)
	}
	if ran {
		t.Fatal("action ran for a term the authority does not back")
	}
}

func TestObserverExecuteFencedFailsClosedWhenTheAuthorityIsUnreadable(t *testing.T) {
	signer, _ := testSigner(t)
	fence, nodeID, _, _ := testFence(t, signer)
	authorityFailure := errors.New("database unavailable")
	observer, err := newObserver(&recordingGuard{err: authorityFailure}, &recordingReader{fence: fence}, signer)
	if err != nil {
		t.Fatalf("new observer: %v", err)
	}
	ran := false
	err = observer.ExecuteFenced(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, mustUUIDv7(t), "ocserv.service.reload",
		func(context.Context, *agentv1.ConnectionFenceV2, *agentv1.FenceBindingV2) error {
			ran = true
			return nil
		})
	if !errors.Is(err, authorityFailure) {
		t.Fatalf("error = %v, want the authority failure", err)
	}
	if errors.Is(err, ErrNoFence) || errors.Is(err, ErrNotOwner) {
		t.Fatalf("authority failure = %v must stay a distinct fail-closed error", err)
	}
	if ran {
		t.Fatal("action ran while the authority was unreadable")
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

func TestSessionInventoryMatchesOnlyTheExactOwnerTerm(t *testing.T) {
	snapshot := sessionTermSnapshot{ownerEpoch: 8}
	copy(snapshot.nodeID[:], "exact-node-id-16")
	copy(snapshot.endpointID[:], "exact-endpoint-id-32-bytes-value!")
	exact := &transportv1.NodeConnection{
		NodeId:     append([]byte(nil), snapshot.nodeID[:]...),
		EndpointId: append([]byte(nil), snapshot.endpointID[:]...),
		OwnerEpoch: uint64(snapshot.ownerEpoch),
	}
	if !sessionInventoryMatches(snapshot, exact) {
		t.Fatal("exact transport inventory term did not match")
	}
	tests := []struct {
		name       string
		connection *transportv1.NodeConnection
	}{
		{name: "missing", connection: nil},
		{name: "epoch zero", connection: &transportv1.NodeConnection{NodeId: exact.GetNodeId(), EndpointId: exact.GetEndpointId()}},
		{name: "different epoch", connection: &transportv1.NodeConnection{NodeId: exact.GetNodeId(), EndpointId: exact.GetEndpointId(), OwnerEpoch: exact.GetOwnerEpoch() + 1}},
		{name: "different node", connection: &transportv1.NodeConnection{NodeId: append([]byte{0xff}, exact.GetNodeId()[1:]...), EndpointId: exact.GetEndpointId(), OwnerEpoch: exact.GetOwnerEpoch()}},
		{name: "different endpoint", connection: &transportv1.NodeConnection{NodeId: exact.GetNodeId(), EndpointId: append([]byte{0xff}, exact.GetEndpointId()[1:]...), OwnerEpoch: exact.GetOwnerEpoch()}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if sessionInventoryMatches(snapshot, test.connection) {
				t.Fatal("non-authoritative transport inventory term matched")
			}
		})
	}
}

func TestReadSessionInventoryCoversFiveHundredNodesWithinDeadline(t *testing.T) {
	const nodeCount = 500
	snapshots := make([]sessionTermSnapshot, nodeCount)
	for index := range snapshots {
		snapshots[index].nodeID[0] = byte(index >> 8)
		snapshots[index].nodeID[1] = byte(index)
		snapshots[index].endpointID[0] = byte(index >> 8)
		snapshots[index].endpointID[1] = byte(index)
		snapshots[index].ownerEpoch = int64(index + 1)
	}

	var active atomic.Int64
	var peak atomic.Int64
	var visitedMu sync.Mutex
	visited := make([]int, nodeCount)
	readConnection := func(ctx context.Context, nodeID []byte) (*transportv1.NodeConnection, error) {
		if len(nodeID) != 16 {
			return nil, fmt.Errorf("node id length = %d", len(nodeID))
		}
		index := int(nodeID[0])<<8 | int(nodeID[1])
		if index < 0 || index >= nodeCount {
			return nil, fmt.Errorf("unexpected node index %d", index)
		}
		current := active.Add(1)
		defer active.Add(-1)
		for {
			previous := peak.Load()
			if current <= previous || peak.CompareAndSwap(previous, current) {
				break
			}
		}
		visitedMu.Lock()
		visited[index]++
		visitedMu.Unlock()
		timer := time.NewTimer(10 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &transportv1.NodeConnection{
			NodeId:     append([]byte(nil), nodeID...),
			EndpointId: append([]byte(nil), snapshots[index].endpointID[:]...),
			OwnerEpoch: uint64(snapshots[index].ownerEpoch),
		}, nil
	}

	const deadline = 3 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	started := time.Now()
	results := readSessionInventory(ctx, snapshots, readConnection)
	if elapsed := time.Since(started); elapsed >= deadline {
		t.Fatalf("500-node inventory took %s, exceeded %s", elapsed, deadline)
	}
	if len(results) != nodeCount {
		t.Fatalf("inventory results = %d, want %d", len(results), nodeCount)
	}
	for index, result := range results {
		if result.err != nil {
			t.Fatalf("inventory result %d: %v", index, result.err)
		}
		if !sessionInventoryMatches(result.snapshot, result.connection) {
			t.Fatalf("inventory result %d did not preserve its exact term", index)
		}
		if visited[index] != 1 {
			t.Fatalf("node %d inventory reads = %d, want 1", index, visited[index])
		}
	}
	if got := peak.Load(); got <= 1 || got > gapReconciliationConcurrency {
		t.Fatalf("peak inventory concurrency = %d, want 2..%d", got, gapReconciliationConcurrency)
	}
}
