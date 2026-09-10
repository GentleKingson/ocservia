package mysql

import (
	"context"

	certificatestore "github.com/GentleKingson/ocservia/control-plane/internal/certificates/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

const secretReferenceColumns = `id,workspace_id,provider,key_path,version,state,rotated_at,created_at,updated_at`

func scanSecretReference(row database.Row) (v certificatestore.SecretReference, err error) {
	err = row.Scan(&v.ID, &v.WorkspaceID, &v.Provider, &v.KeyPath, &v.Version, &v.State, &v.RotatedAt, &v.CreatedAt, &v.UpdatedAt)
	return
}

func (s certificateStore) InsertSecretReference(ctx context.Context, v certificatestore.SecretReference) error {
	_, err := s.Exec(ctx, `INSERT INTO secret_provider_refs(id,workspace_id,provider,key_path,version,state,created_at,updated_at)VALUES(?,?,?,?,?,'active',?,?)`, UUIDBytes(v.ID), UUIDBytes(v.WorkspaceID), v.Provider, v.KeyPath, v.Version, v.CreatedAt, v.UpdatedAt)
	return err
}

func (s certificateStore) RotateSecretReference(ctx context.Context, id uuid.UUID, version string, at value.Timestamp) (certificatestore.SecretReference, error) {
	var locked uuid.UUID
	if err := s.QueryRow(ctx, `SELECT id FROM secret_provider_refs WHERE id=? FOR UPDATE`, UUIDBytes(id)).Scan(&locked); err != nil {
		return certificatestore.SecretReference{}, err
	}
	if _, err := s.Exec(ctx, `UPDATE secret_provider_refs SET version=?,state='active',rotated_at=?,updated_at=? WHERE id=?`, version, at, at, UUIDBytes(id)); err != nil {
		return certificatestore.SecretReference{}, err
	}
	return s.GetSecretReference(ctx, id)
}

func (s certificateStore) GetSecretReference(ctx context.Context, id uuid.UUID) (certificatestore.SecretReference, error) {
	return scanSecretReference(s.QueryRow(ctx, `SELECT `+secretReferenceColumns+` FROM secret_provider_refs WHERE id=?`, UUIDBytes(id)))
}

func (s certificateStore) SecretReferenceResource(ctx context.Context, id uuid.UUID) (workspace uuid.UUID, err error) {
	err = s.QueryRow(ctx, `SELECT workspace_id FROM secret_provider_refs WHERE id=?`, UUIDBytes(id)).Scan(&workspace)
	return
}
