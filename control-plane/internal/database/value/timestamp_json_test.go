package value

import (
	"encoding/json"
	"testing"
)

func TestTimestampJSON(t *testing.T) {
	for _, micros := range []int64{MinTimestamp, EndTimestamp - 1, NegativeInfinity, PositiveInfinity, -1, 0, 1} {
		want := Timestamp{Micros: micros, Valid: true}
		encoded, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		var text string
		if err := json.Unmarshal(encoded, &text); err != nil {
			t.Fatal(err)
		}
		got, err := ParseTimestamp(text)
		if err != nil || got != want {
			t.Fatalf("round trip %s: %+v %v", encoded, got, err)
		}
		var decoded Timestamp
		if err := json.Unmarshal(encoded, &decoded); err != nil || decoded != want {
			t.Fatalf("JSON round trip %s: %+v %v", encoded, decoded, err)
		}
	}
	for text, expected := range map[string]string{
		"2026-09-10T12:34:56.123456+08:00": `"2026-09-10T04:34:56.123456Z"`,
		"+010000-02-29T00:00:00Z":          `"+010000-02-29T00:00:00Z"`,
		"-000400-02-29T00:00:00Z":          `"-000400-02-29T00:00:00Z"`,
		"+010000-01-01T00:00:00+01:00":     `"9999-12-31T23:00:00Z"`,
	} {
		at, err := ParseTimestamp(text)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(at)
		if err != nil || string(encoded) != expected {
			t.Fatalf("%s: %s %v", text, encoded, err)
		}
	}
	for _, text := range []string{"", "null", "+010001-02-29T00:00:00Z", "-000100-02-29T00:00:00Z", "+999999-01-01T00:00:00Z", "+01x000-01-01T00:00:00Z"} {
		if _, err := ParseTimestamp(text); err == nil {
			t.Fatalf("accepted %q", text)
		}
	}
	if got, err := json.Marshal(Timestamp{}); err != nil || string(got) != "null" {
		t.Fatalf("NULL: %s %v", got, err)
	}
	if _, err := json.Marshal(Timestamp{Micros: EndTimestamp, Valid: true}); err == nil {
		t.Fatal("out-of-range timestamp serialized")
	}
	stored := Timestamp{Micros: 1, Valid: true}
	if err := json.Unmarshal([]byte(`"invalid"`), &stored); err == nil || stored.Micros != 1 {
		t.Fatal("invalid JSON timestamp replaced destination")
	}
	if err := json.Unmarshal([]byte(`null`), &stored); err != nil || stored.Valid {
		t.Fatal("NULL failed to clear timestamp", err)
	}
}
