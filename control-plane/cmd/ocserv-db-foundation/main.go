// ocserv-db-foundation is an experimental schema tool, not a Controller entrypoint.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/mysql"
	"github.com/GentleKingson/ocservia/control-plane/internal/platform/config"
)

func run() error {
	mode := flag.String("mode", "check", "check, migrate, repair, grant-test-privileges, or manifest-checksum")
	checksum := flag.String("repair-checksum", "", "reviewed manifest checksum for forward repair")
	flag.Parse()
	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	environment := os.Getenv("OCSERV_ENVIRONMENT")
	if environment != "test" && environment != "development" {
		return fmt.Errorf("database foundation requires test/development; production is not supported")
	}
	engine := mysql.Engine(os.Getenv("OCSERV_DATABASE_BACKEND"))
	sum, err := mysql.ManifestChecksum(engine)
	if err != nil {
		return err
	}
	if *mode == "manifest-checksum" {
		fmt.Println(sum)
		return nil
	}
	if *mode != "check" && *mode != "migrate" && *mode != "repair" && *mode != "grant-test-privileges" {
		return fmt.Errorf("invalid foundation mode")
	}
	if (*mode == "repair") != (*checksum != "") {
		return fmt.Errorf("repair requires --repair-checksum; other modes cannot supply it")
	}
	dsn, err := config.FoundationDatabaseURL(os.LookupEnv)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	b, err := mysql.Open(ctx, mysql.Options{Engine: engine, Environment: environment, DSN: dsn, CAFile: os.Getenv("OCSERV_DATABASE_TLS_CA_FILE")})
	if err != nil {
		return err
	}
	defer b.Close()
	if *mode == "check" {
		return b.ValidateSchema(ctx, 34)
	}
	if *mode == "grant-test-privileges" {
		if err := b.ValidateSchema(ctx, 34); err != nil {
			return err
		}
		return b.GrantTestPrivileges(ctx)
	}
	return b.Migrate(ctx, *checksum)
}

func main() {
	if err := run(); err != nil {
		slog.Error("experimental database foundation rejected", "error", err)
		os.Exit(1)
	}
	slog.Info("experimental schema operation complete; Controller business support is not enabled")
}
