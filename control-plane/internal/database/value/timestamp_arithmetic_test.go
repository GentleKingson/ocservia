package value

import (
	"testing"
	"time"
)

func TestTimestampAdd(t *testing.T) {
	for _, v := range []Timestamp{{}, {Valid: true, Micros: NegativeInfinity}, {Valid: true, Micros: PositiveInfinity}} {
		for _, d := range []time.Duration{-time.Hour, time.Hour} {
			got, err := v.Add(d)
			if err != nil || got != v {
				t.Fatalf("%+v + %s = %+v: %v", v, d, got, err)
			}
		}
	}
	for _, test := range []struct {
		at       int64
		duration time.Duration
		want     int64
		invalid  bool
	}{
		{MinTimestamp, time.Microsecond, MinTimestamp + 1, false},
		{MinTimestamp, -time.Microsecond, 0, true},
		{EndTimestamp - 1, -time.Microsecond, EndTimestamp - 2, false},
		{EndTimestamp - 1, time.Microsecond, 0, true},
		{123, time.Nanosecond, 123, false},
		{0, -time.Microsecond, -1, false},
	} {
		got, err := (Timestamp{Valid: true, Micros: test.at}).Add(test.duration)
		if (err != nil) != test.invalid || (!test.invalid && (!got.Valid || got.Micros != test.want)) {
			t.Fatalf("%+v: %+v %v", test, got, err)
		}
	}
}
