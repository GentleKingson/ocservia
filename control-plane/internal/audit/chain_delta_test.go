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

type memAuditEvent struct {
	id              uuid.UUID
	workspaceID     uuid.UUID
	at              time.Time
	actorType       string
	actorID         string
	sessionID       *uuid.UUID
	action          string
	resourceType    string
	resourceID      *uuid.UUID
	nodeID          *uuid.UUID
	requestID       string
	traceID         string
	commandID       *uuid.UUID
	approvalID      *uuid.UUID
	result          string
	reason          string
	before          []byte
	after           []byte
	errorType       string
	previousHash    []byte
	eventHash       []byte
	authVersion     int16
	eventKeyID      *string
	eventMAC        []byte
	compactedAt     *time.Time
	compactionKeyID *string
	compactionMAC   []byte
}

type memAuditCheckpoint struct {
	eventID   uuid.UUID
	eventHash []byte
	signature []byte
}

type memAuditBackend struct {
	database.Backend
	mu             sync.Mutex
	clock          time.Time
	events         []memAuditEvent
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
		if s.backend.events[i].workspaceID == workspaceID {
			return bytes.Clone(s.backend.events[i].eventHash), nil
		}
	}
	return nil, database.ErrNotFound
}

func (s *memAuditStore) Append(_ context.Context, args []any) error {
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	ev := memAuditEvent{
		id:           args[0].(uuid.UUID),
		workspaceID:  args[1].(uuid.UUID),
		at:           args[2].(time.Time),
		actorType:    args[3].(string),
		actorID:      args[4].(string),
		action:       args[6].(string),
		resourceType: args[7].(string),
		requestID:    args[10].(string),
		result:       args[14].(string),
		previousHash: bytes.Clone(args[19].([]byte)),
		eventHash:    bytes.Clone(args[20].([]byte)),
		authVersion:  args[21].(int16),
		eventMAC:     bytes.Clone(args[23].([]byte)),
	}
	if v, ok := args[16].([]byte); ok {
		ev.before = bytes.Clone(v)
	}
	if v, ok := args[17].([]byte); ok {
		ev.after = bytes.Clone(v)
	}
	if v, ok := args[5].(*uuid.UUID); ok {
		ev.sessionID = v
	}
	if v, ok := args[8].(uuid.UUID); ok {
		id := v
		ev.resourceID = &id
	} else if v, ok := args[8].(*uuid.UUID); ok {
		ev.resourceID = v
	}
	if v, ok := args[9].(*uuid.UUID); ok {
		ev.nodeID = v
	}
	if v, ok := args[11].(string); ok {
		ev.traceID = v
	}
	if v, ok := args[12].(*uuid.UUID); ok {
		ev.commandID = v
	}
	if v, ok := args[13].(*uuid.UUID); ok {
		ev.approvalID = v
	}
	if v, ok := args[15].(string); ok {
		ev.reason = v
	} else if v, ok := args[15].(*string); ok && v != nil {
		ev.reason = *v
	}
	if v, ok := args[18].(string); ok {
		ev.errorType = v
	} else if v, ok := args[18].(*string); ok && v != nil {
		ev.errorType = *v
	}
	if v, ok := args[22].(string); ok {
		keyID := v
		ev.eventKeyID = &keyID
	}
	s.backend.events = append(s.backend.events, ev)
	return nil
}

func (s *memAuditStore) LatestEvent(_ context.Context, workspaceID uuid.UUID) (uuid.UUID, []byte, error) {
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	for i := len(s.backend.events) - 1; i >= 0; i-- {
		if s.backend.events[i].workspaceID == workspaceID {
			ev := s.backend.events[i]
			return ev.id, bytes.Clone(ev.eventHash), nil
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
		if ev.workspaceID == workspaceID && ev.id == cp.eventID {
			return cp.eventID, bytes.Clone(cp.eventHash), bytes.Clone(cp.signature), ev.at, ev.authVersion, nil
		}
	}
	return uuid.Nil, nil, nil, time.Time{}, 0, database.ErrNotFound
}

func (s *memAuditStore) CheckpointProof(_ context.Context, _, eventID uuid.UUID) ([]byte, []byte, error) {
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	for _, cp := range s.backend.checkpoints {
		if cp.eventID == eventID {
			return bytes.Clone(cp.eventHash), bytes.Clone(cp.signature), nil
		}
	}
	return nil, nil, database.ErrNotFound
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

func (s *memAuditStore) Events(_ context.Context, workspaceID uuid.UUID) (database.Rows, error) {
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	s.backend.fullScanCalls++
	var matched []memAuditEvent
	for _, ev := range s.backend.events {
		if ev.workspaceID == workspaceID {
			matched = append(matched, ev)
		}
	}
	return &memEventRows{items: matched, idx: -1}, nil
}

func (s *memAuditStore) EventsFrom(_ context.Context, workspaceID uuid.UUID, afterAt time.Time, afterID uuid.UUID) (database.Rows, error) {
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	s.backend.deltaScanCalls++
	var matched []memAuditEvent
	for _, ev := range s.backend.events {
		if ev.workspaceID != workspaceID {
			continue
		}
		if ev.at.After(afterAt) || (ev.at.Equal(afterAt) && bytes.Compare(ev.id[:], afterID[:]) >= 0) {
			matched = append(matched, ev)
		}
	}
	return &memEventRows{items: matched, idx: -1}, nil
}

type memEventRows struct {
	items []memAuditEvent
	idx   int
}

func (r *memEventRows) Next() bool { r.idx++; return r.idx < len(r.items) }
func (r *memEventRows) Err() error { return nil }
func (r *memEventRows) Close()     {}
func (r *memEventRows) Scan(dest ...any) error {
	ev := r.items[r.idx]
	*(dest[0].(*uuid.UUID)) = ev.id
	*(dest[1].(*time.Time)) = ev.at
	*(dest[2].(*string)) = ev.actorType
	*(dest[3].(*string)) = ev.actorID
	*(dest[4].(*string)) = ev.action
	*(dest[5].(*string)) = ev.resourceType
	*(dest[6].(**uuid.UUID)) = ev.resourceID
	*(dest[7].(*string)) = ev.requestID
	*(dest[8].(*string)) = ev.traceID
	*(dest[9].(*string)) = ev.result
	*(dest[10].(*string)) = ev.reason
	*(dest[11].(**uuid.UUID)) = ev.sessionID
	*(dest[12].(**uuid.UUID)) = ev.nodeID
	*(dest[13].(**uuid.UUID)) = ev.commandID
	*(dest[14].(**uuid.UUID)) = ev.approvalID
	*(dest[15].(*[]byte)) = bytes.Clone(ev.before)
	*(dest[16].(*[]byte)) = bytes.Clone(ev.after)
	*(dest[17].(*string)) = ev.errorType
	*(dest[18].(*[]byte)) = bytes.Clone(ev.previousHash)
	*(dest[19].(*[]byte)) = bytes.Clone(ev.eventHash)
	*(dest[20].(*int16)) = ev.authVersion
	*(dest[21].(**string)) = ev.eventKeyID
	*(dest[22].(*[]byte)) = bytes.Clone(ev.eventMAC)
	*(dest[23].(**time.Time)) = ev.compactedAt
	*(dest[24].(**string)) = ev.compactionKeyID
	*(dest[25].(*[]byte)) = bytes.Clone(ev.compactionMAC)
	return nil
}

func TestCheckpointIncrementalDeltaNoOpAndTamperDetection(t *testing.T) {
	ctx := t.Context()
	workspaceID := uuid.Must(uuid.NewV7())
	checkpointKey := sha256.Sum256([]byte("checkpoint-test-key"))
	backend := &memAuditBackend{clock: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
	manager := NewBackendManager(backend, checkpointKey[:])

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
	if backend.checkpoints[1].eventID != backend.events[7].id {
		t.Fatalf("delta checkpoint anchored %s, want %s", backend.checkpoints[1].eventID, backend.events[7].id)
	}

	// Startup verifies the checkpoint and appended suffix without a genesis scan.
	appendEvents(1)
	testutil.NoError(t, manager.ensureWorkspaceAuthenticity(ctx, workspaceID))
	if backend.fullScanCalls != 1 || backend.deltaScanCalls != 3 {
		t.Fatalf("startup scans full=%d delta=%d", backend.fullScanCalls, backend.deltaScanCalls)
	}

	// 4. Tampering with a newly appended event causes delta verification to reject.
	appendEvents(1)
	backend.events[len(backend.events)-1].reason = "tampered reason"
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
