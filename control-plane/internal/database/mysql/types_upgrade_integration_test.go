package mysql

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/observedstate"
	"github.com/google/uuid"
)

func seedTypeUpgradeNode(t *testing.T, b *Backend) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	workspace, node := uuid.New(), uuid.New()
	if _, err := b.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES(?,'type-upgrade',?,CURRENT_TIMESTAMP(6),CURRENT_TIMESTAMP(6))`, UUIDBytes(workspace), workspace.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES(?,?,'type-upgrade','active',CURRENT_TIMESTAMP(6),CURRENT_TIMESTAMP(6))`, UUIDBytes(node), UUIDBytes(workspace)); err != nil {
		t.Fatal(err)
	}
	return node
}

func TestRealPopulatedTypeUpgrade(t *testing.T) {
	ctx := context.Background()
	for _, old := range []bool{false, true} {
		t.Run(map[bool]string{false: "144b660", true: "f6cd0e0"}[old], func(t *testing.T) {
			b, _ := versionTwoFixture(t, old)
			node := seedTypeUpgradeNode(t, b)
			stamps := []time.Time{time.Date(1000, 1, 1, 0, 0, 0, 1e3, time.UTC), time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)}
			events := []uuid.UUID{uuid.New(), uuid.New()}
			expectedJSON := make([]value.JSONB, len(stamps))
			for i, stamp := range stamps {
				name := []string{"earliest", "latest"}[i]
				if _, err := b.Exec(ctx, `INSERT INTO observed_groups(node_id,group_name,members,revision,fingerprint,observed_at) VALUES(?,?,?,3,?,?)`, UUIDBytes(node), name, `["member",null,""]`, make([]byte, 32), stamp); err != nil {
					t.Fatal(err)
				}
				if _, err := b.Exec(ctx, `INSERT INTO telemetry_security_events(event_id,node_id,observed_at,severity,event_type,detail) VALUES(?,?,?,'info','upgrade',?)`, UUIDBytes(events[i]), UUIDBytes(node), stamp, `{"wide":1e100,"precise":123.4500,"empty":null}`); err != nil {
					t.Fatal(err)
				}
				var raw []byte
				if err := b.QueryRow(ctx, `SELECT detail FROM telemetry_security_events WHERE event_id=?`, UUIDBytes(events[i])).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				var err error
				expectedJSON[i], err = value.ParseJSONB(raw)
				if err != nil {
					t.Fatal(err)
				}
			}
			receipts := versionTwoReceipts(t, b)
			if err := b.Migrate(ctx, ""); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(receipts, versionTwoReceipts(t, b)) {
				t.Fatal("type upgrade rewrote published receipts")
			}
			if err := b.Migrate(ctx, ""); err != nil {
				t.Fatal("repeat upgrade", err)
			}
			err := database.Within(ctx, b, database.ReadCommitted, func(tx database.Tx) error {
				s, err := observedstate.FromTransaction(tx)
				if err != nil {
					return err
				}
				groups, err := s.Groups(ctx, node)
				if err != nil {
					return err
				}
				if len(groups) != 2 {
					t.Fatal("group lost")
				}
				for i, stamp := range stamps {
					logical, err := value.FromTime(stamp)
					if err != nil {
						return err
					}
					g := groups[i]
					if g.ObservedAt != logical || len(g.Members.Dimensions) != 1 || g.Members.Dimensions[0] != (value.Dimension{Length: 3, LowerBound: 1}) || len(g.Members.Elements) != 3 || g.Members.Elements[1] != nil || g.Members.Elements[0] == nil || *g.Members.Elements[0] != "member" || g.Members.Elements[2] == nil || *g.Members.Elements[2] != "" {
						t.Fatal("group value changed", g)
					}
					e, err := s.SecurityEvent(ctx, events[i])
					if err != nil {
						return err
					}
					if e.ObservedAt != logical || !bytes.Equal(e.Detail.Bytes(), expectedJSON[i].Bytes()) {
						t.Fatal("event value changed")
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRealTypeUpgradeRejectsInvalidLegacyArray(t *testing.T) {
	b, _ := versionTwoFixture(t, false)
	ctx := context.Background()
	node := seedTypeUpgradeNode(t, b)
	if _, err := b.Exec(ctx, `INSERT INTO observed_groups(node_id,group_name,members,revision,fingerprint,observed_at) VALUES(?,'legacy',?,0,?,CURRENT_TIMESTAMP(6))`, UUIDBytes(node), `["valid",{"invalid":"text array element"}]`, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	if err := b.Migrate(ctx, ""); !errors.Is(err, ErrSchema) {
		t.Fatal("invalid legacy array was not rejected", err)
	}
	if err := b.Migrate(ctx, ""); !errors.Is(err, ErrDirty) {
		t.Fatal("dirty upgrade not refused", err)
	}
	// The owner holds the migration lock while correcting the source and its
	// unverified copy. Ordinary owner connections remain blocked by the guard.
	conn, name, err := migrationConnection(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	_, repairErr := conn.ExecContext(ctx, `UPDATE observed_groups SET members='["valid","repaired"]',logical_members=NULL WHERE node_id=?`, UUIDBytes(node))
	unlockErr := releaseMigrationConnection(conn, name)
	if repairErr != nil || unlockErr != nil {
		t.Fatal(repairErr, unlockErr)
	}
	checksum, err := ManifestChecksum(b.engine)
	if err != nil {
		t.Fatal(err)
	}
	if err = b.Migrate(ctx, checksum); err != nil {
		t.Fatal("explicit type repair", err)
	}
	if err = b.ValidateSchema(ctx, 35); err != nil {
		t.Fatal(err)
	}
}
