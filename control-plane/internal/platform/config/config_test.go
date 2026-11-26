package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"golang.org/x/sys/unix"
)

func loadEnv(args []string, values map[string]string) (Config, error) {
	return Load(args, func(key string) (string, bool) {
		v, ok := values[key]
		return v, ok
	})
}

func baseEnv(overrides map[string]string) map[string]string {
	env := map[string]string{"OCSERV_DATABASE_URL": "postgres://db/test"}
	for k, v := range overrides {
		if v == "\x00DELETE" {
			delete(env, k)
		} else {
			env[k] = v
		}
	}
	return env
}

func prodBaseEnv(t *testing.T) map[string]string {
	t.Helper()
	dir := testutil.PrivateTempDir(t)
	keyPath := filepath.Join(dir, "audit-event-key")
	caPath := filepath.Join(dir, "database-ca.pem")
	testutil.NoError(t, os.WriteFile(keyPath, []byte(strings.Repeat("22", 32)), 0o600))
	testutil.NoError(t, os.WriteFile(caPath, []byte("test"), 0o600))
	return map[string]string{
		"OCSERV_ENVIRONMENT":              "production",
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
}

func TestConfigValidationGuards(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		env  map[string]string
		ok   bool
	}{
		{"trusted proxy valid", nil, baseEnv(map[string]string{"OCSERV_AUTH_TRUSTED_PROXY_CIDRS": "10.0.0.2/32, 2001:db8::/64"}), true},
		{"trusted proxy wildcard v4", nil, baseEnv(map[string]string{"OCSERV_AUTH_TRUSTED_PROXY_CIDRS": "0.0.0.0/0"}), false},
		{"dev auth non-loopback", nil, baseEnv(map[string]string{"OCSERV_DEV_AUTH": "true", "OCSERV_HTTP_ADDRESS": "0.0.0.0:8080"}), false},
		{"dev auth production", nil, baseEnv(map[string]string{"OCSERV_DEV_AUTH": "true", "OCSERV_ENVIRONMENT": "production"}), false},
		{"implicit env wildcard rejected", nil, baseEnv(map[string]string{"OCSERV_HTTP_ADDRESS": "0.0.0.0:8080"}), false},
		{"explicit dev wildcard ok", nil, baseEnv(map[string]string{"OCSERV_ENVIRONMENT": "development", "OCSERV_HTTP_ADDRESS": "0.0.0.0:8080"}), true},
		{"dev auth token short rejected", nil, baseEnv(map[string]string{"OCSERV_DEV_AUTH_TOKEN": "short"}), false},
		{"audit event and checkpoint distinct", nil, baseEnv(map[string]string{"OCSERV_ENVIRONMENT": "test", "OCSERV_AUDIT_EVENT_KEY_ID": "k", "OCSERV_TEST_AUDIT_EVENT_KEY_HEX": strings.Repeat("11", 32), "OCSERV_AUDIT_CHECKPOINT_KEY": strings.Repeat("11", 32)}), false},
		{"legacy role rejected", []string{"--role=all"}, baseEnv(nil), false},
		{"mismatched dsn rejected", nil, baseEnv(map[string]string{"OCSERV_DATABASE_URL": "mysql://db/test"}), false},
		{"local auth requires session key", nil, baseEnv(map[string]string{"OCSERV_LOCAL_AUTH_ENABLED": "true"}), false},
		{"local auth with session key ok", nil, baseEnv(map[string]string{"OCSERV_LOCAL_AUTH_ENABLED": "true", "OCSERV_SESSION_KEY": strings.Repeat("11", 32)}), true},
		{"signer https ok", nil, baseEnv(map[string]string{"OCSERV_CERTIFICATE_SIGNER_URL": "https://pki.example.test/v1/sign", "OCSERV_CERTIFICATE_SIGNER_TOKEN": "fixture-token"}), true},
		{"signer http rejected", nil, baseEnv(map[string]string{"OCSERV_CERTIFICATE_SIGNER_URL": "http://pki.example.test/v1/sign", "OCSERV_CERTIFICATE_SIGNER_TOKEN": "fixture-token"}), false},
		{"recommended agent semver ok", nil, baseEnv(map[string]string{"OCSERV_RECOMMENDED_AGENT_VERSION": "0.2.0-rc.1"}), true},
		{"recommended agent non-semver rejected", nil, baseEnv(map[string]string{"OCSERV_RECOMMENDED_AGENT_VERSION": "latest"}), false},
		{"sse session above identity rejected", nil, baseEnv(map[string]string{"OCSERV_SSE_IDENTITY_LIMIT": "6", "OCSERV_SSE_SESSION_LIMIT": "7"}), false},
	}
	for _, tc := range cases {
		if _, err := loadEnv(tc.args, tc.env); (err == nil) != tc.ok {
			t.Fatalf("%s: want ok=%v", tc.name, tc.ok)
		}
	}
}

func TestProductionConfigurationAndFailClosedGates(t *testing.T) {
	t.Parallel()
	base := prodBaseEnv(t)
	if cfg, err := loadEnv(nil, base); err != nil || !cfg.LocalAuth || cfg.BrowserOrigin() != "https://controller.example.test" {
		t.Fatalf("valid production config rejected: err=%v origin=%q", err, cfg.BrowserOrigin())
	}

	retentionEnv := map[string]string{
		"OCSERV_ENVIRONMENT":               "production",
		"OCSERV_DATABASE_BUNDLED_POSTGRES": "true",
		"OCSERV_DATABASE_URL":              "postgres://owner:secret@postgres:5432/ocservia?sslmode=disable",
	}
	if cfg, err := loadEnv([]string{"--telemetry-retention-only", "--telemetry-retention-interval=24h"}, retentionEnv); err != nil || !cfg.TelemetryRetentionOnly || cfg.TelemetryRetentionInterval != 24*time.Hour {
		t.Fatalf("telemetry retention rejected: err=%v", err)
	}
	if _, err := loadEnv(nil, map[string]string{"OCSERV_ENVIRONMENT": "production", "OCSERV_TELEMETRY_RETENTION_INTERVAL": "12h"}); err == nil {
		t.Fatal("interval accepted without --telemetry-retention-only")
	}

	for _, m := range []struct{ name, key, val string }{
		{"local auth disabled", "OCSERV_LOCAL_AUTH_ENABLED", "false"},
		{"test audit event key", "OCSERV_TEST_AUDIT_EVENT_KEY_HEX", strings.Repeat("11", 32)},
		{"relative command key", "OCSERV_COMMAND_SIGNING_KEY_FILE", "relative/controller.pem"},
		{"remote plaintext postgres", "OCSERV_DATABASE_URL", "postgres://app:secret@database.example.test:5432/ocservia?sslmode=disable"},
		{"http public origin", "OCSERV_PUBLIC_ORIGIN", "http://app.example.test"},
		{"local simulator enabled", "OCSERV_LOCAL_SIMULATOR", "true"},
	} {
		env := make(map[string]string, len(base))
		for k, v := range base {
			env[k] = v
		}
		env[m.key] = m.val
		if _, err := loadEnv(nil, env); err == nil {
			t.Fatalf("%s: accepted invalid %s=%q", m.name, m.key, m.val)
		}
	}
}

func TestSensitiveFilesAndLocalBootstrapProtection(t *testing.T) {
	t.Parallel()
	dir := testutil.PrivateTempDir(t)
	evFile := filepath.Join(dir, "audit-event-key")
	pwFile := filepath.Join(dir, "bootstrap-password")
	const password = "bootstrap-test-secret"
	testutil.NoError(t, os.WriteFile(evFile, []byte(strings.Repeat("b", 64)+"\n"), 0o600))
	testutil.NoError(t, os.WriteFile(pwFile, []byte(password+"\r\n"), 0o600))

	testutil.NoError(t, os.Chmod(evFile, 0o644))
	if _, err := readStrictHexKeyFile(evFile, uint32(os.Geteuid())); err == nil {
		t.Fatal("world-readable audit event key accepted")
	}
	testutil.NoError(t, os.Chmod(evFile, 0o600))
	linkPath := filepath.Join(dir, "audit-event-link")
	testutil.NoError(t, os.Symlink(evFile, linkPath))
	if _, err := readStrictHexKeyFile(linkPath, uint32(os.Geteuid())); err == nil {
		t.Fatal("symlinked audit event key accepted")
	}

	bootEnv := baseEnv(map[string]string{
		"OCSERV_LOCAL_AUTH_ENABLED":            "true",
		"OCSERV_SESSION_KEY":                   strings.Repeat("a", 64),
		"OCSERV_LOCAL_BOOTSTRAP_USERNAME":      "first-admin",
		"OCSERV_LOCAL_BOOTSTRAP_WORKSPACE_ID":  "01900000-0000-7000-8000-000000000001",
		"OCSERV_LOCAL_BOOTSTRAP_PASSWORD_FILE": pwFile,
	})
	if cfg, err := loadEnv([]string{"--bootstrap-local-admin"}, bootEnv); err != nil || !cfg.BootstrapLocalAdmin || cfg.LocalBootstrapPassword != password {
		t.Fatalf("bootstrap secret not loaded: %v", err)
	}
	bootEnv["OCSERV_LOCAL_BOOTSTRAP_PASSWORD"] = password
	if _, err := loadEnv([]string{"--bootstrap-local-admin"}, bootEnv); err == nil || strings.Contains(err.Error(), password) {
		t.Fatal("plaintext bootstrap env accepted or leaked password")
	}
	fifo := filepath.Join(dir, "fifo")
	testutil.NoError(t, unix.Mkfifo(fifo, 0o600))
	if _, err := readBootstrapSecret(fifo); err == nil {
		t.Fatal("FIFO bootstrap secret accepted")
	}
}
