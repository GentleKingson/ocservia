package value

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestTimestampRangeAndPrecision(t *testing.T) {
	for _, micros := range []int64{MinTimestamp, EndTimestamp - 1, -1, 0, 1} {
		v := Timestamp{Micros: micros, Valid: true}
		stamp, err := v.Time()
		if err != nil {
			t.Fatal(err)
		}
		got, err := FromTime(stamp)
		if err != nil || got != v {
			t.Fatal(micros, stamp, got, err)
		}
	}
	for _, micros := range []int64{MinTimestamp - 1, EndTimestamp} {
		if (Timestamp{Micros: micros, Valid: true}).Validate() == nil {
			t.Fatal("range accepted")
		}
	}
	a, _ := FromTime(time.Date(1969, 1, 1, 3, 0, 0, 123456789, time.FixedZone("east", 3*3600)))
	b, _ := FromTime(time.Date(1969, 1, 1, 0, 0, 0, 123456000, time.UTC))
	if a != b {
		t.Fatal("timezone or microsecond precision changed")
	}
	for _, v := range []Timestamp{{}, {Micros: NegativeInfinity, Valid: true}, {Micros: PositiveInfinity, Valid: true}} {
		if _, err := v.Time(); err == nil {
			t.Fatal("sentinel converted to finite time")
		}
	}
}

func TestJSONBExactDecimalAndValidation(t *testing.T) {
	for input, want := range map[string]string{`-0.000`: `0.000`, `1.2300e2`: `123.00`, `1.00e-2`: `0.0100`, `9007199254740993`: `9007199254740993`, `{"x":1,"x":2}`: `{"x":2}`} {
		v, err := ParseJSONB([]byte(input))
		if err != nil || string(v.Bytes()) != want {
			t.Fatal(input, string(v.Bytes()), err)
		}
	}
	for _, input := range []string{`"\ud800"`, `"\udc00"`, `"\u0000"`, `{"x":"\u0000","x":"ok"}`, `{"x":1e999999,"x":1}`, `1e-16384`, string([]byte{34, 255, 34}), `null null`} {
		if _, err := ParseJSONB([]byte(input)); err == nil {
			t.Fatalf("accepted %q", input)
		}
	}
	v, err := ParseJSONB([]byte(`1e1000`))
	if err != nil || string(v.Bytes()) != "1"+strings.Repeat("0", 1000) {
		t.Fatal("wide decimal", err)
	}
	if (JSONB{}).Valid() {
		t.Fatal("zero JSONB is not SQL NULL")
	}
	v, err = ParseJSONB([]byte(`null`))
	if err != nil || !v.Valid() {
		t.Fatal("JSON null lost", err)
	}
}

func TestJSONBOutputBudgetFailsBeforeExpansion(t *testing.T) {
	// Each number is individually legal; 3 x 1001 digits + 2 commas + 2 brackets.
	wide := `[1e1000,1e1000,1e1000]`
	for budget, ok := range map[int64]bool{3007: true, 3006: false} {
		_, err := parseJSONB([]byte(wide), budget)
		if (err == nil) != ok {
			t.Fatalf("budget %d: %v", budget, err)
		}
	}
	// A string and structure are charged too.
	for _, input := range []string{`{"key":"value"}`, `["<<<<"]`} {
		full, err := parseJSONB([]byte(input), maxJSONBBytes)
		if err != nil {
			t.Fatal(err)
		}
		size := int64(len(full.Bytes()))
		if _, err := parseJSONB([]byte(input), size); err != nil {
			t.Fatalf("exact budget for %s: %v", input, err)
		}
		if _, err := parseJSONB([]byte(input), size-1); err == nil {
			t.Fatalf("over-budget %s accepted", input)
		}
	}
	// One 100 KB legal number must be rejected from its computed size alone.
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := parseJSONB([]byte(`1e100000`), 1000)
	runtime.ReadMemStats(&after)
	if err == nil || after.TotalAlloc-before.TotalAlloc > 50000 {
		t.Fatalf("expanded before budget check: err=%v alloc=%d", err, after.TotalAlloc-before.TotalAlloc)
	}
	// A discarded duplicate never expands.
	if _, err := parseJSONB([]byte(`{"x":1e100000,"x":1}`), 20); err != nil {
		t.Fatal(err)
	}
}

func TestDecimalSizeMatchesCanonicalText(t *testing.T) {
	for _, input := range []string{`0`, `-0.000`, `0.00`, `1.2300e2`, `-1.00e-2`, `5e3`, `0.001e2`, `-123.456e-8`, `100e-1`, `0e5`, `1e1000`} {
		n, err := newDecimal(input)
		if err != nil || int64(len(n.String())) != n.size {
			t.Fatal(input, n.String(), n.size, err)
		}
	}
}
