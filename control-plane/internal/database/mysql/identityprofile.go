package mysql

import (
	"context"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/identityprofile"
	"github.com/google/uuid"
)

type identityProfileStore struct{ tx database.Tx }

func (t *transaction) IdentityProfileStore() identityprofile.Store { return identityProfileStore{t} }
func (s identityProfileStore) Upsert(ctx context.Context, id uuid.UUID, issuer, subject, email, name string, now time.Time) (uuid.UUID, error) {
	return UpsertIdentity(ctx, s.tx, id, issuer, subject, email, name, now)
}
