package transportevents

import (
	"context"
	"errors"
	"fmt"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/transportclient"
	eventstore "github.com/GentleKingson/ocservia/control-plane/internal/transportevents/store"
	"github.com/google/uuid"
)

const (
	ListEventsAscending  = "asc"
	ListEventsDescending = "desc"
)

func (s *Service) ListEventsInWorkspace(ctx context.Context, workspace, after uuid.UUID, limit int, order string) ([]Event, bool, error) {
	if limit < 1 || limit > 200 {
		return nil, false, errors.New("event page size must be between 1 and 200")
	}
	if order != ListEventsAscending && order != ListEventsDescending {
		return nil, false, errors.New("event order must be asc or desc")
	}
	var events []Event
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) (err error) {
		events, err = eventstore.FromTransaction(tx).Events(ctx, workspace, after, limit+1, order == ListEventsDescending)
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
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) (err error) {
		sequence, err = eventstore.FromTransaction(tx).EventSequence(ctx, workspace, event)
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

func (s *Service) ReconcileEventGap(ctx context.Context, readConnection transportclient.NodeConnectionReader) error {
	var applyGap func(context.Context, database.Tx) error
	if s.gapHook != nil {
		var err error
		applyGap, err = s.gapHook(ctx, readConnection)
		if err != nil {
			return err
		}
	}
	return database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		if err := eventstore.FromTransaction(tx).InvalidateCursor(ctx); err != nil {
			return fmt.Errorf("invalidate transport cursor after event gap: %w", err)
		}
		if applyGap != nil {
			return applyGap(ctx, tx)
		}
		return nil
	})
}
