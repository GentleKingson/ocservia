// Package ownersession implements the Controller side of the connection
// owner fencing runtime: it takes the per-node ownership lease from the
// PostgreSQL authority, signs the frozen ConnectionFenceV2 and
// FenceBindingV2 contracts, pushes fences to transportd, and renews leases
// so a stale owner stops issuing proofs as soon as its term is taken over.
package ownersession

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/connectionowner"
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

var (
	// ErrNotOwner reports that the local ownership term is stale or its lease
	// expired; the caller must stop issuing fenced proofs and fail closed.
	ErrNotOwner = errors.New("ownersession: connection ownership lost")

	// ErrNoFence reports that no owner session exists for the node. Callers
	// treat it as the unfenced compatibility path, not as a failure.
	ErrNoFence = errors.New("ownersession: no owner fence for node")

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
	lost                  bool
}

// Manager owns the per-node connection leases of one Controller process. It
// runs inside the worker-role process that serves session authorization and
// command dispatch; every deadline it stores is the exact value returned by
// the PostgreSQL authority, never a local reconstruction.
type Manager struct {
	pool       *pgxpool.Pool
	signer     *commandauth.Signer
	registrar  FenceRegistrar
	identity   connectionowner.Identity
	leaseTTL   time.Duration
	bindingTTL time.Duration
	fenceGrace time.Duration
	renewAhead time.Duration
	interval   time.Duration
	now        func() time.Time
	logger     *slog.Logger

	mu       sync.Mutex
	sessions map[[16]byte]*nodeSession
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
		pool:       pool,
		signer:     signer,
		registrar:  registrar,
		identity:   connectionowner.Identity{InstanceID: instanceID, Incarnation: time.Now().UnixNano()},
		leaseTTL:   leaseTTL,
		bindingTTL: 5 * time.Minute,
		fenceGrace: 5 * time.Minute,
		renewAhead: leaseTTL / 2,
		interval:   time.Second,
		now:        time.Now,
		logger:     logger,
		sessions:   make(map[[16]byte]*nodeSession),
	}, nil
}

// Run renews unexpired sessions and retries fence registrations until the
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

// OpenSession takes the ownership lease for a newly accepted mutation-capable
// agent session and returns the signed fence for the handshake response. A
// different unexpired owner blocks the session: the caller must downgrade to
// the read-only compatibility path instead of granting mutations.
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
	}
	fence, err := m.signFence(session)
	if err != nil {
		return nil, err
	}
	// The initial registration must reach transportd before the session is
	// granted: registering a higher epoch is what retires a superseded
	// owner's live session without waiting for lease expiry.
	if err := m.registrar.RegisterOwnerFence(ctx, fence); err != nil {
		return nil, fmt.Errorf("ownersession: register owner fence: %w", err)
	}
	session.fence = fence

	m.mu.Lock()
	m.sessions[nodeID] = session
	m.mu.Unlock()
	return fence, nil
}

// BindOperation signs one operation binding against the node's live session.
// It renews the lease first when the exact deadline is close, fails closed
// once the term was taken over, and reports ErrNoFence when the node session
// never negotiated fencing.
func (m *Manager) BindOperation(ctx context.Context, nodeID [16]byte, kind agentv1.FenceOperationKind, operationID [16]byte, capability string) (*agentv1.ConnectionFenceV2, *agentv1.FenceBindingV2, error) {
	m.mu.Lock()
	session, ok := m.sessions[nodeID]
	if ok && session.lost {
		ok = false
	}
	if ok {
		if err := m.refreshLocked(ctx, session); err != nil {
			m.mu.Unlock()
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
	}
	m.mu.Unlock()
	if !ok {
		return nil, nil, ErrNoFence
	}
	return m.issueBinding(session, kind, operationID, capability)
}

// maintain renews sessions approaching their deadline and retries pending
// fence registrations.
func (m *Manager) maintain(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for nodeID, session := range m.sessions {
		if session.lost {
			delete(m.sessions, nodeID)
			continue
		}
		if err := m.refreshLocked(ctx, session); err != nil {
			if errors.Is(err, ErrNotOwner) {
				m.logger.WarnContext(ctx, "connection ownership lost", "node_id", fmt.Sprintf("%x", nodeID), "epoch", session.term.Epoch(), "alert_kind", "connection_owner.lost")
				delete(m.sessions, nodeID)
				continue
			}
			m.logger.ErrorContext(ctx, "renew connection ownership", "node_id", fmt.Sprintf("%x", nodeID), "error", err)
			continue
		}
		if session.registrationPending {
			if err := m.registrar.RegisterOwnerFence(ctx, session.fence); err != nil {
				m.logger.WarnContext(ctx, "retry owner fence registration", "node_id", fmt.Sprintf("%x", nodeID), "error", err)
				continue
			}
			session.registrationPending = false
		}
	}
}

// refreshLocked renews the lease when the exact deadline is within the
// renewal margin and refreshes the signed fence. Callers hold the manager
// mutex; a renewal that fails because the term was taken over marks the
// session lost.
func (m *Manager) refreshLocked(ctx context.Context, session *nodeSession) error {
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

// issueBinding signs one binding for the session's current term. The fence
// travels with the binding so enforcing peers never need to guess the term.
func (m *Manager) issueBinding(session *nodeSession, kind agentv1.FenceOperationKind, operationID [16]byte, capability string) (*agentv1.ConnectionFenceV2, *agentv1.FenceBindingV2, error) {
	if !slices.Contains(session.capabilities, capability) {
		return nil, nil, ErrCapabilityNotFenced
	}
	now := m.now().UTC()
	binding, err := m.signer.IssueFenceBindingV2(kind, operationID, session.fenceID, session.nodeID, session.endpointID,
		session.term.Identity().InstanceID, uint64(session.term.Identity().Incarnation), uint64(session.term.Epoch()),
		session.term.ConnectionID(), session.authorizationRevision, capability, now, now.Add(m.bindingTTL))
	if err != nil {
		return nil, nil, fmt.Errorf("ownersession: sign fence binding: %w", err)
	}
	return session.fence, binding, nil
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
