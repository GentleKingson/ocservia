package localslice

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestOperationSummaryInWorkspaceCountsAllStatesIntegration(t *testing.T) {
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
	otherWorkspaceID := uuid.Must(uuid.NewV7())
	for _, workspace := range []uuid.UUID{workspaceID, otherWorkspaceID} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id,name,slug,created_at,updated_at) VALUES ($1,'Operation summary test',$2,now(),now())`, workspace, "operation-summary-"+workspace.String()); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM operations WHERE workspace_id IN($1,$2)`, workspaceID, otherWorkspaceID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM workspaces WHERE id IN($1,$2)`, workspaceID, otherWorkspaceID)
	})

	insert := func(workspace uuid.UUID, state string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO operations (id,workspace_id,state,request_id,created_at,updated_at) VALUES ($1,$2,$3,$4,now(),now())`, uuid.Must(uuid.NewV7()), workspace, state, "operation-summary-test"); err != nil {
			t.Fatalf("insert %s operation: %v", state, err)
		}
	}
	for _, state := range []string{"draft", "queued", "dispatched", "accepted", "running", "offline_pending"} {
		insert(workspaceID, state)
	}
	insert(workspaceID, "unknown")
	insert(workspaceID, "unknown")
	for _, state := range []string{"succeeded", "failed", "expired", "rolled_back", "drifted", "superseded"} {
		insert(workspaceID, state)
	}
	insert(otherWorkspaceID, "queued")
	insert(otherWorkspaceID, "unknown")

	service := New(pool)
	summary, err := service.OperationSummaryInWorkspace(ctx, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Active != 6 || summary.Unknown != 2 {
		t.Fatalf("workspace summary = %+v, want active=6 unknown=2", summary)
	}

	otherSummary, err := service.OperationSummaryInWorkspace(ctx, otherWorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if otherSummary.Active != 1 || otherSummary.Unknown != 1 {
		t.Fatalf("other workspace summary = %+v, want active=1 unknown=1", otherSummary)
	}
}
