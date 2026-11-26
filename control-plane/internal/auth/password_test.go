package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/crypto/argon2"
)

func TestNewPasswordPolicyAndEntrypoints(t *testing.T) {
	t.Parallel()
	s := &Service{localEnabled: true}
	id := uuid.Must(uuid.NewV7())
	for _, tc := range []struct {
		name, password string
		valid          bool
	}{
		{"seven", "nA6/X2k", false},
		{"eight", "nA6/X2kZ", true},
		{"unicode-eight", strings.Repeat("\u754c", 8), true},
		{"over-byte-limit", strings.Repeat("\u754c", 342), false},
		{"common-long", "12345678901234567890", false},
		{"service", "OcserviaPassword", false},
		{"lowercase-only", "violet rivers under distant stars", true},
	} {
		err := ValidateNewPassword(tc.password)
		if tc.valid && err != nil || !tc.valid && !errors.Is(err, ErrPasswordPolicy) {
			t.Fatalf("%s: policy result: %v", tc.name, err)
		}
		if !tc.valid {
			if _, err := s.CreateLocalCredential(context.Background(), "alice", tc.password); !errors.Is(err, ErrPasswordPolicy) {
				t.Fatalf("%s: write entrypoint bypassed policy: %v", tc.name, err)
			}
			if _, err := s.BootstrapLocalAdmin(context.Background(), "alice", tc.password, id); !errors.Is(err, ErrPasswordPolicy) {
				t.Fatalf("%s: bootstrap bypassed policy: %v", tc.name, err)
			}
		}
	}
}

func legacyPasswordHash(password string) string {
	salt := []byte("legacy-test-salt!")
	key := argon2.IDKey([]byte(password), salt, passwordTime, passwordMemory, passwordThreads, passwordKeyBytes)
	return "$argon2id$v=19$m=19456,t=2,p=1$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(key)
}

func TestArgon2idHashAndVerify(t *testing.T) {
	t.Parallel()
	password := "correct password with spaces "
	first, err := hashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatal("missing algorithm/version/parameters")
	}
	if ok, err := verifyPassword(first, password); err != nil || !ok {
		t.Fatalf("correct password: %v, %v", ok, err)
	}
	if ok, err := verifyPassword(first, strings.TrimSpace(password)); err != nil || ok {
		t.Fatalf("password was trimmed: %v, %v", ok, err)
	}
	if ok, err := verifyPassword(dummyPasswordHash, password); err != nil || ok {
		t.Fatalf("dummy verification: %v, %v", ok, err)
	}
	for _, invalid := range []string{"", strings.Replace(first, "v=19", "v=16", 1), first + "$extra"} {
		if ok, err := verifyPassword(invalid, password); ok || err == nil {
			t.Fatalf("invalid hash accepted: %q", invalid)
		}
	}
}

func TestNormalizeLocalUsername(t *testing.T) {
	t.Parallel()
	if got, err := normalizeLocalUsername(" Alice.Example_1-2 "); err != nil || got != "alice.example_1-2" {
		t.Fatalf("normalization = %q, %v", got, err)
	}
	for _, input := range []string{"", "alice bob", "alice@example.com", ".alice", "\x00alice"} {
		if _, err := normalizeLocalUsername(input); err == nil {
			t.Fatalf("invalid username accepted: %q", input)
		}
	}
}
