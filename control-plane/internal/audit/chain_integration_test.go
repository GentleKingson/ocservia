package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestHashChainCheckpointAndTamperDetectionIntegration(t *testing.T) {
	runtimeURL, ownerURL := os.Getenv("OCSERV_TEST_DATABASE_URL"), os.Getenv("OCSERV_TEST_OWNER_DATABASE_URL")
	if runtimeURL == "" || ownerURL == "" {
		t.Skip("test database URLs are not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, runtimeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	owner, err := pgxpool.New(ctx, ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	workspaceID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES($1,'audit tamper',$2,now(),now())`, workspaceID, "audit-"+workspaceID.String()); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	eventID := uuid.Must(uuid.NewV7())
	if err := AppendChain(ctx, tx, ChainRecord{EventID: eventID, WorkspaceID: workspaceID, ActorType: "user", ActorID: "auditor", Action: "test", ResourceType: "workspace", ResourceID: workspaceID, RequestID: "audit-test", Reason: "original"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(pool, integrationCheckpointKey(t))
	if err := manager.Checkpoint(ctx, workspaceID); err != nil {
		t.Fatal(err)
	}
	verified, err := manager.Verify(ctx, workspaceID)
	if err != nil || !verified.Valid || !verified.Checkpoint {
		t.Fatalf("verification = %+v, %v", verified, err)
	}
	if _, err := owner.Exec(ctx, `ALTER TABLE audit_events DISABLE TRIGGER audit_events_append_only`); err != nil {
		t.Fatal(err)
	}
	var originalHash, originalMAC []byte
	var originalKeyID string
	if err := owner.QueryRow(ctx, `SELECT event_hash,event_key_id,event_mac FROM audit_events WHERE id=$1`, eventID).Scan(&originalHash, &originalKeyID, &originalMAC); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = owner.Exec(context.Background(), `UPDATE audit_events SET reason='original',event_hash=$2,event_key_id=$3,event_mac=$4 WHERE id=$1`, eventID, originalHash, originalKeyID, originalMAC)
		_, _ = owner.Exec(context.Background(), `ALTER TABLE audit_events ENABLE TRIGGER audit_events_append_only`)
	}()
	if _, err := owner.Exec(ctx, `UPDATE audit_events SET reason='tampered' WHERE id=$1`, eventID); err != nil {
		t.Fatal(err)
	}
	verified, err = manager.Verify(ctx, workspaceID)
	if err != nil || verified.Valid {
		t.Fatalf("tamper verification = %+v, %v", verified, err)
	}

	var occurredAt time.Time
	if err := owner.QueryRow(ctx, `SELECT occurred_at FROM audit_events WHERE id=$1`, eventID).Scan(&occurredAt); err != nil {
		t.Fatal(err)
	}
	payload, err := encodeChainPayload(nil, ChainRecord{EventID: eventID, WorkspaceID: workspaceID, ActorType: "user", ActorID: "auditor", Action: "test", ResourceType: "workspace", ResourceID: workspaceID, RequestID: "audit-test", Result: "intent", Reason: "tampered", At: occurredAt})
	if err != nil {
		t.Fatal(err)
	}
	recomputed := sha256.Sum256(payload)
	if _, err := owner.Exec(ctx, `UPDATE audit_events SET event_hash=$2 WHERE id=$1`, eventID, recomputed[:]); err != nil {
		t.Fatal(err)
	}
	verified, err = manager.Verify(ctx, workspaceID)
	if err != nil || verified.Valid || verified.Checkpoint {
		t.Fatalf("recomputed tamper verification = %+v, %v", verified, err)
	}
	if _, err := owner.Exec(ctx, `UPDATE audit_events SET reason='original',event_hash=$2,event_key_id='unknown-key',event_mac=$3 WHERE id=$1`, eventID, originalHash, originalMAC); err != nil {
		t.Fatal(err)
	}
	verified, err = manager.Verify(ctx, workspaceID)
	if err != nil || verified.Valid {
		t.Fatalf("unknown key verification = %+v, %v", verified, err)
	}
	flippedMAC := append([]byte(nil), originalMAC...)
	flippedMAC[0] ^= 0x01
	if _, err := owner.Exec(ctx, `UPDATE audit_events SET event_key_id=$2,event_mac=$3 WHERE id=$1`, eventID, originalKeyID, flippedMAC); err != nil {
		t.Fatal(err)
	}
	verified, err = manager.Verify(ctx, workspaceID)
	if err != nil || verified.Valid {
		t.Fatalf("flipped MAC verification = %+v, %v", verified, err)
	}
}

func TestForgedDatabaseTailCannotVerifyOrCheckpointIntegration(t *testing.T) {
	runtimeURL, ownerURL := os.Getenv("OCSERV_TEST_DATABASE_URL"), os.Getenv("OCSERV_TEST_OWNER_DATABASE_URL")
	if runtimeURL == "" || ownerURL == "" {
		t.Skip("test database URLs are not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, runtimeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	owner, err := pgxpool.New(ctx, ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	workspaceID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES($1,'audit forged tail',$2,now(),now())`, workspaceID, "audit-forged-"+workspaceID.String()); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := AppendChain(ctx, tx, ChainRecord{WorkspaceID: workspaceID, ActorType: "user", ActorID: "auditor", Action: "test", ResourceType: "workspace", ResourceID: workspaceID, RequestID: "audit-authenticated", Reason: "authenticated"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(pool, integrationCheckpointKey(t))
	if err := manager.Checkpoint(ctx, workspaceID); err != nil {
		t.Fatal(err)
	}
	var previous []byte
	if err := owner.QueryRow(ctx, `SELECT event_hash FROM audit_events WHERE workspace_id=$1 ORDER BY occurred_at DESC,id DESC LIMIT 1`, workspaceID).Scan(&previous); err != nil {
		t.Fatal(err)
	}
	forgedID := uuid.Must(uuid.NewV7())
	var occurredAt time.Time
	if err := owner.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&occurredAt); err != nil {
		t.Fatal(err)
	}
	forged := ChainRecord{EventID: forgedID, WorkspaceID: workspaceID, ActorType: "database", ActorID: "forged", Action: "forged.tail", ResourceType: "workspace", ResourceID: workspaceID, RequestID: "forged-tail", Result: "succeeded", Reason: "not application authenticated", At: occurredAt}
	payload, err := encodeChainPayload(previous, forged)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	if _, err := owner.Exec(ctx, `INSERT INTO audit_events(id,workspace_id,occurred_at,actor_type,actor_id,action,resource_type,resource_id,request_id,result,reason,previous_event_hash,event_hash,auth_version) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,0)`, forgedID, workspaceID, occurredAt, forged.ActorType, forged.ActorID, forged.Action, forged.ResourceType, forged.ResourceID, forged.RequestID, forged.Result, forged.Reason, previous, digest[:]); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = owner.Exec(context.Background(), `ALTER TABLE audit_events DISABLE TRIGGER audit_events_append_only`)
		_, _ = owner.Exec(context.Background(), `DELETE FROM audit_events WHERE id=$1`, forgedID)
		_, _ = owner.Exec(context.Background(), `ALTER TABLE audit_events ENABLE TRIGGER audit_events_append_only`)
	}()
	verified, err := manager.Verify(ctx, workspaceID)
	if err != nil || verified.Valid {
		t.Fatalf("forged tail verification = %+v, %v", verified, err)
	}
	var checkpointsBefore int
	if err := owner.QueryRow(ctx, `SELECT count(*) FROM audit_checkpoints WHERE workspace_id=$1`, workspaceID).Scan(&checkpointsBefore); err != nil {
		t.Fatal(err)
	}
	if err := manager.Checkpoint(ctx, workspaceID); err == nil {
		t.Fatal("forged unauthenticated tail was checkpointed")
	}
	var checkpointsAfter int
	if err := owner.QueryRow(ctx, `SELECT count(*) FROM audit_checkpoints WHERE workspace_id=$1`, workspaceID).Scan(&checkpointsAfter); err != nil {
		t.Fatal(err)
	}
	if checkpointsAfter != checkpointsBefore {
		t.Fatalf("checkpoint count changed from %d to %d", checkpointsBefore, checkpointsAfter)
	}
}

func TestLegacyAuditTransitionRequiresCheckpointedTailIntegration(t *testing.T) {
	runtimeURL, ownerURL := os.Getenv("OCSERV_TEST_DATABASE_URL"), os.Getenv("OCSERV_TEST_OWNER_DATABASE_URL")
	if runtimeURL == "" || ownerURL == "" {
		t.Skip("test database URLs are not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, runtimeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	owner, err := pgxpool.New(ctx, ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()

	workspaceID := uuid.Must(uuid.NewV7())
	eventID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES($1,'legacy audit transition',$2,now(),now())`, workspaceID, "audit-transition-"+workspaceID.String()); err != nil {
		t.Fatal(err)
	}
	var occurredAt time.Time
	if err := owner.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&occurredAt); err != nil {
		t.Fatal(err)
	}
	legacy := ChainRecord{EventID: eventID, WorkspaceID: workspaceID, ActorType: "controller", ActorID: "legacy", Action: "legacy.event", ResourceType: "workspace", ResourceID: workspaceID, RequestID: "legacy-event", Result: "succeeded", At: occurredAt}
	payload, err := encodeChainPayload(nil, legacy)
	if err != nil {
		t.Fatal(err)
	}
	eventHash := sha256.Sum256(payload)
	if _, err := owner.Exec(ctx, `INSERT INTO audit_events(id,workspace_id,occurred_at,actor_type,actor_id,action,resource_type,resource_id,request_id,result,event_hash,auth_version) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,0)`, eventID, workspaceID, occurredAt, legacy.ActorType, legacy.ActorID, legacy.Action, legacy.ResourceType, legacy.ResourceID, legacy.RequestID, legacy.Result, eventHash[:]); err != nil {
		t.Fatal(err)
	}
	checkpointKey := integrationCheckpointKey(t)
	manager := NewManager(pool, checkpointKey)
	if err := manager.EnsureAuthenticity(ctx); err == nil {
		t.Fatal("uncheckpointed legacy audit tail was accepted")
	}
	if _, err := owner.Exec(ctx, `INSERT INTO audit_checkpoints(id,workspace_id,through_event_id,through_event_hash,signature,created_at) VALUES($1,$2,$3,$4,$5,now())`, uuid.Must(uuid.NewV7()), workspaceID, eventID, eventHash[:], signCheckpoint(checkpointKey, workspaceID, eventID, eventHash[:])); err != nil {
		t.Fatal(err)
	}
	if err := manager.EnsureAuthenticity(ctx); err != nil {
		t.Fatal(err)
	}
	verified, err := manager.Verify(ctx, workspaceID)
	if err != nil || !verified.Valid || !verified.Checkpoint || verified.Events != 2 {
		t.Fatalf("transition verification = %+v, %v", verified, err)
	}
	var version int16
	var keyID string
	if err := owner.QueryRow(ctx, `SELECT auth_version,event_key_id FROM audit_events WHERE workspace_id=$1 ORDER BY occurred_at DESC,id DESC LIMIT 1`, workspaceID).Scan(&version, &keyID); err != nil {
		t.Fatal(err)
	}
	if version != eventAuthVersionV1 || keyID != manager.current.keyID {
		t.Fatalf("transition auth = version %d key %q", version, keyID)
	}
}

func integrationCheckpointKey(t *testing.T) []byte {
	t.Helper()
	encoded := os.Getenv("OCSERV_AUDIT_CHECKPOINT_KEY")
	if encoded == "" {
		return make([]byte, sha256.Size)
	}
	key, err := hex.DecodeString(encoded)
	if err != nil || len(key) != sha256.Size {
		t.Fatalf("invalid integration audit checkpoint key")
	}
	return key
}
