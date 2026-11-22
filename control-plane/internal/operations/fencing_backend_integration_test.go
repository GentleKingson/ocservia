package operations_test

import (
	"context"
	"errors"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/connectionowner"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"github.com/google/uuid"
)

func TestFencingBackendIntegration(t *testing.T) {
	f := newOutboxFixture(t)
	ctx := context.Background()
	t.Run("monotonic-epoch-and-delete-denied", func(t *testing.T) {
		node := f.node(t)
		winner, err := connectionowner.AcquireBackend(ctx, f.backend, node, uuid.Must(uuid.NewV7()))
		testutil.NoError(t, err)
		if err := winner.ReleaseBackend(ctx, f.backend); err != nil {
			t.Fatal(err)
		}
		for range 3 {
			next, err := connectionowner.AcquireBackend(ctx, f.backend, node, uuid.Must(uuid.NewV7()))
			if err != nil || next.Epoch() <= winner.Epoch() {
				t.Fatal("epoch reused", next, err)
			}
			if err := winner.ReleaseBackend(ctx, f.backend); !errors.Is(err, connectionowner.ErrNotOwner) {
				t.Fatal("old release accepted", err)
			}
			if err := winner.AssertCurrentBackend(ctx, f.backend); !errors.Is(err, connectionowner.ErrNotOwner) {
				t.Fatal("old assert accepted", err)
			}
			winner = next
		}
		// Runtime cannot delete fencing authority to reset its epoch.
		if _, err := f.backend.Exec(ctx, "DELETE FROM connection_owner_fencing WHERE 1=0"); !errors.Is(err, database.ErrPermission) {
			t.Fatal("fencing deletion permitted", err)
		}
	})
	t.Run("owner-readback-on-commit-fault", func(t *testing.T) {
		node := f.node(t)
		fault := &commitFaultBackend{Backend: f.backend, commit: true}
		fault.armed.Store(true)
		term, err := connectionowner.AcquireBackend(ctx, fault, node, uuid.Must(uuid.NewV7()))
		testutil.NoError(t, err)
		fault.armed.Store(true)
		if err := term.ReleaseBackend(ctx, fault); err != nil {
			t.Fatal("release readback", err)
		}
	})
}
