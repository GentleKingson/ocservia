package postgres

import (
	"bytes"
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
)

func TestLogicalValuesIntegration(t *testing.T) {
	b := testBackend(t)
	ctx := context.Background()
	tx, err := b.Begin(ctx, database.ReadCommitted)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `CREATE TEMP TABLE logical_values(id INT PRIMARY KEY,ts timestamptz,j jsonb,a text[])`); err != nil {
		t.Fatal(err)
	}
	a, empty, nullliteral := "a", "", "NULL"
	arrays := []value.TextArray{{}, {Valid: true}, {Valid: true, Dimensions: []value.Dimension{{Length: 2, LowerBound: -7}, {Length: 2, LowerBound: 0}}, Elements: []*string{&a, nil, &empty, &nullliteral}}}
	times := []value.Timestamp{{}, {Valid: true, Micros: value.MinTimestamp}, {Valid: true, Micros: value.EndTimestamp - 1}, {Valid: true, Micros: value.NegativeInfinity}, {Valid: true, Micros: value.PositiveInfinity}, {Valid: true, Micros: -1}, {Valid: true, Micros: 0}}
	jsons := [][]byte{nil, []byte(`null`), []byte(`{"a":1,"a":2.000,"wide":9007199254740993,"tiny":1e-1000,"large":1e1000,"unicode":"\ud83d\ude00"}`)}
	for i, ts := range times {
		arr := arrays[i%len(arrays)]
		j, err := value.ParseJSONB(jsons[i%len(jsons)])
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO logical_values VALUES($1,$2,$3,$4)`, i, ts, j, arr); err != nil {
			t.Fatal(err)
		}
		var gotTime value.Timestamp
		var gotJSON value.JSONB
		var gotArray value.TextArray
		if err = tx.QueryRow(ctx, `SELECT ts,j,a FROM logical_values WHERE id=$1`, i).Scan(&gotTime, &gotJSON, &gotArray); err != nil {
			t.Fatal(err)
		}
		if gotTime != ts || !bytes.Equal(gotJSON.Bytes(), j.Bytes()) {
			t.Fatalf("logical value changed at %d", i)
		}
		if arr.Valid != gotArray.Valid || len(arr.Dimensions) != len(gotArray.Dimensions) || len(arr.Elements) != len(gotArray.Elements) || len(arr.Elements) > 0 && (!reflect.DeepEqual(arr.Dimensions, gotArray.Dimensions) || !reflect.DeepEqual(arr.Elements, gotArray.Elements)) {
			t.Fatalf("array changed: %+v != %+v", arr, gotArray)
		}
	}
	first, err := database.TransactionTime(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(25 * time.Millisecond)
	second, err := database.TransactionTime(ctx, tx)
	if err != nil || first != second {
		t.Fatal("transaction timestamp changed", err)
	}
	var wall value.Timestamp
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&wall); err != nil || wall.Micros <= first.Micros {
		t.Fatal("transaction-start time not distinct from server wall clock", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = database.TransactionTime(cancelled, tx); err == nil {
		t.Fatal("cancelled clock accepted")
	}
}

func TestJSONBInputParityIntegration(t *testing.T) {
	b := testBackend(t)
	inputs := []string{`{"a":1,"a":2}`, `1e131071`, `1e131072`, `1e-16383`, `1e-16384`, `0e1000000`, `0e-1000000`, `0.000e16386`, `1000e-16385`, `{"x":1e999999,"x":1}`, `"\ud800"`, `"\udc00"`, `"\ud800\udc00"`, `"\u0000"`, `{"x":"\u0000","x":"ok"}`, `-0.000`, `1.2300e2`, `1.00e-2`}
	for _, input := range inputs {
		t.Run(input, func(t *testing.T) {
			var native string
			pgErr := b.QueryRow(context.Background(), `SELECT $1::jsonb::text`, input).Scan(&native)
			ours, err := value.ParseJSONB([]byte(input))
			if (pgErr == nil) != (err == nil) {
				t.Fatalf("acceptance mismatch pg=%v ours=%v", pgErr, err)
			}
			if err != nil {
				return
			}
			canonical, err := value.ParseJSONB([]byte(native))
			if err != nil || string(canonical.Bytes()) != string(ours.Bytes()) {
				t.Fatalf("value differs: native=%s local=%s err=%v", native, ours.Bytes(), err)
			}
		})
	}
}
