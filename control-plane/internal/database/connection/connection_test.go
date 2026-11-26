package connection

import (
	"strings"
	"testing"
)

func TestExternalPostgreSQLTLSContract(t *testing.T) {
	t.Parallel()
	base := Options{
		Environment: "production",
		URL:         "postgres://owner:secret@database.example.test:5432/ocservia",
		CAFile:      "/run/secrets/database_ca",
	}
	for _, mode := range []string{"", "disable", "allow", "prefer", "require", "verify-ca"} {
		options := base
		if mode != "" {
			options.URL += "?sslmode=" + mode
		}
		if err := ValidateOptions(options); err == nil {
			t.Fatalf("sslmode %q accepted", mode)
		}
	}
	configured, err := postgresURL(Options{URL: base.URL + "?sslmode=verify-full", CAFile: base.CAFile})
	if err != nil || !strings.Contains(configured, "sslmode=verify-full") || !strings.Contains(configured, "sslrootcert=%2Frun%2Fsecrets%2Fdatabase_ca") {
		t.Fatalf("verified TLS configuration missing: %q err=%v", configured, err)
	}
}

func TestRejectNonPostgreSQLAndUnsafePlaintext(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		options Options
		ok      bool
	}{
		{"mysql", Options{Environment: "production", URL: "app:secret@tcp(database.example.test:3306)/ocservia?tls=true"}, false},
		{"mariadb", Options{Environment: "test", URL: "app:secret@tcp(127.0.0.1:3306)/db?tls=false"}, false},
		{"remote disable", Options{Environment: "production", URL: "postgres://app:secret@database.example.test:5432/ocservia?sslmode=disable"}, false},
		{"undeclared bundled", Options{Environment: "production", URL: "postgres://app:secret@postgres:5432/ocservia?sslmode=disable"}, false},
		{"declared bundled", Options{Environment: "production", BundledPostgres: true, URL: "postgres://app:secret@postgres:5432/ocservia?sslmode=disable"}, true},
		{"declared bundled host override", Options{Environment: "production", BundledPostgres: true, URL: "postgres://app:secret@postgres:5432/ocservia?sslmode=disable&host=database.example.test"}, false},
		{"loopback", Options{Environment: "production", URL: "postgres://app:secret@127.0.0.1:5432/ocservia?sslmode=disable"}, true},
	} {
		err := ValidateOptions(tc.options)
		if (err == nil) != tc.ok {
			t.Fatalf("%s: err=%v want ok=%v", tc.name, err, tc.ok)
		}
		if err != nil && strings.Contains(err.Error(), "secret") {
			t.Fatalf("error leaks credentials: %v", err)
		}
	}
}
