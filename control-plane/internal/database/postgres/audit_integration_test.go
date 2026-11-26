package postgres_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit/auditstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAuditControllerIntegration(t *testing.T) {
	dsn := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("real PostgreSQL required")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	workspace, actor := uuid.New(), uuid.New()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err = pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,$2,$3,$4,$4)`, workspace, "audit", "audit-"+workspace.String(), now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO identities(id,issuer,subject,email,display_name,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$6)`, actor, "audit-test", actor.String(), "", "audit", now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO workspace_admins(workspace_id,identity_id,created_at) VALUES($1,$2,$3)`, workspace, actor, now); err != nil {
		t.Fatal(err)
	}
	runAuditControllerTest(t, postgres.WrapPool(pool), workspace, actor)
}

func runAuditControllerTest(t *testing.T, b database.Backend, workspace, actor uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	m := audit.NewManager(b, bytes.Repeat([]byte{7}, 32))
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
	if err != nil || !verified.Valid || verified.Events != 9 {
		t.Fatalf("concurrent audit chain: %+v %v", verified, err)
	}
	// Commit an append and checkpoint between the verifier's two reads. Both
	// reads must still see the original repeatable-read snapshot.
	snapshot := audit.NewManager(snapshotBackend{Backend: b, between: func() {
		if err := appendEvent(ctx, nil); err != nil {
			t.Fatal(err)
		}
		if err := m.Checkpoint(ctx, workspace); err != nil {
			t.Fatal(err)
		}
	}}, bytes.Repeat([]byte{7}, 32))
	verified, err = snapshot.Verify(ctx, workspace)
	if err != nil || !verified.Valid || !verified.Checkpoint || verified.Events != 9 {
		t.Fatalf("inconsistent verification snapshot: %+v %v", verified, err)
	}
	verified, err = m.Verify(ctx, workspace)
	if err != nil || !verified.Valid || !verified.Checkpoint || verified.Events != 10 {
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

func (tx snapshotTx) Unwrap() database.Tx { return tx.Tx }

func (tx snapshotTx) AuditStore() auditstore.Store {
	return snapshotStore{Store: auditstore.Of(tx.Tx), between: tx.between}
}

type snapshotStore struct {
	auditstore.Store
	between func()
}

func (s snapshotStore) Events(ctx context.Context, workspace uuid.UUID, visit func(auditstore.EventRow) error) error {
	s.between()
	return s.Store.Events(ctx, workspace, visit)
}
