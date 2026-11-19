// Package ownersession implements the Controller side of the connection
// owner fencing runtime: it takes the per-node ownership lease from the
// database authority, signs the frozen ConnectionFenceV2 and
// FenceBindingV2 contracts, pushes fences to transportd, and renews leases
// so a stale owner stops issuing proofs as soon as its term is taken over.
package ownersession

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/connectionowner"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/transportclient"
	"github.com/google/uuid"
)

// FencingCapability is the session capability an agent advertises to accept
// connection-owner fences. It is negotiated like a protocol capability, not a
// per-node business capability: the Controller attaches a fence only when the
// agent understands it, so fences never reach an endpoint that would reject
// the carrying envelope.
const FencingCapability = "ocserv.fencing.v2"

// gapReconciliationConcurrency bounds parallel transport inventory reads. At
// the 500-node capacity target it leaves enough waves to complete inside the
// transport client's three-second recovery deadline without an unbounded dial
// burst against the local Unix socket.
const gapReconciliationConcurrency = 32

// maxMaintainConcurrency bounds parallel per-node lease renewals and fence
// heartbeats so one slow node never delays renewal of the remaining fleet.
const maxMaintainConcurrency = 16

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

	errConnectionPublicationPending = errors.New("ownersession: connection publication is still pending after transport event gap")
)

// FenceRegistrar pushes a Controller-signed fence to transportd, which
// verifies it, records it as the node's active fence, and retires a mutation
// session when a strictly higher epoch arrives.
type FenceRegistrar interface {
	RegisterOwnerFence(ctx context.Context, fence *agentv1.ConnectionFenceV2) error
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

// FencedAction performs the mutation that consumes one fence binding. It
// runs inside the issuer's fencing interval: the ownership authority guards
// the verified term until the action returns, so the fencing epoch cannot
// advance between verification and mutation.
type FencedAction func(ctx context.Context, fence *agentv1.ConnectionFenceV2, binding *agentv1.FenceBindingV2) error

// FencedExecutor issues one fenced mutation. The action receives the fence
// and binding to carry on the wire — nil, nil keeps the unfenced
// compatibility path — and must run the transport RPC that presents them
// before returning, because the ownership guard is held only until then.
type FencedExecutor interface {
	ExecuteFenced(ctx context.Context, nodeID [16]byte, kind agentv1.FenceOperationKind, operationID [16]byte, capability string, action FencedAction) error
}

type nodeSession struct {
	term                  *connectionowner.Term
	fenceID               [16]byte
	nodeID                [16]byte
	endpointID            [32]byte
	authorizationRevision uint64
	capabilities          []string
	openedAt              time.Time
	leaseUntil            time.Time
	fence                 *agentv1.ConnectionFenceV2
	registrationPending   bool
	registrationFailures  int
	registrationFailedAt  time.Time
	nextRegistration      time.Time
	lost                  bool
}

type nodeLock struct {
	mu   sync.Mutex
	refs int
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
// runs inside the Controller process that serves session authorization and
// command dispatch; every ownership deadline it stores is the exact value
// returned by the database authority, never a local reconstruction. The
// local publication grace only makes event-gap cleanup retryable and never
// exceeds the authoritative owner lease lifetime.
//
// Locking: mu guards the session and ended maps; the per-node mutex returned
// by lockNode serializes every lease, registration, and binding action of
// one node, including all mutation of a nodeSession. Database and
// transportd RPCs run under the per-node lock only, so one slow node never
// blocks another node's renewal or dispatch.
type Manager struct {
	backend           database.Backend
	signer            *commandauth.Signer
	registrar         FenceRegistrar
	identity          connectionowner.Identity
	leaseTTL          time.Duration
	bindingTTL        time.Duration
	fenceGrace        time.Duration
	renewAhead        time.Duration
	interval          time.Duration
	registrationEvery time.Duration
	publicationGrace  time.Duration
	now               func() time.Time
	logger            *slog.Logger

	mu        sync.Mutex
	sessions  map[[16]byte]*nodeSession
	ended     map[[16]byte]endedReason
	nodeLocks map[[16]byte]*nodeLock
}

func NewManagerBackend(backend database.Backend, signer *commandauth.Signer, registrar FenceRegistrar, leaseTTL time.Duration, logger *slog.Logger) (*Manager, error) {
	if signer == nil || registrar == nil || backend == nil {
		return nil, errors.New("ownersession: backend, signer, and registrar are required")
	}
	if leaseTTL <= 0 {
		return nil, errors.New("ownersession: lease TTL must be positive")
	}
	instanceID, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	return &Manager{
		backend:           backend,
		signer:            signer,
		registrar:         registrar,
		identity:          connectionowner.Identity{InstanceID: instanceID, Incarnation: time.Now().UnixNano()},
		leaseTTL:          leaseTTL,
		bindingTTL:        5 * time.Minute,
		fenceGrace:        5 * time.Minute,
		renewAhead:        leaseTTL / 2,
		interval:          time.Second,
		registrationEvery: 10 * time.Second,
		// transportd publishes inventory after delivering the handshake response,
		// collecting path metadata, and rechecking trust. Give that bounded
		// pipeline the full owner lease lifetime before destructive cleanup; this
		// does not extend the lease or authorize any mutation.
		publicationGrace: leaseTTL,
		now:              time.Now,
		logger:           logger,
		sessions:         make(map[[16]byte]*nodeSession),
		ended:            make(map[[16]byte]endedReason),
		nodeLocks:        make(map[[16]byte]*nodeLock),
	}, nil
}

// lockNode returns a release function for the per-node mutex. Every action
// that takes or mutates a node's session runs under it. Entries for nodes
// without a live session are reclaimed once the last waiter unlocks so churned
// or revoked nodes do not retain mutex entries indefinitely.
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
	if err := session.term.ReleaseBackend(ctx, m.backend); err != nil && !errors.Is(err, connectionowner.ErrNotOwner) {
		return fmt.Errorf("ownersession: release node lease: %w", err)
	}
	return nil
}

// OwnsTerm reports whether this process still recognizes the exact local
// connection-owner term. It is a process-authority check only: callers that
// mutate durable state must additionally fence the write against PostgreSQL.
func (m *Manager) OwnsTerm(nodeID, connectionID [16]byte, epoch int64) bool {
	if m == nil || epoch <= 0 {
		return false
	}
	unlock := m.lockNode(nodeID)
	defer unlock()
	m.mu.Lock()
	session, ok := m.sessions[nodeID]
	m.mu.Unlock()
	return ok && !session.lost && session.term.ConnectionID() == connectionID && session.term.Epoch() == epoch
}

// OpenSession takes the ownership lease for a newly accepted mutation-capable
// agent session and returns the signed fence for the handshake response. A
// different unexpired owner blocks the session: the caller must fail the
// fencing-capable handshake so the Agent retries after the lease can be taken
// over instead of granting mutations or parking it on an unfenced session.
//
// A reconnect of the same identity is a replacement, not a continuation:
// Acquire irrevocably advances the PostgreSQL fencing epoch, so the previous
// local session of this node stops issuing bindings the moment Acquire
// returns — before the new term is even registered. Any post-Acquire failure
// leaves the node fence-unavailable and fail-closed; it can never resurrect
// the superseded session or its epoch.
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
	term, err := connectionowner.AcquireBackend(ctx, m.backend, nodeID, m.identity, connectionID, m.leaseTTL)
	if errors.Is(err, connectionowner.ErrLeaseHeld) {
		// Another owner holds an unexpired lease, so any previous local
		// session of this node is expired by definition. End it instead of
		// letting bindings ride its locally recorded deadline.
		m.mu.Lock()
		previous, held := m.sessions[nodeID]
		m.mu.Unlock()
		if held {
			m.endSession(ctx, nodeID, previous, endedOwnershipLost)
		}
		return nil, ErrNotOwner
	}
	if err != nil {
		return nil, fmt.Errorf("ownersession: acquire node lease: %w", err)
	}
	// The old local session lost the database authority the instant Acquire
	// returned; the ended marker keeps the node fail-closed until the new
	// term registers successfully.
	m.mu.Lock()
	delete(m.sessions, nodeID)
	m.ended[nodeID] = endedRegistrationUnavailable
	m.mu.Unlock()
	session := &nodeSession{
		term:                  term,
		fenceID:               fenceID,
		nodeID:                nodeID,
		endpointID:            endpointID,
		authorizationRevision: authorizationRevision,
		capabilities:          fenceCapabilities(capabilities),
		leaseUntil:            term.LeaseUntil(),
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
		return nil, fmt.Errorf("ownersession: register owner fence: %w", err)
	}
	session.openedAt = m.now()
	session.nextRegistration = session.openedAt.Add(m.registrationEvery)
	session.fence = fence
	m.mu.Lock()
	m.sessions[nodeID] = session
	delete(m.ended, nodeID)
	m.mu.Unlock()
	return fence, nil
}

// releaseAcquiredTerm best-effort releases a lease whose session was never
// registered, so a failed open does not hold the node for a full TTL.
func (m *Manager) releaseAcquiredTerm(ctx context.Context, term *connectionowner.Term) {
	if err := term.ReleaseBackend(ctx, m.backend); err != nil && !errors.Is(err, connectionowner.ErrNotOwner) {
		m.logger.ErrorContext(ctx, "release unregistered node lease", "node_id", fmt.Sprintf("%x", term.NodeID()), "error", err)
	}
}

// ExecuteFenced issues one fenced mutation for the node's live session. The
// per-node mutex stays held across the action, so a same-process reopen
// cannot advance the fencing epoch between the lease check, the signature,
// and the mutation the action performs.
func (m *Manager) ExecuteFenced(ctx context.Context, nodeID [16]byte, kind agentv1.FenceOperationKind, operationID [16]byte, capability string, action FencedAction) error {
	unlock := m.lockNode(nodeID)
	defer unlock()
	term, err := m.termForBinding(ctx, nodeID)
	if errors.Is(err, ErrNoFence) {
		return action(ctx, nil, nil)
	}
	if err != nil {
		return err
	}
	fence, binding, err := m.issueBinding(term, kind, operationID, capability)
	if err != nil {
		return err
	}
	return action(ctx, fence, binding)
}

// termForBinding returns the binding snapshot of the node's live session, or
// the fail-closed reason there is none. Callers hold the node mutex; the
// snapshot is taken under it so renewal cannot race binding issuance.
func (m *Manager) termForBinding(ctx context.Context, nodeID [16]byte) (*bindingTerm, error) {
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
			return nil, ErrFenceUnavailable
		case ended:
			return nil, ErrNotOwner
		default:
			return nil, ErrNoFence
		}
	}
	if err := m.refresh(ctx, session); err != nil {
		return nil, err
	}
	// A renewal re-signed the fence; push it to transportd eagerly so
	// its recorded lease stays current. A failure only leaves the
	// registration pending for the maintain loop: the binding still
	// carries the fresh fence, and transportd accepts the refreshed
	// term because the fence identity is unchanged.
	if session.registrationPending && m.registrar.RegisterOwnerFence(ctx, session.fence) == nil {
		session.registrationPending = false
	}
	snapshot := bindingTerm{
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
	return &snapshot, nil
}

// maintain renews sessions approaching their deadline and retries pending
// fence registrations with bounded parallelism across nodes.
func (m *Manager) maintain(ctx context.Context) {
	m.mu.Lock()
	nodes := make([][16]byte, 0, len(m.sessions))
	for nodeID := range m.sessions {
		nodes = append(nodes, nodeID)
	}
	m.mu.Unlock()
	if len(nodes) == 0 {
		return
	}
	runOne := func(nodeID [16]byte) {
		unlock := m.lockNode(nodeID)
		m.mu.Lock()
		session, ok := m.sessions[nodeID]
		m.mu.Unlock()
		if ok {
			m.maintainNode(ctx, nodeID, session)
		}
		unlock()
	}
	if len(nodes) == 1 {
		runOne(nodes[0])
		return
	}
	workerCount := min(maxMaintainConcurrency, len(nodes))
	queue := make(chan [16]byte, len(nodes))
	for _, nodeID := range nodes {
		queue <- nodeID
	}
	close(queue)
	var wg sync.WaitGroup
	wg.Add(workerCount)
	for range workerCount {
		go func() {
			defer wg.Done()
			for nodeID := range queue {
				if ctx.Err() != nil {
					return
				}
				runOne(nodeID)
			}
		}()
	}
	wg.Wait()
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
		if session.registrationFailures == 0 {
			session.registrationFailedAt = now
		}
		session.registrationFailures++
		session.nextRegistration = now.Add(m.registrationEvery)
		m.logger.WarnContext(ctx, "retry owner fence registration", "node_id", fmt.Sprintf("%x", nodeID), "failures", session.registrationFailures, "error", err)
		// The bound is real elapsed time: pending registrations retry on the
		// maintenance interval, which is much shorter than the heartbeat
		// period, so counting attempts would end a healthy owner early.
		if now.Sub(session.registrationFailedAt) >= m.leaseTTL+m.registrationEvery {
			m.logger.WarnContext(ctx, "owner fence registration unavailable beyond the lease TTL", "node_id", fmt.Sprintf("%x", nodeID), "epoch", session.term.Epoch(), "alert_kind", "connection_owner.registration_unavailable")
			m.endSession(ctx, nodeID, session, endedRegistrationUnavailable)
		}
		return
	}
	session.registrationPending = false
	session.registrationFailures = 0
	session.registrationFailedAt = time.Time{}
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
	if err := session.term.ReleaseBackend(ctx, m.backend); err != nil && !errors.Is(err, connectionowner.ErrNotOwner) {
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
	renewed, err := session.term.RenewBackend(ctx, m.backend)
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
