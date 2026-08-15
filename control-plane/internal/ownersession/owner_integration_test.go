package ownersession

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
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
	// closed afterwards.
	operationID := mustUUIDv7(t)
	if _, _, err := owner.BindOperation(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, operationID, "ocserv.fencing.v2"); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("stale owner bind error = %v, want ErrNotOwner", err)
	}
	if _, _, err := owner.BindOperation(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, operationID, "ocserv.fencing.v2"); !errors.Is(err, ErrNoFence) {
		t.Fatalf("lost session bind error = %v, want ErrNoFence", err)
	}

	// The successor keeps operating on the node.
	if _, _, err := successor.BindOperation(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_STATE_UPDATE, mustUUIDv7(t), "ocserv.fencing.v2"); err != nil {
		t.Fatalf("successor bind: %v", err)
	}
}

// TestManagerRegistrationFailureFailsClosedIntegration verifies that a
// session is never granted when transportd did not accept the fence.
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
	if _, _, err := manager.BindOperation(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, mustUUIDv7(t), "ocserv.fencing.v2"); !errors.Is(err, ErrNoFence) {
		t.Fatalf("bind error = %v, want ErrNoFence", err)
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

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
