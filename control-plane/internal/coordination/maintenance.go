package coordination

import (
	"context"
	"errors"
	"fmt"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/schedulerlease"
)

// RecordMaintenanceCompletion writes the G6-only durable completion marker
// under the same exact live scheduler term as the maintenance transaction.
func RecordMaintenanceCompletion(ctx context.Context, backend database.Backend, session *Session) error {
	if session == nil {
		return errors.New("coordination: scheduler maintenance completion requires a session")
	}
	identity := session.Identity()
	err := database.Within(ctx, backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := schedulerlease.FromTransaction(tx)
		if err != nil {
			return err
		}
		if err := store.RecordMaintenanceCompletion(ctx, schedulerlease.Owner(identity), session.Epoch()); err != nil {
			return fmt.Errorf("coordination: record scheduler maintenance completion: %w", err)
		}
		return AssertFenceTx(ctx, tx, session)
	})
	if err != nil {
		return fmt.Errorf("coordination: commit scheduler maintenance completion: %w", err)
	}
	return nil
}
