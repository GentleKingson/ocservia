package privdattestation

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
)

const AttestationCapability = "privd_result_attestation_v1"

type KeyStateMetric struct {
	State string `json:"state"`
	Total int64  `json:"total"`
}

func KeyStateMetrics(ctx context.Context, backend database.Backend) ([]KeyStateMetric, error) {
	metrics := []KeyStateMetric{{State: "pending"}, {State: "active"}, {State: "revoked"}}
	if backend == nil {
		return metrics, nil
	}
	err := database.Within(ctx, backend, database.ReadCommitted, func(tx database.Tx) error {
		counts, err := StoreFrom(tx).KeyStateCounts(ctx)
		if err != nil {
			return err
		}
		for index := range metrics {
			metrics[index].Total = counts[metrics[index].State]
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return metrics, nil
}
