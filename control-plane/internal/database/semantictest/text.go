// Package semantictest contains shared real-server regression cases, not a
// runtime SQL translation or regular-expression compatibility implementation.
package semantictest

import (
	"regexp"
	"strings"
	"testing"
)

// These are the complete distinct regex predicates in Controller schema 34.
// Only this ASCII subset is tested; this is not general POSIX/ICU/PCRE parity.
var Patterns = map[string]string{
	`^[a-z0-9][a-z0-9._-]*$`:                     "alice",
	`^[a-z][a-z0-9_]{0,63}$`:                     "reason_code",
	`^[0-9a-f]{40}$`:                             strings.Repeat("a", 40),
	`^00-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$`: "00-" + strings.Repeat("a", 32) + "-" + strings.Repeat("b", 16) + "-01",
	`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`:          "Alice",
	`^[A-Za-z0-9._-]+$`:                          "key-A",
	`^[A-Za-z0-9_.-]{1,128}$`:                    "key-A",
	`^ed25519-sha256:[0-9a-f]{64}$`:              "ed25519-sha256:" + strings.Repeat("a", 64),
	`(^|/)\.\.(/|$)`:                             "../secret",
	`^[A-Za-z0-9._-]{1,64}$`:                     "provider-A",
}

func PatternsMatch(t *testing.T, actual map[string]string, match func(string, string) (bool, error)) {
	t.Helper()
	if len(actual) != len(Patterns) {
		t.Fatalf("schema regex inventory changed: got %d want %d", len(actual), len(Patterns))
	}
	for pg, seed := range Patterns {
		pattern, ok := actual[pg]
		if !ok {
			t.Fatalf("missing schema regex %q", pg)
		}
		t.Run(pg, func(t *testing.T) {
			oracle := regexp.MustCompile(pg)
			values := []string{"", seed, strings.Repeat("a", 64), strings.Repeat("a", 65), strings.Repeat("a", 128), strings.Repeat("a", 129), "a/../b", "a/..", "a/..\n", "a/..\r\n", "a/..\u2028", "a/.../b", "a/./b", "a/..x", "..x"}
			for r := rune(1); r < 128; r++ {
				values = append(values, string(r), seed+string(r), string(r)+seed)
			}
			for _, s := range []string{"é", "İ", "ſ", "K", "Ａ", "١", "\u0085", "\u2028", "\u2029", "\r\n", " ", "\n\n"} {
				values = append(values, s, seed+s, s+seed)
			}
			for _, value := range values {
				got, err := match(value, pattern)
				if err != nil {
					t.Fatalf("value %q: %v", value, err)
				}
				if want := oracle.MatchString(value); got != want {
					t.Fatalf("value %q: got %v want %v", value, got, want)
				}
			}
		})
	}
}

// Existing PostgreSQL CHECK limits, not new product restrictions. The four-byte
// character cases exercise index byte budgets rather than only ASCII lengths.
func TextBounds(t *testing.T, table string, insert func(string) error) {
	t.Helper()
	t.Run(table, func(t *testing.T) {
		maximum := 256
		if table == "secret_provider_refs" {
			maximum = 512
			for _, value := range []string{"a/..\n", "a/..\r\n", "a/..\u2028", "a/.../b"} {
				if err := insert(value); err != nil {
					t.Fatalf("non-traversal path %q rejected: %v", value, err)
				}
			}
			for _, value := range []string{"../a", "a/..", "a/../b"} {
				if err := insert(value); err == nil {
					t.Fatalf("traversal path %q accepted", value)
				}
			}
		}
		for _, unit := range []string{"a", "\U0001f642"} {
			for _, n := range []int{191, 192, maximum - 1, maximum} {
				value := strings.Repeat(unit, n)
				if err := insert(value); err != nil {
					t.Fatalf("%d characters rejected: %v", n, err)
				}
				if err := insert(value); err == nil {
					t.Fatal("exact duplicate accepted")
				}
			}
			if err := insert(strings.Repeat(unit, maximum-1) + " "); err != nil {
				t.Fatal("trailing space changed uniqueness", err)
			}
			for _, value := range []string{"x" + strings.Repeat(unit, maximum), "y" + strings.Repeat(unit, maximum-1) + " ", "z" + strings.Repeat(unit, maximum-1) + "  "} {
				if err := insert(value); err == nil {
					t.Fatal("existing length CHECK bypassed or trailing spaces silently truncated")
				}
			}
		}
	})
}
