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

func TestMigrateOnlyRequiresRuntimeRole(t *testing.T) {
	lookup := func(key string) (string, bool) {
		values := map[string]string{"OCSERV_DATABASE_URL": "postgres://owner@db/test"}
		value, ok := values[key]
		return value, ok
	}
	if _, err := Load([]string{"--migrate-only"}, lookup); err == nil {
		t.Fatal("Load() accepted migration mode without a runtime database role")
	}
}

func TestMigrateOnlyAcceptsRuntimeRole(t *testing.T) {
	lookup := func(key string) (string, bool) {
		values := map[string]string{
			"OCSERV_DATABASE_URL":          "postgres://owner@db/test",
			"OCSERV_RUNTIME_DATABASE_ROLE": "ocservia_app",
		}
		value, ok := values[key]
		return value, ok
	}
	config, err := Load([]string{"--migrate-only"}, lookup)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !config.MigrateOnly || config.RuntimeDBRole != "ocservia_app" {
		t.Fatalf("unexpected migration config: %+v", config)
	}
}

func TestLocalSimulatorIsRejectedInProduction(t *testing.T) {
	lookup := func(key string) (string, bool) {
		values := map[string]string{
			"OCSERV_DATABASE_URL":    "postgres://db/test",
			"OCSERV_ENVIRONMENT":     "production",
			"OCSERV_LOCAL_SIMULATOR": "true",
		}
		value, ok := values[key]
		return value, ok
	}
	if _, err := Load(nil, lookup); err == nil {
		t.Fatal("Load() accepted the local simulator in production")
	}
}
