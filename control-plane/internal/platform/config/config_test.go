package config

import "testing"

func TestDevAuthGuard(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		ok   bool
	}{
		{"development loopback", map[string]string{"OCSERV_DATABASE_URL": "postgres://db/test", "OCSERV_DEV_AUTH": "true"}, true},
		{"production", map[string]string{"OCSERV_DATABASE_URL": "postgres://db/test", "OCSERV_DEV_AUTH": "true", "OCSERV_ENVIRONMENT": "production"}, false},
		{"non-loopback", map[string]string{"OCSERV_DATABASE_URL": "postgres://db/test", "OCSERV_DEV_AUTH": "true", "OCSERV_HTTP_ADDRESS": "0.0.0.0:8080"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(nil, func(key string) (string, bool) { v, ok := tt.env[key]; return v, ok })
			if (err == nil) != tt.ok {
				t.Fatalf("Load() error = %v, want success %v", err, tt.ok)
			}
		})
	}
}

func TestRoleValidation(t *testing.T) {
	_, err := Load([]string{"--role=invalid"}, func(key string) (string, bool) {
		if key == "OCSERV_DATABASE_URL" {
			return "postgres://db/test", true
		}
		return "", false
	})
	if err == nil {
		t.Fatal("Load() accepted invalid role")
	}
}
