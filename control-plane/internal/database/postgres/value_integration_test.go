package postgres

import (
	"context"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/semantictest"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"testing"
)

func TestLogicalValuesIntegration(t *testing.T) {
	b := testBackend(t)
	semantictest.LogicalValues(t, b, `CREATE TEMP TABLE logical_values(id INT PRIMARY KEY,ts timestamptz,j jsonb,a text[])`, `INSERT INTO logical_values VALUES($1,$2,$3,$4)`, `SELECT ts,j,a FROM logical_values WHERE id=$1`, `SELECT clock_timestamp()`)
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
