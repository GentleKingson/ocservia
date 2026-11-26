package operations

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	"github.com/google/uuid"
)

type OperationSummary = operationstore.Summary

type cachedSummary struct {
	workspace uuid.UUID
	counts    OperationSummary
	expires   time.Time
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (Operation, error) {
	var result Operation
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) (err error) {
		result, err = operationstore.FromTransaction(tx).Get(ctx, id)
		return err
	})
	return result, err
}

func (s *Service) ListInWorkspace(ctx context.Context, workspaceID, after uuid.UUID, limit int) ([]Operation, bool, error) {
	if limit < 1 || limit > 200 {
		return nil, false, errors.New("operation page size must be between 1 and 200")
	}
	var values []Operation
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) (err error) {
		values, err = operationstore.FromTransaction(tx).ListInWorkspace(ctx, workspaceID, after, limit+1)
		return err
	})
	if err != nil {
		return nil, false, fmt.Errorf("list operations: %w", err)
	}
	hasMore := len(values) > limit
	if hasMore {
		values = values[:limit]
	}
	if values == nil {
		values = []Operation{}
	}
	return values, hasMore, nil
}

// A nil workspace retains the development-auth aggregate view. Unknown
// recovery is counted separately; terminal states do not count as active.
func (s *Service) SummaryInWorkspace(ctx context.Context, workspaceID uuid.UUID) (OperationSummary, error) {
	// Keep one dashboard aggregate across requests. Start the TTL before the
	// query so a slow database read cannot extend the freshness window.
	now := time.Now()
	if cached := s.lastSummary.Load(); cached != nil && cached.workspace == workspaceID && now.Before(cached.expires) {
		return cached.counts, nil
	}
	var summary OperationSummary
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) (err error) {
		summary, err = operationstore.FromTransaction(tx).SummaryInWorkspace(ctx, workspaceID)
		return err
	})
	if err != nil {
		return OperationSummary{}, fmt.Errorf("operation summary: %w", err)
	}
	s.lastSummary.Store(&cachedSummary{workspace: workspaceID, counts: summary, expires: now.Add(time.Second)})
	return summary, nil
}
