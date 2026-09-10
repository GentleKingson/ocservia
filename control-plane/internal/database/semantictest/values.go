package semantictest

import (
	"bytes"
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
)

// LogicalValues exercises actual prepared writes and scans using the backend's
// physical storage, including extremes that native DATETIME/JSON cannot store.
func LogicalValues(t *testing.T, b database.Backend, create, insert, selectRow, clockQuery string) {
	t.Helper()
	ctx := context.Background()
	tx, err := b.Begin(ctx, database.ReadCommitted)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, create); err != nil {
		t.Fatal(err)
	}
	a := "a"
	empty := ""
	nullliteral := "NULL"
	arrays := []value.TextArray{{}, {Valid: true}, {Valid: true, Dimensions: []value.Dimension{{Length: 2, LowerBound: -7}, {Length: 2, LowerBound: 0}}, Elements: []*string{&a, nil, &empty, &nullliteral}}}
	times := []value.Timestamp{{}, {Valid: true, Micros: value.MinTimestamp}, {Valid: true, Micros: value.EndTimestamp - 1}, {Valid: true, Micros: value.NegativeInfinity}, {Valid: true, Micros: value.PositiveInfinity}, {Valid: true, Micros: -1}, {Valid: true, Micros: 0}}
	jsons := [][]byte{nil, []byte(`null`), []byte(`{"a":1,"a":2.000,"wide":9007199254740993,"tiny":1e-1000,"large":1e1000,"unicode":"\ud83d\ude00"}`)}
	for i, ts := range times {
		arr := arrays[i%len(arrays)]
		j, err := value.ParseJSONB(jsons[i%len(jsons)])
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, insert, i, ts, j, arr); err != nil {
			t.Fatal(err)
		}
		var gotTime value.Timestamp
		var gotJSON value.JSONB
		var gotArray value.TextArray
		if err = tx.QueryRow(ctx, selectRow, i).Scan(&gotTime, &gotJSON, &gotArray); err != nil {
			t.Fatal(err)
		}
		if gotTime != ts || !bytes.Equal(gotJSON.Bytes(), j.Bytes()) {
			t.Fatalf("logical value changed at %d", i)
		}
		wantBytes, _ := arr.Bytes()
		gotBytes, _ := gotArray.Bytes()
		// Empty dimension slices have no semantic difference from nil slices.
		if arr.Valid != gotArray.Valid || len(arr.Dimensions) != len(gotArray.Dimensions) || len(arr.Elements) != len(gotArray.Elements) || len(arr.Elements) > 0 && (!reflect.DeepEqual(arr.Dimensions, gotArray.Dimensions) || !reflect.DeepEqual(arr.Elements, gotArray.Elements)) {
			t.Fatalf("array changed: %s != %s", wantBytes, gotBytes)
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
	if err = tx.QueryRow(ctx, clockQuery).Scan(&wall); err != nil || wall.Micros <= first.Micros {
		t.Fatal("transaction-start time not distinct from server wall clock", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = database.TransactionTime(cancelled, tx); err == nil {
		t.Fatal("cancelled clock accepted")
	}
}
