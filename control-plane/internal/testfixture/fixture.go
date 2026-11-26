package testfixture

import (
	"context"
	"crypto/sha256"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Traceparent is a canonical W3C traceparent suitable for deterministic tests.
const Traceparent = "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"

// CommandSigner returns a deterministic Ed25519 command signer seeded from seedByte.
func CommandSigner(seedByte byte) *commandauth.Signer {
	var seed [32]byte
	seed[0] = seedByte
	return commandauth.NewSignerFromSeed(seed)
}

// EndpointForNode derives a deterministic 32-byte endpoint ID from domain and nodeID.
func EndpointForNode(domain string, nodeID uuid.UUID) [32]byte {
	return sha256.Sum256(append([]byte(domain), nodeID[:]...))
}

// UUIDBytes returns a fresh UUIDv7 encoded as a 16-byte slice.
func UUIDBytes() []byte {
	id := uuid.Must(uuid.NewV7())
	return id[:]
}

// InsertWorkspace inserts a workspace row and returns its ID.
func InsertWorkspace(t testing.TB, ctx context.Context, pool *pgxpool.Pool, name, slugPrefix string) uuid.UUID {
	t.Helper()
	workspaceID := uuid.Must(uuid.NewV7())
	now := time.Now().UTC().Truncate(time.Microsecond)
	testutil.Exec(t, ctx, pool,
		`INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,$2,$3,$4,$4)`,
		workspaceID, name, slugPrefix+"-"+workspaceID.String(), now,
	)
	return workspaceID
}

// InsertActiveNode inserts an active node row and an active endpoint binding.
func InsertActiveNode(t testing.TB, ctx context.Context, pool *pgxpool.Pool, workspaceID uuid.UUID, domain string) (uuid.UUID, [32]byte) {
	t.Helper()
	nodeID := uuid.Must(uuid.NewV7())
	endpointID := EndpointForNode(domain, nodeID)
	now := time.Now().UTC().Truncate(time.Microsecond)
	testutil.Exec(t, ctx, pool,
		`INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at) VALUES($1,$2,$3,'active',1,$4,$4)`,
		nodeID, workspaceID, "node-"+nodeID.String(), now,
	)
	testutil.Exec(t, ctx, pool,
		`INSERT INTO node_endpoint_keys(node_id,endpoint_id,state,bound_at) VALUES($1,$2,'active',$3)`,
		nodeID, endpointID[:], now,
	)
	return nodeID, endpointID
}

// InsertWorkspaceAdminSession inserts an identity, workspace admin membership,
// and active auth session using PostgreSQL wall-clock timestamps.
func InsertWorkspaceAdminSession(t testing.TB, ctx context.Context, pool *pgxpool.Pool, workspaceID uuid.UUID, issuer string) (identityID, sessionID uuid.UUID) {
	t.Helper()
	identityID, sessionID = uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	for _, stmt := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO identities(id,issuer,subject,created_at,updated_at) VALUES($1,$2,$3,now(),now())`, []any{identityID, issuer, issuer + "-" + identityID.String()}},
		{`INSERT INTO workspace_admins(workspace_id,identity_id,created_at) VALUES($1,$2,now()-interval '1 minute')`, []any{workspaceID, identityID}},
		{`INSERT INTO auth_sessions(id,identity_id,expires_at,created_at) VALUES($1,$2,now()+interval '1 hour',now())`, []any{sessionID, identityID}},
	} {
		testutil.Exec(t, ctx, pool, stmt.query, stmt.args...)
	}
	return identityID, sessionID
}

// CommitFaultBackend wraps a real database.Backend to inject commit failures
// without introducing fake SQL storage.
type CommitFaultBackend struct {
	database.Backend
	CommitBeforeFail bool
	Err              error
	Always           bool
	armed            atomic.Bool
}

// Arm enables commit failure injection for the next Begin call.
func (b *CommitFaultBackend) Arm() { b.armed.Store(true) }

// Begin intercepts transaction commits when Always or Arm is set.
func (b *CommitFaultBackend) Begin(ctx context.Context, isolation database.Isolation) (database.Tx, error) {
	tx, err := b.Backend.Begin(ctx, isolation)
	if err != nil {
		return nil, err
	}
	if !b.Always && !b.armed.CompareAndSwap(true, false) {
		return tx, nil
	}
	return &commitFaultTx{Tx: tx, commitBeforeFail: b.CommitBeforeFail, err: b.Err}, nil
}

type commitFaultTx struct {
	database.Tx
	commitBeforeFail bool
	err              error
}

func (t *commitFaultTx) Commit(ctx context.Context) error {
	var err error
	if t.commitBeforeFail {
		err = t.Tx.Commit(ctx)
	} else {
		err = t.Tx.Rollback(ctx)
	}
	if err != nil {
		return err
	}
	return t.err
}

func (t *commitFaultTx) Unwrap() database.Tx { return t.Tx }
