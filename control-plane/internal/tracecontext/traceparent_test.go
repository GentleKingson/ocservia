package tracecontext

import (
	"fmt"
	"strings"
	"testing"
)

const valid = "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"

func TestTraceID(t *testing.T) {
	for _, input := range []string{
		"", "00-0123-0123-01", valid[:54], valid + "-00", valid + " ",
		"01" + valid[2:], "ff" + valid[2:], strings.ToUpper(valid),
		strings.Replace(valid, "0123456789abcdef0123456789abcdef", strings.Repeat("0", 32), 1),
		strings.Replace(valid, "-0123456789abcdef-", "-0000000000000000-", 1),
		strings.Replace(valid, "a", "界", 1), strings.Replace(valid, "a", "g", 1),
	} {
		if got := TraceID(input); got != "" {
			t.Errorf("accepted %q: %q", input, got)
		}
	}
	for flags := range 256 {
		input := valid[:53] + fmt.Sprintf("%02x", flags)
		if got := TraceID(input); got != valid[3:35] {
			t.Errorf("flags %02x: %q", flags, got)
		}
	}
	// Every one-byte mutation exercises delimiters, field lengths, ASCII and hex.
	for i := range len(valid) {
		for c := range 256 {
			input := []byte(valid)
			input[i] = byte(c)
			checkLegacy(t, string(input))
		}
	}
}

func FuzzTraceID(f *testing.F) {
	for _, input := range []string{"", valid, valid + "-00", "00--界-ff", strings.Repeat("0", 55)} {
		f.Add(input)
	}
	f.Fuzz(func(t *testing.T, input string) { checkLegacy(t, input) })
}

func checkLegacy(t *testing.T, input string) {
	t.Helper()
	want := legacyTraceID(input)
	if got := TraceID(input); got != want {
		t.Fatalf("TraceID(%q) = %q; legacy = %q", input, got, want)
	}
}

// Independent oracle for the pre-consolidation service acceptance contract.
func legacyTraceID(value string) string {
	parts := strings.Split(value, "-")
	if len(parts) != 4 || parts[0] != "00" || len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return ""
	}
	for _, part := range parts[1:] {
		for _, c := range part {
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return ""
			}
		}
	}
	if parts[1] == strings.Repeat("0", 32) || parts[2] == strings.Repeat("0", 16) {
		return ""
	}
	return parts[1]
}
