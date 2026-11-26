package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/audit/auditstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"github.com/google/uuid"
)

type memAuditCheckpoint struct {
	eventID   uuid.UUID
	eventHash []byte
	signature []byte
}

type memAuditBackend struct {
	database.Backend
	mu             sync.Mutex
	clock          time.Time
	events         []auditstore.EventRow
	checkpoints    []memAuditCheckpoint
	fullScanCalls  int
	deltaScanCalls int
}

func (b *memAuditBackend) Begin(_ context.Context, _ database.Isolation) (database.Tx, error) {
	return &memAuditTx{backend: b}, nil
}

type memAuditTx struct {
	database.Tx
	backend *memAuditBackend
}

func (t *memAuditTx) Commit(context.Context) error   { return nil }
func (t *memAuditTx) Rollback(context.Context) error { return nil }
func (t *memAuditTx) AuditStore() auditstore.Store   { return &memAuditStore{backend: t.backend} }

type memAuditStore struct {
	auditstore.Store
	backend *memAuditBackend
}

func (s *memAuditStore) Lock(context.Context, uuid.UUID) error { return nil }

func (s *memAuditStore) Clock(context.Context) (time.Time, error) {
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	s.backend.clock = s.backend.clock.Add(time.Millisecond)
	return s.backend.clock, nil
}

func (s *memAuditStore) Previous(_ context.Context, workspaceID uuid.UUID) ([]byte, error) {
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	for i := len(s.backend.events) - 1; i >= 0; i-- {
		if s.backend.events[i].WorkspaceID == workspaceID {
			return bytes.Clone(s.backend.events[i].EventHash), nil
		}
	}
	return nil, database.ErrNotFound
}

func (s *memAuditStore) Append(_ context.Context, row auditstore.EventRow) error {
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	row.BeforeSummary = bytes.Clone(row.BeforeSummary)
	row.AfterSummary = bytes.Clone(row.AfterSummary)
	row.PreviousHash = bytes.Clone(row.PreviousHash)
	row.EventHash = bytes.Clone(row.EventHash)
	row.EventMAC = bytes.Clone(row.EventMAC)
	s.backend.events = append(s.backend.events, row)
	return nil
}

func (s *memAuditStore) LatestEvent(_ context.Context, workspaceID uuid.UUID) (uuid.UUID, []byte, error) {
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	for i := len(s.backend.events) - 1; i >= 0; i-- {
		if s.backend.events[i].WorkspaceID == workspaceID {
			ev := s.backend.events[i]
			return ev.ID, bytes.Clone(ev.EventHash), nil
		}
	}
	return uuid.Nil, nil, database.ErrNotFound
}

func (s *memAuditStore) LatestCheckpoint(_ context.Context, workspaceID uuid.UUID) (uuid.UUID, []byte, []byte, time.Time, int16, error) {
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	if len(s.backend.checkpoints) == 0 {
		return uuid.Nil, nil, nil, time.Time{}, 0, database.ErrNotFound
	}
	cp := s.backend.checkpoints[len(s.backend.checkpoints)-1]
	for _, ev := range s.backend.events {
		if ev.WorkspaceID == workspaceID && ev.ID == cp.eventID {
			return cp.eventID, bytes.Clone(cp.eventHash), bytes.Clone(cp.signature), ev.At, ev.AuthVersion, nil
		}
	}
	return uuid.Nil, nil, nil, time.Time{}, 0, database.ErrNotFound
}

func (s *memAuditStore) AddCheckpoint(_ context.Context, _, _, eventID uuid.UUID, eventHash, signature []byte) error {
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	s.backend.checkpoints = append(s.backend.checkpoints, memAuditCheckpoint{
		eventID:   eventID,
		eventHash: bytes.Clone(eventHash),
		signature: bytes.Clone(signature),
	})
	return nil
}

func (s *memAuditStore) Events(_ context.Context, workspaceID uuid.UUID, visit func(auditstore.EventRow) error) error {
	s.backend.mu.Lock()
	s.backend.fullScanCalls++
	var matched []auditstore.EventRow
	for _, ev := range s.backend.events {
		if ev.WorkspaceID == workspaceID {
			matched = append(matched, ev)
		}
	}
	s.backend.mu.Unlock()
	for _, ev := range matched {
		if err := visit(ev); err != nil {
			return err
		}
	}
	return nil
}

func (s *memAuditStore) EventsFrom(_ context.Context, workspaceID uuid.UUID, afterAt time.Time, afterID uuid.UUID, visit func(auditstore.EventRow) error) error {
	s.backend.mu.Lock()
	s.backend.deltaScanCalls++
	var matched []auditstore.EventRow
	for _, ev := range s.backend.events {
		if ev.WorkspaceID != workspaceID {
			continue
		}
		if ev.At.After(afterAt) || (ev.At.Equal(afterAt) && bytes.Compare(ev.ID[:], afterID[:]) >= 0) {
			matched = append(matched, ev)
		}
	}
	s.backend.mu.Unlock()
	for _, ev := range matched {
		if err := visit(ev); err != nil {
			return err
		}
	}
	return nil
}

func TestCheckpointIncrementalDeltaNoOpAndTamperDetection(t *testing.T) {
	ctx := t.Context()
	workspaceID := uuid.Must(uuid.NewV7())
	checkpointKey := sha256.Sum256([]byte("checkpoint-test-key"))
	backend := &memAuditBackend{clock: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
	manager := NewManager(backend, checkpointKey[:])

	appendEvents := func(count int) {
		for range count {
			err := database.Within(ctx, backend, database.ReadCommitted, func(tx database.Tx) error {
				return AppendChainTx(ctx, tx, ChainRecord{
					WorkspaceID:  workspaceID,
					ActorType:    "user",
					ActorID:      "operator-1",
					Action:       "node.update",
					ResourceType: "node",
					ResourceID:   uuid.Must(uuid.NewV7()),
					RequestID:    uuid.NewString(),
					Result:       "succeeded",
					Reason:       "routine update",
				})
			})
			testutil.NoError(t, err)
		}
	}

	// 1. Initial checkpoint falls back to full scan and anchors the chain.
	appendEvents(5)
	testutil.NoError(t, manager.Checkpoint(ctx, workspaceID))
	if backend.fullScanCalls != 1 || backend.deltaScanCalls != 0 || len(backend.checkpoints) != 1 {
		t.Fatalf("initial checkpoint scans full=%d delta=%d checkpoints=%d", backend.fullScanCalls, backend.deltaScanCalls, len(backend.checkpoints))
	}

	// 2. Repeated checkpoint with no new events is an O(1) no-op.
	testutil.NoError(t, manager.Checkpoint(ctx, workspaceID))
	if backend.fullScanCalls != 1 || backend.deltaScanCalls != 1 || len(backend.checkpoints) != 1 {
		t.Fatalf("no-op checkpoint triggered scan/write: full=%d delta=%d checkpoints=%d", backend.fullScanCalls, backend.deltaScanCalls, len(backend.checkpoints))
	}

	// 3. Appending 3 events validates the anchor and delta via EventsFrom.
	appendEvents(3)
	testutil.NoError(t, manager.Checkpoint(ctx, workspaceID))
	if backend.fullScanCalls != 1 || backend.deltaScanCalls != 2 || len(backend.checkpoints) != 2 {
		t.Fatalf("delta checkpoint scans full=%d delta=%d checkpoints=%d", backend.fullScanCalls, backend.deltaScanCalls, len(backend.checkpoints))
	}
	if backend.checkpoints[1].eventID != backend.events[7].ID {
		t.Fatalf("delta checkpoint anchored %s, want %s", backend.checkpoints[1].eventID, backend.events[7].ID)
	}

	// Startup verifies the checkpoint and appended suffix without a genesis scan.
	appendEvents(1)
	testutil.NoError(t, manager.ensureWorkspaceAuthenticity(ctx, workspaceID))
	if backend.fullScanCalls != 1 || backend.deltaScanCalls != 3 {
		t.Fatalf("startup scans full=%d delta=%d", backend.fullScanCalls, backend.deltaScanCalls)
	}

	// 4. Tampering with a newly appended event causes delta verification to reject.
	appendEvents(1)
	backend.events[len(backend.events)-1].Reason = "tampered reason"
	err := manager.Checkpoint(ctx, workspaceID)
	if err == nil {
		t.Fatal("tampered audit delta was checkpointed")
	}
	if !errors.Is(err, err) || len(backend.checkpoints) != 2 {
		t.Fatalf("tampered checkpoint modified checkpoint count: %d (err=%v)", len(backend.checkpoints), err)
	}
	if backend.fullScanCalls != 1 || backend.deltaScanCalls != 4 {
		t.Fatalf("tampered delta did not short-circuit on delta scan: full=%d delta=%d", backend.fullScanCalls, backend.deltaScanCalls)
	}
}
