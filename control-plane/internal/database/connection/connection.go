// Package connection owns Controller database selection and owner-only startup.
// Business services receive only the selected Store, never driver handles.
package connection

import (
	"context"
	"errors"
	"net/url"

	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/mysql"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Options struct {
	Backend, Environment, URL, CAFile string
}

type Store interface {
	database.Backend
	database.Diagnostics
}

type Connection struct {
	Store Store
	pg    *pgxpool.Pool
	mysql *mysql.Backend
}

func ValidateOptions(options Options) error {
	switch options.Backend {
	case "", "postgres":
		u, err := url.Parse(options.URL)
		if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
			return errors.New("OCSERV_DATABASE_URL must be a PostgreSQL URL")
		}
		if options.CAFile != "" {
			return errors.New("PostgreSQL TLS roots must be configured in OCSERV_DATABASE_URL")
		}
		return nil
	case "mysql", "mariadb":
		return mysql.ValidateOptions(mysql.Options{Engine: mysql.Engine(options.Backend), Environment: options.Environment, DSN: options.URL, CAFile: options.CAFile})
	default:
		return errors.New("unsupported Controller database backend")
	}
}

func Open(ctx context.Context, options Options) (*Connection, error) {
	if err := ValidateOptions(options); err != nil {
		return nil, err
	}
	switch options.Backend {
	case "", "postgres":
		pool, err := migrations.Open(ctx, options.URL)
		if err != nil {
			return nil, err
		}
		return &Connection{Store: postgres.WrapPool(pool), pg: pool}, nil
	case "mysql", "mariadb":
		backend, err := mysql.Open(ctx, mysql.Options{Engine: mysql.Engine(options.Backend), Environment: options.Environment, DSN: options.URL, CAFile: options.CAFile})
		if err != nil {
			return nil, err
		}
		return &Connection{Store: backend, mysql: backend}, nil
	default:
		return nil, errors.New("unsupported Controller database backend")
	}
}

func (c *Connection) Close() {
	if c.pg != nil {
		c.pg.Close()
	} else {
		_ = c.mysql.Close()
	}
}

func (c *Connection) Migrate(ctx context.Context, manager *audit.Manager) error {
	if c.pg != nil {
		return migrations.Migrate(ctx, c.pg, manager.PreflightAuthenticityMigration)
	}
	// MySQL's immutable chain includes its own guarded audit-copy validation.
	// Normal startup never repairs an interrupted revision implicitly.
	if err := c.mysql.Migrate(ctx, ""); err != nil {
		return err
	}
	return c.mysql.PrepareControllerTelemetry(ctx)
}

func (c *Connection) GrantRuntimePrivileges(ctx context.Context, account string) error {
	if c.pg != nil {
		return migrations.GrantRuntimePrivileges(ctx, c.pg, account)
	}
	return c.mysql.GrantRuntimePrivileges(ctx, account)
}
