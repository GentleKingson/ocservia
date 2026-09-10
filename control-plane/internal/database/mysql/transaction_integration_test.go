package mysql

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	driver "github.com/go-sql-driver/mysql"
)

// Blackhole one finalization packet/response without mocking the SQL driver.
// The response case proves that a failed Commit may already be durable.
type finalizeProxy struct {
	listener                          net.Listener
	stop, accepted, hit, disconnected chan struct{}
	once                              sync.Once
	wg                                sync.WaitGroup
	blocked                           atomic.Bool
	command                           string
	forward                           bool
}

func newFinalizeProxy(t *testing.T, address, command string, forward bool) *finalizeProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &finalizeProxy{listener: listener, stop: make(chan struct{}), accepted: make(chan struct{}), hit: make(chan struct{}), disconnected: make(chan struct{}), command: command, forward: forward}
	go func() {
		defer close(p.accepted)
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			p.wg.Add(1)
			go func() { defer p.wg.Done(); p.serve(client, address) }()
		}
	}()
	t.Cleanup(p.close)
	return p
}

func (p *finalizeProxy) close() {
	p.once.Do(func() { close(p.stop); _ = p.listener.Close() })
	<-p.accepted
	p.wg.Wait()
}

func readPacket(conn net.Conn) ([]byte, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return nil, err
	}
	size := int(header[0]) | int(header[1])<<8 | int(header[2])<<16
	packet := make([]byte, 4+size)
	copy(packet, header)
	_, err := io.ReadFull(conn, packet[4:])
	return packet, err
}

func (p *finalizeProxy) serve(client net.Conn, address string) {
	defer client.Close()
	server, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		return
	}
	finished := make(chan struct{})
	go func() {
		select {
		case <-p.stop:
			_ = client.Close()
			_ = server.Close()
		case <-finished:
		}
	}()
	var blackhole atomic.Bool
	responseDone := make(chan struct{})
	go func() {
		defer close(responseDone)
		for {
			packet, err := readPacket(server)
			if err != nil {
				return
			}
			if blackhole.Load() {
				close(p.hit)
				return
			}
			if _, err = client.Write(packet); err != nil {
				return
			}
		}
	}()
	defer func() {
		_ = client.Close()
		_ = server.Close()
		<-responseDone
		close(finished)
		if blackhole.Load() {
			close(p.disconnected)
		}
	}()
	for {
		packet, err := readPacket(client)
		if err != nil {
			return
		}
		if len(packet) > 4 && packet[4] == 3 && strings.EqualFold(string(packet[5:]), p.command) && p.blocked.CompareAndSwap(false, true) {
			blackhole.Store(true)
			if !p.forward {
				close(p.hit)
				continue
			}
		}
		if _, err = server.Write(packet); err != nil {
			return
		}
	}
}

func TestRealBoundedTransactionFinalization(t *testing.T) {
	for _, test := range []struct {
		name, command string
		forward       bool
		wantCount     int
	}{
		{"rollback-request-blackholed", "ROLLBACK", false, 0},
		{"commit-request-blackholed", "COMMIT", false, 0},
		{"commit-response-lost", "COMMIT", true, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			b, _, options := fixture(t)
			ctx := context.Background()
			if _, err := b.Exec(ctx, "CREATE TABLE tx_probe(id INT PRIMARY KEY) ENGINE=InnoDB"); err != nil {
				t.Fatal(err)
			}
			config, _ := driver.ParseDSN(options.DSN)
			proxy := newFinalizeProxy(t, config.Addr, test.command, test.forward)
			config.Addr = proxy.listener.Addr().String()
			options.DSN = config.FormatDSN()
			backend, err := Open(ctx, options)
			if err != nil {
				t.Fatal(err)
			}
			defer backend.Close()
			backend.pool.SetMaxOpenConns(1)
			backend.pool.SetMaxIdleConns(1)
			tx, err := backend.Begin(ctx, database.ReadCommitted)
			if err != nil {
				t.Fatal(err)
			}
			var oldID int
			if err = tx.QueryRow(ctx, "SELECT CONNECTION_ID()").Scan(&oldID); err != nil {
				t.Fatal(err)
			}
			if _, err = tx.Exec(ctx, "INSERT INTO tx_probe VALUES(1)"); err != nil {
				t.Fatal(err)
			}
			cleanup, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
			defer cancel()
			result := make(chan error, 1)
			go func() {
				if test.command == "COMMIT" {
					result <- tx.Commit(cleanup)
				} else {
					result <- tx.Rollback(cleanup)
				}
			}()
			select {
			case <-proxy.hit:
			case <-time.After(2 * time.Second):
				proxy.close()
				<-result
				t.Fatal("finalization did not reach the proxy")
			}
			select {
			case err = <-result:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("lost cleanup deadline: %v", err)
				}
			case <-time.After(time.Second):
				proxy.close()
				<-result
				t.Fatal("finalization ignored its context (driver I/O timeout is 30s)")
			}
			select {
			case <-proxy.disconnected:
			case <-time.After(time.Second):
				t.Fatal("timed-out physical connection was not closed")
			}
			if err = tx.Rollback(ctx); !errors.Is(err, database.ErrTxClosed) {
				t.Fatal("finalized transaction remained usable")
			}
			var newID, count int
			if err = backend.QueryRow(ctx, "SELECT CONNECTION_ID()").Scan(&newID); err != nil {
				t.Fatal(err)
			}
			if oldID == newID {
				t.Fatal("uncertain session returned to the pool")
			}
			if err = b.QueryRow(ctx, "SELECT count(*) FROM tx_probe").Scan(&count); err != nil || count != test.wantCount {
				t.Fatalf("unexpected durable state: count=%d error=%v", count, err)
			}
		})
	}
}

func TestRealIndependentTransactionCleanup(t *testing.T) {
	b, _, _ := fixture(t)
	ctx := context.Background()
	b.pool.SetMaxOpenConns(1)
	b.pool.SetMaxIdleConns(1)
	if _, err := b.Exec(ctx, "CREATE TABLE tx_probe(id INT PRIMARY KEY) ENGINE=InnoDB"); err != nil {
		t.Fatal(err)
	}
	request, cancelRequest := context.WithCancel(ctx)
	tx, err := b.Begin(request, database.ReadCommitted)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(request, "INSERT INTO tx_probe VALUES(1)"); err != nil {
		t.Fatal(err)
	}
	var originalID int
	if err = tx.QueryRow(ctx, "SELECT CONNECTION_ID()").Scan(&originalID); err != nil {
		t.Fatal(err)
	}
	cancelRequest()
	cleanup, cancelCleanup := context.WithTimeout(ctx, 5*time.Second)
	if err = tx.Rollback(cleanup); err != nil {
		t.Fatalf("independent cleanup failed: %v", err)
	}
	cancelCleanup()
	// A completed finalizer's watchdog must never close a reused connection.
	var firstID, secondID, count int
	if err = b.QueryRow(ctx, "SELECT CONNECTION_ID(),(SELECT count(*) FROM tx_probe)").Scan(&firstID, &count); err != nil || count != 0 || firstID != originalID {
		t.Fatal("rollback failed", err)
	}
	for _, level := range []database.Isolation{database.DefaultIsolation, database.ReadCommitted, database.RepeatableRead, database.Serializable} {
		if err = database.Within(ctx, b, level, func(tx database.Tx) error {
			_, err := tx.Exec(ctx, "INSERT INTO tx_probe VALUES(?)", int(level)+1)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err = b.QueryRow(ctx, "SELECT CONNECTION_ID()").Scan(&secondID); err != nil || firstID != secondID {
		t.Fatal("healthy finalization discarded its connection", err)
	}
	marker := errors.New("callback rejected")
	err = database.Within(ctx, b, database.ReadCommitted, func(tx database.Tx) error {
		if _, err := tx.Exec(ctx, "INSERT INTO tx_probe VALUES(99)"); err != nil {
			return err
		}
		return marker
	})
	if !errors.Is(err, marker) {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if recover() != marker {
				t.Error("panic not preserved")
			}
		}()
		_ = database.Within(ctx, b, database.ReadCommitted, func(tx database.Tx) error {
			if _, err := tx.Exec(ctx, "INSERT INTO tx_probe VALUES(99)"); err != nil {
				t.Fatal(err)
			}
			panic(marker)
		})
	}()
	if err = b.QueryRow(ctx, "SELECT count(*) FROM tx_probe WHERE id=99").Scan(&count); err != nil || count != 0 {
		t.Fatal("Within cleanup did not roll back", err)
	}
	for _, commit := range []bool{false, true} {
		tx, err = b.Begin(ctx, database.ReadCommitted)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO tx_probe VALUES(99)"); err != nil {
			t.Fatal(err)
		}
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if commit {
			err = tx.Commit(cancelled)
		} else {
			err = tx.Rollback(cancelled)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatal("pre-cancelled finalizer lost cancellation", err)
		}
		if err = b.QueryRow(ctx, "SELECT count(*) FROM tx_probe WHERE id=99").Scan(&count); err != nil || count != 0 {
			t.Fatal("pre-cancelled finalizer committed or leaked its transaction", err)
		}
	}
}
