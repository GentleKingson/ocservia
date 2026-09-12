package mysql

import (
	"context"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
)

func typeFixture(t *testing.T) *Backend {
	t.Helper()
	b, _, _ := migrateFixture(t)
	return b
}

func TestRealLogicalValueValidators(t *testing.T) {
	b := typeFixture(t)
	ctx := context.Background()
	inputs := []string{`{"x":1e1000}`, `{"x":1e-16383}`, `{"x":1e-16384}`, `{"x":1e131071}`, `{"x":1e131072}`, `{"x":0e1000000}`, `{"x":0e1073741824}`, `{"x":-0.000}`, `{"x":"\u0000"}`, `{"x":"\ud800"}`, `{"x":"\udc00"}`, `{"x":"\ud800\udc00"}`, `{"x":"\ud800z\udc00"}`, `{"x":"\u0000","x":"ok"}`, `{"x":1e999999,"x":1}`, `{"x":1,"x":2}`, `{"x":"literal\\u0000"}`, `[]`, `{"x":01}`, `{"x":true}`, string([]byte{'{', '"', 'x', '"', ':', '"', 255, '"', '}'})}
	for _, input := range inputs {
		t.Run(input, func(t *testing.T) {
			j, err := value.ParseJSONB([]byte(input))
			want := err == nil && j.Valid() && j.Bytes()[0] == '{'
			var valid bool
			if err := b.QueryRow(ctx, `SELECT ocserv_jsonb_object_valid(?)`, []byte(input)).Scan(&valid); err != nil {
				t.Fatal(err)
			}
			if valid != want {
				t.Fatalf("validator=%v logical=%v", valid, want)
			}
		})
	}
	text := "text"
	empty := ""
	arrays := []value.TextArray{{Valid: true}, {Valid: true, Dimensions: []value.Dimension{{Length: 2, LowerBound: -1}, {Length: 2, LowerBound: 0}}, Elements: []*string{nil, &text, &empty, &text}}}
	for _, a := range arrays {
		raw, err := a.Bytes()
		if err != nil {
			t.Fatal(err)
		}
		var valid bool
		if err = b.QueryRow(ctx, `SELECT ocserv_text_array_valid(?)`, raw).Scan(&valid); err != nil || !valid {
			t.Fatal("array validator", string(raw), valid, err)
		}
	}
	for _, raw := range []string{`{"dimensions":[],"elements":["x"]}`, `{"dimensions":[{"length":1,"lower_bound":2147483648}],"elements":["x"]}`, `{"dimensions":[{"length":1,"lower_bound":0}],"elements":[5]}`, `{"dimensions":[{"length":1,"lower_bound":0}],"elements":["\u0000"]}`, `{"dimensions":[{"length":1,"lower_bound":0}],"elements":["\ud800"]}`, `{"dimensions":null,"elements":[]}`} {
		var valid bool
		if err := b.QueryRow(ctx, `SELECT ocserv_text_array_valid(?)`, raw).Scan(&valid); err != nil || valid {
			t.Fatal("invalid array accepted", raw, valid, err)
		}
	}
}

func TestRealFiniteTimestampBackfill(t *testing.T) {
	b, _, _ := fixture(t)
	ctx := context.Background()
	tx, err := b.Begin(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `CREATE TEMPORARY TABLE time_backfill(original DATETIME(6), logical BIGINT)`); err != nil {
		t.Fatal(err)
	}
	for _, stamp := range []time.Time{time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC), time.Date(1969, 12, 31, 23, 59, 59, 999999000, time.UTC), time.Date(2000, 1, 1, 0, 0, 0, 1, time.UTC)} {
		if _, err = tx.Exec(ctx, `INSERT INTO time_backfill(original) VALUES(?)`, stamp); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE time_backfill SET logical=TIMESTAMPDIFF(MICROSECOND,'2000-01-01 00:00:00',original)`); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(ctx, `SELECT original,logical FROM time_backfill`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var original time.Time
		var logical value.Timestamp
		if err = rows.Scan(&original, &logical); err != nil {
			t.Fatal(err)
		}
		restored, err := logical.Time()
		if err != nil || !restored.Equal(original) {
			t.Fatal("finite value altered", original, restored, err)
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
}
