package certificates

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	certificatestore "github.com/GentleKingson/ocservia/control-plane/internal/certificates/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
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
	now, id := s.now().UTC().Truncate(time.Microsecond), uuid.Must(uuid.NewV7())
	v := certificatestore.SecretReference{ID: id, WorkspaceID: request.WorkspaceID, Provider: request.Provider, KeyPath: request.KeyPath, Version: request.Version, State: "active", CreatedAt: now, UpdatedAt: now}
	err := s.withStore(ctx, func(tx database.Tx, store certificatestore.Store) error {
		if err := auth.AuthorizeMutationTx(ctx, tx, request.ActorID, request.SessionID, request.WorkspaceID); err != nil {
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
	err := s.withStore(ctx, func(tx database.Tx, store certificatestore.Store) error {
		workspaceID, err := store.SecretReferenceResource(ctx, id)
		if err != nil {
			return err
		}
		if err := auth.AuthorizeMutationTx(ctx, tx, request.ActorID, request.SessionID, workspaceID); err != nil {
			return err
		}
		now := s.now().UTC().Truncate(time.Microsecond)
		v, err = store.RotateSecretReference(ctx, id, request.Version, now)
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
	err := s.withStore(ctx, func(_ database.Tx, store certificatestore.Store) error {
		var err error
		v, err = store.GetSecretReference(ctx, id)
		return err
	})
	return readSecretReference(v), err
}
func (s *Service) SecretRefResource(ctx context.Context, id uuid.UUID) (uuid.UUID, error) {
	var workspaceID uuid.UUID
	err := s.withStore(ctx, func(_ database.Tx, store certificatestore.Store) error {
		var err error
		workspaceID, err = store.SecretReferenceResource(ctx, id)
		return err
	})
	return workspaceID, err
}

func readSecretReference(v certificatestore.SecretReference) SecretRef {
	return SecretRef{ID: v.ID, WorkspaceID: v.WorkspaceID, Provider: v.Provider, KeyPath: v.KeyPath, Version: v.Version, State: v.State, RotatedAt: v.RotatedAt, CreatedAt: v.CreatedAt.UTC(), UpdatedAt: v.UpdatedAt.UTC()}
}

func validSecretRefRequest(request SecretRefRequest) bool {
	return request.WorkspaceID != uuid.Nil && request.ActorID != uuid.Nil && request.SessionID != uuid.Nil && providerName.MatchString(request.Provider) && len(request.KeyPath) >= 1 && len(request.KeyPath) <= 512 && !strings.Contains("/"+request.KeyPath+"/", "/../") && len(request.Version) >= 1 && len(request.Version) <= 128 && strings.TrimSpace(request.Reason) != "" && len(request.Reason) <= 512 && request.RequestID != ""
}
