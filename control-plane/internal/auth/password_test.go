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

func TestNewPasswordPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, password string
		valid          bool
	}{
		{"single", "x", false},
		{"fourteen", "aB7!dE9?gH2#jK", false},
		{"fifteen", "aB7!dE9?gH2#jK5", true},
		{"unicode-short", strings.Repeat("\u754c", 14), false},
		{"unicode-fifteen", strings.Repeat("\u754c", 15), true},
		{"spaces", "  a spacious password  ", true},
		{"long", strings.Repeat("long phrase ", 60), true},
		{"byte-limit", strings.Repeat("\u754c", 341) + "x", true},
		{"over-byte-limit", strings.Repeat("\u754c", 342), false},
		{"invalid-utf8", strings.Repeat("x", 15) + "\xff", false},
		{"common-long", "12345678901234567890", false},
		{"common-case", "ManchesterUnited", false},
		{"service", "OcserviaPassword", false},
		{"dictionary-substring", "my ocservia password has room for a unique phrase", true},
		{"lowercase-only", "violet rivers under distant stars", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateNewPassword(tc.password)
			if tc.valid && err != nil || !tc.valid && !errors.Is(err, ErrPasswordPolicy) {
				t.Fatalf("policy result: %v", err)
			}
			if !tc.valid {
				if hash, err := hashPassword(tc.password); hash != "" || !errors.Is(err, ErrPasswordPolicy) {
					t.Fatal("hashing bypassed policy")
				}
			}
		})
	}
}

// Historical hashes are constructed only in tests, never via a production bypass.
func legacyPasswordHash(password string) string {
	salt := []byte("legacy-test-salt!")
	key := argon2.IDKey([]byte(password), salt, passwordTime, passwordMemory, passwordThreads, passwordKeyBytes)
	return "$argon2id$v=19$m=19456,t=2,p=1$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(key)
}

func TestLegacyPasswordVerification(t *testing.T) {
	for _, password := range []string{"x", "password", "12345678901234567890"} {
		if err := ValidateNewPassword(password); !errors.Is(err, ErrPasswordPolicy) {
			t.Fatal("fixture is not rejected by new policy")
		}
		if ok, err := verifyPassword(legacyPasswordHash(password), password); !ok || err != nil {
			t.Fatalf("historical password rejected: %v", err)
		}
	}
}

func TestPasswordWriteEntrypointsRejectBeforeDatabase(t *testing.T) {
	// A nil pool makes any attempt to access the database fail this test.
	s := &Service{localEnabled: true}
	ctx := context.Background()
	id := uuid.Must(uuid.NewV7())
	for _, password := range []string{"x", "12345678901234567890", "ocserviapassword"} {
		calls := []func() (uuid.UUID, error){
			func() (uuid.UUID, error) { return s.CreateLocalCredential(ctx, "alice", password) },
			func() (uuid.UUID, error) { return s.BootstrapLocalAdmin(ctx, "alice", password, id) },
		}
		for _, action := range []string{"create", "reset-password"} {
			calls = append(calls, func() (uuid.UUID, error) {
				return s.MutateLocalUser(ctx, LocalUserMutation{Action: action, Username: "alice", Password: password, IdentityID: id, WorkspaceID: id, ActorID: id, SessionID: id, ApprovalID: id, RequestID: "policy-test"})
			})
		}
		for _, call := range calls {
			if got, err := call(); got != uuid.Nil || !errors.Is(err, ErrPasswordPolicy) {
				t.Fatalf("write entrypoint did not reject: %v", err)
			}
		}
	}
}

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
