// Package ownersession implements the Controller side of the connection
// owner fencing runtime: it takes the per-node ownership lease from the
// PostgreSQL authority, signs the frozen ConnectionFenceV2 and
// FenceBindingV2 contracts, pushes fences to transportd, and renews leases
// so a stale owner stops issuing proofs as soon as its term is taken over.
package ownersession

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/connectionowner"
	"github.com/GentleKingson/ocservia/control-plane/internal/transportclient"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// FencingCapability is the session capability an agent advertises to accept
// connection-owner fences. It is negotiated like a protocol capability, not a
// per-node business capability: the Controller attaches a fence only when the
// agent understands it, so fences never reach an endpoint that would reject
// the carrying envelope.
const FencingCapability = "ocserv.fencing.v2"

// DefaultLeaseTTL keeps the takeover window inside the G6
// connection_owner_takeover_seconds budget while leaving dispatch enough time
// to complete under a valid lease.
const DefaultLeaseTTL = 30 * time.Second

// stateUpdateOperationDomain prefixes the canonical operation identity of one
// node trust update. transportd derives the identity from the same encoding,
// so a fence binding covers exactly the update on the wire.
var stateUpdateOperationDomain = []byte("ocservia/state-update-operation/v1\x00")

var (
	// ErrNotOwner reports that the local ownership term is stale, expired, or
	// ended; the caller must stop issuing fenced proofs and fail closed. It
	// is never the unfenced compatibility path.
	ErrNotOwner = errors.New("ownersession: connection ownership lost")

	// ErrNoFence reports that the node never opened an owner session in this
	// process. Only this error selects the unfenced compatibility path.
	ErrNoFence = errors.New("ownersession: no owner fence for node")

	// ErrFenceUnavailable reports that the node needs fencing but this
	// process could not register its fence with transportd. Dispatch must
	// fail closed; the node is not on the unfenced compatibility path.
	ErrFenceUnavailable = errors.New("ownersession: owner fence registration is unavailable")

	// ErrCapabilityNotFenced reports that the requested capability is not part
	// of the fence's capability set, so no binding may be signed for it.
	ErrCapabilityNotFenced = errors.New("ownersession: capability is not fenced")
)

// FenceRegistrar pushes a Controller-signed fence to transportd, which
// verifies it, records it as the node's active fence, and retires a mutation
// session when a strictly higher epoch arrives.
type FenceRegistrar interface {
	RegisterOwnerFence(ctx context.Context, fence *agentv1.ConnectionFenceV2) error
}

// FenceReader reads the fence transportd registered for a node. Controller
// processes that do not hold the node lease use it to issue bindings for the
// current term instead of guessing one.
type FenceReader interface {
	GetOwnerFence(ctx context.Context, nodeID []byte) (*agentv1.ConnectionFenceV2, error)
}

// SessionOpener opens an owner session for a mutation-capable agent session.
type SessionOpener interface {
	OpenSession(ctx context.Context, nodeID [16]byte, endpointID [32]byte, authorizationRevision uint64, capabilities []string) (*agentv1.ConnectionFenceV2, error)
}

// SessionCloser ends the owner session of exactly one term. Handshake paths
// that opened a session but failed before committing the authorization call
// it so the lease does not outlive a session that was never granted.
type SessionCloser interface {
	CloseSession(ctx context.Context, nodeID [16]byte, connectionID [16]byte, epoch int64) error
}

// EventStream is the transport event source that ends owner terms when their
// connection goes away. *transportclient.Client satisfies it.
type EventStream interface {
	RunWatch(ctx context.Context, cursors transportclient.CursorStore, handler transportclient.EventHandler) error
}

// OperationBinder signs one operation binding for the node's current fence.
type OperationBinder interface {
	BindOperation(ctx context.Context, nodeID [16]byte, kind agentv1.FenceOperationKind, operationID [16]byte, capability string) (*agentv1.ConnectionFenceV2, *agentv1.FenceBindingV2, error)
}

type nodeSession struct {
	term                  *connectionowner.Term
	fenceID               [16]byte
	nodeID                [16]byte
	endpointID            [32]byte
	authorizationRevision uint64
	capabilities          []string
	leaseUntil            time.Time
	fence                 *agentv1.ConnectionFenceV2
	registrationPending   bool
	registrationFailures  int
	nextRegistration      time.Time
	lost                  bool
}

// endedReason records why a node has no live session although it is not on
// the never-fenced compatibility path. Ended nodes fail closed instead of
// degrading to unfenced carriers.
type endedReason uint8

const (
	endedOwnershipLost endedReason = iota + 1
	endedRegistrationUnavailable
)

// Manager owns the per-node connection leases of one Controller process. It
// runs inside the worker-role process that serves session authorization and
// command dispatch; every deadline it stores is the exact value returned by
// the PostgreSQL authority, never a local reconstruction.
//
// Locking: mu guards the session and ended maps; the per-node mutex returned
// by lockNode serializes every lease, registration, and binding action of
// one node, including all mutation of a nodeSession. PostgreSQL and
// transportd RPCs run under the per-node lock only, so one slow node never
// blocks another node's renewal or dispatch.
type Manager struct {
	pool              *pgxpool.Pool
	signer            *commandauth.Signer
	registrar         FenceRegistrar
	identity          connectionowner.Identity
	leaseTTL          time.Duration
	bindingTTL        time.Duration
	fenceGrace        time.Duration
	renewAhead        time.Duration
	interval          time.Duration
	registrationEvery time.Duration
	now               func() time.Time
	logger            *slog.Logger

	mu        sync.Mutex
	sessions  map[[16]byte]*nodeSession
	ended     map[[16]byte]endedReason
	nodeLocks map[[16]byte]*sync.Mutex
}

// NewManager mints the process owner identity and prepares per-node lease
// management. The signer must be the production Controller signing key; the
// registrar is the transport client of this deployment unit.
func NewManager(pool *pgxpool.Pool, signer *commandauth.Signer, registrar FenceRegistrar, leaseTTL time.Duration, logger *slog.Logger) (*Manager, error) {
	if signer == nil || registrar == nil || pool == nil {
		return nil, errors.New("ownersession: pool, signer, and registrar are required")
	}
	if leaseTTL <= 0 {
		return nil, errors.New("ownersession: lease TTL must be positive")
	}
	instanceID, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	return &Manager{
		pool:              pool,
		signer:            signer,
		registrar:         registrar,
		identity:          connectionowner.Identity{InstanceID: instanceID, Incarnation: time.Now().UnixNano()},
		leaseTTL:          leaseTTL,
		bindingTTL:        5 * time.Minute,
		fenceGrace:        5 * time.Minute,
		renewAhead:        leaseTTL / 2,
		interval:          time.Second,
		registrationEvery: 10 * time.Second,
		now:               time.Now,
		logger:            logger,
		sessions:          make(map[[16]byte]*nodeSession),
		ended:             make(map[[16]byte]endedReason),
		nodeLocks:         make(map[[16]byte]*sync.Mutex),
	}, nil
}

// lockNode returns a release function for the per-node mutex. Every action
// that takes or mutates a node's session runs under it.
func (m *Manager) lockNode(nodeID [16]byte) func() {
	m.mu.Lock()
	lock, ok := m.nodeLocks[nodeID]
	if !ok {
		lock = &sync.Mutex{}
		m.nodeLocks[nodeID] = lock
	}
	m.mu.Unlock()
	lock.Lock()
	return lock.Unlock
}

// Run renews unexpired sessions, retries fence registrations, and ends terms
// whose registration stayed unavailable for a full lease TTL, until the
// context is cancelled. Losing a renewal marks the session stale so later
// bindings fail closed.
func (m *Manager) Run(ctx context.Context) error {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			m.maintain(ctx)
		}
	}
}

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

// CloseSession ends the session of exactly one owner term. The connection
// identity and epoch must match the live term, so a late disconnect for an
// already-replaced term ends nothing. The lease is released at PostgreSQL
// time so a successor takes over without waiting out the TTL.
func (m *Manager) CloseSession(ctx context.Context, nodeID [16]byte, connectionID [16]byte, epoch int64) error {
	unlock := m.lockNode(nodeID)
	defer unlock()
	m.mu.Lock()
	session, ok := m.sessions[nodeID]
	if !ok || session.term.ConnectionID() != connectionID || session.term.Epoch() != epoch {
		m.mu.Unlock()
		return nil
	}
	delete(m.sessions, nodeID)
	m.ended[nodeID] = endedOwnershipLost
	m.mu.Unlock()
	m.logger.WarnContext(ctx, "connection owner session ended", "node_id", fmt.Sprintf("%x", nodeID), "epoch", epoch, "alert_kind", "connection_owner.ended")
	if err := session.term.Release(ctx, m.pool); err != nil && !errors.Is(err, connectionowner.ErrNotOwner) {
		return fmt.Errorf("ownersession: release node lease: %w", err)
	}
	return nil
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

func (h transportEventHandler) Ingest(ctx context.Context, event *transportv1.TransportEvent) error {
	if err := h.manager.handleTransportEvent(ctx, event); err != nil {
		return err
	}
	h.cursor.set(event.GetEventId())
	return nil
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

// OpenSession takes the ownership lease for a newly accepted mutation-capable
// agent session and returns the signed fence for the handshake response. A
// different unexpired owner blocks the session: the caller must downgrade to
// the read-only compatibility path instead of granting mutations. When the
// initial fence registration fails, the lease is released and the node is
// recorded as fence-unavailable, so later bindings fail closed instead of
// degrading to unfenced carriers.
func (m *Manager) OpenSession(ctx context.Context, nodeID [16]byte, endpointID [32]byte, authorizationRevision uint64, capabilities []string) (*agentv1.ConnectionFenceV2, error) {
	if authorizationRevision == 0 {
		return nil, errors.New("ownersession: authorization revision must be nonzero")
	}
	connectionID, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	fenceID, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	unlock := m.lockNode(nodeID)
	defer unlock()
	term, err := connectionowner.Acquire(ctx, m.pool, nodeID, m.identity, connectionID, m.leaseTTL)
	if errors.Is(err, connectionowner.ErrLeaseHeld) {
		return nil, ErrNotOwner
	}
	if err != nil {
		return nil, fmt.Errorf("ownersession: acquire node lease: %w", err)
	}
	session := &nodeSession{
		term:                  term,
		fenceID:               fenceID,
		nodeID:                nodeID,
		endpointID:            endpointID,
		authorizationRevision: authorizationRevision,
		capabilities:          fenceCapabilities(capabilities),
		leaseUntil:            term.LeaseUntil(),
		nextRegistration:      m.now().Add(m.registrationEvery),
	}
	fence, err := m.signFence(session)
	if err != nil {
		m.releaseAcquiredTerm(ctx, term)
		return nil, err
	}
	// The initial registration must reach transportd before the session is
	// granted: registering a higher epoch is what retires a superseded
	// owner's live session without waiting for lease expiry.
	if err := m.registrar.RegisterOwnerFence(ctx, fence); err != nil {
		m.releaseAcquiredTerm(ctx, term)
		m.mu.Lock()
		m.ended[nodeID] = endedRegistrationUnavailable
		m.mu.Unlock()
		return nil, fmt.Errorf("ownersession: register owner fence: %w", err)
	}
	session.fence = fence
	m.mu.Lock()
	// Per-node serialization makes the write-back the only writer; the epoch
	// guard keeps a stale lower epoch from replacing a higher one.
	if current, ok := m.sessions[nodeID]; !ok || session.term.Epoch() >= current.term.Epoch() {
		m.sessions[nodeID] = session
	}
	delete(m.ended, nodeID)
	m.mu.Unlock()
	return fence, nil
}

// releaseAcquiredTerm best-effort releases a lease whose session was never
// registered, so a failed open does not hold the node for a full TTL.
func (m *Manager) releaseAcquiredTerm(ctx context.Context, term *connectionowner.Term) {
	if err := term.Release(ctx, m.pool); err != nil && !errors.Is(err, connectionowner.ErrNotOwner) {
		m.logger.ErrorContext(ctx, "release unregistered node lease", "node_id", fmt.Sprintf("%x", term.NodeID()), "error", err)
	}
}

// BindOperation signs one operation binding against the node's live session.
// It renews the lease first when the exact deadline is close and fails
// closed once the term was taken over, ended, or unregistered. ErrNoFence —
// the unfenced compatibility path — is returned only when this process never
// opened a session for the node.
func (m *Manager) BindOperation(ctx context.Context, nodeID [16]byte, kind agentv1.FenceOperationKind, operationID [16]byte, capability string) (*agentv1.ConnectionFenceV2, *agentv1.FenceBindingV2, error) {
	unlock := m.lockNode(nodeID)
	defer unlock()
	m.mu.Lock()
	session, ok := m.sessions[nodeID]
	if ok && session.lost {
		ok = false
		m.ended[nodeID] = endedOwnershipLost
	}
	reason, ended := m.ended[nodeID]
	m.mu.Unlock()
	if !ok {
		switch {
		case ended && reason == endedRegistrationUnavailable:
			return nil, nil, ErrFenceUnavailable
		case ended:
			return nil, nil, ErrNotOwner
		default:
			return nil, nil, ErrNoFence
		}
	}
	if err := m.refresh(ctx, session); err != nil {
		return nil, nil, err
	}
	// A renewal re-signed the fence; push it to transportd eagerly so
	// its recorded lease stays current. A failure only leaves the
	// registration pending for the maintain loop: the binding still
	// carries the fresh fence, and transportd accepts the refreshed
	// term because the fence identity is unchanged.
	if session.registrationPending && m.registrar.RegisterOwnerFence(ctx, session.fence) == nil {
		session.registrationPending = false
	}
	term := bindingTerm{
		fence:                 session.fence,
		fenceID:               session.fenceID,
		nodeID:                session.nodeID,
		endpointID:            session.endpointID,
		identity:              session.term.Identity(),
		epoch:                 session.term.Epoch(),
		connectionID:          session.term.ConnectionID(),
		authorizationRevision: session.authorizationRevision,
		capabilities:          session.capabilities,
	}
	return m.issueBinding(&term, kind, operationID, capability)
}

// maintain renews sessions approaching their deadline and retries pending
// fence registrations, one node at a time.
func (m *Manager) maintain(ctx context.Context) {
	m.mu.Lock()
	nodes := make([][16]byte, 0, len(m.sessions))
	for nodeID := range m.sessions {
		nodes = append(nodes, nodeID)
	}
	m.mu.Unlock()
	for _, nodeID := range nodes {
		unlock := m.lockNode(nodeID)
		m.mu.Lock()
		session, ok := m.sessions[nodeID]
		m.mu.Unlock()
		if ok {
			m.maintainNode(ctx, nodeID, session)
		}
		unlock()
	}
}

func (m *Manager) maintainNode(ctx context.Context, nodeID [16]byte, session *nodeSession) {
	if session.lost {
		m.endSession(ctx, nodeID, session, endedOwnershipLost)
		return
	}
	if err := m.refresh(ctx, session); err != nil {
		if errors.Is(err, ErrNotOwner) {
			m.logger.WarnContext(ctx, "connection ownership lost", "node_id", fmt.Sprintf("%x", nodeID), "epoch", session.term.Epoch(), "alert_kind", "connection_owner.lost")
			m.endSession(ctx, nodeID, session, endedOwnershipLost)
			return
		}
		m.logger.ErrorContext(ctx, "renew connection ownership", "node_id", fmt.Sprintf("%x", nodeID), "error", err)
	}
	now := m.now()
	if !session.registrationPending && now.Before(session.nextRegistration) {
		return
	}
	// The heartbeat re-registers the current fence periodically: it heals a
	// registration lost to a transportd restart and bounds how long an
	// unreachable transportd can keep a dead owner renewing its lease.
	if err := m.registrar.RegisterOwnerFence(ctx, session.fence); err != nil {
		session.registrationFailures++
		session.nextRegistration = now.Add(m.registrationEvery)
		m.logger.WarnContext(ctx, "retry owner fence registration", "node_id", fmt.Sprintf("%x", nodeID), "failures", session.registrationFailures, "error", err)
		if time.Duration(session.registrationFailures)*m.registrationEvery >= m.leaseTTL+m.registrationEvery {
			m.logger.WarnContext(ctx, "owner fence registration unavailable beyond the lease TTL", "node_id", fmt.Sprintf("%x", nodeID), "epoch", session.term.Epoch(), "alert_kind", "connection_owner.registration_unavailable")
			m.endSession(ctx, nodeID, session, endedRegistrationUnavailable)
		}
		return
	}
	session.registrationPending = false
	session.registrationFailures = 0
	session.nextRegistration = now.Add(m.registrationEvery)
}

// endSession removes one session from the map, records why it ended so later
// bindings fail closed, and best-effort releases the lease at PostgreSQL
// time to accelerate takeover.
func (m *Manager) endSession(ctx context.Context, nodeID [16]byte, session *nodeSession, reason endedReason) {
	m.mu.Lock()
	if current, ok := m.sessions[nodeID]; ok && current == session {
		delete(m.sessions, nodeID)
	}
	m.ended[nodeID] = reason
	m.mu.Unlock()
	if err := session.term.Release(ctx, m.pool); err != nil && !errors.Is(err, connectionowner.ErrNotOwner) {
		m.logger.ErrorContext(ctx, "release ended node lease", "node_id", fmt.Sprintf("%x", nodeID), "error", err)
	}
}

// refresh renews the lease when the exact deadline is within the renewal
// margin and refreshes the signed fence. Callers hold the node mutex; a
// renewal that fails because the term was taken over marks the session lost.
func (m *Manager) refresh(ctx context.Context, session *nodeSession) error {
	now := m.now()
	if now.Add(m.renewAhead).Before(session.leaseUntil) {
		return nil
	}
	renewed, err := session.term.Renew(ctx, m.pool)
	if errors.Is(err, connectionowner.ErrNotOwner) {
		session.lost = true
		return ErrNotOwner
	}
	if err != nil {
		return fmt.Errorf("ownersession: renew node lease: %w", err)
	}
	if !renewed.After(session.leaseUntil) {
		session.lost = true
		return ErrNotOwner
	}
	session.leaseUntil = renewed
	fence, err := m.signFence(session)
	if err != nil {
		return err
	}
	session.fence = fence
	session.registrationPending = true
	return nil
}

// bindingTerm is an immutable snapshot of the session's current term. It is
// copied under the node mutex and signed outside it, so renewal can never
// race a concurrent binding issuance on the same fields.
type bindingTerm struct {
	fence                 *agentv1.ConnectionFenceV2
	fenceID               [16]byte
	nodeID                [16]byte
	endpointID            [32]byte
	identity              connectionowner.Identity
	epoch                 int64
	connectionID          [16]byte
	authorizationRevision uint64
	capabilities          []string
}

// issueBinding signs one binding for the term's current fence. The fence
// travels with the binding so enforcing peers never need to guess the term.
func (m *Manager) issueBinding(term *bindingTerm, kind agentv1.FenceOperationKind, operationID [16]byte, capability string) (*agentv1.ConnectionFenceV2, *agentv1.FenceBindingV2, error) {
	if !slices.Contains(term.capabilities, capability) {
		return nil, nil, ErrCapabilityNotFenced
	}
	now := m.now().UTC()
	binding, err := m.signer.IssueFenceBindingV2(kind, operationID, term.fenceID, term.nodeID, term.endpointID,
		term.identity.InstanceID, uint64(term.identity.Incarnation), uint64(term.epoch),
		term.connectionID, term.authorizationRevision, capability, now, now.Add(m.bindingTTL))
	if err != nil {
		return nil, nil, fmt.Errorf("ownersession: sign fence binding: %w", err)
	}
	return term.fence, binding, nil
}

// signFence signs the session term with its exact PostgreSQL lease deadline.
// The fence expiry deliberately outlives the lease: the lease authorizes
// current mutations while the proof remains verifiable for audit and late
// result matching.
func (m *Manager) signFence(session *nodeSession) (*agentv1.ConnectionFenceV2, error) {
	issued := m.now().UTC()
	if !session.leaseUntil.After(issued) {
		return nil, errors.New("ownersession: lease deadline is not in the future")
	}
	fence, err := m.signer.IssueConnectionFenceV2(session.fenceID, session.nodeID, session.endpointID,
		session.term.Identity().InstanceID, uint64(session.term.Identity().Incarnation), uint64(session.term.Epoch()),
		session.term.ConnectionID(), session.authorizationRevision, session.capabilities,
		session.leaseUntil, issued, session.leaseUntil.Add(m.fenceGrace))
	if err != nil {
		return nil, fmt.Errorf("ownersession: sign connection fence: %w", err)
	}
	return fence, nil
}

// StateUpdateOperationID derives the canonical operation identity of one node
// trust update from its exact carrier: node, endpoint, state, revision, and
// reason. The Controller signs fence bindings over this identity and
// transportd derives it again independently, so one binding cannot authorize
// a different update.
func StateUpdateOperationID(nodeID [16]byte, endpointID []byte, state int32, revision uint64, reason string) [16]byte {
	digest := sha256.New()
	digest.Write(stateUpdateOperationDomain)
	digest.Write(nodeID[:])
	digest.Write(endpointID)
	var stateBytes [4]byte
	binary.BigEndian.PutUint32(stateBytes[:], uint32(state))
	digest.Write(stateBytes[:])
	var revisionBytes [8]byte
	binary.BigEndian.PutUint64(revisionBytes[:], revision)
	digest.Write(revisionBytes[:])
	reasonBytes := []byte(reason)
	var lengthBytes [4]byte
	binary.BigEndian.PutUint32(lengthBytes[:], uint32(len(reasonBytes)))
	digest.Write(lengthBytes[:])
	digest.Write(reasonBytes)
	var operationID [16]byte
	copy(operationID[:], digest.Sum(nil)[:16])
	return operationID
}

// fenceCapabilities normalizes a negotiated capability set into the frozen
// fence encoding: sorted, unique, and bounded.
func fenceCapabilities(capabilities []string) []string {
	normalized := make([]string, 0, len(capabilities))
	seen := make(map[string]struct{}, len(capabilities))
	for _, capability := range capabilities {
		if capability == "" || len(capability) > 128 {
			continue
		}
		if _, duplicate := seen[capability]; duplicate {
			continue
		}
		seen[capability] = struct{}{}
		normalized = append(normalized, capability)
	}
	slices.Sort(normalized)
	return normalized
}

// Observer issues operation bindings for Controller processes that do not
// hold the node lease. It reads the fence transportd registered for the node
// — already signature-verified by transportd — and signs bindings for exactly
// that term, so a stale owner's term can never be reintroduced by an
// observer.
type Observer struct {
	reader     FenceReader
	signer     *commandauth.Signer
	bindingTTL time.Duration
	now        func() time.Time
}

// NewObserver prepares the observer binder used by API and scheduler role
// processes for artifact, close, and state-update fencing.
func NewObserver(reader FenceReader, signer *commandauth.Signer) (*Observer, error) {
	if reader == nil || signer == nil {
		return nil, errors.New("ownersession: observer requires a fence reader and signer")
	}
	return &Observer{reader: reader, signer: signer, bindingTTL: 5 * time.Minute, now: time.Now}, nil
}

// BindOperation signs one binding for the fence transportd registered for
// the node. It returns ErrNoFence when transportd has none registered, which
// callers treat as the unfenced compatibility path.
func (o *Observer) BindOperation(ctx context.Context, nodeID [16]byte, kind agentv1.FenceOperationKind, operationID [16]byte, capability string) (*agentv1.ConnectionFenceV2, *agentv1.FenceBindingV2, error) {
	fence, err := o.reader.GetOwnerFence(ctx, nodeID[:])
	if err != nil {
		return nil, nil, fmt.Errorf("ownersession: read registered fence: %w", err)
	}
	if fence == nil {
		return nil, nil, ErrNoFence
	}
	fenceID, err := fixed16(fence.GetFenceId())
	if err != nil {
		return nil, nil, err
	}
	endpointID, err := fixed32(fence.GetEndpointId())
	if err != nil {
		return nil, nil, err
	}
	ownerInstanceID, err := fixed16(fence.GetOwnerInstanceId())
	if err != nil {
		return nil, nil, err
	}
	connectionID, err := fixed16(fence.GetConnectionId())
	if err != nil {
		return nil, nil, err
	}
	if !slices.Contains(fence.GetCapabilities(), capability) {
		return nil, nil, ErrCapabilityNotFenced
	}
	now := o.now().UTC()
	binding, err := o.signer.IssueFenceBindingV2(kind, operationID, fenceID, nodeID, endpointID,
		ownerInstanceID, fence.GetOwnerIncarnation(), fence.GetOwnerEpoch(), connectionID,
		fence.GetAuthorizationRevision(), capability, now, now.Add(o.bindingTTL))
	if err != nil {
		return nil, nil, fmt.Errorf("ownersession: sign observed fence binding: %w", err)
	}
	return fence, binding, nil
}

func fixed16(value []byte) ([16]byte, error) {
	if len(value) != 16 {
		return [16]byte{}, errors.New("ownersession: value must be 16 bytes")
	}
	var fixed [16]byte
	copy(fixed[:], value)
	return fixed, nil
}

func fixed32(value []byte) ([32]byte, error) {
	if len(value) != 32 {
		return [32]byte{}, errors.New("ownersession: value must be 32 bytes")
	}
	var fixed [32]byte
	copy(fixed[:], value)
	return fixed, nil
}
