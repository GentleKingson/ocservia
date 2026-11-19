package ownersession

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
)

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

func TestFixedWidthHelpers(t *testing.T) {
	if _, err := fixed16(make([]byte, 15)); err == nil {
		t.Fatal("fixed16 accepted a short value")
	}
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
