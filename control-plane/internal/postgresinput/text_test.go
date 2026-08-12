package postgresinput

import "testing"

func TestValidText(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		maxBytes int
		want     bool
	}{
		{name: "valid", value: "capacity_exceeded", maxBytes: 128, want: true},
		{name: "exact byte limit", value: "abcd", maxBytes: 4, want: true},
		{name: "empty", value: "", maxBytes: 128, want: false},
		{name: "over byte limit", value: "abcde", maxBytes: 4, want: false},
		{name: "nul", value: "poison\x00code", maxBytes: 128, want: false},
		{name: "invalid utf8", value: string([]byte{0xff}), maxBytes: 128, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ValidText(test.value, test.maxBytes); got != test.want {
				t.Fatalf("ValidText(%q, %d) = %t, want %t", test.value, test.maxBytes, got, test.want)
			}
		})
	}
}
