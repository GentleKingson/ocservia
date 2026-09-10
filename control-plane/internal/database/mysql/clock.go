package mysql

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
)

func (t *transaction) WallTime(ctx context.Context) (at value.Timestamp, err error) {
	err = t.QueryRow(ctx, `SELECT TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6))`).Scan(&at)
	return
}
