package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

func deriveAccountLogKey(sessionKey []byte) []byte {
	mac := hmac.New(sha256.New, sessionKey)
	_, _ = mac.Write([]byte("ocservia/auth-log/account-key/v1\x00"))
	return mac.Sum(nil)
}

// LocalAccountRef is a keyed pseudonym, not anonymous data. It follows the
// credential normalization and changes when the session encryption key rotates.
func (s *Service) LocalAccountRef(username string) string {
	name, err := normalizeLocalUsername(username)
	if err != nil {
		return "invalid"
	}
	mac := hmac.New(sha256.New, s.accountLogKey)
	_, _ = mac.Write([]byte("ocservia/auth-log/local-account/v1\x00" + name))
	return "v1:" + hex.EncodeToString(mac.Sum(nil))
}
