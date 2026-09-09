package config

import "testing"

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
