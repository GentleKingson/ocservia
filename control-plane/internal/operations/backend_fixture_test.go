package operations_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/audit/auditstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandlimit"
	"github.com/GentleKingson/ocservia/control-plane/internal/connectionowner/ownerstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/schedulerlease"
	"github.com/GentleKingson/ocservia/control-plane/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type outboxFixture struct {
	backend, owner database.Backend
	workspace      uuid.UUID
	signer         *commandauth.Signer
}

// Each workflow owns a disposable database. Business calls use the restricted
// runtime account; only provisioning and deliberate fault injection use owner.
func newOutboxFixture(t *testing.T) *outboxFixture {
	t.Helper()
	ctx := context.Background()
	f := &outboxFixture{workspace: uuid.Must(uuid.NewV7())}
	name := "dbtest_outbox_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	ownerURL, runtimeURL := os.Getenv("OCSERV_TEST_OWNER_DATABASE_URL"), os.Getenv("OCSERV_TEST_DATABASE_URL")
	if runtimeURL == "" {
		t.Skip("real PostgreSQL required")
	}
	if ownerURL == "" {
		t.Fatal("OCSERV_TEST_OWNER_DATABASE_URL required for disposable Outbox transaction test database")
	}
	admin, err := pgxpool.New(ctx, ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP DATABASE "+name); err != nil {
			t.Error(err)
		}
	})
	open := func(raw string) *pgxpool.Pool {
		t.Helper()
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		u.Path = "/" + name
		pool, err := pgxpool.New(ctx, u.String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		return pool
	}
	owner := open(ownerURL)
	if err := migrations.Migrate(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if err := migrations.GrantRuntimePrivileges(ctx, owner, "ocservia_app"); err != nil {
		t.Fatal(err)
	}
	f.owner, f.backend = postgres.WrapPool(owner), postgres.WrapPool(open(runtimeURL))
	f.signer, err = commandauth.NewRandomSigner()
	if err != nil {
		t.Fatal(err)
	}
	at, err := value.FromTime(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	f.exec(t, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'Outbox transaction test',$2,$3,$4)`, f.workspace, f.workspace.String(), at, at)
	return f
}

func (f *outboxFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := f.owner.Exec(context.Background(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func (f *outboxFixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var count int
	if err := f.owner.QueryRow(context.Background(), query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func (f *outboxFixture) node(t *testing.T) uuid.UUID {
	t.Helper()
	node := uuid.Must(uuid.NewV7())
	at, err := value.FromTime(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	f.exec(t, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES($1,$2,$3,'active',$4,$5)`, node, f.workspace, node.String(), at, at)
	endpoint := sha256.Sum256(node[:])
	f.exec(t, `INSERT INTO node_endpoint_keys(node_id,endpoint_id,state,bound_at) VALUES($1,$2,'active',$3)`, node, endpoint[:], at)
	return node
}

func outboxRequest(node uuid.UUID) operations.CreateRequest {
	return operations.CreateRequest{NodeID: node, IdempotencyKey: uuid.NewString(), ExpectedVersion: 1,
		Kind: operations.SyntheticEcho, Message: "Outbox transaction test", TTL: time.Hour, RequestID: uuid.NewString(),
		Traceparent: "00-" + strings.Repeat("1", 32) + "-" + strings.Repeat("2", 16) + "-01"}
}

var lostCommit = errors.New("injected lost commit acknowledgement")

type commitFaultBackend struct {
	database.Backend
	armed  atomic.Bool
	commit bool
}

func (b *commitFaultBackend) Begin(ctx context.Context, isolation database.Isolation) (database.Tx, error) {
	tx, err := b.Backend.Begin(ctx, isolation)
	if err != nil {
		return nil, err
	}
	if !b.armed.CompareAndSwap(true, false) {
		return tx, nil
	}
	return &commitFaultTx{Tx: tx, commit: b.commit}, nil
}

// Only the commit acknowledgement is injected. Every store and SQL statement
// still uses the real caller-owned transaction and restricted database user.
type commitFaultTx struct {
	database.Tx
	commit bool
}

func (t *commitFaultTx) Commit(ctx context.Context) error {
	var err error
	if t.commit {
		err = t.Tx.Commit(ctx)
	} else {
		err = t.Tx.Rollback(ctx)
	}
	if err != nil {
		return err
	}
	return lostCommit
}
func (t *commitFaultTx) WallTime(ctx context.Context) (value.Timestamp, error) {
	return database.WallTime(ctx, t.Tx)
}
func (t *commitFaultTx) TransactionTime(ctx context.Context) (value.Timestamp, error) {
	return database.TransactionTime(ctx, t.Tx)
}
func (t *commitFaultTx) OperationStore() operationstore.Store {
	s, _ := operationstore.FromTransaction(t.Tx)
	return s
}
func (t *commitFaultTx) CommandLimitStore() commandlimit.Store {
	return t.Tx.(commandlimit.Provider).CommandLimitStore()
}
func (t *commitFaultTx) AuditStore() auditstore.Store {
	return t.Tx.(interface{ AuditStore() auditstore.Store }).AuditStore()
}
func (t *commitFaultTx) ConnectionOwnerStore() ownerstore.Store {
	s, _ := ownerstore.FromTransaction(t.Tx)
	return s
}
func (t *commitFaultTx) SchedulerLeaseStore() schedulerlease.Store {
	s, _ := schedulerlease.FromTransaction(t.Tx)
	return s
}
