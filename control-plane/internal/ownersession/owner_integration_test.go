package ownersession

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/transportclient"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func mustUUIDv7(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid v7: %v", err)
	}
	return id
}

type recordingRegistrar struct {
	fences    []*agentv1.ConnectionFenceV2
	failtures bool
}

func (r *recordingRegistrar) RegisterOwnerFence(_ context.Context, fence *agentv1.ConnectionFenceV2) error {
	if r.failtures {
		return errors.New("transport unavailable")
	}
	r.fences = append(r.fences, fence)
	return nil
}

type eventStreamFunc func(context.Context, transportclient.CursorStore, transportclient.EventHandler) error

func (f eventStreamFunc) RunWatch(ctx context.Context, cursor transportclient.CursorStore, handler transportclient.EventHandler) error {
	return f(ctx, cursor, handler)
}

func testNodeAndEndpoint(t *testing.T) ([16]byte, [32]byte) {
	t.Helper()
	nodeID := mustUUIDv7(t)
	endpointID := [32]byte{}
	copy(endpointID[:], nodeID[:])
	copy(endpointID[16:], "endpoint-pad-16b!")
	return nodeID, endpointID
}

// TestManagerSessionOwnershipIntegration covers the worker-role owner
// lifecycle: acquiring the lease, registering the fence, issuing bindings,
// refusing a second owner while the lease is held, and failing closed after
// a takeover takes a strictly higher epoch.
func TestManagerSessionOwnershipIntegration(t *testing.T) {
	pool := testPool(t)
	signer, _ := testSigner(t)
	registrar := &recordingRegistrar{}
	manager, err := NewManager(pool, signer, registrar, 30*time.Second, testLogger())
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	nodeID, endpointID := testNodeAndEndpoint(t)

	fence, err := manager.OpenSession(context.Background(), nodeID, endpointID, 5, []string{"ocserv.fencing.v2", "ocserv.service.reload"})
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	if len(registrar.fences) != 1 {
		t.Fatalf("registered fences = %d, want 1", len(registrar.fences))
	}
	if fence.GetOwnerEpoch() == 0 || fence.GetAuthorizationRevision() != 5 {
		t.Fatalf("fence epoch/revision = %d/%d", fence.GetOwnerEpoch(), fence.GetAuthorizationRevision())
	}
	connectionID, err := fixed16(fence.GetConnectionId())
	if err != nil {
		t.Fatal(err)
	}
	if !manager.OwnsTerm(nodeID, connectionID, int64(fence.GetOwnerEpoch())) {
		t.Fatal("manager did not recognize its exact local owner term")
	}
	wrongConnection := connectionID
	wrongConnection[0] ^= 0xff
	if manager.OwnsTerm(nodeID, wrongConnection, int64(fence.GetOwnerEpoch())) || manager.OwnsTerm(nodeID, connectionID, int64(fence.GetOwnerEpoch())+1) {
		t.Fatal("manager accepted a different local owner term")
	}
	if !fenceClaimsMatchCapabilities(fence, []string{"ocserv.fencing.v2", "ocserv.service.reload"}) {
		t.Fatalf("fence capabilities = %v", fence.GetCapabilities())
	}
	leaseUntil := fence.GetLeaseUntil().AsTime().UTC()
	if leaseUntil.Before(time.Now().UTC().Add(25 * time.Second)) {
		t.Fatalf("fence lease deadline %v is shorter than the negotiated TTL", leaseUntil)
	}
	// The fence deadline must be the exact PostgreSQL lease deadline, not a
	// locally reconstructed one.
	var stored time.Time
	if err := pool.QueryRow(context.Background(), `SELECT lease_until FROM connection_owner_fencing WHERE node_id = $1`, nodeID[:]).Scan(&stored); err != nil {
		t.Fatalf("read stored lease: %v", err)
	}
	stored = stored.UTC()
	if !stored.Equal(leaseUntil) {
		t.Fatalf("fence lease %v != stored lease %v", leaseUntil, stored)
	}

	// A different owner cannot take the node while the lease is unexpired.
	rivalRegistrar := &recordingRegistrar{}
	rival, err := NewManager(pool, signer, rivalRegistrar, 30*time.Second, testLogger())
	if err != nil {
		t.Fatalf("new rival manager: %v", err)
	}
	if _, err := rival.OpenSession(context.Background(), nodeID, endpointID, 6, []string{"ocserv.fencing.v2"}); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("rival open error = %v, want ErrNotOwner", err)
	}
	if len(rivalRegistrar.fences) != 0 {
		t.Fatal("rival registered a fence without holding the lease")
	}

	// The owner issues bindings for its own term.
	operationID := mustUUIDv7(t)
	fence, binding, err := manager.BindOperation(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, operationID, "ocserv.service.reload")
	if err != nil {
		t.Fatalf("bind operation: %v", err)
	}
	if binding.GetOwnerEpoch() != fence.GetOwnerEpoch() {
		t.Fatalf("binding epoch %d != fence epoch %d", binding.GetOwnerEpoch(), fence.GetOwnerEpoch())
	}
	if _, _, err := manager.BindOperation(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, operationID, "ocserv.tenant.admin"); !errors.Is(err, ErrCapabilityNotFenced) {
		t.Fatalf("unheld capability error = %v, want ErrCapabilityNotFenced", err)
	}
	unknownNode := mustUUIDv7(t)
	if _, _, err := manager.BindOperation(context.Background(), unknownNode, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, operationID, "ocserv.service.reload"); !errors.Is(err, ErrNoFence) {
		t.Fatalf("unknown node error = %v, want ErrNoFence", err)
	}
}

// TestManagerTakeoverFailsClosedIntegration expires one owner's lease, lets a
// second owner take a strictly higher epoch, and verifies the first owner can
// no longer bind operations for the node.
func TestManagerTakeoverFailsClosedIntegration(t *testing.T) {
	pool := testPool(t)
	signer, _ := testSigner(t)
	ownerRegistrar := &recordingRegistrar{}
	owner, err := NewManager(pool, signer, ownerRegistrar, time.Second, testLogger())
	if err != nil {
		t.Fatalf("new owner manager: %v", err)
	}
	nodeID, endpointID := testNodeAndEndpoint(t)
	firstFence, err := owner.OpenSession(context.Background(), nodeID, endpointID, 3, []string{"ocserv.fencing.v2"})
	if err != nil {
		t.Fatalf("open session: %v", err)
	}

	// Wait past the lease deadline, then take over with a higher epoch.
	time.Sleep(1200 * time.Millisecond)
	successorRegistrar := &recordingRegistrar{}
	successor, err := NewManager(pool, signer, successorRegistrar, 30*time.Second, testLogger())
	if err != nil {
		t.Fatalf("new successor manager: %v", err)
	}
	successorFence, err := successor.OpenSession(context.Background(), nodeID, endpointID, 4, []string{"ocserv.fencing.v2"})
	if err != nil {
		t.Fatalf("successor open session: %v", err)
	}
	if successorFence.GetOwnerEpoch() <= firstFence.GetOwnerEpoch() {
		t.Fatalf("successor epoch %d does not exceed owner epoch %d", successorFence.GetOwnerEpoch(), firstFence.GetOwnerEpoch())
	}

	// The stale owner fails closed on its next binding attempt and stays
	// closed afterwards: a lost session never degrades to the unfenced
	// compatibility path.
	operationID := mustUUIDv7(t)
	if _, _, err := owner.BindOperation(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, operationID, "ocserv.fencing.v2"); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("stale owner bind error = %v, want ErrNotOwner", err)
	}
	if _, _, err := owner.BindOperation(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, operationID, "ocserv.fencing.v2"); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("lost session bind error = %v, want ErrNotOwner", err)
	}
	firstConnection, err := fixed16(firstFence.GetConnectionId())
	if err != nil {
		t.Fatal(err)
	}
	if owner.OwnsTerm(nodeID, firstConnection, int64(firstFence.GetOwnerEpoch())) {
		t.Fatal("stale manager retained local recovery authority")
	}
	successorConnection, err := fixed16(successorFence.GetConnectionId())
	if err != nil {
		t.Fatal(err)
	}
	if !successor.OwnsTerm(nodeID, successorConnection, int64(successorFence.GetOwnerEpoch())) {
		t.Fatal("successor did not acquire local recovery authority")
	}

	// The successor keeps operating on the node.
	if _, _, err := successor.BindOperation(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_STATE_UPDATE, mustUUIDv7(t), "ocserv.fencing.v2"); err != nil {
		t.Fatalf("successor bind: %v", err)
	}
}

// TestManagerRegistrationFailureFailsClosedIntegration verifies that a
// session is never granted when transportd did not accept the fence, that the
// acquired lease is released instead of blocking takeover for a full TTL, and
// that later bindings fail closed instead of degrading to the unfenced path.
func TestManagerRegistrationFailureFailsClosedIntegration(t *testing.T) {
	pool := testPool(t)
	signer, _ := testSigner(t)
	manager, err := NewManager(pool, signer, &recordingRegistrar{failtures: true}, 30*time.Second, testLogger())
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	nodeID, endpointID := testNodeAndEndpoint(t)
	if _, err := manager.OpenSession(context.Background(), nodeID, endpointID, 2, []string{"ocserv.fencing.v2"}); err == nil {
		t.Fatal("session granted despite fence registration failure")
	}
	if _, _, err := manager.BindOperation(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, mustUUIDv7(t), "ocserv.fencing.v2"); !errors.Is(err, ErrFenceUnavailable) {
		t.Fatalf("bind error = %v, want ErrFenceUnavailable", err)
	}
	// The failed open released its lease, so a healthy owner takes over
	// immediately without waiting out the TTL.
	successor, err := NewManager(pool, signer, &recordingRegistrar{}, 30*time.Second, testLogger())
	if err != nil {
		t.Fatalf("new successor manager: %v", err)
	}
	started := time.Now()
	successorFence, err := successor.OpenSession(context.Background(), nodeID, endpointID, 3, []string{"ocserv.fencing.v2"})
	if err != nil {
		t.Fatalf("successor take over after released lease: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("takeover after release took %v, want immediate", elapsed)
	}
	if successorFence.GetOwnerEpoch() < 2 {
		t.Fatalf("successor epoch %d does not advance the epoch line", successorFence.GetOwnerEpoch())
	}
}

// TestManagerRenewalRefreshesFenceIntegration renews near the deadline and
// verifies the refreshed fence keeps the term identity while the lease
// deadline strictly advances and transportd sees the new registration.
func TestManagerRenewalRefreshesFenceIntegration(t *testing.T) {
	pool := testPool(t)
	signer, _ := testSigner(t)
	registrar := &recordingRegistrar{}
	manager, err := NewManager(pool, signer, registrar, 4*time.Second, testLogger())
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	nodeID, endpointID := testNodeAndEndpoint(t)
	firstFence, err := manager.OpenSession(context.Background(), nodeID, endpointID, 8, []string{"ocserv.fencing.v2"})
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	// The renewal margin is half the TTL, so a bind two seconds in falls
	// inside it and must renew.
	time.Sleep(2 * time.Second)
	secondFence, _, err := manager.BindOperation(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, mustUUIDv7(t), "ocserv.fencing.v2")
	if err != nil {
		t.Fatalf("bind after renewal margin: %v", err)
	}
	if secondFence.GetFenceId() != nil && string(secondFence.GetFenceId()) != string(firstFence.GetFenceId()) {
		t.Fatalf("renewal changed the fence identity: %x != %x", secondFence.GetFenceId(), firstFence.GetFenceId())
	}
	if !secondFence.GetLeaseUntil().AsTime().After(firstFence.GetLeaseUntil().AsTime()) {
		t.Fatalf("renewal did not extend the lease: %v -> %v", firstFence.GetLeaseUntil().AsTime(), secondFence.GetLeaseUntil().AsTime())
	}
	var stored time.Time
	if err := pool.QueryRow(context.Background(), `SELECT lease_until FROM connection_owner_fencing WHERE node_id = $1`, nodeID[:]).Scan(&stored); err != nil {
		t.Fatalf("read stored lease: %v", err)
	}
	if !stored.UTC().Equal(secondFence.GetLeaseUntil().AsTime().UTC()) {
		t.Fatalf("refreshed fence lease %v != stored lease %v", secondFence.GetLeaseUntil().AsTime(), stored.UTC())
	}
	if len(registrar.fences) < 2 {
		t.Fatalf("registrations = %d, want at least the initial and refreshed fence", len(registrar.fences))
	}
}

func fenceClaimsMatchCapabilities(fence *agentv1.ConnectionFenceV2, want []string) bool {
	capabilities := fence.GetCapabilities()
	if len(capabilities) != len(want) {
		return false
	}
	for index := range want {
		if capabilities[index] != want[index] {
			return false
		}
	}
	return true
}

// TestManagerDisconnectEndsTermWhileRunIsActiveIntegration reproduces the HA
// takeover scenario: the old owner process keeps renewing (Run is active),
// then the exact connection behind its term disconnects. The term ends, the
// lease is released at PostgreSQL time, and a second manager takes over with
// a strictly higher epoch well inside the lease TTL.
func TestManagerDisconnectEndsTermWhileRunIsActiveIntegration(t *testing.T) {
	pool := testPool(t)
	signer, _ := testSigner(t)
	owner, err := NewManager(pool, signer, &recordingRegistrar{}, 30*time.Second, testLogger())
	if err != nil {
		t.Fatalf("new owner manager: %v", err)
	}
	nodeID, endpointID := testNodeAndEndpoint(t)
	fence, err := owner.OpenSession(context.Background(), nodeID, endpointID, 4, []string{"ocserv.fencing.v2"})
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	runCtx, stopRun := context.WithCancel(context.Background())
	defer stopRun()
	go func() { _ = owner.Run(runCtx) }()

	// The transport reports the disconnect of exactly this term's connection.
	connectionID, err := fixed16(fence.GetConnectionId())
	if err != nil {
		t.Fatalf("fence connection id: %v", err)
	}
	disconnect := &transportv1.TransportEvent{
		NodeId:       nodeID[:],
		ConnectionId: connectionID[:],
		OwnerEpoch:   fence.GetOwnerEpoch(),
		Type:         transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_DISCONNECTED,
	}
	if err := owner.handleTransportEvent(context.Background(), disconnect); err != nil {
		t.Fatalf("handle disconnect: %v", err)
	}

	// The ended owner never falls back to the unfenced compatibility path.
	if _, _, err := owner.BindOperation(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, mustUUIDv7(t), "ocserv.fencing.v2"); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("ended owner bind error = %v, want ErrNotOwner", err)
	}

	successor, err := NewManager(pool, signer, &recordingRegistrar{}, 30*time.Second, testLogger())
	if err != nil {
		t.Fatalf("new successor manager: %v", err)
	}
	started := time.Now()
	successorFence, err := successor.OpenSession(context.Background(), nodeID, endpointID, 5, []string{"ocserv.fencing.v2"})
	if err != nil {
		t.Fatalf("successor take over while the old process still runs: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("takeover after disconnect took %v, want immediate release", elapsed)
	}
	if successorFence.GetOwnerEpoch() <= fence.GetOwnerEpoch() {
		t.Fatalf("successor epoch %d does not exceed owner epoch %d", successorFence.GetOwnerEpoch(), fence.GetOwnerEpoch())
	}

	// A late replay of the old disconnect cannot end the successor's term.
	late := &transportv1.TransportEvent{
		NodeId:       nodeID[:],
		ConnectionId: connectionID[:],
		OwnerEpoch:   fence.GetOwnerEpoch(),
		Type:         transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_DISCONNECTED,
	}
	if err := owner.handleTransportEvent(context.Background(), late); err != nil {
		t.Fatalf("late disconnect handling: %v", err)
	}
	if _, _, err := successor.BindOperation(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, mustUUIDv7(t), "ocserv.fencing.v2"); err != nil {
		t.Fatalf("successor binding after late disconnect: %v", err)
	}
}

// TestManagerCloseSessionMatchesTheExactTermIntegration verifies the exact
// term guard: closing with a different connection identity or epoch ends
// nothing, and a second open on the same node supersedes cleanly.
func TestManagerCloseSessionMatchesTheExactTermIntegration(t *testing.T) {
	pool := testPool(t)
	signer, _ := testSigner(t)
	manager, err := NewManager(pool, signer, &recordingRegistrar{}, 30*time.Second, testLogger())
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	nodeID, endpointID := testNodeAndEndpoint(t)
	fence, err := manager.OpenSession(context.Background(), nodeID, endpointID, 6, []string{"ocserv.fencing.v2"})
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	connectionID, err := fixed16(fence.GetConnectionId())
	if err != nil {
		t.Fatalf("fence connection id: %v", err)
	}
	wrongConnection := mustUUIDv7(t)
	var wrongFixed [16]byte
	copy(wrongFixed[:], wrongConnection[:])
	if err := manager.CloseSession(context.Background(), nodeID, wrongFixed, int64(fence.GetOwnerEpoch())); err != nil {
		t.Fatalf("wrong-connection close: %v", err)
	}
	if err := manager.CloseSession(context.Background(), nodeID, connectionID, int64(fence.GetOwnerEpoch())+1); err != nil {
		t.Fatalf("wrong-epoch close: %v", err)
	}
	if _, _, err := manager.BindOperation(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, mustUUIDv7(t), "ocserv.fencing.v2"); err != nil {
		t.Fatalf("binding after mismatched closes: %v", err)
	}

	// The exact term closes on demand, expires the lease immediately, and
	// lets a rival take over through a real Acquire. The closed manager
	// itself must fail closed forever after, never legacy.
	rival, err := NewManager(pool, signer, &recordingRegistrar{}, 30*time.Second, testLogger())
	if err != nil {
		t.Fatalf("new rival manager: %v", err)
	}
	if err := manager.CloseSession(context.Background(), nodeID, connectionID, int64(fence.GetOwnerEpoch())); err != nil {
		t.Fatalf("exact-term close: %v", err)
	}
	rivalFence, err := rival.OpenSession(context.Background(), nodeID, endpointID, 7, []string{"ocserv.fencing.v2"})
	if err != nil {
		t.Fatalf("rival takeover after exact close: %v", err)
	}
	if rivalFence.GetOwnerEpoch() <= fence.GetOwnerEpoch() {
		t.Fatalf("rival epoch = %d, want above %d", rivalFence.GetOwnerEpoch(), fence.GetOwnerEpoch())
	}
	if _, _, err := manager.BindOperation(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, mustUUIDv7(t), "ocserv.fencing.v2"); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("closed manager bind = %v, want ErrNotOwner", err)
	}
}

// TestManagerTransportEventGapReconcilesConnectionInventoryIntegration pins
// the transportd-restart path. A stale in-memory cursor is reconciled against
// the live connection inventory, disconnected exact terms are released, live
// terms remain usable, and the watch stays active with a cleared cursor.
func TestManagerTransportEventGapReconcilesConnectionInventoryIntegration(t *testing.T) {
	pool := testPool(t)
	signer, _ := testSigner(t)
	manager, err := NewManager(pool, signer, &recordingRegistrar{}, 30*time.Second, testLogger())
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	liveNode, liveEndpoint := testNodeAndEndpoint(t)
	deadNode, deadEndpoint := testNodeAndEndpoint(t)
	liveFence, err := manager.OpenSession(context.Background(), liveNode, liveEndpoint, 7, []string{FencingCapability})
	if err != nil {
		t.Fatalf("open live session: %v", err)
	}
	deadFence, err := manager.OpenSession(context.Background(), deadNode, deadEndpoint, 8, []string{FencingCapability})
	if err != nil {
		t.Fatalf("open disconnected session: %v", err)
	}
	reconciliationNow := time.Now().UTC().Add(manager.publicationGrace + time.Second)
	manager.now = func() time.Time { return reconciliationNow }
	liveConnection, err := fixed16(liveFence.GetConnectionId())
	if err != nil {
		t.Fatalf("live connection id: %v", err)
	}
	deadConnection, err := fixed16(deadFence.GetConnectionId())
	if err != nil {
		t.Fatalf("disconnected connection id: %v", err)
	}

	reconciled := make(chan struct{})
	cursorID := mustUUIDv7(t)
	stream := eventStreamFunc(func(ctx context.Context, cursor transportclient.CursorStore, handler transportclient.EventHandler) error {
		if err := handler.Ingest(ctx, &transportv1.TransportEvent{
			EventId: cursorID[:],
			NodeId:  liveNode[:],
			Type:    transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_HEARTBEAT,
		}); err != nil {
			return fmt.Errorf("seed owner event cursor: %w", err)
		}
		before, err := cursor.LastEventID(ctx)
		if err != nil {
			return fmt.Errorf("read seeded owner event cursor: %w", err)
		}
		if !bytes.Equal(before, cursorID[:]) {
			return fmt.Errorf("seeded owner event cursor = %x, want %x", before, cursorID)
		}
		reconciler, ok := handler.(transportclient.OwnerGapReconciler)
		if !ok {
			return errors.New("owner transport event handler cannot reconcile exact terms after a retention gap")
		}
		if err := reconciler.ReconcileOwnerEventGap(ctx, func(_ context.Context, nodeID []byte) (*transportv1.NodeConnection, error) {
			switch {
			case bytes.Equal(nodeID, liveNode[:]):
				return &transportv1.NodeConnection{
					NodeId:     bytes.Clone(liveNode[:]),
					EndpointId: bytes.Clone(liveEndpoint[:]),
					OwnerEpoch: liveFence.GetOwnerEpoch(),
				}, nil
			case bytes.Equal(nodeID, deadNode[:]):
				return nil, nil
			default:
				return nil, fmt.Errorf("unexpected owner inventory node %x", nodeID)
			}
		}); err != nil {
			return err
		}
		after, err := cursor.LastEventID(ctx)
		if err != nil {
			return fmt.Errorf("read reconciled owner event cursor: %w", err)
		}
		if len(after) != 0 {
			return fmt.Errorf("reconciled owner event cursor was not cleared: %x", after)
		}
		close(reconciled)
		<-ctx.Done()
		return ctx.Err()
	})

	watchCtx, stopWatch := context.WithCancel(context.Background())
	defer stopWatch()
	watchResult := make(chan error, 1)
	go func() { watchResult <- manager.WatchTransport(watchCtx, stream) }()
	select {
	case <-reconciled:
	case err := <-watchResult:
		t.Fatalf("owner watch ended during gap reconciliation: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("owner watch did not reconcile the transport event gap")
	}

	if !manager.OwnsTerm(liveNode, liveConnection, int64(liveFence.GetOwnerEpoch())) {
		t.Fatal("gap reconciliation closed a still-connected owner term")
	}
	if manager.OwnsTerm(deadNode, deadConnection, int64(deadFence.GetOwnerEpoch())) {
		t.Fatal("gap reconciliation retained a disconnected owner term")
	}
	if _, _, err := manager.BindOperation(context.Background(), deadNode, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, mustUUIDv7(t), FencingCapability); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("disconnected owner bind = %v, want ErrNotOwner", err)
	}
	successor, err := NewManager(pool, signer, &recordingRegistrar{}, 30*time.Second, testLogger())
	if err != nil {
		t.Fatalf("new successor manager: %v", err)
	}
	successorFence, err := successor.OpenSession(context.Background(), deadNode, deadEndpoint, 9, []string{FencingCapability})
	if err != nil {
		t.Fatalf("take over disconnected gap term: %v", err)
	}
	if successorFence.GetOwnerEpoch() <= deadFence.GetOwnerEpoch() {
		t.Fatalf("successor epoch %d does not exceed disconnected epoch %d", successorFence.GetOwnerEpoch(), deadFence.GetOwnerEpoch())
	}

	stopWatch()
	select {
	case err := <-watchResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("owner watch shutdown = %v, want context canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("owner watch did not stop after cancellation")
	}
}

// TestManagerTransportEventGapRejectsNonAuthoritativeInventoryTermsIntegration
// proves that an unfenced epoch-zero connection and a different owner term do
// not keep local PostgreSQL owner leases alive merely because the node IDs are
// still present in transportd's inventory.
func TestManagerTransportEventGapRejectsNonAuthoritativeInventoryTermsIntegration(t *testing.T) {
	pool := testPool(t)
	signer, _ := testSigner(t)
	manager, err := NewManager(pool, signer, &recordingRegistrar{}, 30*time.Second, testLogger())
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	epochZeroNode, epochZeroEndpoint := testNodeAndEndpoint(t)
	wrongTermNode, wrongTermEndpoint := testNodeAndEndpoint(t)
	epochZeroFence, err := manager.OpenSession(context.Background(), epochZeroNode, epochZeroEndpoint, 13, []string{FencingCapability})
	if err != nil {
		t.Fatalf("open epoch-zero-masked session: %v", err)
	}
	wrongTermFence, err := manager.OpenSession(context.Background(), wrongTermNode, wrongTermEndpoint, 14, []string{FencingCapability})
	if err != nil {
		t.Fatalf("open wrong-term-masked session: %v", err)
	}
	manager.now = func() time.Time {
		return time.Now().UTC().Add(manager.publicationGrace + time.Second)
	}

	cursor := &memoryCursor{}
	cursorID := mustUUIDv7(t)
	cursor.set(cursorID[:])
	handler := transportEventHandler{manager: manager, cursor: cursor}
	err = handler.ReconcileOwnerEventGap(context.Background(), func(_ context.Context, nodeID []byte) (*transportv1.NodeConnection, error) {
		switch {
		case bytes.Equal(nodeID, epochZeroNode[:]):
			return &transportv1.NodeConnection{
				NodeId:     bytes.Clone(epochZeroNode[:]),
				EndpointId: bytes.Clone(epochZeroEndpoint[:]),
				OwnerEpoch: 0,
			}, nil
		case bytes.Equal(nodeID, wrongTermNode[:]):
			return &transportv1.NodeConnection{
				NodeId:     bytes.Clone(wrongTermNode[:]),
				EndpointId: bytes.Clone(wrongTermEndpoint[:]),
				OwnerEpoch: wrongTermFence.GetOwnerEpoch() + 1,
			}, nil
		default:
			return nil, fmt.Errorf("unexpected owner inventory node %x", nodeID)
		}
	})
	if err != nil {
		t.Fatalf("reconcile non-authoritative connection terms: %v", err)
	}
	for _, test := range []struct {
		name  string
		node  [16]byte
		fence *agentv1.ConnectionFenceV2
	}{
		{name: "epoch zero", node: epochZeroNode, fence: epochZeroFence},
		{name: "wrong term", node: wrongTermNode, fence: wrongTermFence},
	} {
		connectionID, err := fixed16(test.fence.GetConnectionId())
		if err != nil {
			t.Fatalf("%s connection id: %v", test.name, err)
		}
		if manager.OwnsTerm(test.node, connectionID, int64(test.fence.GetOwnerEpoch())) {
			t.Fatalf("%s inventory retained a non-authoritative owner term", test.name)
		}
	}
	last, err := cursor.LastEventID(context.Background())
	if err != nil {
		t.Fatalf("read reconciled cursor: %v", err)
	}
	if len(last) != 0 {
		t.Fatalf("successful exact-term reconciliation retained cursor %x", last)
	}
}

// TestManagerTransportEventGapDoesNotCloseConcurrentReplacementIntegration
// forces a successor open between the gap snapshot and its stale close. The
// exact connection-and-epoch predicate must leave that replacement authoritative.
func TestManagerTransportEventGapDoesNotCloseConcurrentReplacementIntegration(t *testing.T) {
	pool := testPool(t)
	signer, _ := testSigner(t)
	manager, err := NewManager(pool, signer, &recordingRegistrar{}, 30*time.Second, testLogger())
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	nodeID, endpointID := testNodeAndEndpoint(t)
	oldFence, err := manager.OpenSession(context.Background(), nodeID, endpointID, 10, []string{FencingCapability})
	if err != nil {
		t.Fatalf("open old session: %v", err)
	}
	reconciliationNow := time.Now().UTC().Add(manager.publicationGrace + time.Second)
	manager.now = func() time.Time { return reconciliationNow }

	cursor := &memoryCursor{}
	cursorID := mustUUIDv7(t)
	cursor.set(cursorID[:])
	handler := transportEventHandler{manager: manager, cursor: cursor}
	lookupStarted := make(chan struct{})
	replacementReady := make(chan struct{})
	reconcileResult := make(chan error, 1)
	reconcileCtx, stopReconcile := context.WithCancel(context.Background())
	defer stopReconcile()
	go func() {
		reconcileResult <- handler.ReconcileOwnerEventGap(reconcileCtx, func(ctx context.Context, candidate []byte) (*transportv1.NodeConnection, error) {
			if !bytes.Equal(candidate, nodeID[:]) {
				return nil, fmt.Errorf("unexpected owner inventory node %x", candidate)
			}
			close(lookupStarted)
			select {
			case <-replacementReady:
				return nil, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		})
	}()
	select {
	case <-lookupStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("gap reconciliation did not reach the inventory lookup")
	}
	// Reconciliation captured its synthetic post-grace instant before the
	// callback signalled lookupStarted. Restore the real clock before opening
	// the concurrent successor so its freshly acquired PostgreSQL lease remains
	// in the future and the test isolates the exact-term close race.
	manager.now = func() time.Time { return time.Now().UTC() }
	replacementFence, err := manager.OpenSession(context.Background(), nodeID, endpointID, 11, []string{FencingCapability})
	if err != nil {
		close(replacementReady)
		t.Fatalf("open replacement during gap reconciliation: %v", err)
	}
	close(replacementReady)
	select {
	case err := <-reconcileResult:
		if err != nil {
			t.Fatalf("reconcile gap around replacement: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("gap reconciliation did not finish after replacement")
	}

	replacementConnection, err := fixed16(replacementFence.GetConnectionId())
	if err != nil {
		t.Fatalf("replacement connection id: %v", err)
	}
	if !manager.OwnsTerm(nodeID, replacementConnection, int64(replacementFence.GetOwnerEpoch())) {
		t.Fatal("stale gap close ended the concurrent replacement term")
	}
	if replacementFence.GetOwnerEpoch() <= oldFence.GetOwnerEpoch() {
		t.Fatalf("replacement epoch %d does not exceed old epoch %d", replacementFence.GetOwnerEpoch(), oldFence.GetOwnerEpoch())
	}
	last, err := cursor.LastEventID(context.Background())
	if err != nil {
		t.Fatalf("read cursor after replacement reconciliation: %v", err)
	}
	if len(last) != 0 {
		t.Fatalf("successful replacement reconciliation retained cursor %x", last)
	}
}

// TestManagerTransportEventGapWaitsForBoundedConnectionPublicationIntegration
// pins the interval where OpenSession has recorded and registered a new term,
// but transportd has not yet published its NodeConnection. A fresh negative
// inventory read is retryable; publication succeeds, while a term that remains
// absent beyond the fixed grace is closed exactly and cannot renew forever.
func TestManagerTransportEventGapWaitsForBoundedConnectionPublicationIntegration(t *testing.T) {
	pool := testPool(t)
	signer, _ := testSigner(t)
	manager, err := NewManager(pool, signer, &recordingRegistrar{}, 30*time.Second, testLogger())
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	now := time.Now().UTC()
	manager.now = func() time.Time { return now }
	nodeID, endpointID := testNodeAndEndpoint(t)
	fence, err := manager.OpenSession(context.Background(), nodeID, endpointID, 12, []string{FencingCapability})
	if err != nil {
		t.Fatalf("open pending session: %v", err)
	}
	connectionID, err := fixed16(fence.GetConnectionId())
	if err != nil {
		t.Fatalf("pending connection id: %v", err)
	}
	cursor := &memoryCursor{}
	firstCursor := mustUUIDv7(t)
	cursor.set(firstCursor[:])
	handler := transportEventHandler{manager: manager, cursor: cursor}

	err = handler.ReconcileOwnerEventGap(context.Background(), func(context.Context, []byte) (*transportv1.NodeConnection, error) {
		return nil, nil
	})
	if !errors.Is(err, errConnectionPublicationPending) {
		t.Fatalf("fresh unpublished term reconciliation = %v, want publication pending", err)
	}
	if !manager.OwnsTerm(nodeID, connectionID, int64(fence.GetOwnerEpoch())) {
		t.Fatal("fresh unpublished term was closed during its publication grace")
	}
	last, err := cursor.LastEventID(context.Background())
	if err != nil {
		t.Fatalf("read retryable gap cursor: %v", err)
	}
	if !bytes.Equal(last, firstCursor[:]) {
		t.Fatalf("retryable publication gap cursor = %x, want %x", last, firstCursor)
	}

	// The accepted response precedes transportd's path-metadata collection and
	// trust recheck, whose combined bounded pipeline can legitimately take more
	// than ten seconds before NodeConnection becomes visible.
	const delayedPublication = 12 * time.Second
	if manager.publicationGrace <= delayedPublication {
		t.Fatalf("publication grace = %s, must cover the %s post-response pipeline", manager.publicationGrace, delayedPublication)
	}
	now = now.Add(delayedPublication)
	err = handler.ReconcileOwnerEventGap(context.Background(), func(context.Context, []byte) (*transportv1.NodeConnection, error) {
		return nil, nil
	})
	if !errors.Is(err, errConnectionPublicationPending) {
		t.Fatalf("delayed unpublished term reconciliation = %v, want publication pending", err)
	}
	if !manager.OwnsTerm(nodeID, connectionID, int64(fence.GetOwnerEpoch())) {
		t.Fatal("legitimate delayed publication lost its owner term")
	}
	last, err = cursor.LastEventID(context.Background())
	if err != nil {
		t.Fatalf("read delayed publication gap cursor: %v", err)
	}
	if !bytes.Equal(last, firstCursor[:]) {
		t.Fatalf("delayed publication gap cursor = %x, want %x", last, firstCursor)
	}

	if err := handler.ReconcileOwnerEventGap(context.Background(), func(context.Context, []byte) (*transportv1.NodeConnection, error) {
		return &transportv1.NodeConnection{
			NodeId:     bytes.Clone(nodeID[:]),
			EndpointId: bytes.Clone(endpointID[:]),
			OwnerEpoch: fence.GetOwnerEpoch(),
		}, nil
	}); err != nil {
		t.Fatalf("reconcile published connection: %v", err)
	}
	if !manager.OwnsTerm(nodeID, connectionID, int64(fence.GetOwnerEpoch())) {
		t.Fatal("published connection lost its owner term")
	}
	last, err = cursor.LastEventID(context.Background())
	if err != nil {
		t.Fatalf("read published gap cursor: %v", err)
	}
	if len(last) != 0 {
		t.Fatalf("published connection retained gap cursor %x", last)
	}

	secondCursor := mustUUIDv7(t)
	cursor.set(secondCursor[:])
	now = now.Add(manager.publicationGrace + time.Second)
	if err := handler.ReconcileOwnerEventGap(context.Background(), func(context.Context, []byte) (*transportv1.NodeConnection, error) {
		return nil, nil
	}); err != nil {
		t.Fatalf("reconcile unpublished term after grace: %v", err)
	}
	if manager.OwnsTerm(nodeID, connectionID, int64(fence.GetOwnerEpoch())) {
		t.Fatal("unpublished term outlived its bounded publication grace")
	}
	last, err = cursor.LastEventID(context.Background())
	if err != nil {
		t.Fatalf("read expired publication gap cursor: %v", err)
	}
	if len(last) != 0 {
		t.Fatalf("expired publication gap retained cursor %x", last)
	}
}

// TestManagerConcurrentLifecycleIsRaceFreeIntegration runs the renewal loop,
// concurrent same-node session opens, and concurrent binding issuance at
// once. Under -race it proves the per-node serialization and the lock-scoped
// binding snapshot keep the manager data-race free, and that the highest
// epoch survives concurrent write-backs.
func TestManagerConcurrentLifecycleIsRaceFreeIntegration(t *testing.T) {
	pool := testPool(t)
	signer, _ := testSigner(t)
	manager, err := NewManager(pool, signer, &recordingRegistrar{}, 2*time.Second, testLogger())
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	nodeID, endpointID := testNodeAndEndpoint(t)
	if _, err := manager.OpenSession(context.Background(), nodeID, endpointID, 9, []string{"ocserv.fencing.v2"}); err != nil {
		t.Fatalf("open session: %v", err)
	}
	runCtx, stopRun := context.WithCancel(context.Background())
	defer stopRun()
	go func() { _ = manager.Run(runCtx) }()

	const concurrency = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	maxEpoch := make(chan uint64, concurrency)
	for index := 0; index < concurrency; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			if index%2 == 0 {
				fence, err := manager.OpenSession(context.Background(), nodeID, endpointID, 9, []string{"ocserv.fencing.v2"})
				if err != nil {
					t.Errorf("concurrent open %d: %v", index, err)
					return
				}
				maxEpoch <- fence.GetOwnerEpoch()
				return
			}
			for attempt := 0; attempt < 8; attempt++ {
				if _, _, err := manager.BindOperation(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, mustUUIDv7(t), "ocserv.fencing.v2"); err != nil {
					t.Errorf("concurrent bind %d/%d: %v", index, attempt, err)
					return
				}
			}
			maxEpoch <- 0
		}(index)
	}
	close(start)
	wg.Wait()
	close(maxEpoch)
	highest := uint64(1)
	for epoch := range maxEpoch {
		if epoch > highest {
			highest = epoch
		}
	}
	fence, _, err := manager.BindOperation(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, mustUUIDv7(t), "ocserv.fencing.v2")
	if err != nil {
		t.Fatalf("post-race bind: %v", err)
	}
	if fence.GetOwnerEpoch() < highest {
		t.Fatalf("retained epoch %d is lower than the observed maximum %d", fence.GetOwnerEpoch(), highest)
	}
}

// TestManagerReopenRegistrationFailureRetiresTheOldTermIntegration pins the
// reconnect failure branch: once a same-identity reopen advances the
// PostgreSQL fencing epoch, the previous local term must never issue bindings
// again — even though registering the new fence failed, the failed term was
// released, and the old fence may still sit registered in transportd.
func TestManagerReopenRegistrationFailureRetiresTheOldTermIntegration(t *testing.T) {
	pool := testPool(t)
	signer, _ := testSigner(t)
	manager, err := NewManager(pool, signer, &recordingRegistrar{}, 30*time.Second, testLogger())
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	nodeID, endpointID := testNodeAndEndpoint(t)
	fence, err := manager.OpenSession(context.Background(), nodeID, endpointID, 21, []string{"ocserv.fencing.v2"})
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	manager.mu.Lock()
	manager.registrar = &recordingRegistrar{failtures: true}
	manager.mu.Unlock()
	if _, err := manager.OpenSession(context.Background(), nodeID, endpointID, 22, []string{"ocserv.fencing.v2"}); err == nil {
		t.Fatal("reopen through a failing registrar unexpectedly succeeded")
	}
	if _, _, err := manager.BindOperation(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, mustUUIDv7(t), "ocserv.fencing.v2"); !errors.Is(err, ErrFenceUnavailable) {
		t.Fatalf("bind after a failed reopen = %v, want ErrFenceUnavailable", err)
	}
	// The failed reopen released its own term, so a successor takes the node
	// over through a real Acquire with a strictly higher epoch.
	successor, err := NewManager(pool, signer, &recordingRegistrar{}, 30*time.Second, testLogger())
	if err != nil {
		t.Fatalf("new successor manager: %v", err)
	}
	successorFence, err := successor.OpenSession(context.Background(), nodeID, endpointID, 23, []string{"ocserv.fencing.v2"})
	if err != nil {
		t.Fatalf("successor take over after a failed reopen: %v", err)
	}
	if successorFence.GetOwnerEpoch() <= fence.GetOwnerEpoch() {
		t.Fatalf("successor epoch %d does not exceed the retired epoch %d", successorFence.GetOwnerEpoch(), fence.GetOwnerEpoch())
	}
}

// TestManagerRegistrationHeartbeatEndsUnreachableTermsIntegration verifies
// both sides of the registration heartbeat bound: an unreachable transportd
// does not end a healthy owner before the lease TTL has actually elapsed,
// and once it has, the term's renewal stops and the lease is released so
// another controller can take over. The maintenance interval is deliberately
// much shorter than the heartbeat period: the bound must measure real
// elapsed time, not the number of retry attempts.
func TestManagerRegistrationHeartbeatEndsUnreachableTermsIntegration(t *testing.T) {
	pool := testPool(t)
	signer, _ := testSigner(t)
	manager, err := NewManager(pool, signer, &recordingRegistrar{}, time.Second, testLogger())
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	// Registration must succeed once for the session to exist, so flip the
	// registrar to failing after the open.
	nodeID, endpointID := testNodeAndEndpoint(t)
	fence, err := manager.OpenSession(context.Background(), nodeID, endpointID, 11, []string{"ocserv.fencing.v2"})
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	manager.mu.Lock()
	manager.registrar = &recordingRegistrar{failtures: true}
	manager.registrationEvery = 200 * time.Millisecond
	manager.interval = 20 * time.Millisecond
	manager.mu.Unlock()
	runCtx, stopRun := context.WithCancel(context.Background())
	defer stopRun()
	go func() { _ = manager.Run(runCtx) }()

	// The failure bound is lease TTL + heartbeat period = 1.2s. Well before
	// it — at a point where attempt counting would already have ended the
	// term several times over — the owner must still be issuing bindings.
	time.Sleep(400 * time.Millisecond)
	if _, _, err := manager.BindOperation(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, mustUUIDv7(t), "ocserv.fencing.v2"); err != nil {
		t.Fatalf("bind 400ms into registration failures = %v, want the term alive", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, _, err := manager.BindOperation(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, mustUUIDv7(t), "ocserv.fencing.v2"); errors.Is(err, ErrFenceUnavailable) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("term outlived an unreachable registration channel for a full lease TTL")
		}
		time.Sleep(10 * time.Millisecond)
	}

	successor, err := NewManager(pool, signer, &recordingRegistrar{}, 30*time.Second, testLogger())
	if err != nil {
		t.Fatalf("new successor manager: %v", err)
	}
	successorFence, err := successor.OpenSession(context.Background(), nodeID, endpointID, 12, []string{"ocserv.fencing.v2"})
	if err != nil {
		t.Fatalf("successor take over after heartbeat bound: %v", err)
	}
	if successorFence.GetOwnerEpoch() <= fence.GetOwnerEpoch() {
		t.Fatalf("successor epoch %d does not exceed owner epoch %d", successorFence.GetOwnerEpoch(), fence.GetOwnerEpoch())
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// registryFence mimics transportd's fence registry closely enough for the
// observer boundary: it keeps the last fence whose registration succeeded and
// serves it to GetOwnerFence readers, exactly like a transportd that has not
// yet seen a higher epoch.
type registryFence struct {
	failures bool
	fence    *agentv1.ConnectionFenceV2
}

func (r *registryFence) RegisterOwnerFence(_ context.Context, fence *agentv1.ConnectionFenceV2) error {
	if r.failures {
		return errors.New("transport unavailable")
	}
	r.fence = fence
	return nil
}

func (r *registryFence) GetOwnerFence(_ context.Context, nodeID []byte) (*agentv1.ConnectionFenceV2, error) {
	if r.fence == nil || !bytes.Equal(r.fence.GetNodeId(), nodeID) {
		return nil, nil
	}
	return r.fence, nil
}

// TestObserverFailsClosedOnAStaleRegisteredFenceIntegration pins the reopen
// failure lifecycle across roles: epoch N is registered in transportd, a
// same-manager reconnect advances the PostgreSQL epoch but its registration
// fails, so transportd keeps serving N while the database authority already
// moved on. The manager fails closed with ErrFenceUnavailable — and so must
// the observer, which may not re-sign the stale registered fence even though
// its lease deadline has not passed. Only a successor's real Acquire with a
// successfully registered higher epoch reopens the observer path.
func TestObserverFailsClosedOnAStaleRegisteredFenceIntegration(t *testing.T) {
	pool := testPool(t)
	signer, _ := testSigner(t)
	registry := &registryFence{}
	manager, err := NewManager(pool, signer, registry, 30*time.Second, testLogger())
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	nodeID, endpointID := testNodeAndEndpoint(t)
	firstFence, err := manager.OpenSession(context.Background(), nodeID, endpointID, 41, []string{"ocserv.fencing.v2"})
	if err != nil {
		t.Fatalf("open session: %v", err)
	}

	observer, err := NewObserver(pool, registry, signer)
	if err != nil {
		t.Fatalf("new observer: %v", err)
	}
	// While the authority still backs the registered term, the observer
	// serves exactly that term.
	var liveBinding *agentv1.FenceBindingV2
	liveErr := observer.ExecuteFenced(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, mustUUIDv7(t), "ocserv.fencing.v2",
		func(_ context.Context, fence *agentv1.ConnectionFenceV2, binding *agentv1.FenceBindingV2) error {
			if fence.GetOwnerEpoch() != firstFence.GetOwnerEpoch() {
				t.Fatalf("observer bind on the live term = epoch %d, want %d", fence.GetOwnerEpoch(), firstFence.GetOwnerEpoch())
			}
			liveBinding = binding
			return nil
		})
	if liveErr != nil || liveBinding == nil {
		t.Fatalf("observer bind on the live term = (%v, %v)", liveErr, liveBinding)
	}

	// The same manager reconnects: Acquire advances the PostgreSQL epoch, but
	// the new fence cannot reach transportd, whose registry keeps serving N.
	registry.failures = true
	if _, err := manager.OpenSession(context.Background(), nodeID, endpointID, 42, []string{"ocserv.fencing.v2"}); err == nil {
		t.Fatal("reopen through a failing registrar unexpectedly succeeded")
	}
	if _, _, err := manager.BindOperation(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, mustUUIDv7(t), "ocserv.fencing.v2"); !errors.Is(err, ErrFenceUnavailable) {
		t.Fatalf("manager bind after the failed reopen = %v, want ErrFenceUnavailable", err)
	}
	registered, err := registry.GetOwnerFence(context.Background(), nodeID[:])
	if err != nil || registered == nil {
		t.Fatalf("registry still serves the old fence: %v %v", registered, err)
	}
	if registered.GetOwnerEpoch() != firstFence.GetOwnerEpoch() {
		t.Fatalf("registry kept epoch %d, want the old epoch %d", registered.GetOwnerEpoch(), firstFence.GetOwnerEpoch())
	}
	// Precondition of the vulnerability: the stale fence still carries an
	// unexpired lease deadline, so a freshly signed N binding would be
	// accepted by the registry.
	if !registered.GetLeaseUntil().AsTime().After(time.Now().UTC()) {
		t.Fatalf("stale fence lease %v already expired; the dangerous window is gone", registered.GetLeaseUntil().AsTime())
	}

	// The split-role observer must fail closed instead of running a mutation
	// the stale registry would accept.
	ran := false
	if err := observer.ExecuteFenced(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, mustUUIDv7(t), "ocserv.fencing.v2",
		func(context.Context, *agentv1.ConnectionFenceV2, *agentv1.FenceBindingV2) error {
			ran = true
			return nil
		}); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("observer bind on the stale registered fence = %v, want ErrNotOwner", err)
	}
	if ran {
		t.Fatal("observer ran a mutation for the stale registered term")
	}

	// A successor takes the node over through a real Acquire strictly above
	// the failed reopen's epoch and registers again, which reopens the
	// observer path on exactly the successor's term.
	registry.failures = false
	successor, err := NewManager(pool, signer, registry, 30*time.Second, testLogger())
	if err != nil {
		t.Fatalf("new successor manager: %v", err)
	}
	successorFence, err := successor.OpenSession(context.Background(), nodeID, endpointID, 43, []string{"ocserv.fencing.v2"})
	if err != nil {
		t.Fatalf("successor take over after the failed reopen: %v", err)
	}
	if successorFence.GetOwnerEpoch() <= firstFence.GetOwnerEpoch()+1 {
		t.Fatalf("successor epoch %d does not exceed the failed reopen epoch %d", successorFence.GetOwnerEpoch(), firstFence.GetOwnerEpoch()+1)
	}
	observedEpoch := int64(0)
	if err := observer.ExecuteFenced(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, mustUUIDv7(t), "ocserv.fencing.v2",
		func(_ context.Context, fence *agentv1.ConnectionFenceV2, binding *agentv1.FenceBindingV2) error {
			if binding == nil {
				t.Fatal("observer served the successor term without a binding")
			}
			observedEpoch = int64(fence.GetOwnerEpoch())
			return nil
		}); err != nil {
		t.Fatalf("observer bind on the successor term: %v", err)
	}
	if observedEpoch != int64(successorFence.GetOwnerEpoch()) {
		t.Fatalf("observer bind on the successor term = epoch %d, want %d", observedEpoch, successorFence.GetOwnerEpoch())
	}
}

// TestObserverGuardSpansTheMutationAgainstTheReopenAdvanceIntegration pins
// the fencing interval with a deterministic barrier, in the exact order the
// TOCTOU review described: the observer has already verified and is holding
// epoch N when a same-manager reconnect Acquires N+1 whose registration then
// fails. The ownership guard — not a point-in-time read — must keep the
// epoch at N until the observer's mutation RPC completed: the reopen's
// Acquire blocks on the guard, so no N binding can be signed after the
// authority moved on, and the N mutation that ran is provably
// authority-backed at mutation time.
func TestObserverGuardSpansTheMutationAgainstTheReopenAdvanceIntegration(t *testing.T) {
	pool := testPool(t)
	signer, _ := testSigner(t)
	registry := &registryFence{}
	manager, err := NewManager(pool, signer, registry, 30*time.Second, testLogger())
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	nodeID, endpointID := testNodeAndEndpoint(t)
	firstFence, err := manager.OpenSession(context.Background(), nodeID, endpointID, 51, []string{"ocserv.fencing.v2"})
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	observer, err := NewObserver(pool, registry, signer)
	if err != nil {
		t.Fatalf("new observer: %v", err)
	}

	// The observer verifies N, acquires the authority guard, and starts a
	// mutation that blocks mid-RPC.
	mutationStarted := make(chan struct{})
	mutationProceed := make(chan struct{})
	mutationDone := make(chan error, 1)
	go func() {
		mutationDone <- observer.ExecuteFenced(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, mustUUIDv7(t), "ocserv.fencing.v2",
			func(_ context.Context, fence *agentv1.ConnectionFenceV2, binding *agentv1.FenceBindingV2) error {
				if binding.GetOwnerEpoch() != firstFence.GetOwnerEpoch() {
					t.Errorf("in-flight mutation epoch = %d, want the verified epoch %d", binding.GetOwnerEpoch(), firstFence.GetOwnerEpoch())
				}
				close(mutationStarted)
				<-mutationProceed
				return nil
			})
	}()
	<-mutationStarted

	// After the observer's verification, the same manager reconnects: the
	// real PostgreSQL Acquire must wait behind the guard instead of
	// advancing the epoch under the in-flight N mutation.
	registry.failures = true
	reopenDone := make(chan error, 1)
	go func() {
		_, err := manager.OpenSession(context.Background(), nodeID, endpointID, 52, []string{"ocserv.fencing.v2"})
		reopenDone <- err
	}()
	select {
	case err := <-reopenDone:
		t.Fatalf("the reopen advanced the epoch while the guarded mutation was in flight: %v", err)
	case <-time.After(1500 * time.Millisecond):
	}

	// The mutation completes inside the fencing interval; only then may the
	// epoch advance — the reopen's Acquire proceeds and its registration
	// fails, leaving transportd serving N.
	close(mutationProceed)
	if err := <-mutationDone; err != nil {
		t.Fatalf("guarded mutation: %v", err)
	}
	if err := <-reopenDone; err == nil {
		t.Fatal("reopen through a failing registrar unexpectedly succeeded")
	}
	if _, _, err := manager.BindOperation(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, mustUUIDv7(t), "ocserv.fencing.v2"); !errors.Is(err, ErrFenceUnavailable) {
		t.Fatalf("manager bind after the failed reopen = %v, want ErrFenceUnavailable", err)
	}
	// The epoch already advanced past N, so the stale registered fence can
	// no longer mint mutations even though its lease deadline has not passed.
	ran := false
	if err := observer.ExecuteFenced(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, mustUUIDv7(t), "ocserv.fencing.v2",
		func(context.Context, *agentv1.ConnectionFenceV2, *agentv1.FenceBindingV2) error {
			ran = true
			return nil
		}); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("observer bind after the guarded mutation and failed reopen = %v, want ErrNotOwner", err)
	}
	if ran {
		t.Fatal("observer ran a mutation after the epoch advanced past the registered fence")
	}
}

// TestManagerExecuteFencedRunsTheActionUnderTheNodeLock covers the
// worker-role executor: a live session's mutation runs with the fence and
// binding, an unknown node keeps the unfenced compatibility path, and an
// ended session fails closed without running the mutation.
func TestManagerExecuteFencedRunsTheActionUnderTheNodeLockIntegration(t *testing.T) {
	pool := testPool(t)
	signer, _ := testSigner(t)
	manager, err := NewManager(pool, signer, &recordingRegistrar{}, 30*time.Second, testLogger())
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	nodeID, endpointID := testNodeAndEndpoint(t)
	fence, err := manager.OpenSession(context.Background(), nodeID, endpointID, 61, []string{"ocserv.fencing.v2"})
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	sawFence, sawBinding := false, false
	if err := manager.ExecuteFenced(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, mustUUIDv7(t), "ocserv.fencing.v2",
		func(_ context.Context, actionFence *agentv1.ConnectionFenceV2, binding *agentv1.FenceBindingV2) error {
			sawFence, sawBinding = actionFence.GetOwnerEpoch() == fence.GetOwnerEpoch(), binding != nil
			return nil
		}); err != nil || !sawFence || !sawBinding {
		t.Fatalf("live session execute = (%v, fence %v, binding %v)", err, sawFence, sawBinding)
	}

	unknown := mustUUIDv7(t)
	ran := false
	if err := manager.ExecuteFenced(context.Background(), unknown, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, mustUUIDv7(t), "ocserv.fencing.v2",
		func(_ context.Context, actionFence *agentv1.ConnectionFenceV2, binding *agentv1.FenceBindingV2) error {
			ran = actionFence == nil && binding == nil
			return nil
		}); err != nil || !ran {
		t.Fatalf("unknown node execute = (%v, ran %v), want the unfenced compatibility path", err, ran)
	}

	connectionID, err := fixed16(fence.GetConnectionId())
	if err != nil {
		t.Fatalf("fence connection id: %v", err)
	}
	if err := manager.CloseSession(context.Background(), nodeID, connectionID, int64(fence.GetOwnerEpoch())); err != nil {
		t.Fatalf("close session: %v", err)
	}
	ran = false
	if err := manager.ExecuteFenced(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, mustUUIDv7(t), "ocserv.fencing.v2",
		func(context.Context, *agentv1.ConnectionFenceV2, *agentv1.FenceBindingV2) error {
			ran = true
			return nil
		}); !errors.Is(err, ErrNotOwner) || ran {
		t.Fatalf("ended session execute = (%v, ran %v), want ErrNotOwner without a mutation", err, ran)
	}
}
