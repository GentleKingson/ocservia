// Package identityprofile owns the identity conflict behavior used by OIDC login.
package identityprofile

import (
	"context"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

type Store interface {
	Upsert(context.Context, uuid.UUID, string, string, string, string, time.Time) (uuid.UUID, error)
}

// Upsert retains the conflict row lock until the caller's session transaction
// ends. A disabled identity is a denial, never an instruction to revive it.
func Upsert(ctx context.Context, tx database.Tx, proposed uuid.UUID, issuer, subject, email, name string, now time.Time) (uuid.UUID, error) {
	provider, ok := tx.(interface{ IdentityProfileStore() Store })
	if !ok {
		return uuid.Nil, database.ErrUnsupported
	}
	return provider.IdentityProfileStore().Upsert(ctx, proposed, issuer, subject, email, name, now)
}
