package ownersession

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
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

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestManagerSessionOwnershipIntegration(t *testing.T) {
	pool := testPool(t)
	signer, _ := testSigner(t)
	registrar := &recordingRegistrar{}
	manager, err := NewManagerBackend(postgres.WrapPool(pool), signer, registrar, 30*time.Second, testLogger())
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	nodeID, endpointID := testNodeAndEndpoint(t)

	fence, err := manager.OpenSession(context.Background(), nodeID, endpointID, 5, []string{"ocserv.fencing.v2", "ocserv.service.reload"})
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	if len(registrar.fences) != 1 || fence.GetOwnerEpoch() == 0 || fence.GetAuthorizationRevision() != 5 {
		t.Fatalf("fence len=%d epoch=%d rev=%d", len(registrar.fences), fence.GetOwnerEpoch(), fence.GetAuthorizationRevision())
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
	if !slices.Equal(fence.GetCapabilities(), []string{"ocserv.fencing.v2", "ocserv.service.reload"}) {
		t.Fatalf("fence capabilities = %v", fence.GetCapabilities())
	}
	var stored time.Time
	if err := pool.QueryRow(context.Background(), `SELECT lease_until FROM connection_owner_fencing WHERE node_id = $1`, nodeID[:]).Scan(&stored); err != nil {
		t.Fatalf("read stored lease: %v", err)
	}
	if !stored.UTC().Equal(fence.GetLeaseUntil().AsTime().UTC()) {
		t.Fatalf("fence lease %v != stored lease %v", fence.GetLeaseUntil().AsTime(), stored)
	}

	rivalRegistrar := &recordingRegistrar{}
	rival, err := NewManagerBackend(postgres.WrapPool(pool), signer, rivalRegistrar, 30*time.Second, testLogger())
	if err != nil {
		t.Fatalf("new rival manager: %v", err)
	}
	if _, err := rival.OpenSession(context.Background(), nodeID, endpointID, 6, []string{"ocserv.fencing.v2"}); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("rival open error = %v, want ErrNotOwner", err)
	}
	if len(rivalRegistrar.fences) != 0 {
		t.Fatal("rival registered a fence without holding the lease")
	}

	operationID := mustUUIDv7(t)
	fence, binding, err := manager.BindOperation(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, operationID, "ocserv.service.reload")
	if err != nil || binding.GetOwnerEpoch() != fence.GetOwnerEpoch() {
		t.Fatalf("bind operation = %v", err)
	}
	if _, _, err := manager.BindOperation(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, operationID, "ocserv.tenant.admin"); !errors.Is(err, ErrCapabilityNotFenced) {
		t.Fatalf("unheld capability error = %v, want ErrCapabilityNotFenced", err)
	}
	if _, _, err := manager.BindOperation(context.Background(), mustUUIDv7(t), agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, operationID, "ocserv.service.reload"); !errors.Is(err, ErrNoFence) {
		t.Fatalf("unknown node error = %v, want ErrNoFence", err)
	}
}

func TestManagerTakeoverFailsClosedIntegration(t *testing.T) {
	pool := testPool(t)
	signer, _ := testSigner(t)
	owner, err := NewManagerBackend(postgres.WrapPool(pool), signer, &recordingRegistrar{}, time.Second, testLogger())
	if err != nil {
		t.Fatalf("new owner manager: %v", err)
	}
	nodeID, endpointID := testNodeAndEndpoint(t)
	firstFence, err := owner.OpenSession(context.Background(), nodeID, endpointID, 3, []string{"ocserv.fencing.v2"})
	if err != nil {
		t.Fatalf("open session: %v", err)
	}

	time.Sleep(1200 * time.Millisecond)
	successor, err := NewManagerBackend(postgres.WrapPool(pool), signer, &recordingRegistrar{}, 30*time.Second, testLogger())
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

	operationID := mustUUIDv7(t)
	if _, _, err := owner.BindOperation(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, operationID, "ocserv.fencing.v2"); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("stale owner bind error = %v, want ErrNotOwner", err)
	}
	firstConnection, err := fixed16(firstFence.GetConnectionId())
	if err != nil {
		t.Fatal(err)
	}
	if owner.OwnsTerm(nodeID, firstConnection, int64(firstFence.GetOwnerEpoch())) {
		t.Fatal("stale manager retained local recovery authority")
	}
	if _, _, err := successor.BindOperation(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_STATE_UPDATE, mustUUIDv7(t), "ocserv.fencing.v2"); err != nil {
		t.Fatalf("successor bind: %v", err)
	}
}

func TestManagerConcurrentLifecycleIsRaceFreeIntegration(t *testing.T) {
	pool := testPool(t)
	signer, _ := testSigner(t)
	manager, err := NewManagerBackend(postgres.WrapPool(pool), signer, &recordingRegistrar{}, 2*time.Second, testLogger())
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

func TestManagerExecuteFencedRunsTheActionUnderTheNodeLockIntegration(t *testing.T) {
	pool := testPool(t)
	signer, _ := testSigner(t)
	manager, err := NewManagerBackend(postgres.WrapPool(pool), signer, &recordingRegistrar{}, 30*time.Second, testLogger())
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

	ran := false
	if err := manager.ExecuteFenced(context.Background(), mustUUIDv7(t), agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, mustUUIDv7(t), "ocserv.fencing.v2",
		func(_ context.Context, actionFence *agentv1.ConnectionFenceV2, binding *agentv1.FenceBindingV2) error {
			ran = actionFence == nil && binding == nil
			return nil
		}); err != nil || !ran {
		t.Fatalf("unknown node execute = (%v, ran %v), want unfenced compatibility path", err, ran)
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
		t.Fatalf("ended session execute = (%v, ran %v), want ErrNotOwner without mutation", err, ran)
	}
}
