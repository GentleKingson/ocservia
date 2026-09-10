package mysql

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/releasecatalog"
	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
)

func TestRealRolloutCreation(t *testing.T) {
	owner, _, options := migrateFixture(t)
	ctx := context.Background()
	id := func() uuid.UUID { return uuid.Must(uuid.NewV7()) }
	workspace, actor, approver, session, approval, rollout := id(), id(), id(), id(), id(), id()
	nodes := []uuid.UUID{id(), id(), id(), id()}
	now := time.Now().UTC().Truncate(time.Microsecond)
	stamp := fixtureTimestamp(t, now)
	infinity := value.Timestamp{Valid: true, Micros: value.PositiveInfinity}
	run := func(q string, args ...any) {
		t.Helper()
		if _, err := owner.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	run(`INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES(?,'rollout',?,?,?)`, UUIDBytes(workspace), workspace.String(), fixtureTimestamp(t, now), fixtureTimestamp(t, now))
	for _, identity := range []uuid.UUID{actor, approver} {
		run(`INSERT INTO identities(id,issuer,subject,created_at,updated_at)VALUES(?,'rollout',?,?,?)`, UUIDBytes(identity), identity.String(), stamp, stamp)
	}
	run(`INSERT INTO auth_sessions(id,identity_id,expires_at,created_at)VALUES(?,?,?,?)`, UUIDBytes(session), UUIDBytes(actor), infinity, stamp)
	for i, node := range nodes {
		status, version := "active", "1.2.0"
		if i == 1 {
			status = "offline"
		}
		if i == 2 {
			version = "2.0.0"
		}
		run(`INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at)VALUES(?,?,?,?,?,?)`, UUIDBytes(node), UUIDBytes(workspace), node.String(), status, fixtureTimestamp(t, now), fixtureTimestamp(t, now))
		run("INSERT INTO node_observed_snapshots(node_id,observed_at,received_at,boot_id,agent_instance_id,agent_version,ocserv_version,os_release,architecture,ocserv,`system`,path,last_heartbeat_at)VALUES(?,?,?,'boot',?,?,'1.3.0','test','amd64','{}','{}','{}',?)", UUIDBytes(node), stamp, stamp, UUIDBytes(uuid.New()), version, infinity)
		run(`INSERT INTO node_capabilities(node_id,capability,approved)VALUES(?,'ocserv.agent.upgrade.v2',true)`, UUIDBytes(node))
		if i == 0 || i == 3 {
			credential := id()
			digest := sha256.Sum256([]byte(node.String()))
			run(`INSERT INTO node_capabilities(node_id,capability,approved)VALUES(?,'privd_result_attestation_v1',true)`, UUIDBytes(node))
			run(`INSERT INTO privd_attestation_enrollment_credentials(id,node_id,secret_sha256,controller_nonce,credential_context_sha256,expires_at,consumed_at,created_by_identity_id,created_by_session_id,created_at)VALUES(?,?,?,?,?,?,?,?,?,?)`, UUIDBytes(credential), UUIDBytes(node), digest[:], digest[:], digest[:], infinity, stamp, UUIDBytes(actor), UUIDBytes(session), stamp)
			run(`INSERT INTO node_privd_attestation_keys(node_id,key_id,algorithm,public_key,state,created_at,approved_at,activated_at,valid_until,registration_credential_id)VALUES(?,?,'ed25519',?,'active',?,?,?,?,?)`, UUIDBytes(node), "ed25519-sha256:"+hex.EncodeToString(digest[:]), digest[:], stamp, stamp, stamp, infinity, UUIDBytes(credential))
		}
	}
	hash, summary := approvals.AgentRolloutBinding("2.0.0", nodes, 2, true)
	run(`INSERT INTO approval_requests(id,workspace_id,requester_id,approver_id,action,resource_type,resource_id,reason,status,expires_at,created_at,request_hash,request_summary)VALUES(?,?,?,?,'agent.rollout','batch_operation',?,'rollout test','approved',?,?,?,?)`, UUIDBytes(approval), UUIDBytes(workspace), UUIDBytes(actor), UUIDBytes(approver), UUIDBytes(rollout), infinity, stamp, hash, summary)
	if err := owner.GrantTestPrivileges(ctx); err != nil {
		t.Fatal(err)
	}
	cfg, err := driver.ParseDSN(options.DSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.User, cfg.Passwd = "ocservia_app", "pr02-runtime-test-only"
	options.DSN = cfg.FormatDSN()
	backend, err := Open(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	manifest := filepath.Join(t.TempDir(), "releases.json")
	if err := os.WriteFile(manifest, []byte(`{"releases":[{"version":"2.0.0","architecture":"amd64","package_sha256":"`+strings.Repeat("ab", 32)+`"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	catalog, err := releasecatalog.Load(manifest)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := commandauth.NewRandomSigner()
	if err != nil {
		t.Fatal(err)
	}
	service := operations.NewBackend(backend, 50, signer)
	service.EnableReleaseCatalog(catalog)
	request := operations.CreateAgentRolloutRequest{WorkspaceID: workspace, TargetVersion: "2.0.0", NodeIDs: nodes, BatchSize: 2, StopOnFailure: true, Reason: "rollout test", ApprovalID: approval, ActorID: actor.String(), ActorIdentityID: actor, ActorSessionID: session, IdempotencyKey: "rollout-create ", RequestID: rollout.String(), Traceparent: "00-11111111111111111111111111111111-2222222222222222-01"}
	run(`CREATE TRIGGER rollout_audit_failure BEFORE INSERT ON audit_events FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='isolated rollout audit failure'`)
	_, _, err = service.CreateAgentRollout(ctx, request)
	run(`DROP TRIGGER rollout_audit_failure`)
	if err == nil {
		t.Fatal("rollout committed without audit")
	}
	var count int
	var status string
	if err := owner.QueryRow(ctx, `SELECT count(*) FROM agent_rollouts WHERE id=?`, UUIDBytes(rollout)).Scan(&count); err != nil || count != 0 {
		t.Fatal("rollout rollback", count, err)
	}
	if err := owner.QueryRow(ctx, `SELECT status FROM approval_requests WHERE id=?`, UUIDBytes(approval)).Scan(&status); err != nil || status != "approved" {
		t.Fatal("approval rollback", status, err)
	}
	got, replay, err := service.CreateAgentRollout(ctx, request)
	if err != nil || replay || got.ID != rollout.String() || len(got.Nodes) != 2 || got.Nodes[0].NodeID != nodes[0].String() || len(got.Excluded) != 2 {
		t.Fatalf("create=%+v replay=%v err=%v", got, replay, err)
	}
	_, replay, err = service.CreateAgentRollout(ctx, request)
	if err != nil || !replay {
		t.Fatal("replay", replay, err)
	}
	request.BatchSize = 1
	if _, _, err := service.CreateAgentRollout(ctx, request); !errors.Is(err, operations.ErrIdempotencyConflict) {
		t.Fatal("conflict", err)
	}
	if err := database.Within(ctx, backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := operationstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		_, _, err = store.FindRollout(ctx, workspace, "rollout-create")
		if !errors.Is(err, database.ErrNotFound) {
			t.Fatalf("trailing-space key aliased: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := owner.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE resource_id=? AND action='agent.rollout'`, UUIDBytes(rollout)).Scan(&count); err != nil || count != 1 {
		t.Fatal("rollout intent audit", count, err)
	}
	if err := owner.QueryRow(ctx, `SELECT status FROM approval_requests WHERE id=?`, UUIDBytes(approval)).Scan(&status); err != nil || status != "consumed" {
		t.Fatal("rollout approval", status, err)
	}
	negative := value.Timestamp{Valid: true, Micros: value.NegativeInfinity}
	run(`UPDATE agent_rollouts SET created_at=?,updated_at=?,exclusions='[null,[1.25],{"extra":9007199254740993}]' WHERE id=?`, negative, infinity, UUIDBytes(rollout))
	got, err = service.GetAgentRollout(ctx, rollout)
	if err != nil || got.CreatedAt != negative || got.UpdatedAt != infinity || len(got.Excluded) != 3 || !strings.Contains(string(got.Excluded[2]), "9007199254740993") {
		t.Fatal("logical rollout read", got, err)
	}
	listed, err := service.ListAgentRollouts(ctx, workspace, 10)
	if err != nil || len(listed) != 1 || listed[0].ID != rollout.String() {
		t.Fatal("rollout list", listed, err)
	}
	ws, err := service.RolloutWorkspace(ctx, rollout)
	if err != nil || ws != workspace {
		t.Fatal("rollout workspace", ws, err)
	}
	for _, check := range []struct {
		raw   string
		valid bool
	}{
		{"[1e1000]", true},
		{"[" + strings.Repeat("null,", 499) + "null]", true},
		{"[" + strings.Repeat("null,", 500) + "null]", false},
		{"{}", false},
		{`["\u0000"]`, false},
	} {
		var valid bool
		if err := owner.QueryRow(ctx, `SELECT ocserv_rollout_exclusions_valid(?)`, []byte(check.raw)).Scan(&valid); err != nil || valid != check.valid {
			t.Fatal("exclusion validator", valid, check.valid, err)
		}
		_, err := owner.Exec(ctx, `UPDATE agent_rollouts SET exclusions=? WHERE id=?`, []byte(check.raw), UUIDBytes(rollout))
		if (err == nil) != check.valid {
			t.Fatal("exclusion writer guard", check.valid, err)
		}
	}
	run(`UPDATE agent_rollout_nodes SET dispatch_lease_until=? WHERE rollout_id=? AND node_id=?`, infinity, UUIDBytes(rollout), UUIDBytes(nodes[0]))
	if err := service.AdvanceAgentRollouts(ctx); err != nil {
		t.Fatal(err)
	}
	got, err = service.GetAgentRollout(ctx, rollout)
	if err != nil || got.State != "running" || got.Nodes[0].OperationID != "" {
		t.Fatal("infinite claim fence", got, err)
	}
	run(`UPDATE agent_rollout_nodes SET dispatch_lease_until=? WHERE rollout_id=? AND node_id=?`, negative, UUIDBytes(rollout), UUIDBytes(nodes[0]))
	run(`UPDATE nodes SET status='offline' WHERE id=?`, UUIDBytes(nodes[0]))
	if err := service.AdvanceAgentRollouts(ctx); err != nil {
		t.Fatal(err)
	}
	got, err = service.GetAgentRollout(ctx, rollout)
	if err != nil || got.State != "paused" || got.PauseCode != "node_skipped" || got.Nodes[0].State != "skipped" || got.Nodes[1].State != "pending" {
		t.Fatal("canary pause", got, err)
	}
	run(`UPDATE nodes SET status='active' WHERE id=?`, UUIDBytes(nodes[0]))
	if _, err := service.ResumeAgentRollout(ctx, rollout, actor.String(), actor, session, rollout.String(), request.Traceparent); err != nil {
		t.Fatal(err)
	}
	if err := service.AdvanceAgentRollouts(ctx); err != nil {
		t.Fatal(err)
	}
	for index, node := range []uuid.UUID{nodes[0], nodes[3]} {
		got, err = service.GetAgentRollout(ctx, rollout)
		if err != nil || got.State != "running" || got.CurrentBatch != index || got.Nodes[index].State != "running" || got.Nodes[index].OperationID == "" {
			t.Fatal("rollout dispatch", index, got, err)
		}
		if index == 0 && got.Nodes[1].OperationID != "" {
			t.Fatal("batch bypassed canary")
		}
		operation := uuid.MustParse(got.Nodes[index].OperationID)
		// Durable receipt projections are seeded here; transport verification
		// remains a separate Controller E2E acceptance requirement.
		run(`UPDATE operations SET state='accepted' WHERE id=?`, UUIDBytes(operation))
		run(`UPDATE commands SET state='accepted' WHERE operation_id=?`, UUIDBytes(operation))
		run(`UPDATE agent_upgrade_operations SET state='accepted',scheduled_at=? WHERE operation_id=?`, stamp, UUIDBytes(operation))
		run(`UPDATE node_observed_snapshots SET agent_version='2.0.0' WHERE node_id=?`, UUIDBytes(node))
		run(`INSERT INTO node_agent_upgrade_results(operation_id,node_id,state,target_version,detail,completed_at,reported_at,privileged_result_proof)VALUES(?,?,'succeeded','2.0.0','',?,?,?)`, UUIDBytes(operation), UUIDBytes(node), stamp, stamp, []byte("verified fixture"))
		if err := service.ReconcileAgentUpgrades(ctx); err != nil {
			t.Fatal(err)
		}
		if err := service.AdvanceAgentRollouts(ctx); err != nil {
			t.Fatal(err)
		}
	}
	got, err = service.GetAgentRollout(ctx, rollout)
	if err != nil || got.State != "succeeded" || got.Nodes[0].State != "succeeded" || got.Nodes[1].State != "succeeded" {
		t.Fatal("rollout terminal", got, err)
	}
	if err := service.AdvanceAgentRollouts(ctx); err != nil {
		t.Fatal(err)
	}
	if err := owner.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE resource_id=? AND action='agent.rollout' AND after_summary LIKE '%rollout_succeeded%'`, UUIDBytes(rollout)).Scan(&count); err != nil || count != 1 {
		t.Fatal("rollout terminal audit", count, err)
	}
}
