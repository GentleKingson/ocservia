package mysql

import (
	"context"
	"errors"
	"testing"

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
	if err = b.ValidateSchema(ctx); err != nil {
		t.Fatal(err)
	}
}
