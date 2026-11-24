// Package store defines the desired-state transaction boundary.
package store

import (
	"context"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

type Desired struct {
	NodeID            uuid.UUID
	Kind, Name        string
	Version, Revision int64
	Members           []string
	At                time.Time
}

type Revision struct {
	State, Kind string
}

type Resource struct {
	Kind, Name, NodeStatus                            string
	DesiredEnabled, ObservedEnabled                   *bool
	DesiredMembers, ObservedMembers                   []string
	DesiredVersion, DesiredRevision, ObservedRevision *int64
	OperationID                                       *uuid.UUID
	OperationState, CommandState, PayloadType         *string
	ObservedAt                                        *time.Time
}

type Store interface {
	HasSealingKey(context.Context, uuid.UUID, uint32, string) (bool, error)
	LockDesired(context.Context, uuid.UUID, string, bool) (int64, int64, error)
	LastRevision(context.Context, uuid.UUID, string, string, int64) (Revision, error)
	CountResources(context.Context, uuid.UUID, string, bool, time.Time) (int, error)
	CountMemberships(context.Context, uuid.UUID, string, time.Time) (int, error)
	WriteDesired(context.Context, Desired) error
	SupersedePending(context.Context, uuid.UUID, string, string, string, int64, time.Time) (bool, error)
	List(context.Context, uuid.UUID) ([]Resource, error)
}

func From(tx database.Tx) (Store, error) {
	if p, ok := tx.(interface{ UserStateStore() Store }); ok {
		return p.UserStateStore(), nil
	}
	return nil, database.ErrUnsupported
}
