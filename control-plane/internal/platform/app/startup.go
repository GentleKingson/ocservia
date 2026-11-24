package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/connection"
	"github.com/GentleKingson/ocservia/control-plane/internal/platform/config"
	"github.com/google/uuid"
)

func validateBootstrapPasswords(cfg config.Config) error {
	// Reject new bootstrap passwords before startup can transition audit state.
	if cfg.BootstrapLocalAdmin {
		if err := auth.ValidateNewPassword(cfg.LocalBootstrapPassword); err != nil {
			return err
		}
	}

	return nil
}

// ResetLocalAdminPassword runs the database-only recovery command alongside the
// live Controller, using the same runtime database role and audit key.
func ResetLocalAdminPassword(ctx context.Context, cfg config.Config) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := connection.Open(ctx, connection.Options{Environment: cfg.Environment, URL: cfg.DatabaseURL, CAFile: cfg.DatabaseTLSCAFile, BundledPostgres: cfg.DatabaseBundledPostgres})
	if err != nil {
		return "", "", err
	}
	defer conn.Close()
	if err := conn.ValidateRuntime(ctx); err != nil {
		return "", "", err
	}
	if _, err := newAuditManager(conn.Store, cfg); err != nil {
		return "", "", err
	}
	service, err := auth.NewBackend(conn.Store, auth.Config{LocalEnabled: cfg.LocalAuthEnabled(), SessionKey: cfg.SessionKey, SessionTTL: cfg.SessionTTL})
	if err != nil {
		return "", "", err
	}
	return service.ResetLocalAdminPassword(ctx)
}

// A nil manager means the requested one-shot command has finished.
func initializeDatabase(ctx context.Context, conn *connection.Connection, cfg config.Config, logger *slog.Logger) (*audit.Manager, error) {
	backend := conn.Store
	databaseTimeout := 30 * time.Second
	if cfg.MigrateOnly || cfg.TelemetryRetentionOnly {
		databaseTimeout = 10 * time.Minute
	}
	databaseCtx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	if cfg.TelemetryRetentionOnly {
		if cfg.TelemetryRetentionInterval > 0 {
			return nil, runTelemetryRetentionSchedule(ctx, cfg.TelemetryRetentionInterval, min(cfg.TelemetryRetentionInterval, 15*time.Minute), func(passCtx context.Context) error {
				passTimeout, cancel := context.WithTimeout(passCtx, 10*time.Minute)
				defer cancel()
				return conn.PruneTelemetryPartitions(passTimeout)
			}, logger)
		}
		if err := conn.PruneTelemetryPartitions(databaseCtx); err != nil {
			return nil, fmt.Errorf("retire expired telemetry partitions: %w", err)
		}
		logger.Info("telemetry partition retention complete")
		return nil, nil
	}
	if cfg.MigrateOnly {
		auditManager, err := newAuditManager(backend, cfg)
		if err != nil {
			return nil, err
		}
		if err := conn.Migrate(databaseCtx); err != nil {
			return nil, fmt.Errorf("migrate database: %w", err)
		}
		if err := auditManager.EnsureAuthenticity(databaseCtx); err != nil {
			return nil, fmt.Errorf("transition audit event authentication: %w", err)
		}
		if err := conn.GrantRuntimePrivileges(databaseCtx, cfg.RuntimeDBRole); err != nil {
			return nil, fmt.Errorf("grant runtime database privileges: %w", err)
		}
		logger.Info("database migrations complete")
		return nil, nil
	}
	if cfg.ProvisionWorkspace {
		id, created, err := conn.ProvisionManagementWorkspace(databaseCtx)
		if err != nil {
			return nil, fmt.Errorf("provision management workspace: %w", err)
		}
		logger.Info("management workspace ready", "workspace_id", id, "created", created)
		return nil, nil
	}
	auditManager, err := newAuditManager(backend, cfg)
	if err != nil {
		return nil, err
	}
	if err := auditManager.EnsureAuthenticity(databaseCtx); err != nil {
		return nil, fmt.Errorf("verify audit event authentication: %w", err)
	}

	if cfg.BootstrapLocalAdmin {
		service, err := auth.NewBackend(backend, auth.Config{LocalEnabled: cfg.LocalAuthEnabled(), SessionKey: cfg.SessionKey, SessionTTL: cfg.SessionTTL})
		if err != nil {
			return nil, errors.New("configure bootstrap authentication failed")
		}
		workspaceID, err := uuid.Parse(cfg.LocalBootstrapWorkspace)
		if err != nil || workspaceID.Version() != 7 {
			return nil, errors.New("bootstrap workspace ID must be UUIDv7")
		}
		id, err := service.BootstrapLocalAdmin(ctx, cfg.LocalBootstrapUsername, cfg.LocalBootstrapPassword, workspaceID)
		if err != nil {
			if errors.Is(err, auth.ErrLocalWorkspaceMissing) || errors.Is(err, auth.ErrLocalInitialized) {
				return nil, err
			}
			return nil, errors.New("Local administrator bootstrap rejected; check initialization state, workspace and credentials")
		}
		logger.Info("Local initialization complete", "administrator_identity_id", id, "workspace_id", workspaceID)
		return nil, nil
	}

	if err := conn.ValidateRuntime(databaseCtx); err != nil {
		return nil, fmt.Errorf("validate required runtime database capabilities: %w", err)
	}

	return auditManager, nil
}

func runTelemetryRetentionSchedule(ctx context.Context, interval, retryInterval time.Duration, prune func(context.Context) error, logger *slog.Logger) error {
	for {
		err := prune(ctx)
		if ctx.Err() != nil {
			if logger != nil {
				logger.Info("telemetry partition retention stopped")
			}
			return nil
		}
		wait := interval
		if err != nil {
			wait = retryInterval
			if logger != nil {
				logger.Warn("telemetry partition retention failed; retrying", "retry_in", wait, "error", err)
			}
		} else if logger != nil {
			logger.Info("telemetry partition retention complete")
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			if logger != nil {
				logger.Info("telemetry partition retention stopped")
			}
			return nil
		case <-timer.C:
		}
	}
}

func newAuditManager(backend database.Backend, cfg config.Config) (*audit.Manager, error) {
	if len(cfg.AuditEventKey) == 0 {
		return audit.NewBackendManager(backend, cfg.AuditCheckpointKey), nil
	}
	manager, err := audit.NewBackendManagerWithEventKey(backend, cfg.AuditCheckpointKey, cfg.AuditEventKeyID, cfg.AuditEventKey)
	if err != nil {
		return nil, fmt.Errorf("configure audit event authentication: %w", err)
	}
	return manager, nil
}
