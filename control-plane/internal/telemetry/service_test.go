package telemetry

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testBatch(nodeID uuid.UUID, sequence uint64, observed time.Time) Batch {
	return Batch{
		ID: uuid.Must(uuid.NewV7()), NodeID: nodeID, Sequence: sequence, Kind: "current_health",
		Snapshot: Snapshot{ObservedAt: observed, BootID: "boot-a", AgentInstance: uuid.Must(uuid.NewV7()), AgentVersion: "0.1.0", OcservVersion: "1.3.0", OSRelease: "debian", Ocserv: json.RawMessage(`{"active_state":"active"}`), System: json.RawMessage(`{"memory_used_bytes":42}`), Path: json.RawMessage(`{"mode":"direct","rtt_ms":12}`)},
		Sessions: []Session{{ID: "session-a", Username: "alice", ClientIP: "192.0.2.1", ConnectedAt: observed.Add(-time.Minute), BytesIn: 10, BytesOut: 20}},
		IPBans:   []IPBan{{IP: "192.0.2.9"}},
		Samples:  []Sample{{SampledAt: observed, Metric: "connection_rtt_ms", Value: 12}},
	}
}

func TestValidateBatchRejectsNonCanonicalIPBan(t *testing.T) {
	now := time.Now().UTC()
	batch := testBatch(uuid.Must(uuid.NewV7()), 1, now)
	batch.IPBans[0].IP = "2001:0db8::1"
	if err := validateBatch(batch, now); err == nil {
		t.Fatal("non-canonical IP ban accepted")
	}
}

func TestValidateBatchRejectsHighCardinalityMetricNames(t *testing.T) {
	now := time.Now().UTC()
	batch := testBatch(uuid.Must(uuid.NewV7()), 1, now)
	batch.Samples[0].Metric = "session_019fc0a4"
	if err := validateBatch(batch, now); err == nil {
		t.Fatal("high-cardinality metric name accepted")
	}
}

func TestIngestOrderingRollupOfflineAndRecoveryIntegration(t *testing.T) {
	databaseURL := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	workspaceID := uuid.Must(uuid.NewV7())
	nodeID := uuid.Must(uuid.NewV7())
	_, err = pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'Telemetry',$2,now(),now())`, workspaceID, "telemetry-"+workspaceID.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES($1,$2,'node','active',now(),now())`, nodeID, workspaceID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM telemetry_security_events WHERE node_id=$1`, nodeID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM telemetry_ingest_batches WHERE node_id=$1`, nodeID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM nodes WHERE id=$1`, nodeID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM workspaces WHERE id=$1`, workspaceID)
	})
	now := time.Now().UTC().Truncate(time.Second)
	service := New(pool)
	service.now = func() time.Time { return now }
	current := testBatch(nodeID, 2, now)
	inserted, err := service.Ingest(ctx, current)
	if err != nil || !inserted {
		t.Fatalf("current ingest: inserted=%v err=%v", inserted, err)
	}
	inserted, err = service.Ingest(ctx, current)
	if err != nil || inserted {
		t.Fatalf("duplicate ingest: inserted=%v err=%v", inserted, err)
	}
	reconnected := testBatch(nodeID, current.Sequence, now.Add(time.Second))
	if inserted, err = service.Ingest(ctx, reconnected); err != nil || !inserted {
		t.Fatalf("reconnected agent sequence: inserted=%v err=%v", inserted, err)
	}
	delayed := testBatch(nodeID, 1, now.Add(-time.Minute))
	delayed.Snapshot.OcservVersion = "0.9.0"
	if inserted, err = service.Ingest(ctx, delayed); err != nil || !inserted {
		t.Fatalf("delayed ingest: %v %v", inserted, err)
	}
	node, err := service.GetNode(ctx, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if node.OcservVersion != "1.3.0" || node.SessionCount != 1 || node.Freshness != "fresh" {
		t.Fatalf("unexpected current node: %#v", node)
	}
	bans, err := service.ListIPBans(ctx, nodeID, 200)
	if err != nil || len(bans) != 1 || bans[0].IP != "192.0.2.9" {
		t.Fatalf("unexpected current IP bans: %#v %v", bans, err)
	}
	if err := service.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	history, err := service.History(ctx, nodeID, "connection_rtt_ms", "5m", now.Add(-time.Hour))
	if err != nil || len(history) == 0 {
		t.Fatalf("rollup missing: %v %v", history, err)
	}
	service.now = func() time.Time { return now.Add(OfflineAfter + 2*time.Second) }
	if err := service.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	node, err = service.GetNode(ctx, nodeID)
	if err != nil || node.ConnectionState != "offline" || node.Freshness != "stale" {
		t.Fatalf("offline state: %#v %v", node, err)
	}
	stale := testBatch(nodeID, 4, now.Add(-time.Minute))
	if inserted, err = service.Ingest(ctx, stale); err != nil || !inserted {
		t.Fatalf("stale ingest: %v %v", inserted, err)
	}
	node, err = service.GetNode(ctx, nodeID)
	if err != nil || node.ConnectionState != "offline" {
		t.Fatalf("stale telemetry revived node: %#v %v", node, err)
	}
	recovered := testBatch(nodeID, 3, now.Add(OfflineAfter+3*time.Second))
	if inserted, err = service.Ingest(ctx, recovered); err != nil || !inserted {
		t.Fatalf("recovery: %v %v", inserted, err)
	}
	node, err = service.GetNode(ctx, nodeID)
	if err != nil || node.ConnectionState != "online" {
		t.Fatalf("online recovery: %#v %v", node, err)
	}
	var partitions int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_inherits WHERE inhparent='telemetry_samples'::regclass`).Scan(&partitions); err != nil || partitions < 2 {
		t.Fatalf("monthly partition missing: %d %v", partitions, err)
	}
}
