// Package telemetryhistory owns the raw-history operations shared by telemetry
// ingestion, queries and the retention worker. Stores borrow the caller's Tx.
package telemetryhistory

import (
	"context"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

type Sample struct {
	SampledAt time.Time
	Metric    string
	Value     float64
}

type Point struct {
	At      value.Timestamp `json:"at"`
	Metric  string          `json:"metric"`
	Count   int64           `json:"count"`
	Minimum float64         `json:"minimum"`
	Maximum float64         `json:"maximum"`
	Average float64         `json:"average"`
}

type Store interface {
	Insert(context.Context, uuid.UUID, uuid.UUID, []Sample) error
	History(context.Context, uuid.UUID, string, string, value.Timestamp) ([]Point, error)
	Maintain(context.Context, time.Time) error
}

type Provider interface{ TelemetryHistoryStore() Store }

// BatchMaintainer advances durable progress in the borrowed transaction. A
// successful batch is not a completed maintenance run until done is true.
type BatchMaintainer interface {
	MaintainBatch(context.Context, time.Time) (done bool, err error)
}

// Maintain commits each batch together with the caller's leadership fence.
// beforeFirst stays atomic with the first batch. A failed batch is not retried;
// already committed progress survives recovery.
func Maintain(ctx context.Context, backend database.Backend, now time.Time, beforeFirst, fence func(context.Context, database.Tx) error) error {
	first := true
	for {
		done := true
		err := database.Within(ctx, backend, database.ReadCommitted, func(tx database.Tx) error {
			if first && beforeFirst != nil {
				if err := beforeFirst(ctx, tx); err != nil {
					return err
				}
			}
			store, err := FromTransaction(tx)
			if err != nil {
				return err
			}
			if batch, ok := store.(BatchMaintainer); ok {
				done, err = batch.MaintainBatch(ctx, now)
			} else {
				err = store.Maintain(ctx, now)
			}
			if err != nil {
				return err
			}
			return fence(ctx, tx)
		})
		if err != nil || done {
			return err
		}
		first = false
	}
}

func FromTransaction(tx database.Tx) (Store, error) {
	provider, ok := tx.(Provider)
	if !ok {
		return nil, database.ErrUnsupported
	}
	return provider.TelemetryHistoryStore(), nil
}
