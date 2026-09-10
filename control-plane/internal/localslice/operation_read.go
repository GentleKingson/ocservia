package localslice

import (
	"context"
	"errors"
	"fmt"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	"github.com/google/uuid"
)

type OperationSummary = operationstore.Summary

func localOperation(v operationstore.Operation) Operation {
	return Operation{ID: v.ID, State: v.State, NodeID: v.NodeID, CommandID: v.CommandID, Version: v.Version, CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt}
}

func (s *Service) GetOperation(ctx context.Context, id uuid.UUID) (Operation, error) {
	var v operationstore.Operation
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := operationstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		v, err = store.Get(ctx, id)
		return err
	})
	if err != nil {
		return Operation{}, fmt.Errorf("get operation: %w", err)
	}
	return localOperation(v), nil
}

func (s *Service) ListOperations(ctx context.Context, after uuid.UUID, limit int) ([]Operation, bool, error) {
	return s.ListOperationsInWorkspace(ctx, uuid.Nil, after, limit)
}

func (s *Service) ListOperationsInWorkspace(ctx context.Context, workspaceID, after uuid.UUID, limit int) ([]Operation, bool, error) {
	if limit < 1 || limit > 200 {
		return nil, false, errors.New("operation page size must be between 1 and 200")
	}
	var values []operationstore.Operation
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := operationstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		values, err = store.ListInWorkspace(ctx, workspaceID, after, limit+1)
		return err
	})
	if err != nil {
		return nil, false, fmt.Errorf("list operations: %w", err)
	}
	hasMore := len(values) > limit
	if hasMore {
		values = values[:limit]
	}
	result := make([]Operation, 0, len(values))
	for _, v := range values {
		result = append(result, localOperation(v))
	}
	return result, hasMore, nil
}

// A nil workspace retains the development-auth aggregate view. Unknown
// recovery is counted separately; terminal states do not count as active.
func (s *Service) OperationSummaryInWorkspace(ctx context.Context, workspaceID uuid.UUID) (OperationSummary, error) {
	var summary OperationSummary
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := operationstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		summary, err = store.SummaryInWorkspace(ctx, workspaceID)
		return err
	})
	if err != nil {
		return OperationSummary{}, fmt.Errorf("operation summary: %w", err)
	}
	return summary, nil
}
