package config

import (
	"fmt"
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
	oidc := map[string]string{
		"OCSERV_OIDC_ISSUER":        "https://id.example.test",
		"OCSERV_OIDC_CLIENT_ID":     "client",
		"OCSERV_OIDC_CLIENT_SECRET": "secret",
		"OCSERV_OIDC_REDIRECT_URL":  "https://APP.example.test:443/api/v1/auth/callback",
	}
	for _, local := range []bool{false, true} {
		for mask := 0; mask < 16; mask++ {
			t.Run(fmt.Sprintf("local=%t/oidc=%04b", local, mask), func(t *testing.T) {
				values := map[string]string{
					"OCSERV_DATABASE_URL":             "postgres://db/test",
					"OCSERV_ENVIRONMENT":              "production",
					"OCSERV_LOCAL_AUTH_ENABLED":       strconv.FormatBool(local),
					"OCSERV_PUBLIC_ORIGIN":            "https://app.example.test/",
					"OCSERV_SESSION_KEY":              strings.Repeat("11", 32),
					"OCSERV_AUDIT_CHECKPOINT_KEY":     strings.Repeat("33", 32),
					"OCSERV_AUDIT_EVENT_KEY_ID":       "audit-v1",
					"OCSERV_AUDIT_EVENT_KEY_FILE":     keyPath,
					"OCSERV_COMMAND_SIGNING_KEY_FILE": "/run/secrets/command-key",
					"OCSERV_TRANSPORT_UID":            strconv.Itoa(os.Geteuid() + 1),
					"OCSERV_TRANSPORT_GID":            strconv.Itoa(os.Getegid()),
				}
				for i, key := range []string{"OCSERV_OIDC_ISSUER", "OCSERV_OIDC_CLIENT_ID", "OCSERV_OIDC_CLIENT_SECRET", "OCSERV_OIDC_REDIRECT_URL"} {
					if mask&(1<<i) != 0 {
						values[key] = oidc[key]
					}
				}
				lookup := func(key string) (string, bool) { v, ok := values[key]; return v, ok }
				cfg, err := Load(nil, lookup)
				valid := mask == 15 || (local && mask == 0)
				if (err == nil) != valid {
					t.Fatalf("Load() error=%v, want success=%t", err, valid)
				}
				if !valid {
					return
				}
				if cfg.LocalAuthEnabled() != local || cfg.OIDCEnabled() != (mask == 15) {
					t.Fatal("authentication enablement does not match configuration")
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
				if mask == 15 {
					for key, invalidValues := range map[string][]string{
						"OCSERV_OIDC_ISSUER":       {"http://id.example.test", "https://user@id.example.test", "https://id.example.test?x=1", "https://id.example.test#x"},
						"OCSERV_OIDC_REDIRECT_URL": {"http://app.example.test/callback", "https://other.example.test/callback", "https://app.example.test:8443/callback", "https://user@app.example.test/callback", "https://app.example.test/callback?x=1", "https://app.example.test/callback#x"},
					} {
						original := values[key]
						for _, value := range invalidValues {
							values[key] = value
							if _, err := Load(nil, lookup); err == nil {
								t.Fatalf("accepted %s=%q", key, value)
							}
						}
						values[key] = original
					}
				}
			})
		}
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

func TestOIDCDevelopmentConfiguration(t *testing.T) {
	values := map[string]string{
		"OCSERV_DATABASE_URL":       "postgres://db/test",
		"OCSERV_OIDC_ISSUER":        "https://id.example.test",
		"OCSERV_OIDC_CLIENT_ID":     "client",
		"OCSERV_OIDC_CLIENT_SECRET": "secret",
		"OCSERV_OIDC_REDIRECT_URL":  "https://app.example.test/api/v1/auth/callback",
		"OCSERV_SESSION_KEY":        strings.Repeat("11", 32),
	}
	lookup := func(key string) (string, bool) { v, ok := values[key]; return v, ok }
	cfg, err := Load(nil, lookup)
	if err != nil || !cfg.OIDCEnabled() || cfg.BrowserOrigin() != "" {
		t.Fatalf("development OIDC must not implicitly set browser origin: %v", err)
	}
	values["OCSERV_PUBLIC_ORIGIN"] = "http://localhost:8080/"
	cfg, err = Load(nil, lookup)
	if err != nil || cfg.BrowserOrigin() != "http://localhost:8080" {
		t.Fatalf("explicit development origin rejected: %v", err)
	}
	for _, key := range []string{"OCSERV_OIDC_ISSUER", "OCSERV_OIDC_CLIENT_ID", "OCSERV_OIDC_CLIENT_SECRET", "OCSERV_OIDC_REDIRECT_URL", "OCSERV_SESSION_KEY"} {
		original := values[key]
		delete(values, key)
		if _, err := Load(nil, lookup); err == nil {
			t.Fatalf("accepted development OIDC without %s", key)
		}
		values[key] = original
	}
}
