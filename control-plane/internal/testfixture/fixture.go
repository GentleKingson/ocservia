package testfixture

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
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

// GrantCapability approves a capability for a node.
func GrantCapability(t testing.TB, ctx context.Context, pool *pgxpool.Pool, nodeID uuid.UUID, capability string) {
	t.Helper()
	testutil.Exec(t, ctx, pool,
		`INSERT INTO node_capabilities(node_id,capability,approved) VALUES($1,$2,true)`,
		nodeID, capability,
	)
}
