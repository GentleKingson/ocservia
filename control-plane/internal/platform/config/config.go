package config

import (
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
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
	BreakGlassEnabled        bool
	BreakGlassTokenHash      []byte
	BodyLimit                int64
	RequestTimeout           time.Duration
	ShutdownTimeout          time.Duration
	LogLevelName             string
	TransportSocket          string
	TrustSocket              string
	ControllerEndpointID     string
	TransportTimeout         time.Duration
	TransportQueue           int
	UserOperationConcurrency int
	LocalSimulator           bool
}

type LookupEnv func(string) (string, bool)

func Load(args []string, lookup LookupEnv) (Config, error) {
	cfg := Config{
		Role: RoleAll, Environment: "development", HTTPAddress: "127.0.0.1:8080",
		BodyLimit: 1 << 20, RequestTimeout: 15 * time.Second, ShutdownTimeout: 10 * time.Second,
		LogLevelName: "info", TransportSocket: "/run/ocserv-platform/transportd.sock",
		TrustSocket:      "/run/ocserv-trust/control-plane.sock",
		TransportTimeout: 3 * time.Second, TransportQueue: 256, UserOperationConcurrency: 50, SessionTTL: 8 * time.Hour,
	}
	setString(lookup, "OCSERV_ENVIRONMENT", &cfg.Environment)
	setString(lookup, "OCSERV_HTTP_ADDRESS", &cfg.HTTPAddress)
	setString(lookup, "OCSERV_DATABASE_URL", &cfg.DatabaseURL)
	setString(lookup, "OCSERV_RUNTIME_DATABASE_ROLE", &cfg.RuntimeDBRole)
	setString(lookup, "OTEL_EXPORTER_OTLP_ENDPOINT", &cfg.OTLPEndpoint)
	setString(lookup, "OCSERV_LOG_LEVEL", &cfg.LogLevelName)
	setString(lookup, "OCSERV_TRANSPORT_SOCKET", &cfg.TransportSocket)
	setString(lookup, "OCSERV_TRUST_SOCKET", &cfg.TrustSocket)
	setString(lookup, "OCSERV_CONTROLLER_ENDPOINT_ID", &cfg.ControllerEndpointID)
	setString(lookup, "OCSERV_DEV_AUTH_TOKEN", &cfg.DevAuthToken)
	setString(lookup, "OCSERV_OIDC_ISSUER", &cfg.OIDCIssuer)
	setString(lookup, "OCSERV_OIDC_CLIENT_ID", &cfg.OIDCClientID)
	setString(lookup, "OCSERV_OIDC_CLIENT_SECRET", &cfg.OIDCClientSecret)
	setString(lookup, "OCSERV_OIDC_REDIRECT_URL", &cfg.OIDCRedirectURL)
	if err := setHex(lookup, "OCSERV_SESSION_KEY", &cfg.SessionKey); err != nil {
		return Config{}, err
	}
	if err := setHex(lookup, "OCSERV_AUDIT_CHECKPOINT_KEY", &cfg.AuditCheckpointKey); err != nil {
		return Config{}, err
	}
	if err := setHex(lookup, "OCSERV_BREAK_GLASS_TOKEN_SHA256", &cfg.BreakGlassTokenHash); err != nil {
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
	if err := setInt(lookup, "OCSERV_TRANSPORT_QUEUE_CAPACITY", &cfg.TransportQueue); err != nil {
		return Config{}, err
	}
	if err := setInt(lookup, "OCSERV_USER_OPERATION_CONCURRENCY", &cfg.UserOperationConcurrency); err != nil {
		return Config{}, err
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
	if !strings.HasPrefix(c.TrustSocket, "/") {
		return errors.New("trust UDS path is invalid")
	}
	if c.ControllerEndpointID != "" {
		decoded, err := hex.DecodeString(c.ControllerEndpointID)
		if err != nil || len(decoded) != 32 || strings.ToLower(c.ControllerEndpointID) != c.ControllerEndpointID {
			return errors.New("controller endpoint ID must be 32-byte lowercase hex")
		}
	}
	if c.LocalSimulator && c.Environment == "production" {
		return errors.New("local simulator is forbidden in production")
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

func setHex(lookup LookupEnv, name string, target *[]byte) error {
	value, ok := lookup(name)
	if !ok || value == "" {
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
