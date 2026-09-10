package postgres

import (
	"context"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/semantictest"
	"github.com/google/uuid"
)

func TestObservedStateTransactionsIntegration(t *testing.T) {
	b := testBackend(t)
	ctx := context.Background()
	workspace, node := uuid.New(), uuid.New()
	if _, err := b.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'types',$2,now(),now())`, workspace, workspace.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES($1,$2,'types','active',now(),now())`, node, workspace); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		b.Exec(ctx, `DELETE FROM telemetry_security_events WHERE node_id=$1`, node)
		b.Exec(ctx, `DELETE FROM observed_groups WHERE node_id=$1`, node)
		b.Exec(ctx, `DELETE FROM nodes WHERE id=$1`, node)
		b.Exec(ctx, `DELETE FROM workspaces WHERE id=$1`, workspace)
	})
	semantictest.ObservedStateTransactions(t, b, node)
}
