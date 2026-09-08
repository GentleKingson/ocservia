package auth

import (
	"encoding/base64"
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"
)

func TestArgon2idHashAndVerify(t *testing.T) {
	password := "correct password with spaces "
	first, err := hashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	second, err := hashPassword(password)
	if err != nil || first == second {
		t.Fatalf("independent salts were not used: %v", err)
	}
	for _, hash := range []string{first, second} {
		if !strings.HasPrefix(hash, "$argon2id$v=19$m=19456,t=2,p=1$") {
			t.Fatal("missing algorithm/version/parameters")
		}
		if ok, err := verifyPassword(hash, password); err != nil || !ok {
			t.Fatalf("correct password: %v, %v", ok, err)
		}
		if ok, err := verifyPassword(hash, strings.TrimSpace(password)); err != nil || ok {
			t.Fatalf("password was trimmed: %v, %v", ok, err)
		}
	}
	if ok, err := verifyPassword(dummyPasswordHash, password); err != nil || ok {
		t.Fatalf("dummy verification: %v, %v", ok, err)
	}
	// A stored hash must retain its own cost, not use today's write defaults.
	salt := []byte("0123456789abcdef")
	key := argon2.IDKey([]byte(password), salt, 1, 37*1024, 1, 32)
	older := "$argon2id$v=19$m=37888,t=1,p=1$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(key)
	if ok, err := verifyPassword(older, password); err != nil || !ok {
		t.Fatalf("stored parameters were ignored: %v, %v", ok, err)
	}
	for _, invalid := range []string{
		"", strings.Repeat("x", 513), strings.Replace(first, "v=19", "v=16", 1),
		strings.Replace(first, "argon2id", "argon2i", 1),
		strings.Replace(first, "m=19456", "m=4294967295", 1),
		strings.Replace(first, "t=2", "t=0", 1),
		strings.Replace(first, "p=1", "p=256", 1),
		strings.Replace(first, "m=19456", "m=invalid", 1),
		first + "$extra", first[:len(first)-1] + "!",
	} {
		if ok, err := verifyPassword(invalid, password); ok || err == nil {
			t.Fatalf("invalid hash accepted: %q", invalid)
		}
	}
	for _, password := range []string{"", strings.Repeat("p", maxPasswordBytes+1)} {
		if _, err := hashPassword(password); err == nil {
			t.Fatal("invalid password length accepted")
		}
		if ok, _ := verifyPassword(first, password); ok {
			t.Fatal("invalid password length verified")
		}
	}
	if _, err := hashPassword(strings.Repeat("p", maxPasswordBytes)); err != nil {
		t.Fatal(err)
	}
}

func TestNormalizeLocalUsername(t *testing.T) {
	if got, err := normalizeLocalUsername(" Alice.Example_1-2 "); err != nil || got != "alice.example_1-2" {
		t.Fatalf("normalization = %q, %v", got, err)
	}
	for _, input := range []string{"", " ", "alice bob", "alice@example.com", ".alice", "\x00alice", "\u00e1lice", strings.Repeat("a", maxUsernameBytes+1)} {
		if _, err := normalizeLocalUsername(input); err == nil {
			t.Fatalf("invalid username accepted: %q", input)
		}
	}
	if _, err := normalizeLocalUsername(strings.Repeat("a", maxUsernameBytes)); err != nil {
		t.Fatal(err)
	}
}
