package coordination

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
)

// AssertFenceTx does not commit: database.Within remains the transaction owner.
func AssertFenceTx(ctx context.Context, tx database.Tx, fence Fence) error {
	if fence == nil {
		return nil
	}
	return fence.AssertTransaction(ctx, tx)
}
