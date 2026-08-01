package config

import (
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
	Role            Role
	Environment     string
	HTTPAddress     string
	DatabaseURL     string
	OTLPEndpoint    string
	DevAuth         bool
	BodyLimit       int64
	RequestTimeout  time.Duration
	ShutdownTimeout time.Duration
	LogLevelName    string
}

type LookupEnv func(string) (string, bool)

func Load(args []string, lookup LookupEnv) (Config, error) {
	cfg := Config{
		Role: RoleAll, Environment: "development", HTTPAddress: "127.0.0.1:8080",
		BodyLimit: 1 << 20, RequestTimeout: 15 * time.Second, ShutdownTimeout: 10 * time.Second,
		LogLevelName: "info",
	}
	setString(lookup, "OCSERV_ENVIRONMENT", &cfg.Environment)
	setString(lookup, "OCSERV_HTTP_ADDRESS", &cfg.HTTPAddress)
	setString(lookup, "OCSERV_DATABASE_URL", &cfg.DatabaseURL)
	setString(lookup, "OTEL_EXPORTER_OTLP_ENDPOINT", &cfg.OTLPEndpoint)
	setString(lookup, "OCSERV_LOG_LEVEL", &cfg.LogLevelName)
	if err := setInt64(lookup, "OCSERV_BODY_LIMIT_BYTES", &cfg.BodyLimit); err != nil {
		return Config{}, err
	}
	if err := setDuration(lookup, "OCSERV_REQUEST_TIMEOUT", &cfg.RequestTimeout); err != nil {
		return Config{}, err
	}
	if err := setDuration(lookup, "OCSERV_SHUTDOWN_TIMEOUT", &cfg.ShutdownTimeout); err != nil {
		return Config{}, err
	}
	if value, ok := lookup("OCSERV_DEV_AUTH"); ok {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return Config{}, fmt.Errorf("OCSERV_DEV_AUTH: %w", err)
		}
		cfg.DevAuth = parsed
	}

	fs := flag.NewFlagSet("ocserv-control", flag.ContinueOnError)
	role := fs.String("role", string(cfg.Role), "process role: api, worker, scheduler, or all")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if fs.NArg() != 0 {
		return Config{}, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	cfg.Role = Role(*role)
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
	if c.BodyLimit < 1 || c.RequestTimeout <= 0 || c.ShutdownTimeout <= 0 {
		return errors.New("limits and timeouts must be positive")
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
