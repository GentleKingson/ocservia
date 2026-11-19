package ownersession

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/transportclient"
)

// WatchTransport consumes the transport event stream so the disconnect,
// replacement, or revoke-driven close of the exact connection behind a term
// ends that term immediately instead of renewing a session whose connection
// is gone.
func (m *Manager) WatchTransport(ctx context.Context, events EventStream) error {
	if events == nil {
		return errors.New("ownersession: transport event stream is required")
	}
	cursor := &memoryCursor{}
	return events.RunWatch(ctx, cursor, transportEventHandler{manager: m, cursor: cursor})
}

// handleTransportEvent ends the owner term named by a disconnect event.
// Events for unfenced connections carry no term identity and match no
// session.
func (m *Manager) handleTransportEvent(ctx context.Context, event *transportv1.TransportEvent) error {
	if event.GetType() != transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_DISCONNECTED {
		return nil
	}
	nodeID, err := fixed16(event.GetNodeId())
	if err != nil {
		return nil
	}
	connectionID, err := fixed16(event.GetConnectionId())
	if err != nil {
		return nil
	}
	return m.CloseSession(ctx, nodeID, connectionID, int64(event.GetOwnerEpoch()))
}

type transportEventHandler struct {
	manager *Manager
	cursor  *memoryCursor
}

var _ transportclient.OwnerGapReconciler = transportEventHandler{}

func (h transportEventHandler) Ingest(ctx context.Context, event *transportv1.TransportEvent) error {
	if err := h.manager.handleTransportEvent(ctx, event); err != nil {
		return err
	}
	h.cursor.set(event.GetEventId())
	return nil
}

// ReconcileOwnerEventGap compares every exact local owner term with
// transportd's current connection inventory. The snapshot is immutable and
// CloseSession exact-matches it, so a concurrent replacement cannot be closed
// by a stale reconciliation decision. The old cursor is cleared only after the
// complete inventory pass succeeds.
func (h transportEventHandler) ReconcileOwnerEventGap(ctx context.Context, readConnection transportclient.NodeConnectionReader) error {
	if readConnection == nil {
		return errors.New("ownersession: node connection reader is required")
	}
	snapshots := h.manager.snapshotSessionTerms()
	reconciliationTime := h.manager.now()
	publicationPending := false
	for _, result := range readSessionInventory(ctx, snapshots, readConnection) {
		if result.err != nil {
			return fmt.Errorf("ownersession: inspect node %x after transport event gap: %w", result.snapshot.nodeID, result.err)
		}
		if sessionInventoryMatches(result.snapshot, result.connection) {
			continue
		}
		// OpenSession records the term after registering its fence, then returns
		// the handshake response that lets transportd publish NodeConnection.
		// A gap recovery racing that bounded publication window must retry rather
		// than misclassify the new authoritative term as disconnected.
		if reconciliationTime.Before(result.snapshot.openedAt.Add(h.manager.publicationGrace)) {
			publicationPending = true
			continue
		}
		if err := h.manager.CloseSession(ctx, result.snapshot.nodeID, result.snapshot.connectionID, result.snapshot.ownerEpoch); err != nil {
			return fmt.Errorf("ownersession: close disconnected node %x after transport event gap: %w", result.snapshot.nodeID, err)
		}
	}
	if publicationPending {
		return errConnectionPublicationPending
	}
	h.cursor.set(nil)
	return nil
}

type sessionTermSnapshot struct {
	nodeID       [16]byte
	endpointID   [32]byte
	connectionID [16]byte
	ownerEpoch   int64
	openedAt     time.Time
}

type sessionInventoryResult struct {
	snapshot   sessionTermSnapshot
	connection *transportv1.NodeConnection
	err        error
}

func readSessionInventory(ctx context.Context, snapshots []sessionTermSnapshot, readConnection transportclient.NodeConnectionReader) []sessionInventoryResult {
	results := make([]sessionInventoryResult, len(snapshots))
	if len(snapshots) == 0 {
		return results
	}
	workerCount := min(gapReconciliationConcurrency, len(snapshots))
	jobs := make(chan int)
	var workers sync.WaitGroup
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			for index := range jobs {
				results[index].snapshot = snapshots[index]
				if err := ctx.Err(); err != nil {
					results[index].err = err
					continue
				}
				nodeID := snapshots[index].nodeID
				results[index].connection, results[index].err = readConnection(ctx, nodeID[:])
			}
		}()
	}
	for index := range snapshots {
		jobs <- index
	}
	close(jobs)
	workers.Wait()
	return results
}

func sessionInventoryMatches(snapshot sessionTermSnapshot, connection *transportv1.NodeConnection) bool {
	return connection != nil &&
		snapshot.ownerEpoch > 0 &&
		connection.GetOwnerEpoch() == uint64(snapshot.ownerEpoch) &&
		bytes.Equal(connection.GetNodeId(), snapshot.nodeID[:]) &&
		bytes.Equal(connection.GetEndpointId(), snapshot.endpointID[:])
}

// snapshotSessionTerms copies each immutable PostgreSQL term and its fixed
// publication start while holding only the map mutex. Callers may perform
// transport and database I/O after the mutex is released without reversing
// the node-lock -> map-lock order used by the rest of Manager.
func (m *Manager) snapshotSessionTerms() []sessionTermSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	terms := make([]sessionTermSnapshot, 0, len(m.sessions))
	for _, session := range m.sessions {
		terms = append(terms, sessionTermSnapshot{
			nodeID:       session.term.NodeID(),
			endpointID:   session.endpointID,
			connectionID: session.term.ConnectionID(),
			ownerEpoch:   session.term.Epoch(),
			openedAt:     session.openedAt,
		})
	}
	slices.SortFunc(terms, func(left, right sessionTermSnapshot) int {
		return bytes.Compare(left.nodeID[:], right.nodeID[:])
	})
	return terms
}

// memoryCursor resumes the watch after the last ingested event. Owner-term
// closing is exact-term matched and therefore idempotent, so a replayed
// window after a restart is harmless.
type memoryCursor struct {
	mu   sync.Mutex
	last []byte
}

func (c *memoryCursor) LastEventID(context.Context) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.Clone(c.last), nil
}

func (c *memoryCursor) set(eventID []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last = bytes.Clone(eventID)
}
