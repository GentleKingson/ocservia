package store

import (
	"context"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

type Token struct {
	ID, WorkspaceID        uuid.UUID
	Hash                   []byte
	Environment, CreatedBy string
	ExpectedName           *string
	Endpoint               []byte
	ExpectedEndpoint       []byte
	ConsumedNode           *uuid.UUID
	ExpiresAt, CreatedAt   time.Time
	ConsumedAt             *time.Time
}

type Node struct {
	ID, WorkspaceID             uuid.UUID
	Name, Status, EndpointState string
	Endpoint                    []byte
	AuthorizationRevision       uint64
	Version                     int64
}

type SealingKey struct {
	Purpose, Version int32
	ID               string
	Digest           []byte
}

type Lock uint8

const (
	Unlocked Lock = iota
	ForUpdate
	ForShare
)

type EnrollmentStore interface {
	WorkspaceExists(context.Context, uuid.UUID) (bool, error)
	InsertToken(context.Context, Token) error
	TokenByHash(context.Context, []byte, bool) (Token, error)
	ConsumeToken(context.Context, uuid.UUID, uuid.UUID, []byte, time.Time) (bool, error)
	NodeByEndpoint(context.Context, []byte, uuid.UUID, Lock) (Node, error)
	NodeByID(context.Context, uuid.UUID, Lock) (Node, error)
	PendingCount(context.Context, uuid.UUID) (int, error)
	EndpointExists(context.Context, []byte) (bool, error)
	InsertNode(context.Context, uuid.UUID, uuid.UUID, string, time.Time) error
	InsertEndpoint(context.Context, uuid.UUID, []byte, time.Time) error
	InsertSealingKey(context.Context, uuid.UUID, SealingKey, time.Time) error
	SealingKeys(context.Context, uuid.UUID) ([]SealingKey, error)
	Capabilities(context.Context, uuid.UUID, bool) ([]string, error)
	InsertCapabilities(ctx context.Context, node uuid.UUID, capabilities, unapproved []string) error
	Activate(context.Context, uuid.UUID, string, string, time.Time) (uint64, error)
	Revoke(context.Context, uuid.UUID, time.Time) (uint64, error)
	EndpointPermitted(context.Context, []byte, bool) (bool, error)
	TrustSnapshot(context.Context) ([]Node, error)
}

func Enrollment(tx database.Tx) (EnrollmentStore, error) {
	p, ok := tx.(interface{ EnrollmentStore() EnrollmentStore })
	if !ok {
		return nil, database.ErrUnsupported
	}
	return p.EnrollmentStore(), nil
}
