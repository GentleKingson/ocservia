package mysql

import (
	"context"
	"database/sql/driver"
	"net"
	"sync/atomic"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	mysqldriver "github.com/go-sql-driver/mysql"
)

// Keep all optional interfaces implemented by the pinned driver. Embedding
// driver.Conn alone would silently remove context, liveness and value handling.
type nativeConnection interface {
	driver.Conn
	driver.ConnBeginTx
	driver.ConnPrepareContext
	driver.ExecerContext
	driver.QueryerContext
	driver.NamedValueChecker
	driver.Pinger
	driver.SessionResetter
	driver.Validator
}

type connector struct{ config *mysqldriver.Config }

func (c *connector) Driver() driver.Driver { return &mysqldriver.MySQLDriver{} }

func (c *connector) Connect(ctx context.Context) (driver.Conn, error) {
	config := c.config.Clone()
	var wire net.Conn
	config.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		var err error
		wire, err = (&net.Dialer{Timeout: config.Timeout}).DialContext(ctx, network, address)
		return wire, err
	}
	native, err := mysqldriver.NewConnector(config)
	if err != nil {
		return nil, safeError(err)
	}
	conn, err := native.Connect(ctx)
	if err != nil {
		if wire != nil {
			_ = wire.Close()
		}
		return nil, err
	}
	physical, ok := conn.(nativeConnection)
	if !ok || wire == nil {
		if wire != nil {
			_ = wire.Close()
		}
		_ = conn.Close()
		return nil, database.ErrUnsupported
	}
	return &physicalConnection{nativeConnection: physical, wire: wire}, nil
}

type physicalConnection struct {
	nativeConnection
	wire    net.Conn
	aborted atomic.Bool
}

// net.Conn.Close is safe alongside driver I/O and interrupts both TLS and
// plaintext operations without taking database/sql's driver-connection mutex.
func (c *physicalConnection) abort() {
	if !c.aborted.Swap(true) {
		_ = c.wire.Close()
	}
}

func (c *physicalConnection) IsValid() bool {
	return !c.aborted.Load() && c.nativeConnection.IsValid()
}

var _ driver.Connector = (*connector)(nil)
var _ nativeConnection = (*physicalConnection)(nil)
