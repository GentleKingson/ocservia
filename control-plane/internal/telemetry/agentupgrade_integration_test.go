package telemetry

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/releasecatalog"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestAgentUpgradeEligibilityGatesIntegration pins the server-side gate the
// web upgrade button consumes: eligibility needs the trusted catalog, an
// online fresh node reporting its architecture, a real upgrade target, the
// approved capability, and no scheduled upgrade.
func TestAgentUpgradeEligibilityGatesIntegration(t *testing.T) {
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
	workspaceID, nodeID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if _, err = pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'Upgrade eligibility',$2,now(),now())`, workspaceID, "upgrade-eligibility-"+workspaceID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES($1,$2,'node','active',now(),now())`, nodeID, workspaceID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		for _, statement := range []string{
			`DELETE FROM agent_upgrade_operations WHERE workspace_id=$1`,
			`DELETE FROM operations WHERE workspace_id=$1 AND request_id='upgrade-eligibility'`,
			`DELETE FROM node_capabilities WHERE node_id=$2`,
			`DELETE FROM telemetry_ingest_batches WHERE node_id=$2`,
			`DELETE FROM nodes WHERE id=$2`, `DELETE FROM workspaces WHERE id=$1`,
		} {
			_, _ = pool.Exec(cleanupCtx, statement, workspaceID, nodeID)
		}
	})
	manifest := filepath.Join(t.TempDir(), "agent-releases.json")
	if err := os.WriteFile(manifest, []byte(`{"releases":[{"version":"2.0.0","architecture":"amd64","package_sha256":"`+hex.EncodeToString(make([]byte, 32))+`"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, err := releasecatalog.Load(manifest)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	batch := testBatch(nodeID, 1, now)
	batch.Snapshot.AgentVersion = "0.1.1"
	batch.Snapshot.Architecture = "amd64"
	service := NewWithRecommendedAgentVersion(pool, "2.0.0")
	service.EnableAgentUpgradeEligibility(catalog)
	service.now = func() time.Time { return now }
	if inserted, err := service.Ingest(ctx, batch); err != nil || !inserted {
		t.Fatalf("ingest: inserted=%v err=%v", inserted, err)
	}

	node, err := service.GetNode(ctx, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if node.AgentVersionState != AgentVersionStateUpgradeAvailable || node.ConnectionState != "online" || node.Freshness != "fresh" {
		t.Fatalf("eligibility preconditions not exercised: %#v", node)
	}
	if node.AgentUpgradeEligible {
		t.Fatal("node is eligible without the approved upgrade capability")
	}

	if _, err := pool.Exec(ctx, `INSERT INTO node_capabilities(node_id,capability,approved) VALUES($1,'ocserv.agent.upgrade.v1',true)`, nodeID); err != nil {
		t.Fatal(err)
	}
	node, err = service.GetNode(ctx, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if !node.AgentUpgradeEligible {
		t.Fatalf("fully eligible node reports ineligible: %#v", node)
	}
	var encoded struct {
		AgentUpgradeEligible bool `json:"agent_upgrade_eligible"`
	}
	if err := json.Unmarshal(mustJSON(t, node), &encoded); err != nil {
		t.Fatal(err)
	}
	if !encoded.AgentUpgradeEligible {
		t.Fatal("eligibility is not part of the node wire payload")
	}

	// A scheduled upgrade removes eligibility until it reaches a terminal
	// outcome.
	operationID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `INSERT INTO operations(id,workspace_id,node_id,state,version,request_id,trace_id,created_at,updated_at) VALUES($1,$2,$3,'queued',1,'upgrade-eligibility','0123456789abcdef0123456789abcdef',now(),now())`, operationID, workspaceID, nodeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO agent_upgrade_operations(operation_id,workspace_id,node_id,target_version,package_sha256,architecture,from_version,state,created_at,updated_at) VALUES($1,$2,$3,'2.0.0',$4,'amd64','0.1.1','queued',now(),now())`, operationID, workspaceID, nodeID, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	node, err = service.GetNode(ctx, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if node.AgentUpgradeEligible {
		t.Fatal("node with a scheduled upgrade still reports eligible")
	}

	// Without the trusted catalog the gate fails closed.
	uncataloged := NewWithRecommendedAgentVersion(pool, "2.0.0")
	uncataloged.now = func() time.Time { return now }
	node, err = uncataloged.GetNode(ctx, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if node.AgentUpgradeEligible {
		t.Fatal("eligibility derived without a trusted release catalog")
	}
}
