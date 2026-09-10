package audittest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit/auditstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/rbac"
	"github.com/GentleKingson/ocservia/control-plane/internal/rbac/rbacstore"
	"github.com/google/uuid"
)

// Controller exercises public audit and RBAC services with the same runtime
// backend and transaction, including rollback of a consumed approval.
func Controller(t *testing.T, b database.Backend, workspace, actor, target uuid.UUID, approve func(uuid.UUID, uuid.UUID, []byte, time.Time) error) {
	t.Helper()
	ctx := context.Background()
	m := audit.NewBackendManager(b, bytes.Repeat([]byte{7}, 32))
	appendEvent := func(ctx context.Context, after func() error) error {
		return database.Within(ctx, b, database.ReadCommitted, func(tx database.Tx) error {
			if err := audit.AppendChainTx(ctx, tx, audit.ChainRecord{WorkspaceID: workspace, ActorType: "user", ActorID: actor.String(), Action: "audit.workflow", ResourceType: "workspace", ResourceID: workspace, RequestID: uuid.NewString(), BeforeSummary: json.RawMessage(`{ "exact":9007199254740993.125,"null":null,"array":[true,"trailing ",null] }`), AfterSummary: json.RawMessage(" \n null \t")}); err != nil {
				return err
			}
			if after != nil {
				return after()
			}
			return nil
		})
	}
	if err := appendEvent(ctx, nil); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("business failure")
	if err := appendEvent(ctx, func() error { return failure }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	if err := appendEvent(cancelled, func() error { cancel(); return cancelled.Err() }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	verified, err := m.Verify(ctx, workspace)
	if err != nil || !verified.Valid || verified.Events != 1 {
		t.Fatalf("audit rollback: %+v %v", verified, err)
	}
	if err = m.Checkpoint(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	if err = m.Checkpoint(ctx, workspace); err != nil {
		t.Fatal("repeat checkpoint", err)
	}
	if err = m.EnsureAuthenticity(ctx); err != nil {
		t.Fatal(err)
	}
	verified, err = m.Verify(ctx, workspace)
	if err != nil || !verified.Valid || !verified.Checkpoint {
		t.Fatalf("audit checkpoint: %+v %v", verified, err)
	}

	s := rbac.NewBackend(b)
	request := rbac.BindingRequest{IdentityID: target, WorkspaceID: workspace, ActorID: actor, SessionID: uuid.New(), Role: "Viewer", ResourceType: "workspace", RequestID: uuid.NewString(), Reason: "runtime workflow"}
	if _, err = s.CreateBinding(ctx, request); err != nil {
		t.Fatal(err)
	}
	resource := rbac.Resource{WorkspaceID: workspace, Type: "workspace"}
	if err = s.Authorize(ctx, target, "node.read", resource, false); err != nil {
		t.Fatal(err)
	}
	if err = s.Authorize(ctx, target, "role_binding.manage", resource, false); !errors.Is(err, rbac.ErrForbidden) {
		t.Fatal("permission escalation", err)
	}
	denied := request
	denied.ActorID = target
	denied.IdentityID = actor
	denied.Role = "Operator"
	if _, err = s.CreateBinding(ctx, denied); !errors.Is(err, rbac.ErrGrantForbidden) {
		t.Fatal("grant escalation", err)
	}
	if _, err = s.CreateBinding(ctx, request); !errors.Is(err, database.ErrUnique) {
		t.Fatal("duplicate grant", err)
	}
	request.Role = "SecurityAdmin"
	if _, err = s.CreateBinding(ctx, request); !errors.Is(err, approvals.ErrNotReady) {
		t.Fatal("elevation without approval", err)
	}
	request.ApprovalID = uuid.New()
	hash, _ := rbac.BindingApprovalContent(target, workspace, request.Role, request.ResourceType, uuid.Nil)
	if err = approve(request.ApprovalID, target, hash, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateBinding(ctx, request); err != nil {
		t.Fatal("approved elevation", err)
	}
	if err = s.Authorize(ctx, target, "role_binding.manage", resource, false); err != nil {
		t.Fatal(err)
	}
	// A consumed approval is never reusable, including when the desired grant
	// already exists and would otherwise report only a duplicate key.
	if _, err = s.CreateBinding(ctx, request); !errors.Is(err, approvals.ErrNotReady) {
		t.Fatal("approval replay", err)
	}
	request.ApprovalID = uuid.New()
	if err = approve(request.ApprovalID, target, hash, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateBinding(ctx, request); !errors.Is(err, database.ErrUnique) {
		t.Fatal("duplicate elevation must rollback consumption", err)
	}
	if err = database.Within(ctx, b, database.ReadCommitted, func(tx database.Tx) error {
		return approvals.ConsumeBoundTx(ctx, tx, request.ApprovalID, workspace, actor, "role_binding.elevate", "role_binding", target, hash)
	}); err != nil {
		t.Fatal("failed binding consumed approval", err)
	}

	blocker, err := b.Begin(ctx, database.ReadCommitted)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(ctx)
	store, err := rbacstore.From(blocker)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.LockManagement(ctx); err != nil {
		t.Fatal(err)
	}
	waitCtx, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	defer stop()
	request.Role = "Auditor"
	request.ApprovalID = uuid.Nil
	if _, err = s.CreateBinding(waitCtx, request); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("management lock cancellation", err)
	}
	if err = blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateBinding(ctx, request); err != nil {
		t.Fatal("management lock release", err)
	}
	verified, err = m.Verify(ctx, workspace)
	if err != nil || !verified.Valid || verified.Events != 4 {
		t.Fatalf("transactional grant audit: %+v %v", verified, err)
	}
	// Approval expiration intentionally uses the original transaction-start
	// clock, including a lock wait that crosses expiration.
	old, err := b.Begin(ctx, database.ReadCommitted)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Rollback(ctx)
	started, err := database.TransactionTime(ctx, old)
	if err != nil {
		t.Fatal(err)
	}
	startedAt, err := started.Time()
	if err != nil {
		t.Fatal(err)
	}
	expires := startedAt.Add(2 * time.Second)
	approvalID := uuid.New()
	if err = approve(approvalID, target, hash, expires); err != nil {
		t.Fatal(err)
	}
	holder, err := b.Begin(ctx, database.ReadCommitted)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	consume := func(tx database.Tx) error {
		return approvals.ConsumeBoundTx(ctx, tx, approvalID, workspace, actor, "role_binding.elevate", "role_binding", target, hash)
	}
	if err = consume(holder); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- consume(old) }()
	select {
	case err := <-done:
		t.Fatal("approval lock did not block", err)
	case <-time.After(100 * time.Millisecond):
	}
	time.Sleep(time.Until(expires.Add(150 * time.Millisecond)))
	if err = holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal("transaction-start expiration changed", err)
	}
	if err = old.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	approvalID = uuid.New()
	if err = approve(approvalID, target, hash, expires); err != nil {
		t.Fatal(err)
	}
	if err = database.Within(ctx, b, database.ReadCommitted, consume); !errors.Is(err, approvals.ErrNotReady) {
		t.Fatal("already expired approval allowed", err)
	}

	results := make(chan error, 8)
	for range cap(results) {
		go func() { results <- appendEvent(ctx, nil) }()
	}
	for range cap(results) {
		if err := <-results; err != nil {
			t.Error("concurrent audit append", err)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
	verified, err = m.Verify(ctx, workspace)
	if err != nil || !verified.Valid || verified.Events != 12 {
		t.Fatalf("concurrent audit chain: %+v %v", verified, err)
	}
	// Commit an append and checkpoint between the verifier's two reads. Both
	// reads must still see the original repeatable-read snapshot.
	snapshot := audit.NewBackendManager(snapshotBackend{Backend: b, between: func() {
		if err := appendEvent(ctx, nil); err != nil {
			t.Fatal(err)
		}
		if err := m.Checkpoint(ctx, workspace); err != nil {
			t.Fatal(err)
		}
	}}, bytes.Repeat([]byte{7}, 32))
	verified, err = snapshot.Verify(ctx, workspace)
	if err != nil || !verified.Valid || !verified.Checkpoint || verified.Events != 12 {
		t.Fatalf("inconsistent verification snapshot: %+v %v", verified, err)
	}
	verified, err = m.Verify(ctx, workspace)
	if err != nil || !verified.Valid || !verified.Checkpoint || verified.Events != 13 {
		t.Fatalf("committed audit tail: %+v %v", verified, err)
	}
}

type snapshotBackend struct {
	database.Backend
	between func()
}

func (b snapshotBackend) Begin(ctx context.Context, isolation database.Isolation) (database.Tx, error) {
	tx, err := b.Backend.Begin(ctx, isolation)
	if err != nil {
		return nil, err
	}
	return snapshotTx{Tx: tx, between: b.between}, nil
}

type snapshotTx struct {
	database.Tx
	between func()
}

func (tx snapshotTx) AuditStore() auditstore.Store {
	return snapshotStore{Store: tx.Tx.(auditstore.Provider).AuditStore(), between: tx.between}
}

type snapshotStore struct {
	auditstore.Store
	between func()
}

func (s snapshotStore) Events(ctx context.Context, workspace uuid.UUID) (database.Rows, error) {
	s.between()
	return s.Store.Events(ctx, workspace)
}
