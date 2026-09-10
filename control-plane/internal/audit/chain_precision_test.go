package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/audit/auditstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

type precisionStore struct {
	auditstore.Store
	at   time.Time
	args []any
}

func TestAppendHashesPersistedJSON(t *testing.T) {
	store := &precisionStore{at: time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)}
	record := numericFixture(" \n null \t")
	record.BeforeSummary = json.RawMessage(`{ "n":1.2300, "dup":0, "dup":2 }`)
	if err := AppendChainTx(context.Background(), precisionTx{store: store}, record); err != nil {
		t.Fatal(err)
	}
	record.At = store.at
	record.BeforeSummary = store.args[16].(value.JSONB).Bytes()
	record.AfterSummary = store.args[17].(value.JSONB).Bytes()
	payload, err := encodeChainPayload(nil, record)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(payload)
	if !bytes.Equal(hash[:], store.args[20].([]byte)) || !bytes.Equal(signEvent(currentEventAuthenticator().key, hash[:]), store.args[23].([]byte)) {
		t.Fatal("persisted JSON cannot reproduce hash/MAC")
	}
}

func (s *precisionStore) Lock(context.Context, uuid.UUID) error    { return nil }
func (s *precisionStore) Clock(context.Context) (time.Time, error) { return s.at, nil }
func (s *precisionStore) Previous(context.Context, uuid.UUID) ([]byte, error) {
	return nil, database.ErrNotFound
}
func (s *precisionStore) Append(_ context.Context, args []any) error {
	s.args = args
	return nil
}

type precisionTx struct {
	database.Tx
	store *precisionStore
}

func (tx precisionTx) AuditStore() auditstore.Store { return tx.store }

func TestAppendHashesPersistedTime(t *testing.T) {
	for _, year := range []int{1960, 2026} {
		store := &precisionStore{at: time.Date(year, 9, 10, 12, 0, 0, 123456789, time.FixedZone("offset", 8*60*60))}
		record := ChainRecord{EventID: uuid.New(), WorkspaceID: uuid.New(), ActorType: "controller", ActorID: "precision", Action: "audit.precision", ResourceType: "workspace", RequestID: "precision", Result: "succeeded"}
		if err := AppendChainTx(context.Background(), precisionTx{store: store}, record); err != nil {
			t.Fatal(err)
		}
		var err error
		record.At, err = store.args[2].(value.Timestamp).Time()
		if err != nil || !record.At.Equal(store.at.Truncate(time.Microsecond)) {
			t.Fatalf("persisted time: %v %v", record.At, err)
		}
		payload, err := encodeChainPayload(nil, record)
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(payload)
		if !bytes.Equal(hash[:], store.args[20].([]byte)) || !bytes.Equal(signEvent(currentEventAuthenticator().key, hash[:]), store.args[23].([]byte)) {
			t.Fatal("persisted timestamp cannot reproduce hash/MAC")
		}
	}
}
