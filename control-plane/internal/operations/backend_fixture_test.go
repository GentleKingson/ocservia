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
	"github.com/GentleKingson/ocservia/control-plane/internal/database/mysql"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/schedulerlease"
	"github.com/GentleKingson/ocservia/control-plane/migrations"
	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type outboxFixture struct {
	backend, owner database.Backend
	mysql          bool
	workspace      uuid.UUID
	signer         *commandauth.Signer
}

// Each workflow owns a disposable database. Business calls use the restricted
// runtime account; only provisioning and deliberate fault injection use owner.
func newOutboxFixture(t *testing.T) *outboxFixture {
	t.Helper()
	ctx := context.Background()
	f := &outboxFixture{workspace: uuid.Must(uuid.NewV7())}
	name := "pr04_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if dsn := os.Getenv("PR02_DSN"); dsn != "" {
		f.mysql = true
		opts := mysql.Options{Engine: mysql.Engine(os.Getenv("PR02_ENGINE")), Environment: "test", DSN: dsn}
		admin, err := mysql.Open(ctx, opts)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { admin.Close() })
		if _, err := admin.Exec(ctx, "CREATE DATABASE `"+name+"`"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if _, err := admin.Exec(ctx, "DROP DATABASE `"+name+"`"); err != nil {
				t.Error(err)
			}
		})
		if _, err := admin.Exec(ctx, "GRANT ALL ON `"+name+"`.* TO 'ocservia_owner'@'%' WITH GRANT OPTION"); err != nil {
			t.Fatal(err)
		}
		cfg, err := driver.ParseDSN(dsn)
		if err != nil {
			t.Fatal("invalid fixture DSN")
		}
		cfg.DBName, cfg.User, cfg.Passwd = name, "ocservia_owner", "pr02-owner-test-only"
		opts.DSN = cfg.FormatDSN()
		owner, err := mysql.Open(ctx, opts)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { owner.Close() })
		if err := owner.Migrate(ctx, ""); err != nil {
			t.Fatal(err)
		}
		if err := owner.GrantTestPrivileges(ctx); err != nil {
			t.Fatal(err)
		}
		cfg.User, cfg.Passwd = "ocservia_app", "pr02-runtime-test-only"
		opts.DSN = cfg.FormatDSN()
		runtime, err := mysql.Open(ctx, opts)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { runtime.Close() })
		f.backend, f.owner = runtime, owner
	} else {
		ownerURL, runtimeURL := os.Getenv("OCSERV_TEST_OWNER_DATABASE_URL"), os.Getenv("OCSERV_TEST_DATABASE_URL")
		if runtimeURL == "" {
			t.Skip("real PostgreSQL or PR02 database required")
		}
		if ownerURL == "" {
			t.Fatal("OCSERV_TEST_OWNER_DATABASE_URL required for disposable PR04 database")
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
	}
	var err error
	f.signer, err = commandauth.NewRandomSigner()
	if err != nil {
		t.Fatal(err)
	}
	at, err := value.FromTime(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	f.exec(t, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'PR04',$2,$3,$4)`,
		`INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES(?,'PR04',?,?,?)`, f.workspace, f.workspace.String(), at, at)
	return f
}

func (f *outboxFixture) query(pg, my string, args []any) (string, []any) {
	if !f.mysql {
		return pg, args
	}
	converted := append([]any(nil), args...)
	for i, arg := range converted {
		if id, ok := arg.(uuid.UUID); ok {
			converted[i] = mysql.UUIDBytes(id)
		}
	}
	return my, converted
}

func (f *outboxFixture) exec(t *testing.T, pg, my string, args ...any) {
	t.Helper()
	q, a := f.query(pg, my, args)
	if _, err := f.owner.Exec(context.Background(), q, a...); err != nil {
		t.Fatal(err)
	}
}

func (f *outboxFixture) count(t *testing.T, pg, my string, args ...any) int {
	t.Helper()
	q, a := f.query(pg, my, args)
	var count int
	if err := f.owner.QueryRow(context.Background(), q, a...).Scan(&count); err != nil {
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
	f.exec(t, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES($1,$2,$3,'active',$4,$5)`,
		`INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES(?,?,?,'active',?,?)`, node, f.workspace, node.String(), at, at)
	endpoint := sha256.Sum256(node[:])
	f.exec(t, `INSERT INTO node_endpoint_keys(node_id,endpoint_id,state,bound_at) VALUES($1,$2,'active',$3)`,
		`INSERT INTO node_endpoint_keys(node_id,endpoint_id,state,bound_at) VALUES(?,?,'active',?)`, node, endpoint[:], at)
	return node
}

func outboxRequest(node uuid.UUID) operations.CreateRequest {
	return operations.CreateRequest{NodeID: node, IdempotencyKey: uuid.NewString(), ExpectedVersion: 1,
		Kind: operations.SyntheticEcho, Message: "PR04", TTL: time.Hour, RequestID: uuid.NewString(),
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
