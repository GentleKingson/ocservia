package connection

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestExternalPostgreSQLTLSContract(t *testing.T) {
	base := Options{
		Environment: "production",
		URL:         "postgres://owner:secret@database.example.test:5432/ocservia",
		CAFile:      "/run/secrets/database_ca",
	}
	for _, mode := range []string{"", "disable", "allow", "prefer", "require", "verify-ca"} {
		t.Run("reject "+mode, func(t *testing.T) {
			options := base
			if mode != "" {
				options.URL += "?sslmode=" + mode
			}
			if err := ValidateOptions(options); err == nil {
				t.Fatalf("sslmode %q accepted", mode)
			}
		})
	}

	configured, err := postgresURL(Options{
		URL: base.URL + "?sslmode=verify-full", CAFile: base.CAFile,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(configured, "sslmode=verify-full") || !strings.Contains(configured, "sslrootcert=%2Frun%2Fsecrets%2Fdatabase_ca") {
		t.Fatalf("verified TLS configuration missing: %s", configured)
	}
}

func TestRejectNonPostgreSQLURLs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options Options
	}{
		{"mysql production", Options{Environment: "production", URL: "app:secret@tcp(database.example.test:3306)/ocservia?tls=true"}},
		{"mysql development", Options{Environment: "development", URL: "app:secret@tcp(127.0.0.1:3306)/ocservia?tls=false"}},
		{"mariadb", Options{Environment: "test", URL: "app:secret@tcp(127.0.0.1:3306)/db?tls=false"}},
		{"mysql URL", Options{Environment: "development", URL: "mysql://app:secret@127.0.0.1:3306/ocservia"}},
		{"mysql DSN", Options{Environment: "development", URL: "app:secret@tcp(127.0.0.1:3306)/ocservia?tls=false"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateOptions(tc.options)
			if err == nil {
				t.Fatalf("non-PostgreSQL URL accepted: %+v", tc.options)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("error leaks credentials: %v", err)
			}
		})
	}
}

func TestProductionPostgreSQLPlaintextRequiresDeclaredTarget(t *testing.T) {
	cases := []struct {
		name    string
		options Options
		ok      bool
	}{
		{"remote disable", Options{Environment: "production", URL: "postgres://app:secret@database.example.test:5432/ocservia?sslmode=disable"}, false},
		{"remote prefer", Options{Environment: "production", URL: "postgres://app:secret@database.example.test:5432/ocservia"}, false},
		{"remote require", Options{Environment: "production", URL: "postgres://app:secret@database.example.test:5432/ocservia?sslmode=require"}, false},
		{"undeclared bundled name", Options{Environment: "production", URL: "postgres://app:secret@postgres:5432/ocservia?sslmode=disable"}, false},
		{"declared bundled", Options{Environment: "production", BundledPostgres: true, URL: "postgres://app:secret@postgres:5432/ocservia?sslmode=disable"}, true},
		{"declared bundled remote host", Options{Environment: "production", BundledPostgres: true, URL: "postgres://app:secret@database.example.test:5432/ocservia?sslmode=disable"}, false},
		{"declared bundled host override", Options{Environment: "production", BundledPostgres: true, URL: "postgres://app:secret@postgres:5432/ocservia?sslmode=disable&host=database.example.test"}, false},
		{"declared bundled multi-host", Options{Environment: "production", BundledPostgres: true, URL: "postgres://app:secret@postgres:5432,database.example.test:5432/ocservia?sslmode=disable"}, false},
		{"declared bundled with CA", Options{Environment: "production", BundledPostgres: true, CAFile: "/run/secrets/database_ca", URL: "postgres://app:secret@postgres:5432/ocservia?sslmode=verify-full"}, false},
		{"declared bundled on MySQL", Options{Environment: "production", BundledPostgres: true, URL: "app:secret@tcp(127.0.0.1:3306)/ocservia?tls=false"}, false},
		{"loopback", Options{Environment: "production", URL: "postgres://app:secret@127.0.0.1:5432/ocservia?sslmode=disable"}, true},
		{"development remote", Options{Environment: "development", URL: "postgres://app:secret@database.example.test:5432/ocservia?sslmode=disable"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateOptions(tc.options)
			if (err == nil) != tc.ok {
				t.Fatalf("ValidateOptions() error = %v, want ok=%v", err, tc.ok)
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Fatalf("error leaks credentials: %v", err)
			}
		})
	}
}

func TestControllerLockSessionLoss(t *testing.T) {
	dsn := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	conn, err := Open(ctx, Options{Environment: "test", URL: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	monitor, release, err := conn.AcquireControllerLock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, duplicateRelease, err := conn.AcquireControllerLock(ctx); err == nil {
		duplicateRelease()
		t.Fatal("second Controller acquired the same lock")
	}
	result := make(chan error, 1)
	go func() {
		defer close(result)
		result <- monitor(ctx)
	}()
	defer func() {
		cancel()
		<-result
	}()
	const lockSession = `SELECT pid FROM pg_locks
		WHERE locktype='advisory' AND granted AND objsubid=1
		AND database=(SELECT oid FROM pg_database WHERE datname=current_database())
		AND classid=($1::bigint >> 32)::oid AND objid=($1::bigint & 4294967295)::oid`
	var pid int
	if err := conn.pg.QueryRow(ctx, lockSession, controllerLockID).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	ticks := time.NewTicker(100 * time.Millisecond)
	defer ticks.Stop()
	for {
		var probed bool
		err := conn.pg.QueryRow(ctx, `SELECT state='idle' AND query LIKE '%SELECT EXISTS (%'
			FROM pg_stat_activity WHERE pid=$1`, pid).Scan(&probed)
		if err != nil {
			t.Fatal(err)
		}
		if probed {
			break
		}
		select {
		case err := <-result:
			t.Fatalf("controller monitor stopped before session termination: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticks.C:
		}
	}
	var terminated bool
	if err := conn.pg.QueryRow(ctx, "SELECT pg_terminate_backend($1, 1000)", pid).Scan(&terminated); err != nil || !terminated {
		t.Fatalf("terminate lock session: ok=%v err=%v", terminated, err)
	}
	if err := <-result; err == nil {
		t.Fatal("Controller did not detect lock session loss")
	}
	_, nextRelease, err := conn.AcquireControllerLock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	nextRelease()
}

func TestMonitorControllerLockGraceAndImmediateLoss(t *testing.T) {
	t.Run("recovers after two transient errors then fails on third", func(t *testing.T) {
		outcomes := []struct {
			held bool
			err  error
		}{
			{false, errors.New("blip 1")},
			{false, errors.New("blip 2")},
			{true, nil}, // resets consecutive failure counter
			{false, errors.New("fail 1")},
			{false, errors.New("fail 2")},
			{false, errors.New("fail 3")},
		}
		idx := 0
		err := monitorControllerLock(context.Background(), time.Millisecond, func(context.Context) (bool, error) {
			cur := outcomes[idx]
			idx++
			return cur.held, cur.err
		})
		if err == nil || !strings.Contains(err.Error(), "fail 3") || idx != 6 {
			t.Fatalf("unexpected monitor outcome: idx=%d err=%v", idx, err)
		}
	})

	t.Run("exits immediately on confirmed lock loss", func(t *testing.T) {
		calls := 0
		err := monitorControllerLock(context.Background(), time.Millisecond, func(context.Context) (bool, error) {
			calls++
			return false, nil
		})
		if err == nil || calls != 1 {
			t.Fatalf("expected immediate exit on held=false: calls=%d err=%v", calls, err)
		}
	})
}
