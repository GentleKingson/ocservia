package ownersession

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
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
