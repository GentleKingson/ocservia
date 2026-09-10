package semantictest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/identityprofile"
	"github.com/google/uuid"
)

func IdentityProfiles(t *testing.T, b database.Backend, disable func(uuid.UUID) error) {
	ctx := context.Background()
	issuer := uuid.NewString() + strings.Repeat("i", 4096)
	subject := strings.Repeat("s", 4096)
	write := func(ctx context.Context, suffix string, after func() error) (uuid.UUID, error) {
		var id uuid.UUID
		err := database.Within(ctx, b, database.ReadCommitted, func(tx database.Tx) error {
			var err error
			id, err = identityprofile.Upsert(ctx, tx, uuid.New(), issuer, subject+suffix, "", "display", time.Now().UTC())
			if err != nil {
				return err
			}
			if after != nil {
				return after()
			}
			return nil
		})
		return id, err
	}
	id, err := write(ctx, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	again, err := write(ctx, "", nil)
	if err != nil || again != id {
		t.Fatal("exact long conflict identity", again, err)
	}
	tail, err := write(ctx, " ", nil)
	if err != nil || tail == id {
		t.Fatal("trailing space identity collapsed", err)
	}
	failed := errors.New("session insertion failed")
	aborted, err := write(ctx, "rollback", func() error { return failed })
	if !errors.Is(err, failed) {
		t.Fatal(err)
	}
	retied, err := write(ctx, "rollback", nil)
	if err != nil || retied == aborted {
		t.Fatal("failed session retained identity", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	aborted, err = write(cancelled, "cancel", func() error { cancel(); return cancelled.Err() })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	retied, err = write(ctx, "cancel", nil)
	if err != nil || retied == aborted {
		t.Fatal("cancelled session retained identity", err)
	}
	if err = disable(id); err != nil {
		t.Fatal(err)
	}
	if _, err = write(ctx, "", nil); !errors.Is(err, database.ErrNotFound) {
		t.Fatal("disabled identity revived", err)
	}
}
