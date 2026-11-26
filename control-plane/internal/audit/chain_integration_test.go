package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/testfixture"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"github.com/google/uuid"
)

func TestHashChainCheckpointAndTamperDetectionIntegration(t *testing.T) {
	ctx := context.Background()
	pool := testfixture.OpenRuntimePool(t)
	owner := testfixture.OpenOwnerPool(t)
	workspaceID := uuid.Must(uuid.NewV7())
	testutil.Exec(t, ctx, pool, `INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES($1,'audit tamper',$2,now(),now())`, workspaceID, "audit-"+workspaceID.String())
	tx, err := pool.Begin(ctx)
	testutil.NoError(t, err)
	eventID := uuid.Must(uuid.NewV7())
	testutil.NoError(t, AppendChainTx(ctx, postgres.WrapTx(tx), ChainRecord{EventID: eventID, WorkspaceID: workspaceID, ActorType: "user", ActorID: "auditor", Action: "test", ResourceType: "workspace", ResourceID: workspaceID, RequestID: "audit-test", Reason: "original"}))
	testutil.NoError(t, tx.Commit(ctx))
	manager := NewBackendManager(postgres.WrapPool(pool), integrationCheckpointKey(t))
	testutil.NoError(t, manager.Checkpoint(ctx, workspaceID))
	verified, err := manager.Verify(ctx, workspaceID)
	if err != nil || !verified.Valid || !verified.Checkpoint {
		t.Fatalf("verification = %+v, %v", verified, err)
	}
	testutil.Exec(t, ctx, owner, `ALTER TABLE audit_events DISABLE TRIGGER audit_events_append_only`)
	var originalHash, originalMAC []byte
	var originalKeyID string
	testutil.NoError(t, owner.QueryRow(ctx, `SELECT event_hash,event_key_id,event_mac FROM audit_events WHERE id=$1`, eventID).Scan(&originalHash, &originalKeyID, &originalMAC))
	defer func() {
		_, _ = owner.Exec(context.Background(), `UPDATE audit_events SET reason='original',event_hash=$2,event_key_id=$3,event_mac=$4 WHERE id=$1`, eventID, originalHash, originalKeyID, originalMAC)
		_, _ = owner.Exec(context.Background(), `ALTER TABLE audit_events ENABLE TRIGGER audit_events_append_only`)
	}()
	testutil.Exec(t, ctx, owner, `UPDATE audit_events SET reason='tampered' WHERE id=$1`, eventID)
	verified, err = manager.Verify(ctx, workspaceID)
	if err != nil || verified.Valid {
		t.Fatalf("tamper verification = %+v, %v", verified, err)
	}

	var occurredAt time.Time
	testutil.NoError(t, owner.QueryRow(ctx, `SELECT occurred_at FROM audit_events WHERE id=$1`, eventID).Scan(&occurredAt))
	payload, err := encodeChainPayload(nil, ChainRecord{EventID: eventID, WorkspaceID: workspaceID, ActorType: "user", ActorID: "auditor", Action: "test", ResourceType: "workspace", ResourceID: workspaceID, RequestID: "audit-test", Result: "intent", Reason: "tampered", At: occurredAt})
	testutil.NoError(t, err)
	recomputed := sha256.Sum256(payload)
	testutil.Exec(t, ctx, owner, `UPDATE audit_events SET event_hash=$2 WHERE id=$1`, eventID, recomputed[:])
	verified, err = manager.Verify(ctx, workspaceID)
	if err != nil || verified.Valid || verified.Checkpoint {
		t.Fatalf("recomputed tamper verification = %+v, %v", verified, err)
	}
	testutil.Exec(t, ctx, owner, `UPDATE audit_events SET reason='original',event_hash=$2,event_key_id='unknown-key',event_mac=$3 WHERE id=$1`, eventID, originalHash, originalMAC)
	verified, err = manager.Verify(ctx, workspaceID)
	if err != nil || verified.Valid {
		t.Fatalf("unknown key verification = %+v, %v", verified, err)
	}
	flippedMAC := append([]byte(nil), originalMAC...)
	flippedMAC[0] ^= 0x01
	testutil.Exec(t, ctx, owner, `UPDATE audit_events SET event_key_id=$2,event_mac=$3 WHERE id=$1`, eventID, originalKeyID, flippedMAC)
	verified, err = manager.Verify(ctx, workspaceID)
	if err != nil || verified.Valid {
		t.Fatalf("flipped MAC verification = %+v, %v", verified, err)
	}
}

func TestForgedDatabaseTailCannotVerifyOrCheckpointIntegration(t *testing.T) {
	ctx := context.Background()
	pool := testfixture.OpenRuntimePool(t)
	owner := testfixture.OpenOwnerPool(t)
	workspaceID := uuid.Must(uuid.NewV7())
	testutil.Exec(t, ctx, pool, `INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES($1,'audit forged tail',$2,now(),now())`, workspaceID, "audit-forged-"+workspaceID.String())
	tx, err := pool.Begin(ctx)
	testutil.NoError(t, err)
	testutil.NoError(t, AppendChainTx(ctx, postgres.WrapTx(tx), ChainRecord{WorkspaceID: workspaceID, ActorType: "user", ActorID: "auditor", Action: "test", ResourceType: "workspace", ResourceID: workspaceID, RequestID: "audit-authenticated", Reason: "authenticated"}))
	testutil.NoError(t, tx.Commit(ctx))
	manager := NewBackendManager(postgres.WrapPool(pool), integrationCheckpointKey(t))
	testutil.NoError(t, manager.Checkpoint(ctx, workspaceID))
	var previous []byte
	testutil.NoError(t, owner.QueryRow(ctx, `SELECT event_hash FROM audit_events WHERE workspace_id=$1 ORDER BY occurred_at DESC,id DESC LIMIT 1`, workspaceID).Scan(&previous))
	forgedID := uuid.Must(uuid.NewV7())
	var occurredAt time.Time
	testutil.NoError(t, owner.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&occurredAt))
	forged := ChainRecord{EventID: forgedID, WorkspaceID: workspaceID, ActorType: "database", ActorID: "forged", Action: "forged.tail", ResourceType: "workspace", ResourceID: workspaceID, RequestID: "forged-tail", Result: "succeeded", Reason: "not application authenticated", At: occurredAt}
	payload, err := encodeChainPayload(previous, forged)
	testutil.NoError(t, err)
	digest := sha256.Sum256(payload)
	testutil.Exec(t, ctx, owner, `INSERT INTO audit_events(id,workspace_id,occurred_at,actor_type,actor_id,action,resource_type,resource_id,request_id,result,reason,previous_event_hash,event_hash,auth_version) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,0)`, forgedID, workspaceID, occurredAt, forged.ActorType, forged.ActorID, forged.Action, forged.ResourceType, forged.ResourceID, forged.RequestID, forged.Result, forged.Reason, previous, digest[:])
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
	testutil.NoError(t, owner.QueryRow(ctx, `SELECT count(*) FROM audit_checkpoints WHERE workspace_id=$1`, workspaceID).Scan(&checkpointsBefore))
	if err := manager.Checkpoint(ctx, workspaceID); err == nil {
		t.Fatal("forged unauthenticated tail was checkpointed")
	}
	var checkpointsAfter int
	testutil.NoError(t, owner.QueryRow(ctx, `SELECT count(*) FROM audit_checkpoints WHERE workspace_id=$1`, workspaceID).Scan(&checkpointsAfter))
	if checkpointsAfter != checkpointsBefore {
		t.Fatalf("checkpoint count changed from %d to %d", checkpointsBefore, checkpointsAfter)
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
