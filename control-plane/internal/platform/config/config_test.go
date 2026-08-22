package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSensitiveConfigurationFiles(t *testing.T) {
	directory := secureKeyTestDirectory(t)
	database := filepath.Join(directory, "database-url")
	session := filepath.Join(directory, "session-key")
	eventKey := filepath.Join(directory, "audit-event-key")
	if err := os.WriteFile(database, []byte("postgres://app:secret@postgres/ocservia\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(session, []byte(strings.Repeat("a", 64)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(eventKey, []byte(strings.Repeat("b", 64)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		"OCSERV_DATABASE_URL_FILE":         database,
		"OCSERV_AUDIT_CHECKPOINT_KEY_FILE": session,
		"OCSERV_AUDIT_EVENT_KEY_ID":        "audit-event-v1",
		"OCSERV_AUDIT_EVENT_KEY_FILE":      eventKey,
	}
	cfg, err := Load(nil, func(key string) (string, bool) { value, ok := values[key]; return value, ok })
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.DatabaseURL != "postgres://app:secret@postgres/ocservia" || len(cfg.AuditCheckpointKey) != 32 || len(cfg.AuditEventKey) != 32 {
		t.Fatalf("unexpected file-backed config: database=%q key=%d", cfg.DatabaseURL, len(cfg.AuditCheckpointKey))
	}

	values["OCSERV_DATABASE_URL"] = "postgres://inline/ocservia"
	if _, err := Load(nil, func(key string) (string, bool) { value, ok := values[key]; return value, ok }); err == nil {
		t.Fatal("Load() accepted both inline and file-backed database credentials")
	}
	delete(values, "OCSERV_DATABASE_URL")
	symlink := filepath.Join(directory, "database-link")
	if err := os.Symlink(database, symlink); err != nil {
		t.Fatal(err)
	}
	values["OCSERV_DATABASE_URL_FILE"] = symlink
	if _, err := Load(nil, func(key string) (string, bool) { value, ok := values[key]; return value, ok }); err == nil {
		t.Fatal("Load() accepted a symlinked secret file")
	}
}

func TestAuditEventKeyFileSecurity(t *testing.T) {
	directory := secureKeyTestDirectory(t)
	keyPath := filepath.Join(directory, "audit-event-key")
	if err := os.WriteFile(keyPath, []byte(strings.Repeat("a", 64)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if key, err := readStrictHexKeyFile(keyPath, uint32(os.Geteuid())); err != nil || len(key) != 32 {
		t.Fatalf("secure audit event key rejected: key=%d err=%v", len(key), err)
	}
	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readStrictHexKeyFile(keyPath, uint32(os.Geteuid())); err == nil {
		t.Fatal("world-readable audit event key accepted")
	}
	if err := os.Chmod(keyPath, 0o600); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(directory, "audit-event-key-link")
	if err := os.Symlink(keyPath, linkPath); err != nil {
		t.Fatal(err)
	}
	if _, err := readStrictHexKeyFile(linkPath, uint32(os.Geteuid())); err == nil {
		t.Fatal("symlinked audit event key accepted")
	}
	hardlinkPath := filepath.Join(directory, "audit-event-key-hardlink")
	if err := os.Link(keyPath, hardlinkPath); err != nil {
		t.Fatal(err)
	}
	if _, err := readStrictHexKeyFile(keyPath, uint32(os.Geteuid())); err == nil {
		t.Fatal("hard-linked audit event key accepted")
	}
	if err := os.Remove(hardlinkPath); err != nil {
		t.Fatal(err)
	}
	unsafeDirectory := filepath.Join(directory, "unsafe")
	if err := os.Mkdir(unsafeDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	unsafeKey := filepath.Join(unsafeDirectory, "key")
	if err := os.WriteFile(unsafeKey, []byte(strings.Repeat("b", 64)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unsafeDirectory, 0o770); err != nil {
		t.Fatal(err)
	}
	if _, err := readStrictHexKeyFile(unsafeKey, uint32(os.Geteuid())); err == nil {
		t.Fatal("audit event key under writable ancestry accepted")
	}
}

func secureKeyTestDirectory(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp(".", ".audit-key-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	absolute, err := filepath.Abs(directory)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestAuditEventTestKeyCannotBeUsedInProduction(t *testing.T) {
	values := map[string]string{
		"OCSERV_ENVIRONMENT":              "production",
		"OCSERV_TEST_AUDIT_EVENT_KEY_HEX": strings.Repeat("11", 32),
		"OCSERV_AUDIT_EVENT_KEY_ID":       "test-key",
		"OCSERV_DATABASE_URL":             "postgres://owner:test@postgres/ocservia",
		"OCSERV_OIDC_ISSUER":              "https://issuer.example.test",
		"OCSERV_OIDC_CLIENT_ID":           "client",
		"OCSERV_OIDC_CLIENT_SECRET":       strings.Repeat("x", 32),
		"OCSERV_OIDC_REDIRECT_URL":        "https://app.example.test/api/v1/auth/callback",
		"OCSERV_SESSION_KEY":              strings.Repeat("11", 32),
		"OCSERV_AUDIT_CHECKPOINT_KEY":     strings.Repeat("22", 32),
		"OCSERV_COMMAND_SIGNING_KEY_FILE": "/run/secrets/controller-command-signing-key.pem",
		"OCSERV_CONTROLLER_ENDPOINT_ID":   strings.Repeat("0", 64),
		"OCSERV_CERTIFICATE_SIGNER_URL":   "https://signer.example.test",
		"OCSERV_CERTIFICATE_SIGNER_TOKEN": strings.Repeat("t", 32),
	}
	_, err := Load(nil, func(key string) (string, bool) { value, ok := values[key]; return value, ok })
	if err == nil || !strings.Contains(err.Error(), "test-only") {
		t.Fatalf("production test key error = %v", err)
	}
}

func TestAuditEventAndCheckpointKeysMustBeDistinct(t *testing.T) {
	values := map[string]string{
		"OCSERV_ENVIRONMENT":              "test",
		"OCSERV_DATABASE_URL":             "postgres://db/test",
		"OCSERV_AUDIT_EVENT_KEY_ID":       "test-key",
		"OCSERV_TEST_AUDIT_EVENT_KEY_HEX": strings.Repeat("11", 32),
		"OCSERV_AUDIT_CHECKPOINT_KEY":     strings.Repeat("11", 32),
	}
	_, err := Load(nil, func(key string) (string, bool) { value, ok := values[key]; return value, ok })
	if err == nil || !strings.Contains(err.Error(), "must be distinct") {
		t.Fatalf("reused audit key error = %v", err)
	}
}

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

func TestDevAuthTokenGuard(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		ok   bool
	}{
		{"development", map[string]string{"OCSERV_DATABASE_URL": "postgres://db/test", "OCSERV_HTTP_ADDRESS": "0.0.0.0:8080", "OCSERV_DEV_AUTH_TOKEN": "local-development-token-32-characters"}, true},
		{"too short", map[string]string{"OCSERV_DATABASE_URL": "postgres://db/test", "OCSERV_DEV_AUTH_TOKEN": "short"}, false},
		{"production", map[string]string{"OCSERV_DATABASE_URL": "postgres://db/test", "OCSERV_ENVIRONMENT": "production", "OCSERV_DEV_AUTH_TOKEN": "local-development-token-32-characters"}, false},
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

func TestResultCommitBarrierGuard(t *testing.T) {
	for _, test := range []struct {
		name string
		env  map[string]string
		ok   bool
	}{
		{"development absolute", map[string]string{"OCSERV_DATABASE_URL": "postgres://db/test", "OCSERV_TEST_RESULT_COMMIT_BARRIER_DIR": "/run/g6-result-barrier"}, true},
		{"relative", map[string]string{"OCSERV_DATABASE_URL": "postgres://db/test", "OCSERV_TEST_RESULT_COMMIT_BARRIER_DIR": "barrier"}, false},
		{"production", map[string]string{"OCSERV_DATABASE_URL": "postgres://db/test", "OCSERV_ENVIRONMENT": "production", "OCSERV_TEST_RESULT_COMMIT_BARRIER_DIR": "/run/g6-result-barrier"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load(nil, func(key string) (string, bool) { value, ok := test.env[key]; return value, ok })
			if (err == nil) != test.ok {
				t.Fatalf("Load() error = %v, want success %v", err, test.ok)
			}
		})
	}
}

func TestPreSendBarrierGuard(t *testing.T) {
	for _, test := range []struct {
		name      string
		env       map[string]string
		ok        bool
		wantLease time.Duration
	}{
		{"development default lease", map[string]string{"OCSERV_DATABASE_URL": "postgres://db/test", "OCSERV_TEST_PRE_SEND_BARRIER_DIR": "/run/g6-pre-send-barrier"}, true, 10 * time.Second},
		{"test minimum lease", map[string]string{"OCSERV_DATABASE_URL": "postgres://db/test", "OCSERV_ENVIRONMENT": "test", "OCSERV_TEST_PRE_SEND_BARRIER_DIR": "/run/g6-pre-send-barrier", "OCSERV_TEST_COMMAND_LEASE": "10s"}, true, 10 * time.Second},
		{"development maximum lease", map[string]string{"OCSERV_DATABASE_URL": "postgres://db/test", "OCSERV_TEST_PRE_SEND_BARRIER_DIR": "/run/g6-pre-send-barrier", "OCSERV_TEST_COMMAND_LEASE": "60s"}, true, 60 * time.Second},
		{"relative", map[string]string{"OCSERV_DATABASE_URL": "postgres://db/test", "OCSERV_TEST_PRE_SEND_BARRIER_DIR": "barrier"}, false, 0},
		{"production", map[string]string{"OCSERV_DATABASE_URL": "postgres://db/test", "OCSERV_ENVIRONMENT": "production", "OCSERV_TEST_PRE_SEND_BARRIER_DIR": "/run/g6-pre-send-barrier", "OCSERV_TEST_COMMAND_LEASE": "60s"}, false, 0},
		{"lease without barrier", map[string]string{"OCSERV_DATABASE_URL": "postgres://db/test", "OCSERV_TEST_COMMAND_LEASE": "60s"}, false, 0},
		{"lease below minimum", map[string]string{"OCSERV_DATABASE_URL": "postgres://db/test", "OCSERV_TEST_PRE_SEND_BARRIER_DIR": "/run/g6-pre-send-barrier", "OCSERV_TEST_COMMAND_LEASE": "9s"}, false, 0},
		{"lease above maximum", map[string]string{"OCSERV_DATABASE_URL": "postgres://db/test", "OCSERV_TEST_PRE_SEND_BARRIER_DIR": "/run/g6-pre-send-barrier", "OCSERV_TEST_COMMAND_LEASE": "61s"}, false, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := Load(nil, func(key string) (string, bool) { value, ok := test.env[key]; return value, ok })
			if (err == nil) != test.ok {
				t.Fatalf("Load() error = %v, want success %v", err, test.ok)
			}
			if err == nil && cfg.TestCommandLease != test.wantLease {
				t.Fatalf("test command lease = %v, want %v", cfg.TestCommandLease, test.wantLease)
			}
		})
	}
}

func TestSchedulerMaintenanceEvidenceGuard(t *testing.T) {
	for _, test := range []struct {
		name    string
		env     map[string]string
		ok      bool
		enabled bool
	}{
		{"default disabled", map[string]string{"OCSERV_DATABASE_URL": "postgres://db/test"}, true, false},
		{"development enabled", map[string]string{"OCSERV_DATABASE_URL": "postgres://db/test", "OCSERV_TEST_SCHEDULER_MAINTENANCE_EVIDENCE": "true"}, true, true},
		{"test enabled", map[string]string{"OCSERV_DATABASE_URL": "postgres://db/test", "OCSERV_ENVIRONMENT": "test", "OCSERV_TEST_SCHEDULER_MAINTENANCE_EVIDENCE": "true"}, true, true},
		{"invalid boolean", map[string]string{"OCSERV_DATABASE_URL": "postgres://db/test", "OCSERV_TEST_SCHEDULER_MAINTENANCE_EVIDENCE": "yes-please"}, false, false},
		{"production enabled", map[string]string{"OCSERV_DATABASE_URL": "postgres://db/test", "OCSERV_ENVIRONMENT": "production", "OCSERV_TEST_SCHEDULER_MAINTENANCE_EVIDENCE": "true"}, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := Load(nil, func(key string) (string, bool) { value, ok := test.env[key]; return value, ok })
			if (err == nil) != test.ok {
				t.Fatalf("Load() error = %v, want success %v", err, test.ok)
			}
			if err == nil && cfg.TestSchedulerEvidence != test.enabled {
				t.Fatalf("scheduler maintenance evidence = %v, want %v", cfg.TestSchedulerEvidence, test.enabled)
			}
		})
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

func TestPprofAddressRequiresNonProductionLoopback(t *testing.T) {
	base := func(environment, address string) func(string) (string, bool) {
		values := map[string]string{
			"OCSERV_DATABASE_URL":  "postgres://db/test",
			"OCSERV_ENVIRONMENT":   environment,
			"OCSERV_PPROF_ADDRESS": address,
		}
		return func(key string) (string, bool) {
			value, ok := values[key]
			return value, ok
		}
	}
	for name, lookup := range map[string]func(string) (string, bool){
		"production":       base("production", "127.0.0.1:6060"),
		"non-loopback":     base("development", "0.0.0.0:6060"),
		"missing port":     base("development", "127.0.0.1"),
		"unparseable host": base("development", "127.0.0.1:x"),
	} {
		if _, err := Load(nil, lookup); err == nil {
			t.Fatalf("Load() accepted pprof address case %q", name)
		}
	}
	for name, lookup := range map[string]func(string) (string, bool){
		"loopback ipv4": base("development", "127.0.0.1:6060"),
		"loopback ipv6": base("test", "[::1]:6060"),
	} {
		cfg, err := Load(nil, lookup)
		if err != nil {
			t.Fatalf("Load() rejected pprof address case %q: %v", name, err)
		}
		if cfg.PprofAddress == "" {
			t.Fatalf("Load() dropped the pprof address in case %q", name)
		}
	}
}

func TestProductionRequiresAbsoluteControllerCommandSigningKey(t *testing.T) {
	cfg, err := Load(nil, func(key string) (string, bool) {
		if key == "OCSERV_DATABASE_URL" {
			return "postgres://db/test", true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Environment = "production"
	cfg.OIDCIssuer = "https://id.example.test"
	cfg.OIDCClientID = "ocservia"
	cfg.OIDCClientSecret = "test-secret"
	cfg.OIDCRedirectURL = "https://ocservia.example.test/api/v1/auth/callback"
	cfg.SessionKey = make([]byte, 32)
	cfg.AuditCheckpointKey = make([]byte, 32)
	cfg.AuditEventKeyID = "audit-event-v1"
	cfg.AuditEventKey = []byte(strings.Repeat("a", 32))
	cfg.TransportUID = uint32(os.Geteuid() + 1)
	cfg.TransportGID = uint32(os.Getegid())
	cfg.TransportIdentitySet = true
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "command signing key") {
		t.Fatalf("missing production command key error = %v", err)
	}
	cfg.CommandSigningKeyFile = "relative/controller.pem"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative production command key error = %v", err)
	}
	cfg.CommandSigningKeyFile = "/run/secrets/controller_command_signing_key"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("absolute production command key rejected: %v", err)
	}
}

func TestProductionRequiresDistinctTransportIdentity(t *testing.T) {
	values := map[string]string{
		"OCSERV_DATABASE_URL":  "postgres://db/test",
		"OCSERV_TRANSPORT_UID": "12345",
		"OCSERV_TRANSPORT_GID": "12346",
	}
	cfg, err := Load(nil, func(key string) (string, bool) { value, ok := values[key]; return value, ok })
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.TransportIdentitySet || cfg.TransportUID != 12345 || cfg.TransportGID != 12346 {
		t.Fatalf("unexpected transport identity: %+v", cfg)
	}

	delete(values, "OCSERV_TRANSPORT_GID")
	if _, err := Load(nil, func(key string) (string, bool) { value, ok := values[key]; return value, ok }); err == nil {
		t.Fatal("accepted a transport UID without a transport GID")
	}
}

func TestControllerEndpointIDValidation(t *testing.T) {
	for _, value := range []string{"ABC", strings.Repeat("z", 64), strings.Repeat("A", 64)} {
		_, err := Load(nil, func(key string) (string, bool) {
			values := map[string]string{"OCSERV_DATABASE_URL": "postgres://db/test", "OCSERV_CONTROLLER_ENDPOINT_ID": value}
			result, ok := values[key]
			return result, ok
		})
		if err == nil {
			t.Fatalf("accepted controller endpoint %q", value)
		}
	}
}

func TestUserOperationConcurrency(t *testing.T) {
	lookup := func(key string) (string, bool) {
		values := map[string]string{
			"OCSERV_DATABASE_URL":               "postgres://db/test",
			"OCSERV_USER_OPERATION_CONCURRENCY": "17",
		}
		value, ok := values[key]
		return value, ok
	}
	config, err := Load(nil, lookup)
	if err != nil || config.UserOperationConcurrency != 17 {
		t.Fatalf("configured concurrency=%d err=%v", config.UserOperationConcurrency, err)
	}
	if _, err := Load(nil, func(key string) (string, bool) {
		values := map[string]string{"OCSERV_DATABASE_URL": "postgres://db/test", "OCSERV_USER_OPERATION_CONCURRENCY": "501"}
		value, ok := values[key]
		return value, ok
	}); err == nil {
		t.Fatal("accepted user operation concurrency above the batch bound")
	}
}

func TestSSECapacityConfiguration(t *testing.T) {
	values := map[string]string{
		"OCSERV_DATABASE_URL":            "postgres://db/test",
		"OCSERV_SSE_GLOBAL_LIMIT":        "64",
		"OCSERV_SSE_IDENTITY_LIMIT":      "6",
		"OCSERV_SSE_SESSION_LIMIT":       "3",
		"OCSERV_SSE_WORKSPACE_LIMIT":     "16",
		"OCSERV_SSE_RESOURCE_LIMIT":      "8",
		"OCSERV_SSE_WATCHER_LIMIT":       "32",
		"OCSERV_SSE_QUEUE_CAPACITY":      "64",
		"OCSERV_SSE_MAX_LIFETIME":        "20m",
		"OCSERV_SSE_REVALIDATE_INTERVAL": "15s",
	}
	cfg, err := Load(nil, func(key string) (string, bool) { value, ok := values[key]; return value, ok })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EventStreams.GlobalStreams != 64 || cfg.EventStreams.SessionStreams != 3 || cfg.EventStreams.MaxLifetime != 20*time.Minute {
		t.Fatalf("unexpected SSE config: %+v", cfg.EventStreams)
	}
	values["OCSERV_SSE_SESSION_LIMIT"] = "7"
	if _, err := Load(nil, func(key string) (string, bool) { value, ok := values[key]; return value, ok }); err == nil {
		t.Fatal("accepted a session limit above the identity limit")
	}
}

func TestCertificateSignerRequiresHTTPSAndCompleteCredentials(t *testing.T) {
	tests := []struct {
		name, endpoint, token string
		ok                    bool
	}{
		{name: "disabled", ok: true},
		{name: "https", endpoint: "https://pki.example.test/v1/sign", token: "fixture-token", ok: true},
		{name: "http", endpoint: "http://pki.example.test/v1/sign", token: "fixture-token"},
		{name: "userinfo", endpoint: "https://user@pki.example.test/v1/sign", token: "fixture-token"},
		{name: "missing token", endpoint: "https://pki.example.test/v1/sign"},
		{name: "missing endpoint", token: "fixture-token"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values := map[string]string{"OCSERV_DATABASE_URL": "postgres://db/test"}
			if test.endpoint != "" {
				values["OCSERV_CERTIFICATE_SIGNER_URL"] = test.endpoint
			}
			if test.token != "" {
				values["OCSERV_CERTIFICATE_SIGNER_TOKEN"] = test.token
			}
			_, err := Load(nil, func(key string) (string, bool) { value, ok := values[key]; return value, ok })
			if (err == nil) != test.ok {
				t.Fatalf("Load() error=%v want success=%v", err, test.ok)
			}
		})
	}
}
