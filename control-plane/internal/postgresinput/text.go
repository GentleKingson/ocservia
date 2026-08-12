package postgresinput

import (
	"strings"
	"unicode/utf8"
)

// ValidText reports whether a required string is safe to store in a
// PostgreSQL character type with the given UTF-8 byte limit.
func ValidText(value string, maxBytes int) bool {
	return value != "" && len(value) <= maxBytes && utf8.ValidString(value) && strings.IndexByte(value, 0) < 0
}
