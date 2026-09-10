package postgres

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
)

func (t *transaction) WallTime(ctx context.Context) (at value.Timestamp, err error) {
	err = t.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&at)
	return
}
