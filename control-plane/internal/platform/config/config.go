package config

import (
	"bytes"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/eventstream"
	"golang.org/x/sys/unix"
)

type Role string

const (
	RoleAPI       Role = "api"
	RoleWorker    Role = "worker"
	RoleScheduler Role = "scheduler"
	RoleAll       Role = "all"
)

type Config struct {
	Role                     Role
	MigrateOnly              bool
	RuntimeDBRole            string
	Environment              string
	HTTPAddress              string
	DatabaseURL              string
	OTLPEndpoint             string
	DevAuth                  bool
	DevAuthToken             string
	OIDCIssuer               string
	OIDCClientID             string
	OIDCClientSecret         string
	OIDCRedirectURL          string
	SessionKey               []byte
	SessionTTL               time.Duration
	AuditCheckpointKey       []byte
	AuditEventKeyID          string
	AuditEventKey            []byte
	CommandSigningKeyFile    string
	BreakGlassEnabled        bool
	BreakGlassTokenHash      []byte
	BodyLimit                int64
	RequestTimeout           time.Duration
	ShutdownTimeout          time.Duration
	LogLevelName             string
	TransportSocket          string
	TrustSocket              string
	TransportUID             uint32
	TransportGID             uint32
	TransportIdentitySet     bool
	ControllerEndpointID     string
	TransportTimeout         time.Duration
	TransportQueue           int
	OwnerLeaseTTL            time.Duration
	UserOperationConcurrency int
	LocalSimulator           bool
	PprofAddress             string
	TestResultCommitBarrier  string
	TestPreSendBarrier       string
	TestCommandLease         time.Duration
	TestSchedulerEvidence    bool
	CertificateSignerURL     string
	CertificateSignerToken   string
	CertificateSignerTimeout time.Duration
	EventStreams             eventstream.Config
}

type LookupEnv func(string) (string, bool)

func Load(args []string, lookup LookupEnv) (Config, error) {
	cfg := Config{
		Role: RoleAll, Environment: "development", HTTPAddress: "127.0.0.1:8080",
		BodyLimit: 1 << 20, RequestTimeout: 15 * time.Second, ShutdownTimeout: 10 * time.Second,
		LogLevelName: "info", TransportSocket: "/run/ocserv-platform/transportd.sock",
		TrustSocket:      "/run/ocserv-trust/control-plane.sock",
		TransportTimeout: 3 * time.Second, TransportQueue: 256, OwnerLeaseTTL: 30 * time.Second, UserOperationConcurrency: 50, SessionTTL: 8 * time.Hour, CertificateSignerTimeout: 10 * time.Second,
		EventStreams: eventstream.DefaultConfig(),
		TransportUID: uint32(os.Geteuid()), TransportGID: uint32(os.Getegid()),
	}
	setString(lookup, "OCSERV_ENVIRONMENT", &cfg.Environment)
	setString(lookup, "OCSERV_HTTP_ADDRESS", &cfg.HTTPAddress)
	if err := setStringOrFile(lookup, "OCSERV_DATABASE_URL", &cfg.DatabaseURL); err != nil {
		return Config{}, err
	}
	setString(lookup, "OCSERV_RUNTIME_DATABASE_ROLE", &cfg.RuntimeDBRole)
	setString(lookup, "OTEL_EXPORTER_OTLP_ENDPOINT", &cfg.OTLPEndpoint)
	setString(lookup, "OCSERV_LOG_LEVEL", &cfg.LogLevelName)
	setString(lookup, "OCSERV_TRANSPORT_SOCKET", &cfg.TransportSocket)
	setString(lookup, "OCSERV_TRUST_SOCKET", &cfg.TrustSocket)
	setString(lookup, "OCSERV_CONTROLLER_ENDPOINT_ID", &cfg.ControllerEndpointID)
	transportUID, hasTransportUID := lookup("OCSERV_TRANSPORT_UID")
	transportGID, hasTransportGID := lookup("OCSERV_TRANSPORT_GID")
	if hasTransportUID != hasTransportGID {
		return Config{}, errors.New("OCSERV_TRANSPORT_UID and OCSERV_TRANSPORT_GID must be set together")
	}
	if hasTransportUID {
		uid, err := strconv.ParseUint(transportUID, 10, 32)
		if err != nil {
			return Config{}, errors.New("OCSERV_TRANSPORT_UID must be uint32")
		}
		gid, err := strconv.ParseUint(transportGID, 10, 32)
		if err != nil {
			return Config{}, errors.New("OCSERV_TRANSPORT_GID must be uint32")
		}
		cfg.TransportUID, cfg.TransportGID, cfg.TransportIdentitySet = uint32(uid), uint32(gid), true
	}
	setString(lookup, "OCSERV_DEV_AUTH_TOKEN", &cfg.DevAuthToken)
	setString(lookup, "OCSERV_OIDC_ISSUER", &cfg.OIDCIssuer)
	setString(lookup, "OCSERV_OIDC_CLIENT_ID", &cfg.OIDCClientID)
	if err := setStringOrFile(lookup, "OCSERV_OIDC_CLIENT_SECRET", &cfg.OIDCClientSecret); err != nil {
		return Config{}, err
	}
	setString(lookup, "OCSERV_OIDC_REDIRECT_URL", &cfg.OIDCRedirectURL)
	setString(lookup, "OCSERV_CERTIFICATE_SIGNER_URL", &cfg.CertificateSignerURL)
	if err := setStringOrFile(lookup, "OCSERV_CERTIFICATE_SIGNER_TOKEN", &cfg.CertificateSignerToken); err != nil {
		return Config{}, err
	}
	if err := setHexOrFile(lookup, "OCSERV_SESSION_KEY", &cfg.SessionKey); err != nil {
		return Config{}, err
	}
	if err := setHexOrFile(lookup, "OCSERV_AUDIT_CHECKPOINT_KEY", &cfg.AuditCheckpointKey); err != nil {
		return Config{}, err
	}
	setString(lookup, "OCSERV_AUDIT_EVENT_KEY_ID", &cfg.AuditEventKeyID)
	if path, ok := lookup("OCSERV_AUDIT_EVENT_KEY_FILE"); ok {
		key, err := readStrictHexKeyFile(path, uint32(os.Geteuid()))
		if err != nil {
			return Config{}, fmt.Errorf("OCSERV_AUDIT_EVENT_KEY_FILE: %w", err)
		}
		cfg.AuditEventKey = key
	}
	if testKey, ok := lookup("OCSERV_TEST_AUDIT_EVENT_KEY_HEX"); ok {
		if cfg.Environment == "production" || len(cfg.AuditEventKey) != 0 {
			return Config{}, errors.New("OCSERV_TEST_AUDIT_EVENT_KEY_HEX is test-only and cannot override a key file")
		}
		key, err := hex.DecodeString(testKey)
		if err != nil || len(key) != 32 || strings.ToLower(testKey) != testKey {
			return Config{}, errors.New("OCSERV_TEST_AUDIT_EVENT_KEY_HEX must contain exactly 32 bytes as lowercase hex")
		}
		cfg.AuditEventKey = key
		if cfg.AuditEventKeyID == "" {
			cfg.AuditEventKeyID = "test-audit-event-v1"
		}
	}
	setString(lookup, "OCSERV_COMMAND_SIGNING_KEY_FILE", &cfg.CommandSigningKeyFile)
	setString(lookup, "OCSERV_PPROF_ADDRESS", &cfg.PprofAddress)
	setString(lookup, "OCSERV_TEST_RESULT_COMMIT_BARRIER_DIR", &cfg.TestResultCommitBarrier)
	setString(lookup, "OCSERV_TEST_PRE_SEND_BARRIER_DIR", &cfg.TestPreSendBarrier)
	if err := setHexOrFile(lookup, "OCSERV_BREAK_GLASS_TOKEN_SHA256", &cfg.BreakGlassTokenHash); err != nil {
		return Config{}, err
	}
	if err := setDuration(lookup, "OCSERV_SESSION_TTL", &cfg.SessionTTL); err != nil {
		return Config{}, err
	}
	if err := setInt64(lookup, "OCSERV_BODY_LIMIT_BYTES", &cfg.BodyLimit); err != nil {
		return Config{}, err
	}
	if err := setDuration(lookup, "OCSERV_REQUEST_TIMEOUT", &cfg.RequestTimeout); err != nil {
		return Config{}, err
	}
	if err := setDuration(lookup, "OCSERV_SHUTDOWN_TIMEOUT", &cfg.ShutdownTimeout); err != nil {
		return Config{}, err
	}
	if err := setDuration(lookup, "OCSERV_TRANSPORT_TIMEOUT", &cfg.TransportTimeout); err != nil {
		return Config{}, err
	}
	testCommandLeaseConfigured := false
	if value, ok := lookup("OCSERV_TEST_COMMAND_LEASE"); ok {
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return Config{}, fmt.Errorf("OCSERV_TEST_COMMAND_LEASE: %w", err)
		}
		if parsed == 0 {
			return Config{}, errors.New("OCSERV_TEST_COMMAND_LEASE must not be zero")
		}
		cfg.TestCommandLease = parsed
		testCommandLeaseConfigured = true
	}
	if cfg.TestPreSendBarrier != "" && !testCommandLeaseConfigured {
		cfg.TestCommandLease = 10 * time.Second
	}
	if err := setDuration(lookup, "OCSERV_CERTIFICATE_SIGNER_TIMEOUT", &cfg.CertificateSignerTimeout); err != nil {
		return Config{}, err
	}
	if err := setInt(lookup, "OCSERV_TRANSPORT_QUEUE_CAPACITY", &cfg.TransportQueue); err != nil {
		return Config{}, err
	}
	if err := setDuration(lookup, "OCSERV_OWNER_LEASE_TTL", &cfg.OwnerLeaseTTL); err != nil {
		return Config{}, err
	}
	if cfg.OwnerLeaseTTL <= 0 {
		cfg.OwnerLeaseTTL = 30 * time.Second
	}
	if err := setInt(lookup, "OCSERV_USER_OPERATION_CONCURRENCY", &cfg.UserOperationConcurrency); err != nil {
		return Config{}, err
	}
	for name, target := range map[string]*int{
		"OCSERV_SSE_GLOBAL_LIMIT":    &cfg.EventStreams.GlobalStreams,
		"OCSERV_SSE_IDENTITY_LIMIT":  &cfg.EventStreams.IdentityStreams,
		"OCSERV_SSE_SESSION_LIMIT":   &cfg.EventStreams.SessionStreams,
		"OCSERV_SSE_WORKSPACE_LIMIT": &cfg.EventStreams.WorkspaceStreams,
		"OCSERV_SSE_RESOURCE_LIMIT":  &cfg.EventStreams.ResourceStreams,
		"OCSERV_SSE_WATCHER_LIMIT":   &cfg.EventStreams.Watchers,
		"OCSERV_SSE_QUEUE_CAPACITY":  &cfg.EventStreams.SubscriberQueue,
	} {
		if err := setInt(lookup, name, target); err != nil {
			return Config{}, err
		}
	}
	for name, target := range map[string]*time.Duration{
		"OCSERV_SSE_POLL_INTERVAL":        &cfg.EventStreams.PollInterval,
		"OCSERV_SSE_DATABASE_MAX_BACKOFF": &cfg.EventStreams.DatabaseMaxBackoff,
		"OCSERV_SSE_MAX_LIFETIME":         &cfg.EventStreams.MaxLifetime,
		"OCSERV_SSE_REVALIDATE_INTERVAL":  &cfg.EventStreams.RevalidateInterval,
		"OCSERV_SSE_RETRY_AFTER":          &cfg.EventStreams.RetryAfter,
	} {
		if err := setDuration(lookup, name, target); err != nil {
			return Config{}, err
		}
	}
	if value, ok := lookup("OCSERV_DEV_AUTH"); ok {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return Config{}, fmt.Errorf("OCSERV_DEV_AUTH: %w", err)
		}
		cfg.DevAuth = parsed
	}
	if value, ok := lookup("OCSERV_LOCAL_SIMULATOR"); ok {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return Config{}, fmt.Errorf("OCSERV_LOCAL_SIMULATOR: %w", err)
		}
		cfg.LocalSimulator = parsed
	}
	if value, ok := lookup("OCSERV_TEST_SCHEDULER_MAINTENANCE_EVIDENCE"); ok {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return Config{}, fmt.Errorf("OCSERV_TEST_SCHEDULER_MAINTENANCE_EVIDENCE: %w", err)
		}
		cfg.TestSchedulerEvidence = parsed
	}
	if value, ok := lookup("OCSERV_BREAK_GLASS_ENABLED"); ok {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return Config{}, fmt.Errorf("OCSERV_BREAK_GLASS_ENABLED: %w", err)
		}
		cfg.BreakGlassEnabled = parsed
	}

	fs := flag.NewFlagSet("ocserv-control", flag.ContinueOnError)
	role := fs.String("role", string(cfg.Role), "process role: api, worker, scheduler, or all")
	migrateOnly := fs.Bool("migrate-only", false, "apply migrations and grant runtime privileges, then exit")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if fs.NArg() != 0 {
		return Config{}, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	cfg.Role = Role(*role)
	cfg.MigrateOnly = *migrateOnly
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	switch c.Role {
	case RoleAPI, RoleWorker, RoleScheduler, RoleAll:
	default:
		return fmt.Errorf("invalid role %q", c.Role)
	}
	if c.Environment != "development" && c.Environment != "test" && c.Environment != "production" {
		return fmt.Errorf("invalid environment %q", c.Environment)
	}
	if c.DatabaseURL == "" {
		return errors.New("OCSERV_DATABASE_URL is required")
	}
	if c.MigrateOnly && strings.TrimSpace(c.RuntimeDBRole) == "" {
		return errors.New("OCSERV_RUNTIME_DATABASE_ROLE is required with --migrate-only")
	}
	u, err := url.Parse(c.DatabaseURL)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
		return errors.New("OCSERV_DATABASE_URL must be a PostgreSQL URL")
	}
	if c.DevAuth {
		host, _, err := net.SplitHostPort(c.HTTPAddress)
		if err != nil {
			return fmt.Errorf("invalid HTTP address: %w", err)
		}
		ip := net.ParseIP(host)
		if c.Environment != "development" || ip == nil || !ip.IsLoopback() {
			return errors.New("development auth requires environment=development and a loopback HTTP address")
		}
	}
	if c.DevAuthToken != "" && (c.Environment != "development" || len(c.DevAuthToken) < 32) {
		return errors.New("development auth token requires environment=development and at least 32 characters")
	}
	oidcConfigured := c.OIDCIssuer != "" || c.OIDCClientID != "" || c.OIDCClientSecret != "" || c.OIDCRedirectURL != "" || len(c.SessionKey) != 0
	if oidcConfigured {
		issuer, issuerErr := url.Parse(c.OIDCIssuer)
		redirect, redirectErr := url.Parse(c.OIDCRedirectURL)
		if issuerErr != nil || issuer.Scheme != "https" || issuer.Host == "" || issuer.User != nil || issuer.RawQuery != "" || issuer.Fragment != "" ||
			redirectErr != nil || redirect.Scheme != "https" || redirect.Host == "" || redirect.User != nil || redirect.RawQuery != "" || redirect.Fragment != "" ||
			c.OIDCClientID == "" || c.OIDCClientSecret == "" || len(c.SessionKey) != 32 || c.SessionTTL < time.Minute || c.SessionTTL > 24*time.Hour {
			return errors.New("OIDC requires HTTPS issuer/redirect, client credentials, a 32-byte session key, and a session TTL from 1m to 24h")
		}
	}
	if c.Environment == "production" && !oidcConfigured {
		return errors.New("OIDC is required in production")
	}
	if len(c.AuditCheckpointKey) != 0 && len(c.AuditCheckpointKey) != 32 {
		return errors.New("audit checkpoint key must be 32 bytes")
	}
	if c.Environment == "production" && len(c.AuditCheckpointKey) != 32 {
		return errors.New("audit checkpoint key is required in production")
	}
	auditEventConfigured := c.AuditEventKeyID != "" || len(c.AuditEventKey) != 0
	if auditEventConfigured && (!validKeyID(c.AuditEventKeyID) || len(c.AuditEventKey) != 32) {
		return errors.New("audit event authentication requires a valid key ID and 32-byte key file")
	}
	if c.Environment == "production" && !auditEventConfigured {
		return errors.New("audit event authentication key is required in production")
	}
	if len(c.AuditEventKey) == 32 && len(c.AuditCheckpointKey) == 32 && bytes.Equal(c.AuditEventKey, c.AuditCheckpointKey) {
		return errors.New("audit event and checkpoint keys must be distinct")
	}
	if c.CommandSigningKeyFile != "" && !filepath.IsAbs(c.CommandSigningKeyFile) {
		return errors.New("command signing key file path must be absolute")
	}
	if c.Environment == "production" && !c.MigrateOnly && c.CommandSigningKeyFile == "" {
		return errors.New("controller command signing key file is required in production")
	}
	if c.Environment == "production" && !c.MigrateOnly && (!c.TransportIdentitySet || c.TransportUID == uint32(os.Geteuid())) {
		return errors.New("production transport UDS requires an explicit, distinct transport UID/GID")
	}
	if c.BreakGlassEnabled && len(c.BreakGlassTokenHash) != 32 {
		return errors.New("enabled break-glass requires a 32-byte token SHA-256 hash")
	}
	if !c.BreakGlassEnabled && len(c.BreakGlassTokenHash) != 0 {
		return errors.New("break-glass token hash requires explicit enablement")
	}
	if c.BodyLimit < 1 || c.RequestTimeout <= 0 || c.ShutdownTimeout <= 0 {
		return errors.New("limits and timeouts must be positive")
	}
	if !strings.HasPrefix(c.TransportSocket, "/") || c.TransportTimeout <= 0 || c.TransportQueue < 1 || c.TransportQueue > 4096 {
		return errors.New("transport UDS path, timeout, or queue capacity is invalid")
	}
	if c.UserOperationConcurrency < 1 || c.UserOperationConcurrency > 500 {
		return errors.New("user operation concurrency must be between 1 and 500")
	}
	if err := c.EventStreams.Validate(); err != nil {
		return fmt.Errorf("invalid SSE capacity configuration: %w", err)
	}
	signerConfigured := c.CertificateSignerURL != "" || c.CertificateSignerToken != ""
	if signerConfigured {
		signerURL, signerErr := url.Parse(c.CertificateSignerURL)
		if signerErr != nil || signerURL.Scheme != "https" || signerURL.Host == "" || signerURL.User != nil || signerURL.RawQuery != "" || signerURL.Fragment != "" || c.CertificateSignerToken == "" || c.CertificateSignerTimeout < time.Second || c.CertificateSignerTimeout > 30*time.Second {
			return errors.New("certificate signer requires an HTTPS URL, token, and timeout from 1s to 30s")
		}
	}
	if !strings.HasPrefix(c.TrustSocket, "/") {
		return errors.New("trust UDS path is invalid")
	}
	if c.ControllerEndpointID != "" {
		decoded, err := hex.DecodeString(c.ControllerEndpointID)
		if err != nil || len(decoded) != 32 || strings.ToLower(c.ControllerEndpointID) != c.ControllerEndpointID {
			return errors.New("controller endpoint ID must be 32-byte lowercase hex")
		}
	}
	if c.PprofAddress != "" {
		host, port, err := net.SplitHostPort(c.PprofAddress)
		if err != nil {
			return fmt.Errorf("invalid pprof address: %w", err)
		}
		portNumber, err := strconv.ParseUint(port, 10, 16)
		if err != nil || portNumber == 0 {
			return errors.New("pprof address requires a numeric port between 1 and 65535")
		}
		ip := net.ParseIP(host)
		if c.Environment == "production" || ip == nil || !ip.IsLoopback() {
			return errors.New("pprof requires environment=development or test and a loopback address")
		}
	}
	if c.TestResultCommitBarrier != "" {
		if c.Environment == "production" || !filepath.IsAbs(c.TestResultCommitBarrier) {
			return errors.New("result commit barrier is test-only and requires an absolute directory")
		}
	}
	if c.TestPreSendBarrier != "" {
		if c.Environment == "production" || !filepath.IsAbs(c.TestPreSendBarrier) {
			return errors.New("pre-send barrier is test-only and requires an absolute directory")
		}
	}
	if c.TestCommandLease != 0 {
		if c.TestPreSendBarrier == "" {
			return errors.New("test command lease requires the pre-send barrier")
		}
		if c.Environment == "production" || c.TestCommandLease < 10*time.Second || c.TestCommandLease > 60*time.Second {
			return errors.New("test command lease is test-only and must be between 10s and 60s")
		}
	} else if c.TestPreSendBarrier != "" {
		return errors.New("pre-send barrier requires a test command lease")
	}
	if c.LocalSimulator && c.Environment == "production" {
		return errors.New("local simulator is forbidden in production")
	}
	if c.TestSchedulerEvidence && c.Environment == "production" {
		return errors.New("scheduler maintenance evidence is test-only")
	}
	if _, ok := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}[c.LogLevelName]; !ok {
		return fmt.Errorf("invalid log level %q", c.LogLevelName)
	}
	return nil
}

func (c Config) LogLevel() slog.Level {
	return map[string]slog.Level{"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError}[c.LogLevelName]
}

func (c Config) RunsAPI() bool { return c.Role == RoleAPI || c.Role == RoleAll }

func (c Config) RunsWorker() bool { return c.Role == RoleWorker || c.Role == RoleAll }

func (c Config) RunsScheduler() bool { return c.Role == RoleScheduler || c.Role == RoleAll }

func setString(lookup LookupEnv, name string, target *string) {
	if value, ok := lookup(name); ok {
		*target = value
	}
}

func setStringOrFile(lookup LookupEnv, name string, target *string) error {
	value, hasValue := lookup(name)
	path, hasFile := lookup(name + "_FILE")
	if hasValue && hasFile {
		return fmt.Errorf("%s and %s_FILE are mutually exclusive", name, name)
	}
	if hasValue {
		*target = value
		return nil
	}
	if !hasFile {
		return nil
	}
	secret, err := readSecretFile(path)
	if err != nil {
		return fmt.Errorf("%s_FILE: %w", name, err)
	}
	*target = secret
	return nil
}

func readSecretFile(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("secret file path must be absolute")
	}
	before, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !before.Mode().IsRegular() {
		return "", errors.New("secret file must be a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !os.SameFile(before, after) {
		return "", errors.New("secret file changed while opening")
	}
	raw, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return "", err
	}
	if len(raw) == 0 || len(raw) > 4096 {
		return "", errors.New("secret file must contain 1..4096 bytes")
	}
	value := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
	if value == "" || strings.ContainsRune(value, '\x00') {
		return "", errors.New("secret file contains an invalid value")
	}
	return value, nil
}

func readStrictHexKeyFile(path string, expectedUID uint32) ([]byte, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("key path must be absolute and clean")
	}
	components := strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
	if len(components) == 0 || components[len(components)-1] == "" {
		return nil, errors.New("key path is invalid")
	}
	directoryFD, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open key root: %w", err)
	}
	defer func() { _ = unix.Close(directoryFD) }()
	if err := validateKeyDirectoryFD(directoryFD, expectedUID); err != nil {
		return nil, err
	}
	for _, component := range components[:len(components)-1] {
		if component == "" || component == "." || component == ".." {
			return nil, errors.New("key ancestry is invalid")
		}
		nextFD, openErr := unix.Openat(directoryFD, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if openErr != nil {
			return nil, fmt.Errorf("open key ancestor: %w", openErr)
		}
		if err := validateKeyDirectoryFD(nextFD, expectedUID); err != nil {
			_ = unix.Close(nextFD)
			return nil, err
		}
		_ = unix.Close(directoryFD)
		directoryFD = nextFD
	}
	fileFD, err := unix.Openat(directoryFD, components[len(components)-1], unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open key file: %w", err)
	}
	file := os.NewFile(uintptr(fileFD), path)
	if file == nil {
		_ = unix.Close(fileFD)
		return nil, errors.New("open key file descriptor")
	}
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fileFD, &stat); err != nil {
		return nil, fmt.Errorf("stat key file: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != expectedUID || stat.Nlink != 1 {
		return nil, errors.New("key must be a process-owned regular file with one link")
	}
	mode := stat.Mode & 0o777
	if mode != 0o400 && mode != 0o600 {
		return nil, errors.New("key mode must be 0400 or 0600")
	}
	raw, err := io.ReadAll(io.LimitReader(file, 67))
	if err != nil {
		return nil, err
	}
	value := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
	if len(value) != 64 || strings.ToLower(value) != value {
		return nil, errors.New("key must contain exactly 32 bytes as lowercase hex")
	}
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return nil, errors.New("key must contain exactly 32 bytes as lowercase hex")
	}
	return decoded, nil
}

func validateKeyDirectoryFD(fd int, expectedUID uint32) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fmt.Errorf("stat key ancestry: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || (stat.Uid != 0 && stat.Uid != expectedUID) || stat.Mode&0o022 != 0 {
		return errors.New("key ancestry must be root- or process-owned and not group/world writable")
	}
	return nil
}

func validKeyID(value string) bool {
	if len(value) < 1 || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') && (character < '0' || character > '9') && character != '.' && character != '_' && character != '-' {
			return false
		}
	}
	return true
}

func setInt64(lookup LookupEnv, name string, target *int64) error {
	value, ok := lookup(name)
	if !ok {
		return nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	*target = parsed
	return nil
}

func setInt(lookup LookupEnv, name string, target *int) error {
	value, ok := lookup(name)
	if !ok {
		return nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	*target = parsed
	return nil
}

func setDuration(lookup LookupEnv, name string, target *time.Duration) error {
	value, ok := lookup(name)
	if !ok {
		return nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	*target = parsed
	return nil
}

func setHexOrFile(lookup LookupEnv, name string, target *[]byte) error {
	value, hasValue := lookup(name)
	path, hasFile := lookup(name + "_FILE")
	if hasValue && hasFile {
		return fmt.Errorf("%s and %s_FILE are mutually exclusive", name, name)
	}
	if hasFile {
		secret, err := readSecretFile(path)
		if err != nil {
			return fmt.Errorf("%s_FILE: %w", name, err)
		}
		value, hasValue = secret, true
	}
	if !hasValue || value == "" {
		return nil
	}
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return fmt.Errorf("%s must be lowercase hex: %w", name, err)
	}
	if strings.ToLower(value) != value {
		return fmt.Errorf("%s must be lowercase hex", name)
	}
	*target = decoded
	return nil
}

func (c Config) OIDCEnabled() bool { return c.OIDCIssuer != "" }

// BrowserOrigin derives the exact public browser origin that may drive
// cookie-authenticated mutations from the validated OIDC redirect URL, so the
// CSRF boundary introduces no second origin configuration to drift. The
// redirect URL carries no user info, query, or fragment and its host keeps
// any explicit port; an unparseable value yields no origin rather than a
// permissive one. Empty when OIDC is not configured.
func (c Config) BrowserOrigin() string {
	if c.OIDCRedirectURL == "" {
		return ""
	}
	origin, err := url.Parse(c.OIDCRedirectURL)
	if err != nil || origin.Scheme == "" || origin.Host == "" {
		return ""
	}
	return strings.ToLower(origin.Scheme) + "://" + strings.ToLower(origin.Host)
}
