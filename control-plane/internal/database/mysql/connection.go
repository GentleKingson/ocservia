// Package mysql implements the experimental MySQL and MariaDB foundation.
// It does not provide Controller business stores or production support.
package mysql

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	driver "github.com/go-sql-driver/mysql"
)

type Engine string

const (
	MySQL          Engine = "mysql"
	MariaDB        Engine = "mariadb"
	MySQLVersion          = "8.4.10"
	MariaDBVersion        = "12.3.2"
)

// Options deliberately does not accept arbitrary driver/system parameters.
// DSN is a go-sql-driver DSN, not a URL. TLS is mandatory off loopback.
type Options struct {
	Engine      Engine
	Environment string
	DSN         string
	CAFile      string
}

func (o Options) String() string   { return "experimental database options [redacted]" }
func (o Options) GoString() string { return o.String() }

type quietLogger struct{}

func (quietLogger) Print(...any) {}

func configuration(o Options) (*driver.Config, error) {
	if o.Environment != "test" && o.Environment != "development" {
		return nil, errors.New("experimental database: only test/development environments are permitted")
	}
	if o.Engine != MySQL && o.Engine != MariaDB {
		return nil, errors.New("experimental database: select mysql or mariadb explicitly")
	}
	c, err := driver.ParseDSN(o.DSN)
	if err != nil {
		return nil, errors.New("experimental database: invalid DSN")
	}
	if c.Net != "tcp" || c.User == "" || c.Passwd == "" || c.DBName == "" {
		return nil, errors.New("experimental database: DSN requires user, password, tcp host:port and database")
	}
	host, port, err := net.SplitHostPort(c.Addr)
	if err != nil || host == "" || port == "" {
		return nil, errors.New("experimental database: invalid TCP address")
	}
	// Validate the raw query too: ParseDSN recognizes unsafe flags outside Params.
	parameters := o.DSN[strings.LastIndexByte(o.DSN, '/')+1:]
	if index := strings.IndexByte(parameters, '?'); index >= 0 {
		query, err := url.ParseQuery(parameters[index+1:])
		if err != nil {
			return nil, errors.New("experimental database: invalid DSN parameters")
		}
		for key, values := range query {
			if key != "tls" || len(values) != 1 {
				return nil, errors.New("experimental database: only the tls DSN parameter is accepted")
			}
		}
	}
	switch c.TLSConfig {
	case "true":
		c.TLS = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}
		if o.CAFile != "" {
			pem, err := os.ReadFile(o.CAFile)
			if err != nil {
				return nil, errors.New("experimental database: cannot read TLS CA")
			}
			roots := x509.NewCertPool()
			if !roots.AppendCertsFromPEM(pem) {
				return nil, errors.New("experimental database: invalid TLS CA")
			}
			c.TLS.RootCAs = roots
		}
	case "false":
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() || o.CAFile != "" {
			return nil, errors.New("experimental database: plaintext requires a literal loopback address and no CA")
		}
	default:
		return nil, errors.New("experimental database: explicitly set tls=true (or tls=false for loopback tests)")
	}
	c.ParseTime, c.Loc = true, time.UTC
	c.Timeout, c.ReadTimeout, c.WriteTimeout = 5*time.Second, 30*time.Second, 30*time.Second
	c.MultiStatements, c.InterpolateParams, c.AllowAllFiles = false, false, false
	c.AllowCleartextPasswords, c.AllowFallbackToPlaintext = false, false
	c.Logger = quietLogger{}
	c.Params = map[string]string{
		"time_zone":             "'+00:00'",
		"transaction_isolation": "'READ-COMMITTED'",
		"sql_mode":              "'STRICT_ALL_TABLES,NO_ZERO_DATE,NO_ZERO_IN_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION'",
		"character_set_client":  "'utf8mb4'",
		"character_set_results": "'utf8mb4'",
	}
	c.Collation = "utf8mb4_0900_bin"
	if o.Engine == MariaDB {
		c.Collation = "utf8mb4_nopad_bin"
	} else {
		c.Params["sql_mode"] = strings.TrimSuffix(c.Params["sql_mode"], "'") + ",TIME_TRUNCATE_FRACTIONAL'"
	}
	c.Params["collation_connection"] = "'" + c.Collation + "'"
	if err := c.Apply(driver.TimeTruncate(time.Microsecond)); err != nil {
		return nil, errors.New("experimental database: invalid time precision")
	}
	return c, nil
}

func Open(ctx context.Context, o Options) (*Backend, error) {
	c, err := configuration(o)
	if err != nil {
		return nil, err
	}
	_, err = driver.NewConnector(c)
	if err != nil {
		return nil, errors.New("experimental database: invalid connector")
	}
	db := sql.OpenDB(&connector{config: c})
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(time.Hour)
	db.SetConnMaxIdleTime(5 * time.Minute)
	var version string
	if err := db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version); err != nil {
		db.Close()
		return nil, safeError(err)
	}
	valid := o.Engine == MySQL && version == MySQLVersion || o.Engine == MariaDB && strings.HasPrefix(version, MariaDBVersion+"-MariaDB")
	if !valid {
		db.Close()
		return nil, errors.New("experimental database: server flavor/version does not match the pinned backend")
	}
	return &Backend{store: store{db}, pool: db, engine: o.Engine}, nil
}

// Neither server error messages nor DSNs are safe log fields. Preserve neutral
// categories and context cancellation, but never retain driver message text.
func safeError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, sql.ErrNoRows) {
		return database.ErrNotFound
	}
	if errors.Is(err, sql.ErrTxDone) || errors.Is(err, sql.ErrConnDone) {
		return database.ErrTxClosed
	}
	var e *driver.MySQLError
	if errors.As(err, &e) {
		return classifyNumber(e.Number)
	}
	return errors.New("experimental database: operation failed (details redacted)")
}
