// Package telemetryhistory owns the raw-history operations shared by telemetry
// ingestion, queries and the retention worker. Stores borrow the caller's Tx.
package telemetryhistory

import (
	"context"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

type Sample struct {
	SampledAt time.Time
	Metric    string
	Value     float64
}

type Point struct {
	At      time.Time `json:"at"`
	Metric  string    `json:"metric"`
	Count   int64     `json:"count"`
	Minimum float64   `json:"minimum"`
	Maximum float64   `json:"maximum"`
	Average float64   `json:"average"`
}

type Store interface {
	Insert(context.Context, uuid.UUID, uuid.UUID, []Sample) error
	History(context.Context, uuid.UUID, string, string, time.Time) ([]Point, error)
	Maintain(context.Context, time.Time) error
}

type Provider interface{ TelemetryHistoryStore() Store }

func FromTransaction(tx database.Tx) (Store, error) {
	provider, ok := tx.(Provider)
	if !ok {
		return nil, database.ErrUnsupported
	}
	return provider.TelemetryHistoryStore(), nil
}
