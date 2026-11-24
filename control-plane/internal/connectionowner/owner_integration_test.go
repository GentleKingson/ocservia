package connectionowner_test

import (
	"context"
	"errors"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/connectionowner"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/testfixture"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return testfixture.OpenRuntimePool(t)
}

func testNode(t *testing.T) [16]byte {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("mint node id: %v", err)
	}
	return id
}

func testConnection(t *testing.T) [16]byte {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("mint connection id: %v", err)
	}
	return id
}

func TestConnectionOwnerAcquireAssertReleaseIntegration(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	backend := postgres.WrapPool(pool)
	nodeA, nodeB := testNode(t), testNode(t)
	connA := testConnection(t)

	term, err := connectionowner.AcquireBackend(ctx, backend, nodeA, connA)
	if err != nil || term.Epoch() != 1 || term.ConnectionID() != connA {
		t.Fatalf("first acquire = (%v, %v), want epoch 1", term, err)
	}
	if err := term.AssertCurrentBackend(ctx, backend); err != nil {
		t.Fatalf("assert current term: %v", err)
	}

	// Nodes advance independently: a second node starts at epoch 1.
	other, err := connectionowner.AcquireBackend(ctx, backend, nodeB, testConnection(t))
	if err != nil || other.Epoch() != 1 {
		t.Fatalf("independent node acquire = (%v, %v), want epoch 1", other, err)
	}

	state, err := connectionowner.ReadStateBackend(ctx, backend, nodeA)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if state.Epoch != 1 || state.ConnectionID != connA || !state.Active {
		t.Fatalf("unexpected owner state: %+v", state)
	}

	if err := term.ReleaseBackend(ctx, backend); err != nil {
		t.Fatalf("release current term: %v", err)
	}
	if err := term.AssertCurrentBackend(ctx, backend); !errors.Is(err, connectionowner.ErrNotOwner) {
		t.Fatalf("assert after release = %v, want ErrNotOwner", err)
	}
}

func TestConnectionOwnerReconnectIncrementsEpochAndFencesPriorIntegration(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	backend := postgres.WrapPool(pool)
	node := testNode(t)

	first, err := connectionowner.AcquireBackend(ctx, backend, node, testConnection(t))
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	second, err := connectionowner.AcquireBackend(ctx, backend, node, testConnection(t))
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if second.Epoch() <= first.Epoch() {
		t.Fatalf("replacement epoch = %d, want > %d", second.Epoch(), first.Epoch())
	}
	if err := first.AssertCurrentBackend(ctx, backend); !errors.Is(err, connectionowner.ErrNotOwner) {
		t.Fatalf("replaced connection assert = %v, want ErrNotOwner", err)
	}
	if err := second.AssertCurrentBackend(ctx, backend); err != nil {
		t.Fatalf("current connection assert: %v", err)
	}
}

func TestConnectionOwnerEpochNeverReusedIntegration(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	backend := postgres.WrapPool(pool)
	node := testNode(t)
	seen := map[int64]bool{}
	previous := int64(0)

	for index := 0; index < 6; index++ {
		term, err := connectionowner.AcquireBackend(ctx, backend, node, testConnection(t))
		if err != nil {
			t.Fatalf("acquire %d: %v", index, err)
		}
		if term.Epoch() <= previous || seen[term.Epoch()] {
			t.Fatalf("acquire %d epoch = %d, previous = %d, reused = %v", index, term.Epoch(), previous, seen[term.Epoch()])
		}
		seen[term.Epoch()] = true
		previous = term.Epoch()
	}
}
