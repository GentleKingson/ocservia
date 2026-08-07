package certificates

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/google/uuid"
)

var providerName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

type SecretRef struct {
	ID          uuid.UUID  `json:"id"`
	WorkspaceID uuid.UUID  `json:"workspace_id"`
	Provider    string     `json:"provider"`
	KeyPath     string     `json:"key_path"`
	Version     string     `json:"version"`
	State       string     `json:"state"`
	RotatedAt   *time.Time `json:"rotated_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

type SecretRefRequest struct {
	WorkspaceID, ActorID, SessionID               uuid.UUID
	Provider, KeyPath, Version, Reason, RequestID string
}

func (s *Service) CreateSecretRef(ctx context.Context, request SecretRefRequest) (SecretRef, error) {
	if !validSecretRefRequest(request) {
		return SecretRef{}, ErrInvalid
	}
	now, id := s.now(), uuid.Must(uuid.NewV7())
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SecretRef{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	value := SecretRef{ID: id, WorkspaceID: request.WorkspaceID, Provider: request.Provider, KeyPath: request.KeyPath, Version: request.Version, State: "active", CreatedAt: now, UpdatedAt: now}
	if _, err := tx.Exec(ctx, `INSERT INTO secret_provider_refs(id,workspace_id,provider,key_path,version,state,created_at,updated_at) VALUES($1,$2,$3,$4,$5,'active',$6,$6)`, id, request.WorkspaceID, request.Provider, request.KeyPath, request.Version, now); err != nil {
		return SecretRef{}, err
	}
	summary, _ := json.Marshal(map[string]any{"provider": request.Provider, "key_path": request.KeyPath, "version": request.Version, "state": "active"})
	if err := audit.AppendChain(ctx, tx, audit.ChainRecord{WorkspaceID: request.WorkspaceID, ActorType: "user", ActorID: request.ActorID.String(), SessionID: &request.SessionID, Action: "secret_ref.create", ResourceType: "secret_ref", ResourceID: id, RequestID: request.RequestID, Result: "succeeded", Reason: request.Reason, AfterSummary: summary, At: now}); err != nil {
		return SecretRef{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return SecretRef{}, err
	}
	return value, nil
}

func (s *Service) RotateSecretRef(ctx context.Context, id uuid.UUID, request SecretRefRequest) (SecretRef, error) {
	if id == uuid.Nil || request.ActorID == uuid.Nil || request.SessionID == uuid.Nil || strings.TrimSpace(request.Version) == "" || len(request.Version) > 128 || strings.TrimSpace(request.Reason) == "" || request.RequestID == "" {
		return SecretRef{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SecretRef{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	now := s.now()
	var value SecretRef
	err = tx.QueryRow(ctx, `UPDATE secret_provider_refs SET version=$2,state='active',rotated_at=$3,updated_at=$3 WHERE id=$1 RETURNING id,workspace_id,provider,key_path,version,state,rotated_at,created_at,updated_at`, id, request.Version, now).Scan(&value.ID, &value.WorkspaceID, &value.Provider, &value.KeyPath, &value.Version, &value.State, &value.RotatedAt, &value.CreatedAt, &value.UpdatedAt)
	if err != nil {
		return SecretRef{}, err
	}
	summary, _ := json.Marshal(map[string]any{"provider": value.Provider, "key_path": value.KeyPath, "version": value.Version, "state": value.State})
	if err := audit.AppendChain(ctx, tx, audit.ChainRecord{WorkspaceID: value.WorkspaceID, ActorType: "user", ActorID: request.ActorID.String(), SessionID: &request.SessionID, Action: "secret_ref.rotate", ResourceType: "secret_ref", ResourceID: id, RequestID: request.RequestID, Result: "succeeded", Reason: request.Reason, AfterSummary: summary, At: now}); err != nil {
		return SecretRef{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return SecretRef{}, err
	}
	return value, nil
}

func (s *Service) GetSecretRef(ctx context.Context, id uuid.UUID) (SecretRef, error) {
	var value SecretRef
	err := s.pool.QueryRow(ctx, `SELECT id,workspace_id,provider,key_path,version,state,rotated_at,created_at,updated_at FROM secret_provider_refs WHERE id=$1`, id).Scan(&value.ID, &value.WorkspaceID, &value.Provider, &value.KeyPath, &value.Version, &value.State, &value.RotatedAt, &value.CreatedAt, &value.UpdatedAt)
	return value, err
}
func (s *Service) SecretRefResource(ctx context.Context, id uuid.UUID) (uuid.UUID, error) {
	var workspaceID uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT workspace_id FROM secret_provider_refs WHERE id=$1`, id).Scan(&workspaceID)
	return workspaceID, err
}

func validSecretRefRequest(request SecretRefRequest) bool {
	return request.WorkspaceID != uuid.Nil && request.ActorID != uuid.Nil && request.SessionID != uuid.Nil && providerName.MatchString(request.Provider) && len(request.KeyPath) >= 1 && len(request.KeyPath) <= 512 && !strings.Contains("/"+request.KeyPath+"/", "/../") && len(request.Version) >= 1 && len(request.Version) <= 128 && strings.TrimSpace(request.Reason) != "" && len(request.Reason) <= 512 && request.RequestID != ""
}
