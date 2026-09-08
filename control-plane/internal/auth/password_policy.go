package auth

import (
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

var ErrPasswordPolicy = errors.New("Local password policy")

//go:embed password_blocklist/common-passwords.txt
var commonPasswords string

//go:embed password_blocklist/service-passwords.txt
var servicePasswords string

var blockedPasswords = func() map[string]struct{} {
	blocked := make(map[string]struct{})
	for _, list := range []string{commonPasswords, servicePasswords} {
		for _, password := range strings.Split(list, "\n") {
			if password != "" {
				blocked[password] = struct{}{}
			}
		}
	}
	return blocked
}()

// ValidateNewPassword applies only when setting a password, never at login.
// Future self-service password changes must use this same policy via hashPassword.
func ValidateNewPassword(password string) error {
	if len(password) > maxPasswordBytes {
		return fmt.Errorf("%w: use at most 1024 UTF-8 bytes", ErrPasswordPolicy)
	}
	if !utf8.ValidString(password) {
		return fmt.Errorf("%w: use valid Unicode text", ErrPasswordPolicy)
	}
	if utf8.RuneCountInString(password) < 15 {
		return fmt.Errorf("%w: use at least 15 Unicode characters", ErrPasswordPolicy)
	}
	// Compare the whole candidate, case-insensitively, without changing the
	// password passed to Argon2id. Do not trim, normalize or match substrings.
	if _, blocked := blockedPasswords[strings.ToLower(password)]; blocked {
		return fmt.Errorf("%w: choose a password that is not common, compromised or service-related", ErrPasswordPolicy)
	}
	return nil
}
