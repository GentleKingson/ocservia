package auth

import (
	"bytes"
	"strings"
	"testing"
)

func TestLocalAccountLogReference(t *testing.T) {
	s := &Service{accountLogKey: deriveAccountLogKey(bytes.Repeat([]byte{1}, 32))}
	other := &Service{accountLogKey: deriveAccountLogKey(bytes.Repeat([]byte{2}, 32))}
	ref := s.LocalAccountRef("Alice")
	if len(ref) != 67 || ref != s.LocalAccountRef(" ALICE ") || ref == other.LocalAccountRef("alice") || ref == s.LocalAccountRef("bob") {
		t.Fatal("account correlation/key isolation failed")
	}
	for _, invalid := range []string{"", "a\r\nforged", strings.Repeat("a", 129)} {
		if s.LocalAccountRef(invalid) != "invalid" {
			t.Fatal("invalid account was logged")
		}
	}
}
