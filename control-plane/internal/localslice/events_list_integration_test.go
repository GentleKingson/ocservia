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

func TestListEventsDescendingReadsNewestFirstIntegration(t *testing.T) {
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
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id,name,slug,created_at,updated_at) VALUES ($1,'Event order test',$2,now(),now())`, workspaceID, "event-order-"+workspaceID.String()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM transport_events WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`, workspaceID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM node_endpoint_keys WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`, workspaceID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM nodes WHERE workspace_id=$1`, workspaceID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM workspaces WHERE id=$1`, workspaceID)
	})
	nodeID := uuid.Must(uuid.NewV7())
	endpoint := integrationEndpoint(nodeID)
	if _, err := pool.Exec(ctx, `INSERT INTO nodes (id,workspace_id,name,status,created_at,updated_at) VALUES ($1,$2,'event-order-node','active',now(),now())`, nodeID, workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO node_endpoint_keys(node_id,endpoint_id,state,bound_at) VALUES($1,$2,'active',now())`, nodeID, endpoint[:]); err != nil {
		t.Fatal(err)
	}

	service := NewWithSigner(pool, integrationCommandSigner())
	eventIDs := make([]uuid.UUID, 0, 3)
	for range 3 {
		eventID := uuid.Must(uuid.NewV7())
		err := service.Ingest(ctx, &transportv1.TransportEvent{
			EventId:     eventID[:],
			NodeId:      nodeID[:],
			EndpointId:  endpoint[:],
			Type:        transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_DISCONNECTED,
			OccurredAt:  timestamppb.Now(),
			Traceparent: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01",
			Payload:     []byte("event order"),
		})
		if err != nil {
			t.Fatalf("ingest event: %v", err)
		}
		eventIDs = append(eventIDs, eventID)
	}

	service = New(pool)
	ascending, hasMore, err := service.ListEventsInWorkspace(ctx, workspaceID, uuid.Nil, 50, ListEventsAscending)
	if err != nil {
		t.Fatal(err)
	}
	if hasMore || len(ascending) != 3 || ascending[0].ID != eventIDs[0].String() || ascending[2].ID != eventIDs[2].String() {
		t.Fatalf("ascending order = %v hasMore=%v", ascending, hasMore)
	}

	newestPage, hasMore, err := service.ListEventsInWorkspace(ctx, workspaceID, uuid.Nil, 2, ListEventsDescending)
	if err != nil {
		t.Fatal(err)
	}
	if !hasMore || len(newestPage) != 2 || newestPage[0].ID != eventIDs[2].String() || newestPage[1].ID != eventIDs[1].String() {
		t.Fatalf("newest-first page = %v hasMore=%v", newestPage, hasMore)
	}

	older, hasMore, err := service.ListEventsInWorkspace(ctx, workspaceID, eventIDs[2], 50, ListEventsDescending)
	if err != nil {
		t.Fatal(err)
	}
	if hasMore || len(older) != 2 || older[0].ID != eventIDs[1].String() || older[1].ID != eventIDs[0].String() {
		t.Fatalf("descending continuation after newest = %v hasMore=%v", older, hasMore)
	}

	if _, _, err := service.ListEventsInWorkspace(ctx, workspaceID, uuid.Nil, 50, "sideways"); err == nil {
		t.Fatal("invalid order was accepted")
	}
}
