// Package authstore owns the storage contract shared by authentication flows.
package authstore

import (
	"context"
	"errors"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

var ErrAttemptCapacity = errors.New("Local authentication attempt capacity exhausted")

type Credential struct {
	ID       uuid.UUID
	Hash     string
	Disabled bool
}
type Session struct {
	Issuer, Subject string
	BreakGlass      bool
	ExpiresAt       value.Timestamp
}
type Bootstrap struct {
	IdentityID, WorkspaceID uuid.UUID
	Pending                 bool
}

type Store interface {
	InsertCredential(context.Context, uuid.UUID, string, string, time.Time) error
	LockCredential(context.Context, uuid.UUID, string, string, string) (uuid.UUID, error)
	InsertSession(context.Context, uuid.UUID, uuid.UUID, time.Time, bool, time.Time) error
	Session(context.Context, uuid.UUID, uuid.UUID) (Session, error)
	RevokeSession(context.Context, uuid.UUID, uuid.UUID) error
	ReserveAttempt(context.Context, string, uuid.UUID, int, time.Duration, time.Duration) (bool, error)
	FinishAttempt(context.Context, string, uuid.UUID, bool) error
	ClearAttempt(context.Context, string, uuid.UUID) (bool, error)
	ActiveLocalUsername(context.Context, uuid.UUID) (string, error)
	LockActiveSession(context.Context, uuid.UUID, uuid.UUID, bool) error
	LockPassword(context.Context, uuid.UUID, string, string) error
	ManagementWorkspace(context.Context) (uuid.UUID, error)
	SetPassword(context.Context, uuid.UUID, string, time.Time) error
	RevokeIdentitySessions(context.Context, uuid.UUID, time.Time) error
	BreakGlassUsed(context.Context, []byte) (bool, error)
	BreakGlassIdentity(context.Context, uuid.UUID, time.Time) (uuid.UUID, error)
	RecordBreakGlass(context.Context, []byte, uuid.UUID, uuid.UUID, uuid.UUID, time.Time) error
	Workspaces(context.Context) ([]uuid.UUID, error)
	LockManagement(context.Context) error
	WorkspaceExists(context.Context, uuid.UUID) error
	Bootstrap(context.Context) (Bootstrap, error)
	ValidatePendingCredential(context.Context, uuid.UUID, uuid.UUID, string) error
	Initialized(context.Context) (bool, error)
	BindRole(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string, time.Time) error
	InsertBootstrap(context.Context, uuid.UUID, uuid.UUID, time.Time) error
	CompleteBootstrap(context.Context, uuid.UUID, time.Time) error
	MayManage(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (bool, error)
	LockLocalIdentity(context.Context, uuid.UUID) error
	Protected(context.Context, uuid.UUID, uuid.UUID) (bool, error)
	Disable(context.Context, uuid.UUID, time.Time) error
	DeleteIdentityAttempts(context.Context, uuid.UUID) error
}

// CredentialReader preserves a completed credential read independently of
// later request cancellation. Session issuance still rechecks under a Tx lock.
type CredentialReader interface {
	ReadLocalCredential(context.Context, string) (Credential, error)
	HasLocalCredential(context.Context, uuid.UUID) (bool, error)
}

func HasLocalCredential(ctx context.Context, backend database.Backend, id uuid.UUID) (bool, error) {
	reader, ok := backend.(CredentialReader)
	if !ok {
		return false, database.ErrUnsupported
	}
	return reader.HasLocalCredential(ctx, id)
}

func ReadCredential(ctx context.Context, backend database.Backend, username string) (Credential, error) {
	reader, ok := backend.(CredentialReader)
	if !ok {
		return Credential{}, database.ErrUnsupported
	}
	return reader.ReadLocalCredential(ctx, username)
}

type Provider interface{ AuthenticationStore() Store }

func From(tx database.Tx) (Store, error) {
	p, ok := tx.(Provider)
	if !ok {
		return nil, database.ErrUnsupported
	}
	return p.AuthenticationStore(), nil
}
