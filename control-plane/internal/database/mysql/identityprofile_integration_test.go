package mysql

import (
	"context"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/semantictest"
	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
)

func TestRealIdentityProfiles(t *testing.T) {
	owner, _, options := migrateFixture(t)
	ctx := context.Background()
	if err := owner.GrantTestPrivileges(ctx); err != nil {
		t.Fatal(err)
	}
	config, err := driver.ParseDSN(options.DSN)
	if err != nil {
		t.Fatal(err)
	}
	config.User, config.Passwd = "ocservia_app", "pr02-runtime-test-only"
	options.DSN = config.FormatDSN()
	b, err := Open(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	semantictest.IdentityProfiles(t, b, func(id uuid.UUID) error {
		_, err := b.Exec(ctx, `UPDATE identities SET disabled_at=CURRENT_TIMESTAMP(6) WHERE id=?`, UUIDBytes(id))
		return err
	})
}
