package localslice

import (
	"context"
	"os"
	"testing"

	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestDisconnectedEventPreservesUntrustedNodeStatesIntegration(t *testing.T) {
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
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id,name,slug,created_at,updated_at) VALUES ($1,'Revoked event test',$2,now(),now())`, workspaceID, "revoked-event-"+workspaceID.String()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM workspaces WHERE id=$1`, workspaceID) })
	for _, initialStatus := range []string{"pending", "revoked"} {
		nodeID := uuid.Must(uuid.NewV7())
		if _, err := pool.Exec(ctx, `INSERT INTO nodes (id,workspace_id,name,status,created_at,updated_at) VALUES ($1,$2,$3,$4,now(),now())`, nodeID, workspaceID, initialStatus+"-node", initialStatus); err != nil {
			t.Fatal(err)
		}
		eventID := uuid.Must(uuid.NewV7())
		err = New(pool).Ingest(ctx, &transportv1.TransportEvent{EventId: eventID[:], NodeId: nodeID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_DISCONNECTED, OccurredAt: timestamppb.Now(), Traceparent: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01", Payload: []byte(initialStatus + " disconnect")})
		if err != nil {
			t.Fatal(err)
		}
		var status string
		if err := pool.QueryRow(ctx, `SELECT status FROM nodes WHERE id=$1`, nodeID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != initialStatus {
			t.Fatalf("%s node status after disconnect = %q", initialStatus, status)
		}
	}
}
