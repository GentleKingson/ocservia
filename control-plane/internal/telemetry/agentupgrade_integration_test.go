package telemetry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/attestationtest"
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
	// A deferred cleanup (not t.Cleanup) so it runs before the deferred
	// pool.Close, with per-statement arguments: pgx rejects statements
	// bound with more parameters than they reference.
	defer func() {
		cleanupCtx := context.Background()
		workspace, node := workspaceID, nodeID
		for _, statement := range []struct {
			query string
			args  []any
		}{
			{`DELETE FROM node_agent_upgrade_results WHERE node_id=$1`, []any{node}},
			{`DELETE FROM agent_upgrade_operations WHERE workspace_id=$1`, []any{workspace}},
			{`DELETE FROM operations WHERE workspace_id=$1 AND request_id='upgrade-eligibility'`, []any{workspace}},
			{`DELETE FROM node_capabilities WHERE node_id=$1`, []any{node}},
			{`DELETE FROM telemetry_ingest_batches WHERE node_id=$1`, []any{node}},
			{`DELETE FROM nodes WHERE id=$1`, []any{node}},
			{`DELETE FROM workspaces WHERE id=$1`, []any{workspace}},
		} {
			_, _ = pool.Exec(cleanupCtx, statement.query, statement.args...)
		}
	}()
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

	if _, err := pool.Exec(ctx, `INSERT INTO node_capabilities(node_id,capability,approved) VALUES($1,'ocserv.agent.upgrade.v2',true)`, nodeID); err != nil {
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
	forged := testBatch(nodeID, 2, now)
	forged.Snapshot.AgentVersion = "2.0.0"
	forged.Snapshot.Architecture = "amd64"
	forged.Snapshot.UpgradeResults = []UpgradeResult{{OperationID: operationID, State: "succeeded", TargetVersion: "2.0.0", CompletedAt: now, Detail: "forged success"}}
	if inserted, err := service.Ingest(ctx, forged); err == nil || inserted {
		t.Fatalf("forged upgrade telemetry accepted: inserted=%v err=%v", inserted, err)
	}
	var projectionState string
	var durableResults int
	if err := pool.QueryRow(ctx, `SELECT u.state,(SELECT count(*) FROM node_agent_upgrade_results WHERE operation_id=$1) FROM agent_upgrade_operations u WHERE u.operation_id=$1`, operationID).Scan(&projectionState, &durableResults); err != nil {
		t.Fatal(err)
	}
	if projectionState != "queued" || durableResults != 0 {
		t.Fatalf("forged upgrade telemetry changed durable state: projection=%q results=%d", projectionState, durableResults)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_upgrade_operations SET state='accepted',updated_at=now() WHERE operation_id=$1`, operationID); err != nil {
		t.Fatal(err)
	}
	privateKey, err := attestationtest.InstallKey(ctx, pool, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	resultDigest := sha256.Sum256([]byte("root durable result"))
	proof, err := attestationtest.UpgradeResultProof(nodeID, operationID, "2.0.0", make([]byte, 32), resultDigest[:], "succeeded", now, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	reported := testBatch(nodeID, 3, now)
	reported.Snapshot.AgentVersion = "2.0.0"
	reported.Snapshot.Architecture = "amd64"
	reported.Snapshot.UpgradeResults = []UpgradeResult{{OperationID: operationID, State: "succeeded", TargetVersion: "2.0.0", CompletedAt: now, Detail: "root success", Proof: proof}}
	if inserted, err := service.Ingest(ctx, reported); err != nil || !inserted {
		t.Fatalf("valid root-attested upgrade telemetry: inserted=%v err=%v", inserted, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM node_agent_upgrade_results WHERE operation_id=$1`, operationID).Scan(&durableResults); err != nil {
		t.Fatal(err)
	}
	if durableResults != 1 {
		t.Fatalf("valid root-attested result count = %d, want 1", durableResults)
	}
	node, err = service.GetNode(ctx, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if node.AgentUpgradeEligible {
		t.Fatal("node with a scheduled upgrade still reports eligible")
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_upgrade_operations SET state='unknown',completed_at=now(),updated_at=now() WHERE operation_id=$1`, operationID); err != nil {
		t.Fatal(err)
	}
	node, err = service.GetNode(ctx, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if !node.AgentUpgradeEligible {
		t.Fatal("terminal unknown upgrade still blocks eligibility")
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

// TestAgentUpgradeResultSurvivesKeyRotationIntegration pins the signing-time
// validity model: a durable outcome that completed before a privd key
// rotation stays verifiable when the successor key attests it afterwards,
// which is exactly the reconnect-after-rotation recovery path.
func TestAgentUpgradeResultSurvivesKeyRotationIntegration(t *testing.T) {
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
	defer func() {
		for _, statement := range []struct {
			query string
			args  []any
		}{
			{`DELETE FROM node_agent_upgrade_results WHERE node_id=$1`, []any{nodeID}},
			{`DELETE FROM agent_upgrade_operations WHERE workspace_id=$1`, []any{workspaceID}},
			{`DELETE FROM operations WHERE workspace_id=$1 AND request_id='upgrade-rotation'`, []any{workspaceID}},
			{`DELETE FROM node_privd_attestation_keys WHERE node_id=$1`, []any{nodeID}},
			{`DELETE FROM node_capabilities WHERE node_id=$1`, []any{nodeID}},
			{`DELETE FROM telemetry_ingest_batches WHERE node_id=$1`, []any{nodeID}},
			{`DELETE FROM node_observed_snapshots WHERE node_id=$1`, []any{nodeID}},
			{`DELETE FROM nodes WHERE id=$1`, []any{nodeID}},
			{`DELETE FROM workspaces WHERE id=$1`, []any{workspaceID}},
		} {
			_, _ = pool.Exec(context.Background(), statement.query, statement.args...)
		}
	}()
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'Upgrade rotation',$2,now(),now())`, workspaceID, "upgrade-rotation-"+workspaceID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES($1,$2,'node','active',now(),now())`, nodeID, workspaceID); err != nil {
		t.Fatal(err)
	}
	service := NewWithRecommendedAgentVersion(pool, "2.0.0")

	// The upgrade effect completed an hour ago; the successor key activates
	// half an hour later and attests the durable record right now.
	completedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	operationID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `INSERT INTO operations(id,workspace_id,node_id,state,version,request_id,trace_id,created_at,updated_at) VALUES($1,$2,$3,'accepted',1,'upgrade-rotation','0123456789abcdef0123456789abcdef',now(),now())`, operationID, workspaceID, nodeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO agent_upgrade_operations(operation_id,workspace_id,node_id,target_version,package_sha256,architecture,from_version,state,created_at,updated_at) VALUES($1,$2,$3,'2.0.0',$4,'amd64','0.1.1','accepted',now(),now())`, operationID, workspaceID, nodeID, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	privateKey, err := attestationtest.InstallKey(ctx, pool, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	// Rotate the activation time forward: the key now postdates the durable
	// completion, which completion-time validity would wrongly reject.
	if _, err := pool.Exec(ctx, `UPDATE node_privd_attestation_keys SET activated_at=now()-interval '30 minutes' WHERE node_id=$1`, nodeID); err != nil {
		t.Fatal(err)
	}
	resultDigest := sha256.Sum256([]byte("rotated successor key result"))
	proof, err := attestationtest.UpgradeResultProofAttestingAt(nodeID, operationID, "2.0.0", make([]byte, 32), resultDigest[:], "succeeded", completedAt, time.Now().UTC(), privateKey)
	if err != nil {
		t.Fatal(err)
	}
	batch := testBatch(nodeID, 1, time.Now().UTC())
	batch.Snapshot.AgentVersion = "2.0.0"
	batch.Snapshot.Architecture = "amd64"
	batch.Snapshot.UpgradeResults = []UpgradeResult{{OperationID: operationID, State: "succeeded", TargetVersion: "2.0.0", CompletedAt: completedAt, Detail: "attested after rotation", Proof: proof}}
	if inserted, err := service.Ingest(ctx, batch); err != nil || !inserted {
		t.Fatalf("post-rotation attestation rejected: inserted=%v err=%v", inserted, err)
	}
	var durableResults int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM node_agent_upgrade_results WHERE operation_id=$1`, operationID).Scan(&durableResults); err != nil {
		t.Fatal(err)
	}
	if durableResults != 1 {
		t.Fatalf("post-rotation durable result count = %d, want 1", durableResults)
	}
}
