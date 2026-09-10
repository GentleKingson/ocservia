package certificates

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	certificatestore "github.com/GentleKingson/ocservia/control-plane/internal/certificates/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

var providerName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

type SecretRef struct {
	ID          uuid.UUID        `json:"id"`
	WorkspaceID uuid.UUID        `json:"workspace_id"`
	Provider    string           `json:"provider"`
	KeyPath     string           `json:"key_path"`
	Version     string           `json:"version"`
	State       string           `json:"state"`
	RotatedAt   *value.Timestamp `json:"rotated_at,omitempty"`
	CreatedAt   value.Timestamp  `json:"created_at"`
	UpdatedAt   value.Timestamp  `json:"updated_at"`
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
	at, err := value.FromTime(now)
	if err != nil {
		return SecretRef{}, err
	}
	v := certificatestore.SecretReference{ID: id, WorkspaceID: request.WorkspaceID, Provider: request.Provider, KeyPath: request.KeyPath, Version: request.Version, State: "active", CreatedAt: at, UpdatedAt: at}
	err = database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := certificatestore.FromTransaction(tx)
		if err != nil {
			return err
		}
		if err := store.InsertSecretReference(ctx, v); err != nil {
			return err
		}
		summary, _ := json.Marshal(map[string]any{"provider": request.Provider, "key_path": request.KeyPath, "version": request.Version, "state": "active"})
		return audit.AppendChainTx(ctx, tx, audit.ChainRecord{WorkspaceID: request.WorkspaceID, ActorType: "user", ActorID: request.ActorID.String(), SessionID: &request.SessionID, Action: "secret_ref.create", ResourceType: "secret_ref", ResourceID: id, RequestID: request.RequestID, Result: "succeeded", Reason: request.Reason, AfterSummary: summary, At: now})
	})
	if err != nil {
		return SecretRef{}, err
	}
	return readSecretReference(v), nil
}

func (s *Service) RotateSecretRef(ctx context.Context, id uuid.UUID, request SecretRefRequest) (SecretRef, error) {
	if id == uuid.Nil || request.ActorID == uuid.Nil || request.SessionID == uuid.Nil || strings.TrimSpace(request.Version) == "" || len(request.Version) > 128 || strings.TrimSpace(request.Reason) == "" || request.RequestID == "" {
		return SecretRef{}, ErrInvalid
	}
	var v certificatestore.SecretReference
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := certificatestore.FromTransaction(tx)
		if err != nil {
			return err
		}
		now := s.now()
		at, err := value.FromTime(now)
		if err != nil {
			return err
		}
		v, err = store.RotateSecretReference(ctx, id, request.Version, at)
		if err != nil {
			return err
		}
		summary, _ := json.Marshal(map[string]any{"provider": v.Provider, "key_path": v.KeyPath, "version": v.Version, "state": v.State})
		return audit.AppendChainTx(ctx, tx, audit.ChainRecord{WorkspaceID: v.WorkspaceID, ActorType: "user", ActorID: request.ActorID.String(), SessionID: &request.SessionID, Action: "secret_ref.rotate", ResourceType: "secret_ref", ResourceID: id, RequestID: request.RequestID, Result: "succeeded", Reason: request.Reason, AfterSummary: summary, At: now})
	})
	if err != nil {
		return SecretRef{}, err
	}
	return readSecretReference(v), nil
}

func (s *Service) GetSecretRef(ctx context.Context, id uuid.UUID) (SecretRef, error) {
	var v certificatestore.SecretReference
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := certificatestore.FromTransaction(tx)
		if err != nil {
			return err
		}
		v, err = store.GetSecretReference(ctx, id)
		return err
	})
	return readSecretReference(v), err
}
func (s *Service) SecretRefResource(ctx context.Context, id uuid.UUID) (uuid.UUID, error) {
	var workspaceID uuid.UUID
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := certificatestore.FromTransaction(tx)
		if err != nil {
			return err
		}
		workspaceID, err = store.SecretReferenceResource(ctx, id)
		return err
	})
	return workspaceID, err
}

func readSecretReference(v certificatestore.SecretReference) SecretRef {
	var rotated *value.Timestamp
	if v.RotatedAt.Valid {
		rotated = &v.RotatedAt
	}
	return SecretRef{ID: v.ID, WorkspaceID: v.WorkspaceID, Provider: v.Provider, KeyPath: v.KeyPath, Version: v.Version, State: v.State, RotatedAt: rotated, CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt}
}

func validSecretRefRequest(request SecretRefRequest) bool {
	return request.WorkspaceID != uuid.Nil && request.ActorID != uuid.Nil && request.SessionID != uuid.Nil && providerName.MatchString(request.Provider) && len(request.KeyPath) >= 1 && len(request.KeyPath) <= 512 && !strings.Contains("/"+request.KeyPath+"/", "/../") && len(request.Version) >= 1 && len(request.Version) <= 128 && strings.TrimSpace(request.Reason) != "" && len(request.Reason) <= 512 && request.RequestID != ""
}
