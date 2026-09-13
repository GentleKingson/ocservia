package mysql

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Authoring is opt-in and executes every candidate statement on both genuine
// historical roots. It never synthesizes receipts or changes embedded history.
func TestAuthorRevisionThree(t *testing.T) {
	engine := testOptions(t).Engine
	telemetry, err := TelemetryMigrationSteps(engine)
	if err != nil {
		t.Fatal(err)
	}
	inputs := append(TypeMigrationSteps(engine), telemetry...)
	inputs = append(inputs, LongKeyMigrationSteps()...)
	authorRevision(t, 3, guardedRevisionSteps(inputs))
}

func TestAuthorRevisionFour(t *testing.T) {
	inputs := TimeDecisionSteps()
	inputs = append(inputs, AuthenticationTimeSteps()...)
	inputs = append(inputs, SchedulerTimeSteps()...)
	inputs = append(inputs, AuditMigrationSteps()...)
	inputs = append(inputs, RBACMigrationSteps(testOptions(t).Engine)...)
	inputs = append(inputs, TelemetryLegacyMigrationSteps()...)
	authorRevision(t, 4, inputs)
}

func TestAuthorRevisionFive(t *testing.T) {
	engine := testOptions(t).Engine
	inputs := AuthenticationRemainingTimeSteps(engine)
	inputs = append(inputs, ArtifactTimeSteps(engine)...)
	inputs = append(inputs, ApprovalRemainingSteps(engine)...)
	inputs = append(inputs, TelemetryWriteMigrationSteps(engine)...)
	authorRevision(t, 5, inputs)
}

func TestAuthorRevisionSix(t *testing.T) {
	authorRevision(t, 6, AttestationTimeSteps(testOptions(t).Engine))
}

func TestAuthorRevisionSeven(t *testing.T) {
	authorRevision(t, 7, CertificateTypeSteps(testOptions(t).Engine))
}

func TestAuthorRevisionEight(t *testing.T) {
	authorRevision(t, 8, SecretReferenceTimeSteps())
}

func TestAuthorRevisionNine(t *testing.T) {
	authorRevision(t, 9, OperationReadTimeSteps())
}

func TestAuthorRevisionTen(t *testing.T) {
	authorRevision(t, 10, CommandTimeSteps(testOptions(t).Engine))
}

func TestAuthorRevisionEleven(t *testing.T) {
	authorRevision(t, 11, ConfigurationTypeSteps(testOptions(t).Engine))
}

func TestAuthorRevisionTwelve(t *testing.T) {
	authorRevision(t, 12, UpgradeTimeSteps())
}

func TestAuthorRevisionThirteen(t *testing.T) {
	authorRevision(t, 13, RolloutTypeSteps(testOptions(t).Engine))
}

func TestAuthorRevisionFourteen(t *testing.T) {
	authorRevision(t, 14, DispatchTimeSteps(testOptions(t).Engine))
}

func TestAuthorRevisionFifteen(t *testing.T) {
	authorRevision(t, 15, ConnectionOwnerTimeSteps())
}

func TestAuthorRevisionSixteen(t *testing.T) {
	authorRevision(t, 16, ResultTimeSteps(testOptions(t).Engine))
}

func TestAuthorRevisionSeventeen(t *testing.T) {
	authorRevision(t, 17, TransportTimeSteps())
}

func TestAuthorRevisionEighteen(t *testing.T) {
	authorRevision(t, 18, TrustConvergenceTimeSteps(testOptions(t).Engine))
}

func TestAuthorRevisionNineteen(t *testing.T) {
	authorRevision(t, 19, EnrollmentTokenTimeSteps(testOptions(t).Engine))
}

func TestAuthorRevisionTwenty(t *testing.T) {
	authorRevision(t, 20, DesiredStateTypeSteps(testOptions(t).Engine))
}

func TestAuthorRevisionTwentyOne(t *testing.T) {
	authorRevision(t, 21, UserOperationsTimeSteps(testOptions(t).Engine))
}

func TestAuthorRevisionTwentyTwo(t *testing.T) {
	authorRevision(t, 22, UsageTimeSteps())
}

func TestAuthorRevisionTwentyThree(t *testing.T) {
	authorRevision(t, 23, SharedStorageSteps(testOptions(t).Engine))
}

func TestAuthorRevisionTwentyFour(t *testing.T) {
	authorRevision(t, 24, TelemetryRetentionSteps())
}

func authorRevision(t *testing.T, version int, inputs []LongKeyStep) {
	output := os.Getenv("PR02_AUTHOR_DIRECTORY")
	if output == "" {
		t.Skip("explicit manifest authoring only")
	}
	engine := testOptions(t).Engine
	chain, err := loadRevisionChain(engine)
	if err != nil {
		t.Fatal(err)
	}
	chain = chain[:version-2]
	previous := chain[len(chain)-1]
	next := revision{Version: version, PreviousChecksum: previous.sum, Engine: engine, ControllerSchema: 34, MinimumControllerSchema: 34, Parents: map[string]revisionPlan{}, MetadataHashes: previous.MetadataHashes}
	if version == 24 {
		next.ControllerSchema, next.MinimumControllerSchema = 35, 35
	}
	for _, old := range []bool{true, false} {
		b, _ := historicalFixture(t, old)
		ctx := context.Background()
		conn, name, err := migrationConnection(ctx, b)
		if err != nil {
			t.Fatal(err)
		}
		defer releaseMigrationConnection(conn, name)
		if err = b.migrateChainOn(ctx, conn, chain, ""); err != nil {
			t.Fatal(err)
		}
		root, parent, err := b.rootOn(ctx, conn)
		if err != nil {
			t.Fatal(err)
		}
		snapshot := root
		for _, artifact := range chain {
			snapshot = revisedSnapshot(snapshot, artifact.Parents[parent])
		}
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
	f, err := os.OpenFile(filepath.Join(output, string(engine), fmt.Sprintf("%06d.json", version)), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.Write(append(data, '\n'))
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatal(writeErr, closeErr)
	}
}
