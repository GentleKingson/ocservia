package connection

import (
	"strings"
	"testing"
)

func TestExternalPostgreSQLTLSContract(t *testing.T) {
	base := Options{
		Backend:     "postgres",
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
		Backend: "postgres", URL: base.URL + "?sslmode=verify-full", CAFile: base.CAFile,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(configured, "sslmode=verify-full") || !strings.Contains(configured, "sslrootcert=%2Frun%2Fsecrets%2Fdatabase_ca") {
		t.Fatalf("verified TLS configuration missing: %s", configured)
	}
}

func TestExternalPostgreSQLMajorVersion(t *testing.T) {
	for _, version := range []int{160012, 180006} {
		if err := validateExternalPostgreSQLVersion(version); err == nil {
			t.Fatalf("unsupported server version %d accepted", version)
		}
	}
	if err := validateExternalPostgreSQLVersion(170010); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLCompatibleProductionTLSContract(t *testing.T) {
	for _, backend := range []string{"mysql", "mariadb"} {
		t.Run(backend, func(t *testing.T) {
			base := Options{
				Backend: backend, Environment: "production",
				URL: "app:secret@tcp(database.example.test:3306)/ocservia?tls=true",
			}
			if err := ValidateOptions(base); err != nil {
				t.Fatalf("verified production connection rejected: %v", err)
			}
			base.URL = "app:secret@tcp(database.example.test:3306)/ocservia?tls=false"
			if err := ValidateOptions(base); err == nil {
				t.Fatal("remote production plaintext accepted")
			}
		})
	}
}
