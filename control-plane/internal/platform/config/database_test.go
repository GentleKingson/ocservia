package config

import (
	"os"
	"path/filepath"
	"strconv"
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
		{"mysql development", "mysql", "app:secret@tcp(127.0.0.1:3306)/db?tls=false", true, false},
		{"mariadb development", "mariadb", "app:secret@tcp(127.0.0.1:3306)/db?tls=false", true, false},
		{"no DSN guessing", "", "app:secret@tcp(127.0.0.1:3306)/db?tls=false", false, false},
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

func TestPostgreSQLControllerProductionConfiguration(t *testing.T) {
	dir := secureKeyTestDirectory(t)
	keyPath := filepath.Join(dir, "audit-event-key")
	if err := os.WriteFile(keyPath, []byte(strings.Repeat("22", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(dir, "database-ca.pem")
	if err := os.WriteFile(caPath, []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		"OCSERV_ENVIRONMENT":              "production",
		"OCSERV_DATABASE_BACKEND":         "postgres",
		"OCSERV_DATABASE_URL":             "postgres://app:secret@database.example.test:5432/ocservia?sslmode=verify-full",
		"OCSERV_DATABASE_TLS_CA_FILE":     caPath,
		"OCSERV_LOCAL_AUTH_ENABLED":       "true",
		"OCSERV_PUBLIC_ORIGIN":            "https://controller.example.test",
		"OCSERV_SESSION_KEY":              strings.Repeat("11", 32),
		"OCSERV_AUDIT_CHECKPOINT_KEY":     strings.Repeat("33", 32),
		"OCSERV_AUDIT_EVENT_KEY_ID":       "audit-v1",
		"OCSERV_AUDIT_EVENT_KEY_FILE":     keyPath,
		"OCSERV_COMMAND_SIGNING_KEY_FILE": "/run/secrets/controller-command-signing-key.pem",
		"OCSERV_CONTROLLER_ENDPOINT_ID":   strings.Repeat("ab", 32),
		"OCSERV_TRANSPORT_UID":            strconv.Itoa(os.Geteuid() + 1),
		"OCSERV_TRANSPORT_GID":            strconv.Itoa(os.Getegid()),
	}
	lookup := func(key string) (string, bool) { value, ok := values[key]; return value, ok }
	if _, err := Load(nil, lookup); err != nil {
		t.Fatalf("valid production configuration rejected: %v", err)
	}

	values["OCSERV_DATABASE_BACKEND"] = "mysql"
	if _, err := Load(nil, lookup); err == nil {
		t.Fatal("retired MySQL backend accepted in production")
	}
	values["OCSERV_DATABASE_BACKEND"] = "postgres"

	for _, test := range []struct{ name, dsn string }{
		{"missing TLS mode", "postgres://app:secret@database.example.test:5432/ocservia"},
		{"remote plaintext", "postgres://app:secret@database.example.test:5432/ocservia?sslmode=disable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			values["OCSERV_DATABASE_URL"] = test.dsn
			if _, err := Load(nil, lookup); err == nil {
				t.Fatal("unsafe production database configuration accepted")
			}
		})
	}
	values["OCSERV_DATABASE_URL"] = "postgres://app:secret@database.example.test:5432/ocservia?sslmode=verify-full"
	delete(values, "OCSERV_LOCAL_AUTH_ENABLED")
	if _, err := Load(nil, lookup); err == nil || !strings.Contains(err.Error(), "Local authentication") {
		t.Fatalf("incomplete production authentication accepted: %v", err)
	}
}
