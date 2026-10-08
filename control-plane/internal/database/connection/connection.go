// Package connection owns Controller database selection and owner-only startup.
// Business services receive only the selected Store, never driver handles.
package connection

import (
	"context"
	"errors"
	"net/url"
	"path/filepath"

	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/mysql"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrUnmanagedWorkspaces means workspaces exist but none is the provisioned
// management workspace; the operator must select one explicitly.
var ErrUnmanagedWorkspaces = errors.New("workspaces exist without the administration workspace; select the management workspace explicitly")

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

func postgresURL(options Options) (string, error) {
	u, err := url.Parse(options.URL)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
		return "", errors.New("OCSERV_DATABASE_URL must be a PostgreSQL URL")
	}
	if options.CAFile == "" {
		return options.URL, nil
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

func ValidateOptions(options Options) error {
	switch options.Backend {
	case "", "postgres":
		_, err := postgresURL(options)
		return err
	case "mysql":
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
		databaseURL, err := postgresURL(options)
		if err != nil {
			return nil, err
		}
		pool, err := migrations.Open(ctx, databaseURL)
		if err != nil {
			return nil, err
		}
		return &Connection{Store: postgres.WrapPool(pool), pg: pool}, nil
	case "mysql":
		backend, err := mysql.Open(ctx, mysql.Options{Engine: mysql.Engine(options.Backend), Environment: options.Environment, DSN: options.URL, CAFile: options.CAFile})
		if err != nil {
			return nil, err
		}
		return &Connection{Store: backend, mysql: backend}, nil
	default:
		return nil, errors.New("unsupported Controller database backend")
	}
}

// ValidateDeployment runs connection-level deployment gates before migrations.
// PostgreSQL connections are checked centrally by migrations.Open, for bundled,
// external, owner, runtime and tool connections alike.
func (c *Connection) ValidateDeployment(ctx context.Context) error {
	if c.pg != nil {
		return c.pg.Ping(ctx)
	}
	return nil
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
		return migrations.Migrate(ctx, c.pg)
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

// ProvisionManagementWorkspace returns the "administration" workspace, creating
// it only on a database that has no workspace yet. The slug is unique, so a
// concurrent run fails instead of creating a second workspace.
func (c *Connection) ProvisionManagementWorkspace(ctx context.Context) (id uuid.UUID, created bool, err error) {
	insert := `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'Administration','administration',$2,$2)`
	if c.mysql != nil {
		insert = `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES(?,'Administration','administration',?,?)`
	}
	err = database.Within(ctx, c.Store, database.DefaultIsolation, func(tx database.Tx) error {
		find := `SELECT id FROM workspaces WHERE slug='administration'`
		var err error
		if c.mysql != nil {
			var raw []byte
			if err = tx.QueryRow(ctx, find).Scan(&raw); err == nil {
				id, err = uuid.FromBytes(raw)
				return err
			}
		} else if err = tx.QueryRow(ctx, find).Scan(&id); err == nil {
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
		if c.mysql != nil {
			_, err = tx.Exec(ctx, insert, mysql.UUIDBytes(id), at, at)
		} else {
			_, err = tx.Exec(ctx, insert, id, at)
		}
		return err
	})
	return id, created, err
}

// ValidateRuntime runs before starting any role's listeners or background work.
// A valid schema receipt alone does not prove telemetry history is usable.
func (c *Connection) ValidateRuntime(ctx context.Context) error {
	if err := database.Require(c.Store, database.Transactions, database.RowLocks); err != nil {
		return err
	}
	if c.mysql != nil {
		return c.mysql.ValidateTelemetryRuntime(ctx)
	}
	return postgres.WrapPool(c.pg).ValidateTelemetryRuntime(ctx)
}
