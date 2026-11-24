package ownersession

import (
	"context"
	"io"
	"log/slog"
	"sync"
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

func mustUUIDv7(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid v7: %v", err)
	}
	return id
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
	manager, err := NewManagerBackend(postgres.WrapPool(pool), testLogger())
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	nodeID, endpointID := testNodeAndEndpoint(t)

	session, err := manager.OpenSession(context.Background(), nodeID, mustUUIDv7(t), endpointID)
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	if session.SessionEpoch == 0 {
		t.Fatalf("session epoch = 0")
	}
	if !manager.OwnsTerm(nodeID, session.ConnectionID, session.SessionEpoch) {
		t.Fatal("manager did not recognize its exact local session term")
	}
	wrongConnection := session.ConnectionID
	wrongConnection[0] ^= 0xff
	if manager.OwnsTerm(nodeID, wrongConnection, session.SessionEpoch) || manager.OwnsTerm(nodeID, session.ConnectionID, session.SessionEpoch+1) {
		t.Fatal("manager accepted a different local session term")
	}

	next, err := manager.OpenSession(context.Background(), nodeID, mustUUIDv7(t), endpointID)
	if err != nil {
		t.Fatalf("reopen session: %v", err)
	}
	if next.SessionEpoch <= session.SessionEpoch {
		t.Fatalf("reopen epoch %d <= prior epoch %d", next.SessionEpoch, session.SessionEpoch)
	}
	if manager.OwnsTerm(nodeID, session.ConnectionID, session.SessionEpoch) {
		t.Fatal("manager retained superseded session term")
	}
	if !manager.OwnsTerm(nodeID, next.ConnectionID, next.SessionEpoch) {
		t.Fatal("manager did not recognize replacement session term")
	}

	if err := manager.CloseSession(context.Background(), nodeID, next.ConnectionID, next.SessionEpoch); err != nil {
		t.Fatalf("close session: %v", err)
	}
	if manager.OwnsTerm(nodeID, next.ConnectionID, next.SessionEpoch) {
		t.Fatal("closed session still reported owned")
	}

	// Simulate a Controller restart where transportd keeps an existing session
	// open across the restart boundary and later delivers its Disconnected event
	// to a fresh Manager with an empty in-memory session map.
	restarted, err := manager.OpenSession(context.Background(), nodeID, mustUUIDv7(t), endpointID)
	if err != nil {
		t.Fatalf("open session before restart: %v", err)
	}
	backend := postgres.WrapPool(pool)
	successor, err := NewManagerBackend(backend, testLogger())
	if err != nil {
		t.Fatalf("new successor manager: %v", err)
	}
	if err := successor.CloseSession(context.Background(), nodeID, next.ConnectionID, next.SessionEpoch); err != nil {
		t.Fatalf("close stale pre-restart session: %v", err)
	}
	state, err := connectionowner.ReadStateBackend(context.Background(), backend, nodeID)
	if err != nil {
		t.Fatalf("read state after stale close: %v", err)
	}
	if !state.Active || state.ConnectionID != restarted.ConnectionID || uint64(state.Epoch) != restarted.SessionEpoch {
		t.Fatalf("stale close altered active owner state: %+v", state)
	}
	if err := successor.CloseSession(context.Background(), nodeID, restarted.ConnectionID, restarted.SessionEpoch); err != nil {
		t.Fatalf("close active pre-restart session: %v", err)
	}
	state, err = connectionowner.ReadStateBackend(context.Background(), backend, nodeID)
	if err != nil {
		t.Fatalf("read state after restart close: %v", err)
	}
	if state.Active {
		t.Fatalf("pre-restart session remained active after CloseSession: %+v", state)
	}
}

func TestManagerConcurrentLifecycleIsRaceFreeIntegration(t *testing.T) {
	pool := testPool(t)
	manager, err := NewManagerBackend(postgres.WrapPool(pool), testLogger())
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	nodeID, endpointID := testNodeAndEndpoint(t)
	first, err := manager.OpenSession(context.Background(), nodeID, mustUUIDv7(t), endpointID)
	if err != nil {
		t.Fatalf("open session: %v", err)
	}

	const concurrency = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	maxEpoch := make(chan uint64, concurrency)
	for index := 0; index < concurrency; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			session, err := manager.OpenSession(context.Background(), nodeID, mustUUIDv7(t), endpointID)
			if err != nil {
				t.Errorf("concurrent open %d: %v", index, err)
				return
			}
			maxEpoch <- session.SessionEpoch
		}(index)
	}
	close(start)
	wg.Wait()
	close(maxEpoch)
	highest := first.SessionEpoch
	for epoch := range maxEpoch {
		if epoch > highest {
			highest = epoch
		}
	}
	if highest <= first.SessionEpoch {
		t.Fatalf("highest epoch %d did not advance past %d", highest, first.SessionEpoch)
	}
}
