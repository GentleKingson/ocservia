package enrollment

import (
	"bytes"
	"context"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/mysql"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	enrollmentstore "github.com/GentleKingson/ocservia/control-plane/internal/enrollment/store"
	localstore "github.com/GentleKingson/ocservia/control-plane/internal/localslice/store"
	"github.com/google/uuid"
)

func TestEnrollmentSharedStorageBackendIntegration(t *testing.T) {
	b := enrollmentBackend(t)
	ctx := context.Background()
	for i, micros := range []int64{value.NegativeInfinity, value.MinTimestamp, value.EndTimestamp - 1, value.PositiveInfinity} {
		at := value.Timestamp{Valid: true, Micros: micros}
		workspace, node := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
		labels := []string{`{"huge":1e1000,"exact":9007199254740993}`, `[null,1e1000,"member"]`, `"scalar"`, `null`}[i]
		wantJSON, err := value.ParseJSONB([]byte(labels))
		if err != nil {
			t.Fatal(err)
		}
		err = database.Within(ctx, b, database.ReadCommitted, func(tx database.Tx) error {
			local, err := localstore.Local(tx)
			if err != nil {
				return err
			}
			id, err := local.EnsureWorkspace(ctx, workspace, workspace.String(), at)
			if err != nil || id != workspace {
				t.Fatal("logical workspace", id, err)
			}
			id, err = local.EnsureWorkspace(ctx, uuid.New(), workspace.String(), value.Timestamp{Valid: true})
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
			if err := enroll.TouchNode(ctx, node, at); err != nil {
				return err
			}
			if _, err := enroll.Activate(ctx, node, labels, "fixture", at); err != nil {
				return err
			}
			if err := local.DisconnectNode(ctx, localstore.GapNode{ID: node, Traceparent: "00-11111111111111111111111111111111-2222222222222222-01"}, uuid.Must(uuid.NewV7()), at); err != nil {
				return err
			}
			_, err = enroll.Revoke(ctx, node, at)
			return err
		})
		if err != nil {
			t.Fatal("shared storage writer", micros, err)
		}
		query := `SELECT w.created_at,w.updated_at,w.archived_at,n.created_at,n.updated_at,k.bound_at,k.revoked_at,s.created_at,n.labels FROM nodes n JOIN workspaces w ON w.id=n.workspace_id JOIN node_endpoint_keys k ON k.node_id=n.id JOIN node_sealing_keys s ON s.node_id=n.id WHERE n.id=$1`
		var id any = node
		if _, my := b.(*mysql.Backend); my {
			query = `SELECT w.created_at,w.updated_at,w.archived_at,n.created_at,n.updated_at,k.bound_at,k.revoked_at,s.created_at,n.labels FROM nodes n JOIN workspaces w ON w.id=n.workspace_id JOIN node_endpoint_keys k ON k.node_id=n.id JOIN node_sealing_keys s ON s.node_id=n.id WHERE n.id=?`
			id = mysql.UUIDBytes(node)
		}
		var clocks [8]value.Timestamp
		var gotJSON value.JSONB
		if err := b.QueryRow(ctx, query, id).Scan(&clocks[0], &clocks[1], &clocks[2], &clocks[3], &clocks[4], &clocks[5], &clocks[6], &clocks[7], &gotJSON); err != nil {
			t.Fatal(err)
		}
		for i, clock := range clocks {
			want := at
			if i == 2 {
				want = value.Timestamp{}
			}
			if clock != want {
				t.Fatal("shared clock", i, clock, want)
			}
		}
		if !bytes.Equal(gotJSON.Bytes(), wantJSON.Bytes()) {
			t.Fatal("shared labels changed", string(gotJSON.Bytes()))
		}
	}
}
