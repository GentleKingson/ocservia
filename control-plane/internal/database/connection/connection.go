// Package connection owns Controller PostgreSQL connections and owner-only startup.
// Business services receive only the Store, never driver handles.
package connection

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrUnmanagedWorkspaces means workspaces exist but none is the provisioned
// management workspace; the operator must select one explicitly.
var ErrUnmanagedWorkspaces = errors.New("workspaces exist without the administration workspace; select the management workspace explicitly")

type Options struct {
	Environment, URL, CAFile string
	// BundledPostgres is the explicit deployment declaration that the URL
	// targets the official bundled PostgreSQL service on its internal network.
	BundledPostgres bool
}

// bundledPostgresHost is the fixed Compose service name of bundled PostgreSQL.
const bundledPostgresHost = "postgres"

type Store interface {
	database.Backend
	database.Diagnostics
}

type Connection struct {
	Store Store
	pg    *pgxpool.Pool
}

func postgresURL(options Options) (string, error) {
	u, err := url.Parse(options.URL)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
		return "", errors.New("OCSERV_DATABASE_URL must be a PostgreSQL URL")
	}
	if options.CAFile == "" {
		if options.BundledPostgres || options.Environment == "production" {
			if err := validatePlaintextPostgres(options); err != nil {
				return "", err
			}
		}
		return options.URL, nil
	}
	if options.BundledPostgres {
		return "", errors.New("bundled PostgreSQL cannot use an external database CA")
	}
	if !filepath.IsAbs(options.CAFile) {
		return "", errors.New("external PostgreSQL CA file must use an absolute path")
	}
	query := u.Query()
	mode := query["sslmode"]
	if len(mode) != 1 || mode[0] != "verify-full" {
		return "", errors.New("external PostgreSQL requires sslmode=verify-full")
	}
	query.Set("sslrootcert", options.CAFile)
	u.RawQuery = query.Encode()
	return u.String(), nil
}

// validatePlaintextPostgres admits a connection without a verified CA only
// when every target the driver would dial is a loopback address, or the
// deployment explicitly declares the bundled service. A production URL that
// merely names a host like the bundled one is not exempt without that
// declaration, because DNS search domains can resolve it elsewhere.
func validatePlaintextPostgres(options Options) error {
	config, err := pgconn.ParseConfig(options.URL)
	if err != nil {
		return errors.New("OCSERV_DATABASE_URL must be a PostgreSQL URL")
	}
	hosts := []string{config.Host}
	for _, fallback := range config.Fallbacks {
		hosts = append(hosts, fallback.Host)
	}
	for _, host := range hosts {
		ip := net.ParseIP(host)
		switch {
		case options.BundledPostgres && host == bundledPostgresHost:
		case !options.BundledPostgres && ip != nil && ip.IsLoopback():
		case options.BundledPostgres:
			return errors.New("bundled PostgreSQL must target only the postgres service")
		default:
			return errors.New("production PostgreSQL requires OCSERV_DATABASE_TLS_CA_FILE with sslmode=verify-full unless it targets loopback or the declared bundled service")
		}
	}
	return nil
}

func ValidateOptions(options Options) error {
	_, err := postgresURL(options)
	return err
}

func Open(ctx context.Context, options Options) (*Connection, error) {
	databaseURL, err := postgresURL(options)
	if err != nil {
		return nil, err
	}
	pool, err := migrations.Open(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	return &Connection{Store: postgres.WrapPool(pool), pg: pool}, nil
}

func (c *Connection) Close() {
	c.pg.Close()
}

func (c *Connection) Migrate(ctx context.Context) error {
	return migrations.Migrate(ctx, c.pg)
}

func (c *Connection) PruneTelemetryPartitions(ctx context.Context) error {
	return migrations.PruneTelemetryPartitions(ctx, c.pg)
}

func (c *Connection) GrantRuntimePrivileges(ctx context.Context, account string) error {
	return migrations.GrantRuntimePrivileges(ctx, c.pg, account)
}

// ProvisionManagementWorkspace returns the "administration" workspace, creating
// it only on a database that has no workspace yet. The slug is unique, so a
// concurrent run fails instead of creating a second workspace.
func (c *Connection) ProvisionManagementWorkspace(ctx context.Context) (id uuid.UUID, created bool, err error) {
	const insert = `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'Administration','administration',$2,$2)`
	err = database.Within(ctx, c.Store, database.DefaultIsolation, func(tx database.Tx) error {
		err := tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE slug='administration'`).Scan(&id)
		if err == nil {
			return nil
		}
		if !errors.Is(err, database.ErrNotFound) {
			return err
		}
		var existing int64
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM workspaces`).Scan(&existing); err != nil {
			return err
		}
		if existing != 0 {
			return ErrUnmanagedWorkspaces
		}
		at, err := database.WallTime(ctx, tx)
		if err != nil {
			return err
		}
		id, created = uuid.Must(uuid.NewV7()), true
		_, err = tx.Exec(ctx, insert, id, at)
		return err
	})
	return id, created, err
}

// ValidateRuntime runs before starting listeners or background work.
// It verifies both schema migration alignment and telemetry partition readiness.
func (c *Connection) ValidateRuntime(ctx context.Context) error {
	if err := migrations.ValidateSchemaVersion(ctx, c.pg); err != nil {
		return err
	}
	return postgres.WrapPool(c.pg).ValidateTelemetryRuntime(ctx)
}

const controllerLockID int64 = 764057383691829797

// AcquireControllerLock reserves a dedicated PostgreSQL session for this Controller.
func (c *Connection) AcquireControllerLock(ctx context.Context) (monitor func(context.Context) error, release func(), err error) {
	pooled, err := c.pg.Acquire(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("acquire controller lock connection: %w", err)
	}
	conn := pooled.Hijack()
	release = func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}
	var locked bool
	if err = conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", controllerLockID).Scan(&locked); err != nil {
		release()
		return nil, nil, fmt.Errorf("acquire controller advisory lock: %w", err)
	}
	if !locked {
		release()
		return nil, nil, errors.New("another Controller instance already holds the database advisory lock")
	}
	return func(ctx context.Context) error {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
				probe, cancel := context.WithTimeout(ctx, 2*time.Second)
				var held bool
				err := conn.QueryRow(probe, `SELECT EXISTS (
					SELECT 1 FROM pg_locks WHERE locktype='advisory'
					AND pid=pg_backend_pid() AND granted AND mode='ExclusiveLock'
					AND database=(SELECT oid FROM pg_database WHERE datname=current_database())
					AND classid=($1::bigint >> 32)::oid
					AND objid=($1::bigint & 4294967295)::oid AND objsubid=1
				)`, controllerLockID).Scan(&held)
				cancel()
				if ctx.Err() != nil {
					return nil
				}
				if err != nil {
					return fmt.Errorf("check controller advisory lock: %w", err)
				}
				if !held {
					return errors.New("controller advisory lock ownership was lost")
				}
			}
		}
	}, release, nil
}
