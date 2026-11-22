package ownersession

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
)

func TestFixedWidthHelpers(t *testing.T) {
	if _, err := fixed16(make([]byte, 15)); err == nil {
		t.Fatal("fixed16 accepted a short value")
	}
}

func TestSessionInventoryMatchesOnlyTheExactOwnerTerm(t *testing.T) {
	snapshot := sessionTermSnapshot{sessionEpoch: 8}
	copy(snapshot.nodeID[:], "exact-node-id-16")
	copy(snapshot.endpointID[:], "exact-endpoint-id-32-bytes-value!")
	exact := &transportv1.NodeConnection{
		NodeId:       append([]byte(nil), snapshot.nodeID[:]...),
		EndpointId:   append([]byte(nil), snapshot.endpointID[:]...),
		SessionEpoch: uint64(snapshot.sessionEpoch),
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
		{name: "different epoch", connection: &transportv1.NodeConnection{NodeId: exact.GetNodeId(), EndpointId: exact.GetEndpointId(), SessionEpoch: exact.GetSessionEpoch() + 1}},
		{name: "different node", connection: &transportv1.NodeConnection{NodeId: append([]byte{0xff}, exact.GetNodeId()[1:]...), EndpointId: exact.GetEndpointId(), SessionEpoch: exact.GetSessionEpoch()}},
		{name: "different endpoint", connection: &transportv1.NodeConnection{NodeId: exact.GetNodeId(), EndpointId: append([]byte{0xff}, exact.GetEndpointId()[1:]...), SessionEpoch: exact.GetSessionEpoch()}},
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
		snapshots[index].sessionEpoch = int64(index + 1)
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
			NodeId:       append([]byte(nil), nodeID...),
			EndpointId:   append([]byte(nil), snapshots[index].endpointID[:]...),
			SessionEpoch: uint64(snapshots[index].sessionEpoch),
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
