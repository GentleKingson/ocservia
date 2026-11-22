package enrollment

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	enrollmentstore "github.com/GentleKingson/ocservia/control-plane/internal/enrollment/store"
	eventstore "github.com/GentleKingson/ocservia/control-plane/internal/transportevents/store"
	"github.com/google/uuid"
)

func TestEnrollmentSharedStorageBackendIntegration(t *testing.T) {
	b := enrollmentBackend(t)
	ctx := context.Background()
	stamps := []time.Time{
		time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC),
		time.Now().UTC().Truncate(time.Microsecond),
	}
	for i, at := range stamps {
		workspace, node := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
		labels := []string{`{"env":"prod","count":42}`, `["member"]`}[i]
		err := database.Within(ctx, b, database.ReadCommitted, func(tx database.Tx) error {
			local, err := eventstore.FromTransaction(tx)
			if err != nil {
				return err
			}
			id, err := local.EnsureWorkspace(ctx, workspace, workspace.String(), at)
			if err != nil || id != workspace {
				t.Fatal("workspace", id, err)
			}
			id, err = local.EnsureWorkspace(ctx, uuid.New(), workspace.String(), at)
			if err != nil || id != workspace {
				t.Fatal("existing workspace", id, err)
			}
			enroll, err := enrollmentstore.Enrollment(tx)
			if err != nil {
				return err
			}
			if err := enroll.InsertNode(ctx, node, workspace, node.String(), at); err != nil {
				return err
			}
			if err := enroll.InsertEndpoint(ctx, node, append(node[:], node[:]...), at); err != nil {
				return err
			}
			if err := enroll.InsertSealingKey(ctx, node, enrollmentstore.SealingKey{Purpose: 1, Version: 1, ID: "fixture", Digest: bytes.Repeat([]byte{1}, 32)}, at); err != nil {
				return err
			}
			if _, err := enroll.Activate(ctx, node, labels, "fixture", at); err != nil {
				return err
			}
			if err := local.DisconnectNode(ctx, eventstore.GapNode{ID: node, Traceparent: "00-11111111111111111111111111111111-2222222222222222-01"}, uuid.Must(uuid.NewV7()), at); err != nil {
				return err
			}
			_, err = enroll.Revoke(ctx, node, at)
			return err
		})
		if err != nil {
			t.Fatal("shared storage writer", at, err)
		}
		const query = `SELECT w.created_at,w.updated_at,w.archived_at,n.created_at,n.updated_at,k.bound_at,k.revoked_at,s.created_at,n.labels FROM nodes n JOIN workspaces w ON w.id=n.workspace_id JOIN node_endpoint_keys k ON k.node_id=n.id JOIN node_sealing_keys s ON s.node_id=n.id WHERE n.id=$1`
		var clocks [8]*time.Time
		var gotJSON json.RawMessage
		if err := b.QueryRow(ctx, query, node).Scan(&clocks[0], &clocks[1], &clocks[2], &clocks[3], &clocks[4], &clocks[5], &clocks[6], &clocks[7], &gotJSON); err != nil {
			t.Fatal(err)
		}
		for idx, clock := range clocks {
			if idx == 2 {
				if clock != nil {
					t.Fatal("archived_at should be NULL", clock)
				}
				continue
			}
			if clock == nil || !clock.Equal(at) {
				t.Fatal("shared clock", idx, clock, at)
			}
		}
		if len(gotJSON) == 0 {
			t.Fatal("shared labels missing")
		}
	}
}
