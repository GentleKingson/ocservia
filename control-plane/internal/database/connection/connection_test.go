package connection

import (
	"strings"
	"testing"
)

func TestExternalPostgreSQLTLSContract(t *testing.T) {
	base := Options{
		Environment: "production",
		URL:         "postgres://owner:secret@database.example.test:5432/ocservia",
		CAFile:      "/run/secrets/database_ca",
	}
	for _, mode := range []string{"", "disable", "allow", "prefer", "require", "verify-ca"} {
		t.Run("reject "+mode, func(t *testing.T) {
			options := base
			if mode != "" {
				options.URL += "?sslmode=" + mode
			}
			if err := ValidateOptions(options); err == nil {
				t.Fatalf("sslmode %q accepted", mode)
			}
		})
	}

	configured, err := postgresURL(Options{
		URL: base.URL + "?sslmode=verify-full", CAFile: base.CAFile,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(configured, "sslmode=verify-full") || !strings.Contains(configured, "sslrootcert=%2Frun%2Fsecrets%2Fdatabase_ca") {
		t.Fatalf("verified TLS configuration missing: %s", configured)
	}
}

func TestRejectNonPostgreSQLURLs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options Options
	}{
		{"mysql production", Options{Environment: "production", URL: "app:secret@tcp(database.example.test:3306)/ocservia?tls=true"}},
		{"mysql development", Options{Environment: "development", URL: "app:secret@tcp(127.0.0.1:3306)/ocservia?tls=false"}},
		{"mariadb", Options{Environment: "test", URL: "app:secret@tcp(127.0.0.1:3306)/db?tls=false"}},
		{"mysql URL", Options{Environment: "development", URL: "mysql://app:secret@127.0.0.1:3306/ocservia"}},
		{"mysql DSN", Options{Environment: "development", URL: "app:secret@tcp(127.0.0.1:3306)/ocservia?tls=false"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateOptions(tc.options)
			if err == nil {
				t.Fatalf("non-PostgreSQL URL accepted: %+v", tc.options)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("error leaks credentials: %v", err)
			}
		})
	}
}

func TestProductionPostgreSQLPlaintextRequiresDeclaredTarget(t *testing.T) {
	cases := []struct {
		name    string
		options Options
		ok      bool
	}{
		{"remote disable", Options{Environment: "production", URL: "postgres://app:secret@database.example.test:5432/ocservia?sslmode=disable"}, false},
		{"remote prefer", Options{Environment: "production", URL: "postgres://app:secret@database.example.test:5432/ocservia"}, false},
		{"remote require", Options{Environment: "production", URL: "postgres://app:secret@database.example.test:5432/ocservia?sslmode=require"}, false},
		{"undeclared bundled name", Options{Environment: "production", URL: "postgres://app:secret@postgres:5432/ocservia?sslmode=disable"}, false},
		{"declared bundled", Options{Environment: "production", BundledPostgres: true, URL: "postgres://app:secret@postgres:5432/ocservia?sslmode=disable"}, true},
		{"declared bundled remote host", Options{Environment: "production", BundledPostgres: true, URL: "postgres://app:secret@database.example.test:5432/ocservia?sslmode=disable"}, false},
		{"declared bundled host override", Options{Environment: "production", BundledPostgres: true, URL: "postgres://app:secret@postgres:5432/ocservia?sslmode=disable&host=database.example.test"}, false},
		{"declared bundled multi-host", Options{Environment: "production", BundledPostgres: true, URL: "postgres://app:secret@postgres:5432,database.example.test:5432/ocservia?sslmode=disable"}, false},
		{"declared bundled with CA", Options{Environment: "production", BundledPostgres: true, CAFile: "/run/secrets/database_ca", URL: "postgres://app:secret@postgres:5432/ocservia?sslmode=verify-full"}, false},
		{"declared bundled on MySQL", Options{Environment: "production", BundledPostgres: true, URL: "app:secret@tcp(127.0.0.1:3306)/ocservia?tls=false"}, false},
		{"loopback", Options{Environment: "production", URL: "postgres://app:secret@127.0.0.1:5432/ocservia?sslmode=disable"}, true},
		{"development remote", Options{Environment: "development", URL: "postgres://app:secret@database.example.test:5432/ocservia?sslmode=disable"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateOptions(tc.options)
			if (err == nil) != tc.ok {
				t.Fatalf("ValidateOptions() error = %v, want ok=%v", err, tc.ok)
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Fatalf("error leaks credentials: %v", err)
			}
		})
	}
}
