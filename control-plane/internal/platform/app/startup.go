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
	"github.com/GentleKingson/ocservia/control-plane/migrations"
	"github.com/google/uuid"
)

func validateBootstrapPasswords(cfg config.Config) error {
	// Reject new bootstrap passwords before startup can transition audit state.
	if cfg.BootstrapLocalAdmin {
		if err := auth.ValidateNewPassword(cfg.LocalBootstrapPassword); err != nil {
			return err
		}
	}
	if cfg.BootstrapLocalAdmin || cfg.CompleteLocalBootstrap {
		if err := auth.ValidateNewPassword(cfg.LocalBootstrapApproverPassword); err != nil {
			return err
		}
	}

	return nil
}

// A nil result means the requested one-shot command has finished.
type startupState struct {
	audit         *audit.Manager
	schemaVersion int64
}

func initializeDatabase(ctx context.Context, conn *connection.Connection, cfg config.Config, logger *slog.Logger) (*startupState, error) {
	backend := conn.Store
	databaseTimeout := 30 * time.Second
	if cfg.MigrateOnly {
		databaseTimeout = 10 * time.Minute
	}
	databaseCtx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	if err := conn.ValidateDeployment(databaseCtx); err != nil {
		return nil, fmt.Errorf("validate database deployment: %w", err)
	}
	if cfg.SchemaCompatibilityCheck > 0 {
		if _, err := backend.ControllerSchema(databaseCtx, cfg.SchemaCompatibilityCheck); err != nil {
			return nil, fmt.Errorf("validate schema compatibility: %w", err)
		}
		logger.Info("database schema compatibility check passed", "schema", cfg.SchemaCompatibilityCheck)
		return nil, nil
	}
	if cfg.MigrateOnly {
		auditManager, err := newAuditManager(backend, cfg)
		if err != nil {
			return nil, err
		}
		if err := conn.Migrate(databaseCtx, auditManager); err != nil {
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
	expectedSchemaVersion, err := migrations.LatestSchemaVersion()
	if err != nil {
		return nil, err
	}
	if _, err := backend.ControllerSchema(databaseCtx, expectedSchemaVersion); err != nil {
		return nil, fmt.Errorf("validate database schema: %w", err)
	}
	auditManager, err := newAuditManager(backend, cfg)
	if err != nil {
		return nil, err
	}
	if err := auditManager.EnsureAuthenticity(databaseCtx); err != nil {
		return nil, fmt.Errorf("verify audit event authentication: %w", err)
	}

	if cfg.BootstrapLocalAdmin || cfg.CompleteLocalBootstrap {
		service, err := auth.NewBackend(backend, auth.Config{LocalEnabled: cfg.LocalAuthEnabled(), SessionKey: cfg.SessionKey, SessionTTL: cfg.SessionTTL})
		if err != nil {
			return nil, errors.New("configure bootstrap authentication failed")
		}
		workspaceID, err := uuid.Parse(cfg.LocalBootstrapWorkspace)
		if err != nil || workspaceID.Version() != 7 {
			return nil, errors.New("bootstrap workspace ID must be UUIDv7")
		}
		initialize := service.BootstrapLocalAdmin
		if cfg.CompleteLocalBootstrap {
			initialize = service.CompleteLocalBootstrap
		}
		id, err := initialize(ctx, cfg.LocalBootstrapUsername, cfg.LocalBootstrapPassword, workspaceID, cfg.LocalBootstrapApproverUsername, cfg.LocalBootstrapApproverPassword)
		if err != nil {
			if errors.Is(err, auth.ErrLocalWorkspaceMissing) {
				return nil, err
			}
			return nil, errors.New("Local administrator bootstrap rejected; check initialization state, workspace and credentials")
		}
		logger.Info("Local initialization complete", "administrator_identity_id", id, "workspace_id", workspaceID, "upgrade_completion", cfg.CompleteLocalBootstrap)
		return nil, nil
	}

	if err := conn.ValidateRuntime(databaseCtx); err != nil {
		return nil, fmt.Errorf("validate required runtime database capabilities: %w", err)
	}

	return &startupState{audit: auditManager, schemaVersion: expectedSchemaVersion}, nil
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
