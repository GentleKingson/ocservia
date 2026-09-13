package telemetryhistory

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
)

type batchBackend struct {
	database.Backend
	commits, attempts, fences int
	failAt                    int
}

type batchTx struct {
	database.Tx
	backend  *batchBackend
	advanced bool
}

type batchStore struct {
	Store
	tx *batchTx
}

func (b *batchBackend) Begin(context.Context, database.Isolation) (database.Tx, error) {
	b.attempts++
	return &batchTx{backend: b}, nil
}
func (tx *batchTx) Commit(context.Context) error {
	if tx.advanced {
		tx.backend.commits++
	}
	return nil
}
func (tx *batchTx) Rollback(context.Context) error { return nil }
func (tx *batchTx) TelemetryHistoryStore() Store   { return &batchStore{tx: tx} }
func (s *batchStore) MaintainBatch(context.Context, time.Time) (bool, error) {
	s.tx.advanced = true
	return s.tx.backend.commits == 2, nil
}

func TestMaintenanceFencesEveryBatchAndResumes(t *testing.T) {
	b := &batchBackend{failAt: 2}
	lost := errors.New("lost fence")
	firstCalls := 0
	beforeFirst := func(context.Context, database.Tx) error { firstCalls++; return nil }
	fence := func(context.Context, database.Tx) error {
		b.fences++
		if b.fences == b.failAt {
			return lost
		}
		return nil
	}
	if err := Maintain(context.Background(), b, time.Now(), beforeFirst, fence); !errors.Is(err, lost) {
		t.Fatalf("lost fence: %v", err)
	}
	if b.commits != 1 || b.attempts != 2 || b.fences != 2 || firstCalls != 1 {
		t.Fatalf("failed batch committed or retried: %+v", b)
	}
	b.failAt = 0
	if err := Maintain(context.Background(), b, time.Now(), beforeFirst, fence); err != nil {
		t.Fatal(err)
	}
	if b.commits != 3 || b.attempts != 4 || b.fences != 4 || firstCalls != 2 {
		t.Fatalf("did not resume committed progress: %+v", b)
	}
}
