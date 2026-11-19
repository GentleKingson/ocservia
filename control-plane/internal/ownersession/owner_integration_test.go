package ownersession

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/testfixture"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return testfixture.OpenRuntimePool(t)
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
	var boundFence *agentv1.ConnectionFenceV2
	var binding *agentv1.FenceBindingV2
	if err := manager.ExecuteFenced(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, operationID, "ocserv.service.reload",
		func(_ context.Context, f *agentv1.ConnectionFenceV2, b *agentv1.FenceBindingV2) error {
			boundFence, binding = f, b
			return nil
		}); err != nil || boundFence == nil || binding == nil || binding.GetOwnerEpoch() != boundFence.GetOwnerEpoch() {
		t.Fatalf("execute fenced = %v", err)
	}
	if err := manager.ExecuteFenced(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, operationID, "ocserv.tenant.admin",
		func(context.Context, *agentv1.ConnectionFenceV2, *agentv1.FenceBindingV2) error { return nil }); !errors.Is(err, ErrCapabilityNotFenced) {
		t.Fatalf("unheld capability error = %v, want ErrCapabilityNotFenced", err)
	}
	unfenced := false
	if err := manager.ExecuteFenced(context.Background(), mustUUIDv7(t), agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, operationID, "ocserv.service.reload",
		func(_ context.Context, f *agentv1.ConnectionFenceV2, b *agentv1.FenceBindingV2) error {
			unfenced = f == nil && b == nil
			return nil
		}); err != nil || !unfenced {
		t.Fatalf("unknown node execute = (%v, unfenced %v), want unfenced compatibility path", err, unfenced)
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
	if err := owner.ExecuteFenced(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, operationID, "ocserv.fencing.v2",
		func(context.Context, *agentv1.ConnectionFenceV2, *agentv1.FenceBindingV2) error { return nil }); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("stale owner execute error = %v, want ErrNotOwner", err)
	}
	firstConnection, err := fixed16(firstFence.GetConnectionId())
	if err != nil {
		t.Fatal(err)
	}
	if owner.OwnsTerm(nodeID, firstConnection, int64(firstFence.GetOwnerEpoch())) {
		t.Fatal("stale manager retained local recovery authority")
	}
	if err := successor.ExecuteFenced(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_STATE_UPDATE, mustUUIDv7(t), "ocserv.fencing.v2",
		func(_ context.Context, f *agentv1.ConnectionFenceV2, b *agentv1.FenceBindingV2) error {
			if f == nil || b == nil {
				return errors.New("missing fence or binding")
			}
			return nil
		}); err != nil {
		t.Fatalf("successor execute: %v", err)
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
				if err := manager.ExecuteFenced(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, mustUUIDv7(t), "ocserv.fencing.v2",
					func(_ context.Context, f *agentv1.ConnectionFenceV2, b *agentv1.FenceBindingV2) error {
						if f == nil || b == nil {
							return errors.New("missing fence or binding")
						}
						return nil
					}); err != nil {
					t.Errorf("concurrent execute %d/%d: %v", index, attempt, err)
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
	var fence *agentv1.ConnectionFenceV2
	if err := manager.ExecuteFenced(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, mustUUIDv7(t), "ocserv.fencing.v2",
		func(_ context.Context, f *agentv1.ConnectionFenceV2, _ *agentv1.FenceBindingV2) error {
			fence = f
			return nil
		}); err != nil {
		t.Fatalf("post-race execute: %v", err)
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
