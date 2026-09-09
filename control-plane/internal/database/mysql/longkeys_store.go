package mysql

import (
	"context"
	"errors"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

// UpsertIdentity implements the OIDC identity conflict clause on the caller's
// transaction. A disabled conflict row returns ErrNotFound and is never revived.
// Both the exact-key guard and identity row remain locked through session
// insertion and commit. No separate connection or autocommit lookup is used.
func UpsertIdentity(ctx context.Context, tx database.Tx, proposed uuid.UUID, issuer, subject, email, name string, now time.Time) (uuid.UUID, error) {
	stamp, err := value.FromTime(now)
	if err != nil {
		return uuid.Nil, err
	}
	if err := LockExactKey(ctx, tx, "identities"); err != nil {
		return uuid.Nil, err
	}
	var raw []byte
	var disabled bool
	err = tx.QueryRow(ctx, `SELECT id,disabled_at IS NOT NULL FROM identities WHERE CAST(issuer AS BINARY)=? AND CAST(subject AS BINARY)=? FOR UPDATE`, []byte(issuer), []byte(subject)).Scan(&raw, &disabled)
	if errors.Is(err, database.ErrNotFound) {
		_, err = tx.Exec(ctx, `INSERT INTO identities(id,issuer,subject,email,display_name,created_at,updated_at) VALUES(?,?,?,NULLIF(?,''),NULLIF(?,''),?,?)`, UUIDBytes(proposed), issuer, subject, email, name, stamp, stamp)
		return proposed, err
	}
	if err != nil {
		return uuid.Nil, err
	}
	if disabled {
		return uuid.Nil, database.ErrNotFound
	}
	id, err := uuid.FromBytes(raw)
	if err != nil {
		return uuid.Nil, err
	}
	_, err = tx.Exec(ctx, `UPDATE identities SET email=NULLIF(?,''),display_name=NULLIF(?,''),updated_at=? WHERE id=?`, email, name, stamp, raw)
	return id, err
}
