// Package ownersession tracks active single-Controller node sessions and
// assigns monotonically increasing per-node session_epoch values backed by
// PostgreSQL.
package ownersession

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/connectionowner"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/transportclient"
)

const gapReconciliationConcurrency = 32

var (
	// ErrNotOwner reports that the local session term was superseded or closed.
	ErrNotOwner = errors.New("ownersession: session superseded or closed")

	errConnectionPublicationPending = errors.New("ownersession: connection publication is still pending after transport event gap")
)

// Session identifies one active per-node connection epoch.
type Session struct {
	ConnectionID [16]byte
	SessionEpoch uint64
}

// SessionOpener allocates a monotonic session_epoch for a newly accepted
// mutation-capable Agent session.
type SessionOpener interface {
	OpenSession(ctx context.Context, nodeID, connectionID [16]byte, endpointID [32]byte) (Session, error)
}

// SessionCloser ends the session of exactly one (connectionID, epoch) term.
type SessionCloser interface {
	CloseSession(ctx context.Context, nodeID [16]byte, connectionID [16]byte, epoch uint64) error
}

// EventStream is the transport event source that ends sessions on disconnect.
type EventStream interface {
	RunWatch(ctx context.Context, cursors transportclient.CursorStore, handler transportclient.EventHandler) error
}

type nodeSession struct {
	term       *connectionowner.Term
	endpointID [32]byte
	openedAt   time.Time
}

type nodeLock struct {
	mu   sync.Mutex
	refs int
}

// Manager tracks live per-node sessions for the single active Controller.
type Manager struct {
	backend          database.Backend
	publicationGrace time.Duration
	now              func() time.Time
	logger           *slog.Logger

	mu        sync.Mutex
	sessions  map[[16]byte]*nodeSession
	nodeLocks map[[16]byte]*nodeLock
}

func NewManagerBackend(backend database.Backend, logger *slog.Logger) (*Manager, error) {
	if backend == nil {
		return nil, errors.New("ownersession: backend is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		backend:          backend,
		publicationGrace: 10 * time.Second,
		now:              time.Now,
		logger:           logger,
		sessions:         make(map[[16]byte]*nodeSession),
		nodeLocks:        make(map[[16]byte]*nodeLock),
	}, nil
}

func (m *Manager) lockNode(nodeID [16]byte) func() {
	m.mu.Lock()
	entry, ok := m.nodeLocks[nodeID]
	if !ok {
		entry = &nodeLock{}
		m.nodeLocks[nodeID] = entry
	}
	entry.refs++
	m.mu.Unlock()
	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		m.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			if _, active := m.sessions[nodeID]; !active {
				delete(m.nodeLocks, nodeID)
			}
		}
		m.mu.Unlock()
	}
}

// OpenSession advances the node's monotonic session_epoch in PostgreSQL and
// records the active session for disconnect and audit matching.
func (m *Manager) OpenSession(ctx context.Context, nodeID, connectionID [16]byte, endpointID [32]byte) (Session, error) {
	unlock := m.lockNode(nodeID)
	defer unlock()
	term, err := connectionowner.AcquireBackend(ctx, m.backend, nodeID, connectionID)
	if err != nil {
		return Session{}, fmt.Errorf("ownersession: acquire session epoch: %w", err)
	}
	session := &nodeSession{
		term:       term,
		endpointID: endpointID,
		openedAt:   m.now(),
	}
	m.mu.Lock()
	m.sessions[nodeID] = session
	m.mu.Unlock()
	return Session{
		ConnectionID: connectionID,
		SessionEpoch: uint64(term.Epoch()),
	}, nil
}

// CloseSession ends the session of exactly one (connectionID, epoch) term.
func (m *Manager) CloseSession(ctx context.Context, nodeID [16]byte, connectionID [16]byte, epoch uint64) error {
	if epoch == 0 || epoch > math.MaxInt64 {
		return nil
	}
	unlock := m.lockNode(nodeID)
	defer unlock()
	m.mu.Lock()
	session, ok := m.sessions[nodeID]
	if ok {
		if session.term.ConnectionID() != connectionID || uint64(session.term.Epoch()) != epoch {
			m.mu.Unlock()
			return nil
		}
		delete(m.sessions, nodeID)
	}
	m.mu.Unlock()
	// Controller restarts clear m.sessions while pre-restart transportd
	// sessions remain active in PostgreSQL; ReleaseTermBackend atomically
	// deactivates only the exact matching (nodeID, connectionID, epoch).
	if err := connectionowner.ReleaseTermBackend(ctx, m.backend, nodeID, connectionID, int64(epoch)); err != nil && !errors.Is(err, connectionowner.ErrNotOwner) {
		return fmt.Errorf("ownersession: release node session: %w", err)
	}
	return nil
}

// OwnsTerm reports whether this process recognizes the exact live session term.
func (m *Manager) OwnsTerm(nodeID, connectionID [16]byte, epoch uint64) bool {
	if m == nil || epoch == 0 {
		return false
	}
	unlock := m.lockNode(nodeID)
	defer unlock()
	m.mu.Lock()
	session, ok := m.sessions[nodeID]
	m.mu.Unlock()
	return ok && session.term.ConnectionID() == connectionID && uint64(session.term.Epoch()) == epoch
}

func fixed16(value []byte) ([16]byte, error) {
	if len(value) != 16 {
		return [16]byte{}, errors.New("ownersession: value must be 16 bytes")
	}
	var fixed [16]byte
	copy(fixed[:], value)
	return fixed, nil
}
