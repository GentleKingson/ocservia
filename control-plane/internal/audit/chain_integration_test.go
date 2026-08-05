package audit

import (
	"context"
	"os"
	"testing"

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
	manager := NewManager(pool, make([]byte, 32))
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
	defer func() {
		_, _ = owner.Exec(context.Background(), `UPDATE audit_events SET reason='original' WHERE id=$1`, eventID)
		_, _ = owner.Exec(context.Background(), `ALTER TABLE audit_events ENABLE TRIGGER audit_events_append_only`)
	}()
	if _, err := owner.Exec(ctx, `UPDATE audit_events SET reason='tampered' WHERE id=$1`, eventID); err != nil {
		t.Fatal(err)
	}
	verified, err = manager.Verify(ctx, workspaceID)
	if err != nil || verified.Valid {
		t.Fatalf("tamper verification = %+v, %v", verified, err)
	}
}
