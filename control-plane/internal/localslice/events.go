package localslice

import (
	"context"
	"errors"
	"fmt"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	localstore "github.com/GentleKingson/ocservia/control-plane/internal/localslice/store"
	"github.com/google/uuid"
)

const (
	ListEventsAscending  = "asc"
	ListEventsDescending = "desc"
)

func (s *Service) ListEvents(ctx context.Context, after uuid.UUID, limit int) ([]Event, bool, error) {
	return s.ListEventsInWorkspace(ctx, uuid.Nil, after, limit, ListEventsAscending)
}

func (s *Service) ListEventsInWorkspace(ctx context.Context, workspace, after uuid.UUID, limit int, order string) ([]Event, bool, error) {
	if limit < 1 || limit > 200 {
		return nil, false, errors.New("event page size must be between 1 and 200")
	}
	if order != ListEventsAscending && order != ListEventsDescending {
		return nil, false, errors.New("event order must be asc or desc")
	}
	var events []Event
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := localstore.Local(tx)
		if err != nil {
			return err
		}
		events, err = store.Events(ctx, workspace, after, limit+1, order == ListEventsDescending)
		return err
	})
	if err != nil {
		return nil, false, fmt.Errorf("list transport events: %w", err)
	}
	more := len(events) > limit
	if more {
		events = events[:limit]
	}
	return events, more, nil
}

func (s *Service) EventSequenceInWorkspace(ctx context.Context, workspace, event uuid.UUID) (int64, bool, error) {
	if workspace == uuid.Nil || event == uuid.Nil {
		return 0, false, nil
	}
	var sequence int64
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := localstore.Local(tx)
		if err != nil {
			return err
		}
		sequence, err = store.EventSequence(ctx, workspace, event)
		return err
	})
	if errors.Is(err, database.ErrNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("resolve transport event cursor: %w", err)
	}
	return sequence, true, nil
}

func (s *Service) ReconcileEventGap(ctx context.Context, nodeConnected func(context.Context, []byte) (bool, error)) error {
	var candidates []localstore.GapNode
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := localstore.Local(tx)
		if err != nil {
			return err
		}
		candidates, err = store.GapNodes(ctx, workspaceSlug)
		return err
	})
	if err != nil {
		return fmt.Errorf("select simulator nodes after transport event gap: %w", err)
	}
	var disconnected []localstore.GapNode
	// No transaction is held while consulting the external transport registry.
	for _, node := range candidates {
		connected, err := nodeConnected(ctx, node.ID[:])
		if err != nil {
			return fmt.Errorf("reconcile simulator node connection %s: %w", node.ID, err)
		}
		if !connected {
			disconnected = append(disconnected, node)
		}
	}
	return database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := localstore.Local(tx)
		if err != nil {
			return err
		}
		if err := store.InvalidateCursor(ctx); err != nil {
			return fmt.Errorf("invalidate transport cursor after event gap: %w", err)
		}
		if err := store.UnknownDispatched(ctx); err != nil {
			return fmt.Errorf("mark operations unknown after transport event gap: %w", err)
		}
		at, err := value.FromTime(s.now())
		if err != nil {
			return err
		}
		for _, node := range disconnected {
			event, err := uuid.NewV7()
			if err != nil {
				return err
			}
			if err := store.DisconnectNode(ctx, node, event, at); err != nil {
				return fmt.Errorf("record simulator disconnect after transport event gap: %w", err)
			}
		}
		return nil
	})
}
