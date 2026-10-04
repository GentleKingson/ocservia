package mysql

import "regexp"

// Match only redundant inherited charset declarations; the complete candidate
// must still match a pinned SQL fingerprint, including all constraints.
var redundantColumnCharset = regexp.MustCompile("(?m)^(  `[^`]+` (?:(?:var)?char\\([0-9]+\\)|(?:tiny|medium|long)?text)) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin")

func tableFingerprint(definition string) string {
	a, err := loadMySQLArtifacts()
	if err != nil {
		return digest([]byte(definition))
	}
	return a.fingerprint(definition)
}
