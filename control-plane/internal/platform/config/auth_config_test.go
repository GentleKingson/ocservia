package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestProductionAuthenticationConfiguration(t *testing.T) {
	keyPath := filepath.Join(secureKeyTestDirectory(t), "audit-event-key")
	if err := os.WriteFile(keyPath, []byte(strings.Repeat("22", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, local := range []bool{false, true} {
		t.Run("local="+strconv.FormatBool(local), func(t *testing.T) {
			values := map[string]string{
				"OCSERV_DATABASE_URL":              "postgres://postgres/test",
				"OCSERV_DATABASE_BUNDLED_POSTGRES": "true",
				"OCSERV_ENVIRONMENT":               "production",
				"OCSERV_LOCAL_AUTH_ENABLED":        strconv.FormatBool(local),
				"OCSERV_PUBLIC_ORIGIN":             "https://app.example.test/",
				"OCSERV_SESSION_KEY":               strings.Repeat("11", 32),
				"OCSERV_AUDIT_CHECKPOINT_KEY":      strings.Repeat("33", 32),
				"OCSERV_AUDIT_EVENT_KEY_ID":        "audit-v1",
				"OCSERV_AUDIT_EVENT_KEY_FILE":      keyPath,
				"OCSERV_COMMAND_SIGNING_KEY_FILE":  "/run/secrets/command-key",
				"OCSERV_CONTROLLER_ENDPOINT_ID":    strings.Repeat("ab", 32),
				"OCSERV_TRANSPORT_UID":             strconv.Itoa(os.Geteuid() + 1),
				"OCSERV_TRANSPORT_GID":             strconv.Itoa(os.Getegid()),
			}
			lookup := func(key string) (string, bool) { v, ok := values[key]; return v, ok }
			cfg, err := Load(nil, lookup)
			if (err == nil) != local {
				t.Fatalf("Load() error=%v, want success=%t", err, local)
			}
			if !local {
				if !strings.Contains(err.Error(), "Local authentication is required") {
					t.Fatalf("expected production login availability guard, got %v", err)
				}
				return
			}
			if !cfg.LocalAuthEnabled() {
				t.Fatal("Local authentication should be enabled")
			}
			values["OCSERV_RUNTIME_DATABASE_ROLE"] = "ocservia_app"
			if _, err := Load([]string{"--migrate-only"}, lookup); err != nil {
				t.Fatalf("production migration configuration rejected: %v", err)
			}
			for key, invalidValues := range map[string][]string{
				"OCSERV_SESSION_KEY":   {"", "aa", strings.Repeat("AA", 32), strings.Repeat("z", 64)},
				"OCSERV_SESSION_TTL":   {"0s", "59s", "24h1s", "invalid"},
				"OCSERV_PUBLIC_ORIGIN": {"", "http://app.example.test", "https://user@app.example.test", "https://app.example.test/path", "https://app.example.test?x=1", "https://app.example.test?", "https://app.example.test#fragment", "https://app.example.test#", "https://app.example.test:invalid"},
			} {
				original, existed := values[key]
				for _, value := range invalidValues {
					values[key] = value
					if _, err := Load(nil, lookup); err == nil {
						t.Fatalf("accepted %s=%q", key, value)
					}
				}
				if existed {
					values[key] = original
				} else {
					delete(values, key)
				}
			}
			for _, ttl := range []string{"1m", "24h"} {
				values["OCSERV_SESSION_TTL"] = ttl
				if _, err := Load(nil, lookup); err != nil {
					t.Fatalf("TTL %s rejected: %v", ttl, err)
				}
			}
		})
	}
}

func TestLocalAuthStrictBoolean(t *testing.T) {
	for _, value := range []string{"", "yes", " true ", "enabled", "2"} {
		_, err := Load(nil, func(key string) (string, bool) {
			if key == "OCSERV_LOCAL_AUTH_ENABLED" {
				return value, true
			}
			if key == "OCSERV_DATABASE_URL" {
				return "postgres://db/test", true
			}
			return "", false
		})
		if err == nil || !strings.Contains(err.Error(), "OCSERV_LOCAL_AUTH_ENABLED") {
			t.Fatalf("invalid boolean %q: %v", value, err)
		}
	}
}

func TestLocalDevelopmentRequiresSession(t *testing.T) {
	values := map[string]string{"OCSERV_DATABASE_URL": "postgres://db/test", "OCSERV_LOCAL_AUTH_ENABLED": "true"}
	lookup := func(key string) (string, bool) { v, ok := values[key]; return v, ok }
	if _, err := Load(nil, lookup); err == nil {
		t.Fatal("accepted Local without a session key")
	}
	values["OCSERV_SESSION_KEY"] = strings.Repeat("11", 32)
	if _, err := Load(nil, lookup); err != nil {
		t.Fatal(err)
	}
}
