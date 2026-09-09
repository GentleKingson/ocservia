package mysql

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Authoring is opt-in and executes every candidate statement on both genuine
// historical roots. It never synthesizes receipts or changes embedded history.
func TestAuthorRevisionThree(t *testing.T) {
	output := os.Getenv("PR02_AUTHOR_DIRECTORY")
	if output == "" {
		t.Skip("explicit manifest authoring only")
	}
	engine := testOptions(t).Engine
	previous, checksum, err := loadRevision(engine)
	if err != nil {
		t.Fatal(err)
	}
	next := revision{Version: 3, PreviousChecksum: checksum, Engine: engine, ControllerSchema: 34, MinimumControllerSchema: 34, Parents: map[string]revisionPlan{}, MetadataHashes: previous.MetadataHashes}
	telemetry, err := TelemetryMigrationSteps(engine)
	if err != nil {
		t.Fatal(err)
	}
	inputs := append(TypeMigrationSteps(engine), telemetry...)
	inputs = append(inputs, LongKeyMigrationSteps()...)
	inputs = guardedRevisionSteps(inputs)
	for _, old := range []bool{true, false} {
		b, _ := historicalFixture(t, old)
		ctx := context.Background()
		conn, name, err := migrationConnection(ctx, b)
		if err != nil {
			t.Fatal(err)
		}
		defer releaseMigrationConnection(conn, name)
		chain := []revisionArtifact{{previous, checksum}}
		if err = b.migrateChainOn(ctx, conn, chain, ""); err != nil {
			t.Fatal(err)
		}
		root, parent, err := b.rootOn(ctx, conn)
		if err != nil {
			t.Fatal(err)
		}
		snapshot := revisedSnapshot(root, previous.Parents[parent])
		plan := revisionPlan{Steps: []revisionStep{}}
		for _, input := range inputs {
			s := revisionStep{Name: input.Name, Object: input.Object, Kind: input.Kind, SQL: input.SQL, Checksum: digest([]byte(input.SQL)), VerifySQL: input.VerifySQL, CheckBeforeSQL: input.CheckBeforeSQL, Repairable: input.Repairable}
			if s.Kind != "data" {
				s.Before, err = revisionPostcondition(ctx, conn, s)
				if err != nil {
					t.Fatalf("%s before: %v", s.Name, err)
				}
			}
			if err = checkRevisionBefore(ctx, conn, s); err != nil {
				t.Fatalf("%s precondition: %v", s.Name, err)
			}
			if _, err = conn.ExecContext(ctx, s.SQL); err != nil {
				t.Fatalf("%s execution: %v", s.Name, err)
			}
			s.After, err = revisionPostcondition(ctx, conn, s)
			if err != nil {
				t.Fatalf("%s after: %v", s.Name, err)
			}
			if s.Kind == "data" && s.After != digest([]byte("valid")) {
				t.Fatalf("%s data verification failed", s.Name)
			}
			plan.Steps = append(plan.Steps, s)
			if _, err := validateRevisionPlan(snapshot, plan); err != nil {
				t.Fatalf("invalid authored step %s: %v", s.Name, err)
			}
		}
		target, err := validateRevisionPlan(snapshot, plan)
		if err != nil {
			t.Fatal(err)
		}
		if err = validateRevisionSnapshot(ctx, conn, target); err != nil {
			t.Fatal(err)
		}
		next.Parents[parent] = plan
	}
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(output, string(engine)), 0755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(output, string(engine), "000003.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.Write(append(data, '\n'))
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatal(writeErr, closeErr)
	}
}
