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

func TestAgentVersionStateListGetConsistencyIntegration(t *testing.T) {
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
	if _, err = pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'Versions',$2,now(),now())`, workspaceID, "versions-"+workspaceID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES($1,$2,'node','active',now(),now())`, nodeID, workspaceID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM telemetry_ingest_batches WHERE node_id=$1`, nodeID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM nodes WHERE id=$1`, nodeID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM workspaces WHERE id=$1`, workspaceID)
	})
	now := time.Now().UTC().Truncate(time.Second)
	batch := testBatch(nodeID, 1, now)
	batch.Snapshot.AgentVersion = "0.1.1"
	service := NewWithRecommendedAgentVersion(pool, "0.2.0")
	service.now = func() time.Time { return now }
	if inserted, err := service.Ingest(ctx, batch); err != nil || !inserted {
		t.Fatalf("ingest: inserted=%v err=%v", inserted, err)
	}
	node, err := service.GetNode(ctx, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	nodes, _, err := service.ListNodesInWorkspace(ctx, workspaceID, uuid.Nil, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].ID != nodeID.String() {
		t.Fatalf("unexpected node page: %#v", nodes)
	}
	listed := nodes[0]
	if listed.AgentVersionState != node.AgentVersionState || listed.RecommendedAgentVersion != node.RecommendedAgentVersion {
		t.Fatalf("list/get derived state disagrees: list=%#v get=%#v", listed, node)
	}
	if node.AgentVersionState != AgentVersionStateUpgradeAvailable || node.RecommendedAgentVersion != "0.2.0" {
		t.Fatalf("unexpected derived state: %#v", node)
	}
	var encoded struct {
		AgentVersionState       string `json:"agent_version_state"`
		RecommendedAgentVersion string `json:"recommended_agent_version"`
	}
	if err := json.Unmarshal(mustJSON(t, node), &encoded); err != nil {
		t.Fatal(err)
	}
	if encoded.AgentVersionState != AgentVersionStateUpgradeAvailable || encoded.RecommendedAgentVersion != "0.2.0" {
		t.Fatalf("unexpected wire fields: %#v", encoded)
	}
	unconfigured := New(pool)
	unconfigured.now = func() time.Time { return now }
	node, err = unconfigured.GetNode(ctx, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if node.AgentVersionState != AgentVersionStateUnknown || node.RecommendedAgentVersion != "" {
		t.Fatalf("missing recommendation must classify unknown: %#v", node)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
