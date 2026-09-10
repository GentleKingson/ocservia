package postgres

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
	err := s.tx.QueryRow(ctx, `INSERT INTO identities(id,issuer,subject,email,display_name,created_at,updated_at) VALUES($1,$2,$3,NULLIF($4,''),NULLIF($5,''),$6,$6) ON CONFLICT(issuer,subject) DO UPDATE SET email=EXCLUDED.email,display_name=EXCLUDED.display_name,updated_at=EXCLUDED.updated_at WHERE identities.disabled_at IS NULL RETURNING id`, id, issuer, subject, email, name, now).Scan(&id)
	return id, err
}
