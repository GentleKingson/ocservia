package privdattestation

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/privdattestation/attestationstore"
)

const AttestationCapability = "privd_result_attestation_v1"

type KeyStateMetric struct {
	State string `json:"state"`
	Total int64  `json:"total"`
}

func KeyStateMetricsBackend(ctx context.Context, backend database.Backend) ([]KeyStateMetric, error) {
	metrics := []KeyStateMetric{{State: "pending"}, {State: "active"}, {State: "revoked"}}
	if backend == nil {
		return metrics, nil
	}
	err := database.Within(ctx, backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := attestationstore.From(tx)
		if err != nil {
			return err
		}
		rows, err := store.KeyStateCounts(ctx)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var state string
			var total int64
			if err := rows.Scan(&state, &total); err != nil {
				return err
			}
			for index := range metrics {
				if metrics[index].State == state {
					metrics[index].Total = total
				}
			}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return metrics, nil
}
