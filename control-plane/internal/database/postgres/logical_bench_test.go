package postgres

import (
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

func BenchmarkLogicalArguments(b *testing.B) {
	nodeID := uuid.Must(uuid.NewV7())
	ts, err := value.FromTime(time.Now().UTC())
	if err != nil {
		b.Fatal(err)
	}
	jb, err := value.ParseJSONB([]byte(`{"active_state":"active","rtt_ms":12}`))
	if err != nil {
		b.Fatal(err)
	}

	plainArgs := []any{nodeID, "session-001", "alice", "192.0.2.1", int64(1024), int64(2048)}
	mixedArgs := []any{nodeID, ts, jb, "active", int64(42)}

	b.Run("PlainArgs", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			out := logicalArguments(plainArgs)
			if len(out) != len(plainArgs) {
				b.Fatal("unexpected length")
			}
		}
	})

	b.Run("MixedLogicalArgs", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			out := logicalArguments(mixedArgs)
			if len(out) != len(mixedArgs) {
				b.Fatal("unexpected length")
			}
		}
	})
}

func BenchmarkScanLogical(b *testing.B) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	rawJSON := []byte(`{"active_state":"active","rtt_ms":12,"endpoint":"198.51.100.10:443"}`)

	b.Run("PlainColumns", func(b *testing.B) {
		var id uuid.UUID
		var name string
		var version int64
		dest := []any{&id, &name, &version}
		scan := func(args ...any) error {
			*(args[1].(*string)) = "node-01"
			*(args[2].(*int64)) = 7
			return nil
		}
		b.ReportAllocs()
		for b.Loop() {
			if err := scanLogical(scan, dest); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("LogicalColumns", func(b *testing.B) {
		var ts1, ts2 value.Timestamp
		var jb1, jb2 value.JSONB
		dest := []any{&ts1, &ts2, &jb1, &jb2}
		scan := func(args ...any) error {
			*(args[0].(*pgtype.Timestamptz)) = pgtype.Timestamptz{Time: now, Valid: true}
			*(args[1].(*pgtype.Timestamptz)) = pgtype.Timestamptz{Time: now, Valid: true}
			*(args[2].(*[]byte)) = rawJSON
			*(args[3].(*[]byte)) = rawJSON
			return nil
		}
		b.ReportAllocs()
		for b.Loop() {
			if err := scanLogical(scan, dest); err != nil {
				b.Fatal(err)
			}
		}
	})
}
