package store

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

type Token struct {
	ID, WorkspaceID                  uuid.UUID
	Hash                             []byte
	Environment, CreatedBy           string
	ExpectedName                     *string
	Endpoint                         []byte
	ConsumedNode                     *uuid.UUID
	ExpiresAt, ConsumedAt, CreatedAt value.Timestamp
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
	InsertToken(context.Context, Token, bool) error
	TokenByHash(context.Context, []byte, bool, bool) (Token, error)
	ConsumeToken(context.Context, uuid.UUID, uuid.UUID, []byte, value.Timestamp, bool) (bool, error)
	NodeByEndpoint(context.Context, []byte, uuid.UUID, Lock) (Node, error)
	NodeByID(context.Context, uuid.UUID, Lock) (Node, error)
	LegacyPendingNode(context.Context, uuid.UUID, string) (uuid.UUID, error)
	PendingCount(context.Context, uuid.UUID) (int, error)
	EndpointExists(context.Context, []byte) (bool, error)
	InsertNode(context.Context, uuid.UUID, uuid.UUID, string, value.Timestamp) error
	TouchNode(context.Context, uuid.UUID, value.Timestamp) error
	InsertEndpoint(context.Context, uuid.UUID, []byte, value.Timestamp) error
	InsertSealingKey(context.Context, uuid.UUID, SealingKey, value.Timestamp) error
	SealingKeys(context.Context, uuid.UUID) ([]SealingKey, error)
	Capabilities(context.Context, uuid.UUID, bool) ([]string, error)
	PutCapability(context.Context, uuid.UUID, string, bool) error
	ResetCapabilities(context.Context, uuid.UUID) error
	Activate(context.Context, uuid.UUID, string, string, value.Timestamp) (uint64, error)
	Revoke(context.Context, uuid.UUID, value.Timestamp) (uint64, error)
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
