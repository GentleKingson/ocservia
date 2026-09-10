package config

import (
	"strings"
	"testing"
)

func TestDatabaseBackendContract(t *testing.T) {
	for _, test := range []struct {
		name, backend, dsn string
		set, valid         bool
	}{
		{"legacy", "", "postgres://db/test", false, true},
		{"explicit", "postgres", "postgresql://db/test", true, true},
		{"empty", "", "postgres://db/test", true, false},
		{"mysql", "mysql", "postgres://db/test", true, false},
		{"mariadb", "mariadb", "postgres://db/test", true, false},
		{"mysql development", "mysql", "app:secret@tcp(127.0.0.1:3306)/db?tls=false", true, true},
		{"mariadb development", "mariadb", "app:secret@tcp(127.0.0.1:3306)/db?tls=false", true, true},
		{"no DSN guessing", "", "app:secret@tcp(127.0.0.1:3306)/db?tls=false", false, false},
		{"no TLS fallback", "mysql", "app:secret@tcp(db:3306)/db?tls=preferred", true, false},
		{"no remote plaintext", "mariadb", "app:secret@tcp(db:3306)/db?tls=false", true, false},
		{"no driver overrides", "mysql", "app:secret@tcp(127.0.0.1:3306)/db?tls=false&multiStatements=true", true, false},
		{"unknown", "auto", "postgres://db/test", true, false},
		{"no guessing", "", "mysql://db/test", false, false},
		{"mismatched", "postgres", "mysql://db/test", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load(nil, func(key string) (string, bool) {
				switch key {
				case "OCSERV_DATABASE_URL":
					return test.dsn, true
				case "OCSERV_DATABASE_BACKEND":
					return test.backend, test.set
				default:
					return "", false
				}
			})
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v, error=%v", test.valid, err)
			}
		})
	}
}

func TestExperimentalControllerProductionGate(t *testing.T) {
	for _, backend := range []string{"mysql", "mariadb"} {
		_, err := Load(nil, func(key string) (string, bool) {
			values := map[string]string{
				"OCSERV_DATABASE_BACKEND": backend,
				"OCSERV_DATABASE_URL":     "app:secret@tcp(db:3306)/db?tls=true",
				"OCSERV_ENVIRONMENT":      "production",
			}
			v, ok := values[key]
			return v, ok
		})
		if err == nil || !strings.Contains(err.Error(), "pending PR-02 acceptance") {
			t.Fatalf("%s production gate: %v", backend, err)
		}
	}
}
