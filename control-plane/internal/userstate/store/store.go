// Package store defines the desired-state transaction boundary.
package store

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

type Desired struct {
	NodeID            uuid.UUID
	Kind, Name        string
	Version, Revision int64
	Fingerprint       []byte
	Members           value.TextArray
	At                value.Timestamp
}

type Revision struct {
	State, Kind  string
	SafeRejected bool
}

type Resource struct {
	Kind, Name, NodeStatus                            string
	DesiredEnabled, ObservedEnabled                   *bool
	DesiredMembers, ObservedMembers                   value.TextArray
	DesiredVersion, DesiredRevision, ObservedRevision *int64
	DesiredFingerprint, ObservedFingerprint           []byte
	OperationID                                       *uuid.UUID
	OperationState, CommandState, PayloadType         *string
	SafeRejected                                      *bool
	ObservedAt                                        value.Timestamp
}

type Store interface {
	HasSealingKey(context.Context, uuid.UUID, uint32, string) (bool, error)
	LockDesired(context.Context, uuid.UUID, string, bool) (int64, int64, error)
	LastRevision(context.Context, uuid.UUID, string, string, int64) (Revision, error)
	CountResources(context.Context, uuid.UUID, string, bool, value.Timestamp) (int, error)
	CountMemberships(context.Context, uuid.UUID, string, value.Timestamp) (int, error)
	WriteDesired(context.Context, Desired) error
	SupersedePending(context.Context, uuid.UUID, string, string, string, int64, value.Timestamp) (bool, error)
	List(context.Context, uuid.UUID) ([]Resource, error)
}

func From(tx database.Tx) (Store, error) {
	if p, ok := tx.(interface{ UserStateStore() Store }); ok {
		return p.UserStateStore(), nil
	}
	return nil, database.ErrUnsupported
}
