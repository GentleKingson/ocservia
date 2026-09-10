package mysql

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"math"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/connectionowner"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/ownersession"
	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
)

func TestRealConnectionOwner(t *testing.T) {
	owner, _, options := migrateFixture(t)
	ctx := context.Background()
	if err := owner.GrantTestPrivileges(ctx); err != nil {
		t.Fatal(err)
	}
	cfg, err := driver.ParseDSN(options.DSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.User, cfg.Passwd = "ocservia_app", "pr02-runtime-test-only"
	options.DSN = cfg.FormatDSN()
	backend, err := Open(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	identity := connectionowner.Identity{InstanceID: uuid.New(), Incarnation: 1}
	node, connection := uuid.New(), uuid.New()
	acquire := func(node uuid.UUID, identity connectionowner.Identity, ttl time.Duration) *connectionowner.Term {
		t.Helper()
		v, err := connectionowner.AcquireBackend(ctx, backend, node, identity, uuid.New(), ttl)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	run := func(q string, args ...any) {
		t.Helper()
		if _, err := owner.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	assertDeadline := func(until time.Time) {
		t.Helper()
		var stored value.Timestamp
		if err := owner.QueryRow(ctx, `SELECT lease_until FROM connection_owner_fencing WHERE node_id=?`, node[:]).Scan(&stored); err != nil || stored != fixtureTimestamp(t, until) {
			t.Fatal("deadline is not the exact stored value", stored, until, err)
		}
	}
	if _, err := connectionowner.ReadStateBackend(ctx, backend, node); !errors.Is(err, connectionowner.ErrNoOwnerRow) {
		t.Fatal("missing owner", err)
	}
	first, err := connectionowner.AcquireBackend(ctx, backend, node, identity, connection, time.Minute)
	if err != nil || first.Epoch() != 1 {
		t.Fatal("first acquisition", first, err)
	}
	assertDeadline(first.LeaseUntil())
	until, err := first.RenewBackend(ctx, backend)
	if err != nil {
		t.Fatal(err)
	}
	assertDeadline(until)
	state, err := connectionowner.ReadStateBackend(ctx, backend, node)
	if err != nil || state.InstanceID != identity.InstanceID || state.Incarnation != 1 || state.ConnectionID != connection || state.Epoch != 1 || !state.LeaseUntilValid {
		t.Fatal("owner read", state, err)
	}
	if err := first.AssertCurrentBackend(ctx, backend); err != nil {
		t.Fatal("current assertion", err)
	}
	restarted := connectionowner.Identity{InstanceID: identity.InstanceID, Incarnation: 2}
	if _, err := connectionowner.AcquireBackend(ctx, backend, node, restarted, uuid.New(), time.Minute); !errors.Is(err, connectionowner.ErrLeaseHeld) {
		t.Fatal("new incarnation must wait", err)
	}
	second := acquire(node, identity, time.Minute)
	if second.Epoch() != 2 {
		t.Fatal("same owner reused epoch", second.Epoch())
	}
	if _, err := first.RenewBackend(ctx, backend); !errors.Is(err, connectionowner.ErrNotOwner) {
		t.Fatal("stale renew", err)
	}
	if err := first.ReleaseBackend(ctx, backend); !errors.Is(err, connectionowner.ErrNotOwner) {
		t.Fatal("stale release", err)
	}
	if err := first.AssertCurrentBackend(ctx, backend); !errors.Is(err, connectionowner.ErrNotOwner) {
		t.Fatal("stale assertion", err)
	}
	positive := value.Timestamp{Valid: true, Micros: value.PositiveInfinity}
	negative := value.Timestamp{Valid: true, Micros: value.NegativeInfinity}
	run(`UPDATE connection_owner_fencing SET lease_until=? WHERE node_id=?`, positive, node[:])
	if err := second.AssertCurrentBackend(ctx, backend); err != nil {
		t.Fatal("infinite lease", err)
	}
	if _, err := connectionowner.AcquireBackend(ctx, backend, node, restarted, uuid.New(), time.Minute); !errors.Is(err, connectionowner.ErrLeaseHeld) {
		t.Fatal("infinite lease takeover", err)
	}
	run(`UPDATE connection_owner_fencing SET lease_until=? WHERE node_id=?`, negative, node[:])
	if err := second.AssertCurrentBackend(ctx, backend); !errors.Is(err, connectionowner.ErrNotOwner) {
		t.Fatal("expired infinite lease", err)
	}
	third := acquire(node, restarted, time.Minute)
	if third.Epoch() != 3 {
		t.Fatal("expired takeover reused epoch", third.Epoch())
	}
	if err := third.ReleaseBackend(ctx, backend); err != nil {
		t.Fatal("release", err)
	}
	if err := third.AssertCurrentBackend(ctx, backend); !errors.Is(err, connectionowner.ErrNotOwner) {
		t.Fatal("released authority remained valid", err)
	}
	run(`UPDATE connection_owner_fencing SET owner_epoch=? WHERE node_id=?`, int64(math.MaxInt64), node[:])
	if _, err := connectionowner.AcquireBackend(ctx, backend, node, identity, uuid.New(), time.Minute); !errors.Is(err, database.ErrConstraint) {
		t.Fatal("epoch overflow", err)
	}
	state, err = connectionowner.ReadStateBackend(ctx, backend, node)
	if err != nil || state.Epoch != math.MaxInt64 || state.InstanceID != restarted.InstanceID || state.Incarnation != 2 {
		t.Fatal("overflow mutated ownership", state, err)
	}

	t.Run("guard blocks takeover and cleans up after cancellation", func(t *testing.T) {
		guardNode := uuid.New()
		term := acquire(guardNode, identity, time.Minute)
		guardCtx, cancel := context.WithCancel(ctx)
		release, err := connectionowner.GuardObservedTermBackend(guardCtx, backend, guardNode, identity.InstanceID, identity.Incarnation, term.ConnectionID(), term.Epoch())
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		defer release()
		waitCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		defer stop()
		done := make(chan error, 1)
		go func() {
			_, err := connectionowner.AcquireBackend(waitCtx, backend, guardNode, identity, uuid.New(), time.Minute)
			done <- err
		}()
		select {
		case err := <-done:
			t.Fatal("takeover passed held guard", err)
		case <-time.After(200 * time.Millisecond):
		}
		cancel()
		if err := release(); err != nil {
			t.Fatal("release cancelled guard", err)
		}
		if err := <-done; err != nil {
			t.Fatal("takeover after release", err)
		}
		if err := term.AssertCurrentBackend(ctx, backend); !errors.Is(err, connectionowner.ErrNotOwner) {
			t.Fatal("old guard term survived takeover", err)
		}
	})

	t.Run("expired assert leaves no lock in old transaction", func(t *testing.T) {
		expiringNode := uuid.New()
		term := acquire(expiringNode, identity, 300*time.Millisecond)
		tx, err := backend.Begin(ctx, database.ReadCommitted)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := database.TransactionTime(ctx, tx); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Until(term.LeaseUntil()) + 100*time.Millisecond)
		if err := term.AssertTransaction(ctx, tx); !errors.Is(err, connectionowner.ErrNotOwner) {
			t.Fatal("assert used frozen transaction clock", err)
		}
		bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if _, err := connectionowner.AcquireBackend(bounded, backend, expiringNode, restarted, uuid.New(), time.Minute); err != nil {
			t.Fatal("rejected assert blocked takeover", err)
		}
	})
	t.Run("assert rechecks expiry after lock wait", func(t *testing.T) {
		waitingNode := uuid.New()
		term := acquire(waitingNode, identity, time.Second)
		locked, err := owner.Begin(ctx, database.ReadCommitted)
		if err != nil {
			t.Fatal(err)
		}
		defer locked.Rollback(ctx)
		var epoch int64
		if err := locked.QueryRow(ctx, `SELECT owner_epoch FROM connection_owner_fencing WHERE node_id=? FOR UPDATE`, waitingNode[:]).Scan(&epoch); err != nil {
			t.Fatal(err)
		}
		bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- term.AssertCurrentBackend(bounded, backend) }()
		select {
		case err := <-done:
			t.Fatal("assert bypassed exclusive lock", err)
		case <-time.After(200 * time.Millisecond):
		}
		time.Sleep(time.Until(term.LeaseUntil()) + 100*time.Millisecond)
		if err := locked.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-done; !errors.Is(err, connectionowner.ErrNotOwner) {
			t.Fatal("assert accepted statement-start lease after wait", err)
		}
	})
	t.Run("first acquisitions serialize", func(t *testing.T) {
		newNode := uuid.New()
		start := make(chan struct{})
		done := make(chan error, 2)
		bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		for range 2 {
			go func() {
				<-start
				_, err := connectionowner.AcquireBackend(bounded, backend, newNode, connectionowner.Identity{InstanceID: uuid.New(), Incarnation: 1}, uuid.New(), time.Minute)
				done <- err
			}()
		}
		close(start)
		won, held := 0, 0
		for range 2 {
			err := <-done
			switch {
			case err == nil:
				won++
			case errors.Is(err, connectionowner.ErrLeaseHeld):
				held++
			default:
				t.Fatal("simultaneous first acquisition", err)
			}
		}
		if won != 1 || held != 1 {
			t.Fatal("two owners acquired same node", won, held)
		}
	})
	t.Run("session manager and observer", func(t *testing.T) {
		testConnectionOwnerSession(t, backend)
	})
}

type ownerFenceRegistry struct {
	fence *agentv1.ConnectionFenceV2
	fail  bool
}

func (r *ownerFenceRegistry) RegisterOwnerFence(_ context.Context, fence *agentv1.ConnectionFenceV2) error {
	if r.fail {
		return errors.New("transport unavailable")
	}
	r.fence = fence
	return nil
}

func (r *ownerFenceRegistry) GetOwnerFence(_ context.Context, node []byte) (*agentv1.ConnectionFenceV2, error) {
	if r.fence == nil || !bytes.Equal(r.fence.GetNodeId(), node) {
		return nil, nil
	}
	return r.fence, nil
}

func testConnectionOwnerSession(t *testing.T, backend database.Backend) {
	t.Helper()
	ctx := context.Background()
	signer, err := commandauth.NewRandomSigner()
	if err != nil {
		t.Fatal(err)
	}
	registry := &ownerFenceRegistry{}
	manager, err := ownersession.NewManagerBackend(backend, signer, registry, time.Minute, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	node := uuid.Must(uuid.NewV7())
	endpoint := sha256.Sum256(node[:])
	fence, err := manager.OpenSession(ctx, node, endpoint, 41, []string{ownersession.FencingCapability})
	if err != nil {
		t.Fatal("open session", err)
	}
	connection, err := uuid.FromBytes(fence.GetConnectionId())
	if err != nil || !manager.OwnsTerm(node, connection, int64(fence.GetOwnerEpoch())) {
		t.Fatal("manager lost acquired term", err)
	}
	if err := manager.CloseSession(ctx, node, uuid.New(), int64(fence.GetOwnerEpoch())); err != nil {
		t.Fatal("unrelated close", err)
	}
	if !manager.OwnsTerm(node, connection, int64(fence.GetOwnerEpoch())) {
		t.Fatal("unrelated close ended current session")
	}
	observer, err := ownersession.NewObserverBackend(backend, registry, signer)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	action := func(_ context.Context, got *agentv1.ConnectionFenceV2, binding *agentv1.FenceBindingV2) error {
		calls++
		if got == nil || binding == nil || binding.GetOwnerEpoch() != got.GetOwnerEpoch() || !bytes.Equal(binding.GetConnectionId(), got.GetConnectionId()) {
			return errors.New("binding does not name registered term")
		}
		return nil
	}
	if err := observer.ExecuteFenced(ctx, node, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, uuid.Must(uuid.NewV7()), ownersession.FencingCapability, action); err != nil || calls != 1 {
		t.Fatal("live observer", calls, err)
	}
	// The database term advances even when transport keeps serving the old
	// fence. Neither manager nor observer may turn that stale term into proof.
	registry.fail = true
	if _, err := manager.OpenSession(ctx, node, endpoint, 42, []string{ownersession.FencingCapability}); err == nil {
		t.Fatal("failed registration accepted")
	}
	if _, _, err := manager.BindOperation(ctx, node, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, uuid.Must(uuid.NewV7()), ownersession.FencingCapability); !errors.Is(err, ownersession.ErrFenceUnavailable) {
		t.Fatal("manager failed open", err)
	}
	if err := observer.ExecuteFenced(ctx, node, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, uuid.Must(uuid.NewV7()), ownersession.FencingCapability, action); !errors.Is(err, ownersession.ErrNotOwner) || calls != 1 {
		t.Fatal("observer used stale registry", calls, err)
	}
	registry.fail = false
	successor, err := ownersession.NewManagerBackend(backend, signer, registry, time.Minute, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	newFence, err := successor.OpenSession(ctx, node, endpoint, 43, []string{ownersession.FencingCapability})
	if err != nil || newFence.GetOwnerEpoch() <= fence.GetOwnerEpoch() {
		t.Fatal("successor session", newFence, err)
	}
	if err := observer.ExecuteFenced(ctx, node, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, uuid.Must(uuid.NewV7()), ownersession.FencingCapability, action); err != nil || calls != 2 {
		t.Fatal("successor observer", calls, err)
	}
	newConnection, err := uuid.FromBytes(newFence.GetConnectionId())
	if err != nil {
		t.Fatal(err)
	}
	if err := successor.CloseSession(ctx, node, newConnection, int64(newFence.GetOwnerEpoch())); err != nil {
		t.Fatal("close exact successor", err)
	}
	if err := observer.ExecuteFenced(ctx, node, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, uuid.Must(uuid.NewV7()), ownersession.FencingCapability, action); !errors.Is(err, ownersession.ErrNotOwner) || calls != 2 {
		t.Fatal("closed observer", calls, err)
	}
}
