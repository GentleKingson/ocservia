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

func secureKeyTestDirectory(t *testing.T) string {
	t.Helper()
	return testutil.PrivateTempDir(t)
}

func prodBaseEnv(t *testing.T) map[string]string {
	t.Helper()
	dir := secureKeyTestDirectory(t)
	keyPath := filepath.Join(dir, "audit-event-key")
	caPath := filepath.Join(dir, "database-ca.pem")
	testutil.NoError(t, os.WriteFile(keyPath, []byte(strings.Repeat("22", 32)), 0o600))
	testutil.NoError(t, os.WriteFile(caPath, []byte("test"), 0o600))
	return map[string]string{
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
}

func TestConfigValidationGuards(t *testing.T) {
	cases := []struct {
		name string
		args []string
		env  map[string]string
		ok   bool
	}{
		{"trusted proxy empty", nil, baseEnv(map[string]string{"OCSERV_AUTH_TRUSTED_PROXY_CIDRS": ""}), true},
		{"trusted proxy valid", nil, baseEnv(map[string]string{"OCSERV_AUTH_TRUSTED_PROXY_CIDRS": "10.0.0.2/32, 2001:db8::/64"}), true},
		{"trusted proxy wildcard v4", nil, baseEnv(map[string]string{"OCSERV_AUTH_TRUSTED_PROXY_CIDRS": "0.0.0.0/0"}), false},
		{"trusted proxy wildcard v6", nil, baseEnv(map[string]string{"OCSERV_AUTH_TRUSTED_PROXY_CIDRS": "::/0"}), false},
		{"dev auth loopback", nil, baseEnv(map[string]string{"OCSERV_DEV_AUTH": "true"}), true},
		{"dev auth non-loopback", nil, baseEnv(map[string]string{"OCSERV_DEV_AUTH": "true", "OCSERV_HTTP_ADDRESS": "0.0.0.0:8080"}), false},
		{"dev auth production", nil, baseEnv(map[string]string{"OCSERV_DEV_AUTH": "true", "OCSERV_ENVIRONMENT": "production"}), false},
		{"implicit env loopback", nil, baseEnv(map[string]string{"OCSERV_HTTP_ADDRESS": "[::1]:8080"}), true},
		{"implicit env wildcard rejected", nil, baseEnv(map[string]string{"OCSERV_HTTP_ADDRESS": "0.0.0.0:8080"}), false},
		{"explicit dev wildcard ok", nil, baseEnv(map[string]string{"OCSERV_ENVIRONMENT": "development", "OCSERV_HTTP_ADDRESS": "0.0.0.0:8080"}), true},
		{"dev auth token short rejected", nil, baseEnv(map[string]string{"OCSERV_DEV_AUTH_TOKEN": "short"}), false},
		{"dev auth token production rejected", nil, baseEnv(map[string]string{"OCSERV_ENVIRONMENT": "production", "OCSERV_DEV_AUTH_TOKEN": "local-development-token-32-characters"}), false},
		{"audit event and checkpoint distinct", nil, baseEnv(map[string]string{"OCSERV_ENVIRONMENT": "test", "OCSERV_AUDIT_EVENT_KEY_ID": "k", "OCSERV_TEST_AUDIT_EVENT_KEY_HEX": strings.Repeat("11", 32), "OCSERV_AUDIT_CHECKPOINT_KEY": strings.Repeat("11", 32)}), false},
		{"legacy role rejected", []string{"--role=api"}, baseEnv(nil), false},
		{"legacy role all ok", []string{"--role=all"}, baseEnv(map[string]string{"OCSERV_DATABASE_BACKEND": "postgres"}), true},
		{"non-postgres backend rejected", nil, baseEnv(map[string]string{"OCSERV_DATABASE_BACKEND": "mysql"}), false},
		{"mismatched dsn rejected", nil, baseEnv(map[string]string{"OCSERV_DATABASE_BACKEND": "postgres", "OCSERV_DATABASE_URL": "mysql://db/test"}), false},
		{"local auth strict bool rejected", nil, baseEnv(map[string]string{"OCSERV_LOCAL_AUTH_ENABLED": "yes"}), false},
		{"local auth requires session key", nil, baseEnv(map[string]string{"OCSERV_LOCAL_AUTH_ENABLED": "true"}), false},
		{"local auth with session key ok", nil, baseEnv(map[string]string{"OCSERV_LOCAL_AUTH_ENABLED": "true", "OCSERV_SESSION_KEY": strings.Repeat("11", 32)}), true},
		{"transport uid without gid rejected", nil, baseEnv(map[string]string{"OCSERV_TRANSPORT_UID": "12345"}), false},
		{"invalid controller endpoint rejected", nil, baseEnv(map[string]string{"OCSERV_CONTROLLER_ENDPOINT_ID": strings.Repeat("A", 64)}), false},
		{"signer https ok", nil, baseEnv(map[string]string{"OCSERV_CERTIFICATE_SIGNER_URL": "https://pki.example.test/v1/sign", "OCSERV_CERTIFICATE_SIGNER_TOKEN": "fixture-token"}), true},
		{"signer http rejected", nil, baseEnv(map[string]string{"OCSERV_CERTIFICATE_SIGNER_URL": "http://pki.example.test/v1/sign", "OCSERV_CERTIFICATE_SIGNER_TOKEN": "fixture-token"}), false},
		{"signer orphan ca rejected", nil, baseEnv(map[string]string{"OCSERV_CERTIFICATE_SIGNER_CA_FILE": "/run/secrets/signer-ca.pem"}), false},
		{"recommended agent semver ok", nil, baseEnv(map[string]string{"OCSERV_RECOMMENDED_AGENT_VERSION": "0.2.0-rc.1"}), true},
		{"recommended agent non-semver rejected", nil, baseEnv(map[string]string{"OCSERV_RECOMMENDED_AGENT_VERSION": "latest"}), false},
		{"sse session above identity rejected", nil, baseEnv(map[string]string{"OCSERV_SSE_IDENTITY_LIMIT": "6", "OCSERV_SSE_SESSION_LIMIT": "7"}), false},
		{"user op concurrency overflow rejected", nil, baseEnv(map[string]string{"OCSERV_USER_OPERATION_CONCURRENCY": "501"}), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := loadEnv(tc.args, tc.env); (err == nil) != tc.ok {
				t.Fatalf("Load() err = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func TestProductionConfigurationAndFailClosedGates(t *testing.T) {
	base := prodBaseEnv(t)
	if cfg, err := loadEnv(nil, base); err != nil || !cfg.LocalAuthEnabled() || cfg.BrowserOrigin() != "https://controller.example.test" {
		t.Fatalf("valid production config rejected: err=%v origin=%q", err, cfg.BrowserOrigin())
	}

	migrateEnv := make(map[string]string, len(base)+1)
	for k, v := range base {
		migrateEnv[k] = v
	}
	migrateEnv["OCSERV_RUNTIME_DATABASE_ROLE"] = "ocservia_app"
	if _, err := loadEnv([]string{"--migrate-only"}, migrateEnv); err != nil {
		t.Fatalf("production --migrate-only rejected: %v", err)
	}

	retentionEnv := make(map[string]string, len(base))
	for k, v := range base {
		retentionEnv[k] = v
	}
	delete(retentionEnv, "OCSERV_COMMAND_SIGNING_KEY_FILE")
	delete(retentionEnv, "OCSERV_CONTROLLER_ENDPOINT_ID")
	if cfg, err := loadEnv([]string{"--telemetry-retention-only"}, retentionEnv); err != nil || !cfg.TelemetryRetentionOnly {
		t.Fatalf("production telemetry retention command rejected: %v", err)
	}
	if _, err := loadEnv([]string{"--telemetry-retention-only", "--migrate-only"}, migrateEnv); err == nil {
		t.Fatal("telemetry retention and migration flags were accepted together")
	}

	minimalRetentionEnv := map[string]string{
		"OCSERV_ENVIRONMENT":               "production",
		"OCSERV_DATABASE_BUNDLED_POSTGRES": "true",
		"OCSERV_DATABASE_URL":              "postgres://owner:secret@postgres:5432/ocservia?sslmode=disable",
	}
	if cfg, err := loadEnv([]string{"--telemetry-retention-only", "--telemetry-retention-interval=24h"}, minimalRetentionEnv); err != nil || !cfg.TelemetryRetentionOnly || cfg.TelemetryRetentionInterval != 24*time.Hour {
		t.Fatalf("minimal production scheduled telemetry retention rejected: err=%v cfg=%+v", err, cfg)
	}
	minimalRetentionEnv["OCSERV_TELEMETRY_RETENTION_INTERVAL"] = "12h"
	if cfg, err := loadEnv([]string{"--telemetry-retention-only"}, minimalRetentionEnv); err != nil || cfg.TelemetryRetentionInterval != 12*time.Hour {
		t.Fatalf("env telemetry retention interval rejected: err=%v interval=%v", err, cfg.TelemetryRetentionInterval)
	}
	if cfg, err := loadEnv([]string{"--telemetry-retention-only", "--telemetry-retention-interval=48h"}, minimalRetentionEnv); err != nil || cfg.TelemetryRetentionInterval != 48*time.Hour {
		t.Fatalf("flag did not override telemetry retention interval: err=%v interval=%v", err, cfg.TelemetryRetentionInterval)
	}
	if _, err := loadEnv(nil, minimalRetentionEnv); err == nil {
		t.Fatal("OCSERV_TELEMETRY_RETENTION_INTERVAL accepted without --telemetry-retention-only")
	}
	for _, invalidInterval := range []string{"30m", "169h", "-1h", "not-a-duration"} {
		badEnv := map[string]string{
			"OCSERV_ENVIRONMENT":                  "production",
			"OCSERV_DATABASE_BUNDLED_POSTGRES":    "true",
			"OCSERV_DATABASE_URL":                 "postgres://owner:secret@postgres:5432/ocservia?sslmode=disable",
			"OCSERV_TELEMETRY_RETENTION_INTERVAL": invalidInterval,
		}
		if _, err := loadEnv([]string{"--telemetry-retention-only"}, badEnv); err == nil {
			t.Fatalf("invalid telemetry retention interval %q accepted", invalidInterval)
		}
	}
	if _, err := loadEnv([]string{"--telemetry-retention-only"}, map[string]string{
		"OCSERV_ENVIRONMENT":               "production",
		"OCSERV_DATABASE_BUNDLED_POSTGRES": "true",
		"OCSERV_DATABASE_URL":              "postgres://owner:secret@postgres:5432/ocservia?sslmode=disable",
		"OCSERV_PUBLIC_ORIGIN":             "http://insecure.example.test",
	}); err == nil {
		t.Fatal("telemetry retention ignored invalid OCSERV_PUBLIC_ORIGIN")
	}

	mutations := []struct {
		name string
		key  string
		val  string
	}{
		{"local auth disabled", "OCSERV_LOCAL_AUTH_ENABLED", "false"},
		{"test audit event key", "OCSERV_TEST_AUDIT_EVENT_KEY_HEX", strings.Repeat("11", 32)},
		{"relative command key", "OCSERV_COMMAND_SIGNING_KEY_FILE", "relative/controller.pem"},
		{"missing endpoint id", "OCSERV_CONTROLLER_ENDPOINT_ID", ""},
		{"remote plaintext postgres", "OCSERV_DATABASE_URL", "postgres://app:secret@database.example.test:5432/ocservia?sslmode=disable"},
		{"missing postgres sslmode", "OCSERV_DATABASE_URL", "postgres://app:secret@database.example.test:5432/ocservia"},
		{"invalid session key", "OCSERV_SESSION_KEY", strings.Repeat("AA", 32)},
		{"session ttl too short", "OCSERV_SESSION_TTL", "59s"},
		{"http public origin", "OCSERV_PUBLIC_ORIGIN", "http://app.example.test"},
		{"path in public origin", "OCSERV_PUBLIC_ORIGIN", "https://app.example.test/path"},
		{"local simulator enabled", "OCSERV_LOCAL_SIMULATOR", "true"},
	}
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			env := make(map[string]string, len(base))
			for k, v := range base {
				env[k] = v
			}
			if m.val == "" {
				delete(env, m.key)
			} else {
				env[m.key] = m.val
			}
			if _, err := loadEnv(nil, env); err == nil {
				t.Fatalf("production accepted invalid %s=%q", m.key, m.val)
			}
		})
	}
}

func TestSensitiveFilesAndLocalBootstrapProtection(t *testing.T) {
	dir := secureKeyTestDirectory(t)
	dbFile := filepath.Join(dir, "database-url")
	ckFile := filepath.Join(dir, "checkpoint-key")
	evFile := filepath.Join(dir, "audit-event-key")
	pwFile := filepath.Join(dir, "bootstrap-password")
	testutil.NoError(t, os.WriteFile(dbFile, []byte("postgres://app:secret@postgres/ocservia\n"), 0o600))
	testutil.NoError(t, os.WriteFile(ckFile, []byte(strings.Repeat("a", 64)+"\n"), 0o600))
	testutil.NoError(t, os.WriteFile(evFile, []byte(strings.Repeat("b", 64)+"\n"), 0o600))
	const password = "bootstrap-test-secret"
	testutil.NoError(t, os.WriteFile(pwFile, []byte(password+"\r\n"), 0o600))

	cfg, err := loadEnv(nil, map[string]string{
		"OCSERV_DATABASE_URL_FILE":         dbFile,
		"OCSERV_AUDIT_CHECKPOINT_KEY_FILE": ckFile,
		"OCSERV_AUDIT_EVENT_KEY_ID":        "audit-event-v1",
		"OCSERV_AUDIT_EVENT_KEY_FILE":      evFile,
	})
	testutil.NoError(t, err)
	if cfg.DatabaseURL != "postgres://app:secret@postgres/ocservia" || len(cfg.AuditCheckpointKey) != 32 || len(cfg.AuditEventKey) != 32 {
		t.Fatalf("unexpected file-backed config")
	}

	// Reject world-readable, symlinked, hard-linked, or FIFO secret files.
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
	hardPath := filepath.Join(dir, "audit-event-hard")
	testutil.NoError(t, os.Link(evFile, hardPath))
	if _, err := readStrictHexKeyFile(evFile, uint32(os.Geteuid())); err == nil {
		t.Fatal("hard-linked audit event key accepted")
	}
	testutil.NoError(t, os.Remove(hardPath))

	bootEnv := baseEnv(map[string]string{
		"OCSERV_LOCAL_AUTH_ENABLED":            "true",
		"OCSERV_SESSION_KEY":                   strings.Repeat("a", 64),
		"OCSERV_LOCAL_BOOTSTRAP_USERNAME":      "first-admin",
		"OCSERV_LOCAL_BOOTSTRAP_WORKSPACE_ID":  "01900000-0000-7000-8000-000000000001",
		"OCSERV_LOCAL_BOOTSTRAP_PASSWORD_FILE": pwFile,
	})
	bootCfg, err := loadEnv([]string{"--bootstrap-local-admin"}, bootEnv)
	testutil.NoError(t, err)
	if !bootCfg.BootstrapLocalAdmin || bootCfg.LocalBootstrapPassword != password {
		t.Fatal("bootstrap secret not loaded")
	}
	if normalCfg, err := loadEnv(nil, bootEnv); err != nil || normalCfg.LocalBootstrapPassword != "" {
		t.Fatal("normal startup loaded bootstrap credential")
	}
	resetCfg, err := loadEnv([]string{"--reset-local-admin"}, bootEnv)
	testutil.NoError(t, err)
	if !resetCfg.ResetLocalAdmin || resetCfg.LocalBootstrapPassword != "" {
		t.Fatal("reset loaded the bootstrap credential or failed to enter reset mode")
	}
	for _, other := range []string{"--bootstrap-local-admin", "--migrate-only", "--provision-management-workspace", "--telemetry-retention-only"} {
		if _, err := loadEnv([]string{"--reset-local-admin", other}, bootEnv); err == nil {
			t.Fatalf("reset accepted with %s", other)
		}
	}
	if _, err := loadEnv([]string{"--reset-local-admin"}, baseEnv(nil)); err == nil {
		t.Fatal("reset accepted with Local authentication disabled")
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
