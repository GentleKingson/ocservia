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
	mode := flag.String("mode", "check", "check, migrate, repair, grant-test-privileges, manifest-checksum, snapshot-checksum, schema-artifact-checksum, upgrade-artifact-checksum, telemetry-provision, telemetry-migrate-history, or telemetry-collect")
	checksum := flag.String("repair-checksum", "", "reviewed revision or snapshot checksum for forward repair")
	revision := flag.Int64("revision", -1, "upgrade-artifact-checksum revision; 0 is the previous-checkpoint transition")
	month := flag.String("month", "", "UTC month YYYY-MM for owner-only telemetry provisioning")
	flag.Parse()
	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	if *revision < -1 || (*revision != -1 && *mode != "upgrade-artifact-checksum") {
		return fmt.Errorf("--revision requires upgrade-artifact-checksum and a nonnegative revision")
	}
	environment := os.Getenv("OCSERV_ENVIRONMENT")
	if environment != "test" && environment != "development" {
		return fmt.Errorf("database foundation requires test/development; production is not supported")
	}
	engine := mysql.Engine(os.Getenv("OCSERV_DATABASE_BACKEND"))
	var err error
	if *mode == "schema-artifact-checksum" || *mode == "upgrade-artifact-checksum" {
		kind := "schema"
		if *mode == "upgrade-artifact-checksum" {
			kind = "upgrade"
		}
		var selected []int64
		if *revision != -1 {
			selected = append(selected, *revision)
		}
		sum, err := mysql.ArtifactChecksum(engine, kind, selected...)
		if err != nil {
			return err
		}
		fmt.Println(sum)
		return nil
	}
	if *mode == "snapshot-checksum" {
		sum, err := mysql.SnapshotChecksum(engine)
		if err != nil {
			return err
		}
		fmt.Println(sum)
		return nil
	}
	if *mode == "manifest-checksum" {
		sum, err := mysql.ManifestChecksum(engine)
		if err != nil {
			return err
		}
		fmt.Println(sum)
		return nil
	}
	if *mode != "check" && *mode != "migrate" && *mode != "repair" && *mode != "grant-test-privileges" && *mode != "telemetry-provision" && *mode != "telemetry-migrate-history" && *mode != "telemetry-collect" {
		return fmt.Errorf("invalid foundation mode")
	}
	if (*mode == "repair") != (*checksum != "") {
		return fmt.Errorf("repair requires --repair-checksum; other modes cannot supply it")
	}
	if (*mode == "telemetry-provision") != (*month != "") {
		return fmt.Errorf("telemetry-provision requires --month; other modes cannot supply it")
	}
	var telemetryMonth time.Time
	if *month != "" {
		telemetryMonth, err = time.Parse("2006-01", *month)
		if err != nil {
			return fmt.Errorf("invalid UTC telemetry month")
		}
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
		return b.ValidateSchema(ctx)
	}
	if *mode == "telemetry-provision" {
		if err := b.ProvisionTelemetryMonth(ctx, telemetryMonth); err != nil {
			return err
		}
		return b.ValidateSchema(ctx)
	}
	if *mode == "telemetry-migrate-history" {
		if err := b.MigrateTelemetryHistory(ctx); err != nil {
			return err
		}
		if err := b.ValidateSchema(ctx); err != nil {
			return err
		}
		return b.ValidateTelemetryHistoryReady(ctx)
	}
	if *mode == "telemetry-collect" {
		if err := b.ValidateSchema(ctx); err != nil {
			return err
		}
		return b.CollectRetiredTelemetryShards(ctx)
	}
	if *mode == "grant-test-privileges" {
		if err := b.ValidateSchema(ctx); err != nil {
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
