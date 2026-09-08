package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	maxPasswordBytes = 1024
	maxUsernameBytes = 128
	// OWASP Password Storage Cheat Sheet: 19 MiB, two passes, one lane.
	passwordMemory    = 19 * 1024
	passwordTime      = 2
	passwordThreads   = 1
	passwordSaltBytes = 16
	passwordKeyBytes  = 32
	// Public dummy PHC value, not a credential. Verification still performs the
	// same Argon2id work as a current password hash; its result is never accepted.
	dummyPasswordHash = "$argon2id$v=19$m=19456,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
)

var errInvalidPasswordHash = errors.New("invalid Argon2id password hash")

func normalizeLocalUsername(username string) (string, error) {
	if len(username) > maxUsernameBytes {
		return "", errors.New("local username exceeds 128 bytes")
	}
	username = strings.ToLower(strings.TrimSpace(username))
	if username == "" {
		return "", errors.New("local username is empty")
	}
	for i, c := range username {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || i > 0 && (c == '.' || c == '_' || c == '-') {
			continue
		}
		return "", errors.New("local username must use ASCII letters, digits, dots, underscores or hyphens and start with a letter or digit")
	}
	return username, nil
}

func hashPassword(password string) (string, error) {
	if len(password) == 0 || len(password) > maxPasswordBytes {
		return "", errors.New("local password must contain 1 to 1024 bytes")
	}
	salt := make([]byte, passwordSaltBytes)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, passwordTime, passwordMemory, passwordThreads, passwordKeyBytes)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, passwordMemory, passwordTime, passwordThreads, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

func verifyPassword(encoded, password string) (bool, error) {
	if len(password) == 0 || len(password) > maxPasswordBytes {
		return false, nil
	}
	if len(encoded) > 512 {
		return false, errInvalidPasswordHash
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v=19" {
		return false, errInvalidPasswordHash
	}
	params := strings.Split(parts[3], ",")
	if len(params) != 3 {
		return false, errInvalidPasswordHash
	}
	var values [3]uint64
	for i, name := range []string{"m=", "t=", "p="} {
		if !strings.HasPrefix(params[i], name) {
			return false, errInvalidPasswordHash
		}
		value, err := strconv.ParseUint(strings.TrimPrefix(params[i], name), 10, 32)
		if err != nil {
			return false, errInvalidPasswordHash
		}
		values[i] = value
	}
	memory, iterations, threads := values[0], values[1], values[2]
	// Read parameters from the hash, but bound malformed/corrupted database
	// values before allocating memory or starting CPU-intensive work.
	if threads < 1 || threads > 4 || memory < 8*threads || memory > 64*1024 || iterations < 1 || iterations > 5 {
		return false, errInvalidPasswordHash
	}
	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil || len(salt) < 16 || len(salt) > 64 {
		return false, errInvalidPasswordHash
	}
	want, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil || len(want) < 16 || len(want) > 64 {
		return false, errInvalidPasswordHash
	}
	got := argon2.IDKey([]byte(password), salt, uint32(iterations), uint32(memory), uint8(threads), uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
