package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/coordination"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"

	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type checkpointFenceFunc func(context.Context, database.Tx) error

func (f checkpointFenceFunc) AssertTransaction(ctx context.Context, tx database.Tx) error {
	return f(ctx, tx)
}

func TestCheckpointConcurrentLeaseRenewalIntegration(t *testing.T) {
	runtimeURL := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if runtimeURL == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, runtimeURL)
	testutil.NoError(t, err)
	defer pool.Close()
	backend := postgres.WrapPool(pool)
	for _, test := range []struct {
		name     string
		attempts int
		want     error
	}{
		{"renew once", 2, nil},
		{"renew every attempt", 3, database.ErrSerialization},
		{"lose leadership", 2, coordination.ErrNotLeader},
	} {
		t.Run(test.name, func(t *testing.T) {
			testutil.Exec(t, ctx, pool, `UPDATE scheduler_leadership SET lease_until=now()`)
			defer pool.Exec(context.Background(), `UPDATE scheduler_leadership SET lease_until=now()`)
			identity, err := coordination.NewIdentity()
			testutil.NoError(t, err)
			session, err := coordination.AcquireBackend(ctx, backend, identity, time.Minute)
			testutil.NoError(t, err)
			workspaceID := uuid.Must(uuid.NewV7())
			testutil.Exec(t, ctx, pool, `INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES($1,'checkpoint renewal',$2,now(),now())`, workspaceID, "audit-renew-"+workspaceID.String())
			testutil.NoError(t, database.Within(ctx, backend, database.ReadCommitted, func(tx database.Tx) error {
				return AppendChainTx(ctx, tx, ChainRecord{WorkspaceID: workspaceID, ActorType: "user", ActorID: "auditor", Action: "test", ResourceType: "workspace", ResourceID: workspaceID, RequestID: "checkpoint-renewal"})
			}))
			attempts := 0
			fence := checkpointFenceFunc(func(ctx context.Context, tx database.Tx) error {
				attempts++
				// The checkpoint has read its snapshot and inserted its row. Renew
				// on another connection before the real pre-commit fence query.
				conflict := attempts == 1 || test.name == "renew every attempt"
				if conflict {
					testutil.NoError(t, session.RenewBackend(ctx, backend))
				}
				if test.name == "lose leadership" && attempts == 1 {
					testutil.Exec(t, ctx, pool, `UPDATE scheduler_leadership SET lease_until=now()`)
				}
				err := session.AssertTransaction(ctx, tx)
				if conflict && !errors.Is(err, database.ErrSerialization) {
					t.Fatalf("expected real snapshot conflict, got %v", err)
				}
				return err
			})
			manager := NewBackendManager(backend, integrationCheckpointKey(t))
			err = manager.Checkpoint(coordination.WithFence(ctx, fence), workspaceID)
			if !errors.Is(err, test.want) || attempts != test.attempts {
				t.Fatalf("checkpoint = %v after %d attempts; want %v after %d", err, attempts, test.want, test.attempts)
			}
			var count int
			testutil.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM audit_checkpoints WHERE workspace_id=$1`, workspaceID).Scan(&count))
			wantCount := 0
			if test.want == nil {
				wantCount = 1
				verified, err := manager.Verify(ctx, workspaceID)
				if err != nil || !verified.Valid || !verified.Checkpoint {
					t.Fatalf("verification = %+v, %v", verified, err)
				}
			}
			if count != wantCount {
				t.Fatalf("persisted %d checkpoints, want %d", count, wantCount)
			}
		})
	}
}

func TestHashChainCheckpointAndTamperDetectionIntegration(t *testing.T) {
	runtimeURL, ownerURL := os.Getenv("OCSERV_TEST_DATABASE_URL"), os.Getenv("OCSERV_TEST_OWNER_DATABASE_URL")
	if runtimeURL == "" || ownerURL == "" {
		t.Skip("test database URLs are not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, runtimeURL)
	testutil.NoError(t, err)
	defer pool.Close()
	owner, err := pgxpool.New(ctx, ownerURL)
	testutil.NoError(t, err)
	defer owner.Close()
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
	runtimeURL, ownerURL := os.Getenv("OCSERV_TEST_DATABASE_URL"), os.Getenv("OCSERV_TEST_OWNER_DATABASE_URL")
	if runtimeURL == "" || ownerURL == "" {
		t.Skip("test database URLs are not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, runtimeURL)
	testutil.NoError(t, err)
	defer pool.Close()
	owner, err := pgxpool.New(ctx, ownerURL)
	testutil.NoError(t, err)
	defer owner.Close()
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

func TestLegacyAuditTransitionRequiresCheckpointedTailIntegration(t *testing.T) {
	runtimeURL, ownerURL := os.Getenv("OCSERV_TEST_DATABASE_URL"), os.Getenv("OCSERV_TEST_OWNER_DATABASE_URL")
	if runtimeURL == "" || ownerURL == "" {
		t.Skip("test database URLs are not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, runtimeURL)
	testutil.NoError(t, err)
	defer pool.Close()
	owner, err := pgxpool.New(ctx, ownerURL)
	testutil.NoError(t, err)
	defer owner.Close()

	workspaceID := uuid.Must(uuid.NewV7())
	eventID := uuid.Must(uuid.NewV7())
	testutil.Exec(t, ctx, pool, `INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES($1,'legacy audit transition',$2,now(),now())`, workspaceID, "audit-transition-"+workspaceID.String())
	var occurredAt time.Time
	testutil.NoError(t, owner.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&occurredAt))
	legacy := ChainRecord{EventID: eventID, WorkspaceID: workspaceID, ActorType: "controller", ActorID: "legacy", Action: "legacy.event", ResourceType: "workspace", ResourceID: workspaceID, RequestID: "legacy-event", Result: "succeeded", At: occurredAt}
	payload, err := encodeChainPayload(nil, legacy)
	testutil.NoError(t, err)
	eventHash := sha256.Sum256(payload)
	testutil.Exec(t, ctx, owner, `INSERT INTO audit_events(id,workspace_id,occurred_at,actor_type,actor_id,action,resource_type,resource_id,request_id,result,event_hash,auth_version) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,0)`, eventID, workspaceID, occurredAt, legacy.ActorType, legacy.ActorID, legacy.Action, legacy.ResourceType, legacy.ResourceID, legacy.RequestID, legacy.Result, eventHash[:])
	checkpointKey := integrationCheckpointKey(t)
	manager := NewBackendManager(postgres.WrapPool(pool), checkpointKey)
	if err := manager.EnsureAuthenticity(ctx); err == nil {
		t.Fatal("uncheckpointed legacy audit tail was accepted")
	}
	testutil.Exec(t, ctx, owner, `INSERT INTO audit_checkpoints(id,workspace_id,through_event_id,through_event_hash,signature,created_at) VALUES($1,$2,$3,$4,$5,now())`, uuid.Must(uuid.NewV7()), workspaceID, eventID, eventHash[:], signCheckpoint(checkpointKey, workspaceID, eventID, eventHash[:]))
	testutil.NoError(t, manager.EnsureAuthenticity(ctx))
	verified, err := manager.Verify(ctx, workspaceID)
	if err != nil || !verified.Valid || !verified.Checkpoint || verified.Events != 2 {
		t.Fatalf("transition verification = %+v, %v", verified, err)
	}
	var version int16
	var keyID string
	testutil.NoError(t, owner.QueryRow(ctx, `SELECT auth_version,event_key_id FROM audit_events WHERE workspace_id=$1 ORDER BY occurred_at DESC,id DESC LIMIT 1`, workspaceID).Scan(&version, &keyID))
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
