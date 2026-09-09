package value

import (
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
