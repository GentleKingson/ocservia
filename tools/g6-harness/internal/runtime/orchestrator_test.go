package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/tools/g6-harness/internal/cleanup"
	"github.com/GentleKingson/ocservia/tools/g6-harness/internal/execx"
	"github.com/GentleKingson/ocservia/tools/g6-harness/internal/phase"
	"github.com/GentleKingson/ocservia/tools/g6-harness/internal/rendezvous"
	resultmodel "github.com/GentleKingson/ocservia/tools/g6-harness/internal/result"
	"github.com/GentleKingson/ocservia/tools/g6-harness/internal/state"
)

func TestRunSegmentEnforcesDurablePhaseAndCheckpointOrder(t *testing.T) {
	t.Parallel()
	options := testOptions(t, "fd-a")
	if err := RecordManifested(options, "tunnel-fd-a"); err == nil {
		t.Fatal("checkpoint was manifested before its producer phase")
	}
	if err := RunSegment(context.Background(), options, "prepare"); err != nil {
		t.Fatal(err)
	}
	if err := RecordManifested(options, "tunnel-fd-a"); err != nil {
		t.Fatal(err)
	}
	if err := RecordConsumed(options, "tunnel-fd-b"); err != nil {
		t.Fatal(err)
	}
	if err := RunSegment(context.Background(), options, "bootstrap"); err != nil {
		t.Fatal(err)
	}
	if err := RunSegment(context.Background(), options, "bootstrap"); err == nil {
		t.Fatal("duplicate segment was accepted")
	}
	rejections, err := filepath.Glob(filepath.Join(options.diagnosticsRoot(), "rejections", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rejections) != 1 {
		t.Fatalf("structured transition rejection count = %d, want 1", len(rejections))
	}
	content, err := os.ReadFile(filepath.Join(options.RunnerTemp, "leaves.log"))
	if err != nil {
		t.Fatal(err)
	}
	want := "prepare\nimport-peer-tunnel-nodes\nbuild-images\ntunnel-up\npublish-shared-secrets\n"
	if string(content) != want {
		t.Fatalf("leaf order = %q, want %q", content, want)
	}
	results, err := filepath.Glob(filepath.Join(options.stateRoot(), "phase-results", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 5 {
		t.Fatalf("phase result count = %d, want 5", len(results))
	}
	other := options
	other.Binding.CandidateSHA = "1123456789abcdef0123456789abcdef01234567"
	if err := RecordConsumed(other, "promotion-complete"); err == nil {
		t.Fatal("cross-candidate state update was accepted")
	}
}

func TestCleanupRegistryRejectionIdentifiesTheRequestedSegmentPhase(t *testing.T) {
	t.Parallel()
	options := testOptions(t, "fd-b")
	if err := ensureRegistry(options); err != nil {
		t.Fatal(err)
	}
	registry, err := cleanup.Load(options.registryPath())
	if err != nil {
		t.Fatal(err)
	}
	registry.Resources[0].ID += "-tampered"
	if err := cleanup.Write(options.registryPath(), registry); err != nil {
		t.Fatal(err)
	}
	if err := RunSegment(context.Background(), options, "window"); err == nil {
		t.Fatal("tampered cleanup registry was accepted")
	}
	rejections, err := filepath.Glob(filepath.Join(options.diagnosticsRoot(), "rejections", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rejections) != 1 {
		t.Fatalf("structured rejection count = %d, want 1", len(rejections))
	}
	content, err := os.ReadFile(rejections[0])
	if err != nil {
		t.Fatal(err)
	}
	var rejection resultmodel.Phase
	if err := json.Unmarshal(content, &rejection); err != nil {
		t.Fatal(err)
	}
	if rejection.Segment != "window" || rejection.Phase != "window" || rejection.Sequence != 230 {
		t.Fatalf("rejection provenance = %s/%s/%d, want window/window/230", rejection.Segment, rejection.Phase, rejection.Sequence)
	}
	if rejection.Failure == nil || rejection.Failure.Code != "cleanup_registry_rejected" {
		t.Fatalf("unexpected rejection failure: %+v", rejection.Failure)
	}
}

func TestCleanupRecoversFromRegistryWithoutOpeningRuntimeState(t *testing.T) {
	t.Parallel()
	options := testOptions(t, "fd-b")
	if err := Cleanup(context.Background(), options, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	registry, err := cleanup.Load(options.registryPath())
	if err != nil {
		t.Fatal(err)
	}
	if registry.CleanupStatus != "passed" || registry.CleanupAt == nil {
		t.Fatalf("unexpected cleanup registry: %+v", registry)
	}
	if err := Cleanup(context.Background(), options, 5*time.Second); err != nil {
		t.Fatal("idempotent completed cleanup failed:", err)
	}
}

func TestFailedCleanupCanBeRetriedFromTheDurableRegistry(t *testing.T) {
	t.Parallel()
	options := testOptions(t, "fd-b")
	adapter := options.adapterPath()
	content := "#!/bin/sh\nset -eu\nif [ \"$1\" = cleanup ] && [ ! -e \"${RUNNER_TEMP}/cleanup-failed-once\" ]; then\n  touch \"${RUNNER_TEMP}/cleanup-failed-once\"\n  exit 23\nfi\n"
	if err := os.WriteFile(adapter, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Cleanup(context.Background(), options, 5*time.Second); err == nil {
		t.Fatal("injected cleanup failure was accepted")
	}
	failed, err := cleanup.LoadAndValidate(options.registryPath(), mustExpectedRegistry(t, options))
	if err != nil {
		t.Fatal(err)
	}
	if failed.CleanupStatus != "failed" || failed.CleanupAt == nil || failed.CleanupError == "" {
		t.Fatalf("failed cleanup was not durable: %+v", failed)
	}
	if err := Cleanup(context.Background(), options, 5*time.Second); err != nil {
		t.Fatal("retry from failed cleanup registry:", err)
	}
	passed, err := cleanup.LoadAndValidate(options.registryPath(), mustExpectedRegistry(t, options))
	if err != nil {
		t.Fatal(err)
	}
	if passed.CleanupStatus != "passed" || passed.CleanupAt == nil || passed.CleanupError != "" {
		t.Fatalf("cleanup retry did not reach a valid terminal state: %+v", passed)
	}
	logs, err := filepath.Glob(filepath.Join(options.diagnosticsRoot(), "cleanup-*.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 4 {
		t.Fatalf("cleanup retry log count = %d, want 4", len(logs))
	}
	results, err := filepath.Glob(filepath.Join(options.diagnosticsRoot(), "cleanup-results", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("cleanup attempt result count = %d, want 2", len(results))
	}
}

func mustExpectedRegistry(t *testing.T, options Options) cleanup.Registry {
	t.Helper()
	registry, err := expectedRegistry(options)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func testOptions(t *testing.T, domain string) Options {
	t.Helper()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	scripts := filepath.Join(workspace, "scripts")
	if err := os.MkdirAll(scripts, 0o700); err != nil {
		t.Fatal(err)
	}
	adapter := filepath.Join(scripts, "g6-readiness-"+domain+".sh")
	content := "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$1\" >>\"${RUNNER_TEMP}/leaves.log\"\n"
	if err := os.WriteFile(adapter, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	domainRunID := "424242-3-" + domain
	environment := append(os.Environ(),
		"RUNNER_TEMP="+root,
		"RUN_ID="+domainRunID,
		"FD_ID="+domain,
		"G6RD_AGENT_IMAGE=agent:1",
		"G6RD_CONTROL_PLANE_IMAGE=control:1",
		"G6RD_TRANSPORTD_IMAGE=transport:1",
		"G6RD_RELAY_IMAGE=relay:1",
		"G6RD_PROBE_IMAGE=probe:1",
	)
	return Options{
		Domain: domain, DomainRunID: domainRunID, RunnerTemp: root, Workspace: workspace,
		Binding: state.Binding{
			CandidateSHA: "0123456789abcdef0123456789abcdef01234567", RunID: "424242", RunAttempt: 3,
			EnvironmentID: "g6-0123456789abcdef", Authority: "engineering",
		},
		Environment: environment,
		Now:         time.Now,
	}
}

func TestAdapterArgumentsAreFixedAndRunScoped(t *testing.T) {
	t.Parallel()
	options := testOptions(t, "fd-b")
	arguments, err := adapterArguments(options, "agents-enroll")
	if err != nil {
		t.Fatal(err)
	}
	if len(arguments) != 2 || arguments[0] != "agents-enroll" || !strings.HasSuffix(arguments[1], "/g6-rd-agents/nodes.tsv") {
		t.Fatalf("unexpected fixed arguments: %v", arguments)
	}
}

func TestCompleteFailureDomainGraphsAreExecutable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		domain string
		steps  []graphStep
	}{
		{domain: "fd-a", steps: []graphStep{
			{segment: "prepare", manifest: "tunnel-fd-a"},
			{consume: "tunnel-fd-b", segment: "bootstrap", manifest: "shared-trust-ready"},
			{segment: "primary", manifest: "primary-ready"},
			{segment: "enroll", manifest: "fd-a-agent-inventory"},
			{consume: "fd-b-agents-enrolled", segment: "transport-trust", manifest: "transport-trust-ready"},
			{segment: "activate-agents"},
			{consume: "production-load-active", segment: "failover-cut", manifest: "primary-isolated"},
			{consume: "promotion-complete", segment: "recovery", manifest: "relay-rejoin-ready"},
			{consume: "relay-pre-fault-observed", segment: "relay-cut", manifest: "fd-a-scenarios-ready"},
			{consume: "window-barrier-arm-request", segment: "barrier-arm", manifest: "window-barrier-armed-fd-a"},
			{segment: "barrier-release"},
			{consume: "final-freeze-request", segment: "evidence"},
		}},
		{domain: "fd-b", steps: []graphStep{
			{segment: "prepare", manifest: "tunnel-fd-b"},
			{consume: "tunnel-fd-a", segment: "bootstrap"},
			{consume: "shared-trust-ready", segment: "peer-runtime"},
			{consume: "primary-ready", segment: "standby"},
			{consume: "fd-a-agent-inventory", segment: "enroll", manifest: "fd-b-agents-enrolled"},
			{consume: "transport-trust-ready", segment: "load", manifest: "production-load-active"},
			{consume: "primary-isolated", segment: "promote", manifest: "promotion-complete"},
			{consume: "relay-rejoin-ready", segment: "relay-observe", manifest: "relay-pre-fault-observed"},
			{consume: "fd-a-scenarios-ready", segment: "fault-scenarios", manifest: "window-barrier-arm-request"},
			{segment: "resource-preflight"},
			{consume: "window-barrier-armed-fd-a", segment: "window"},
			{segment: "evidence", manifest: "final-freeze-request"},
		}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.domain, func(t *testing.T) {
			t.Parallel()
			options := testOptions(t, test.domain)
			for _, step := range test.steps {
				if step.consume != "" {
					if err := RecordConsumed(options, step.consume); err != nil {
						t.Fatalf("consume %s: %v", step.consume, err)
					}
				}
				if err := RunSegment(context.Background(), options, step.segment); err != nil {
					t.Fatalf("run segment %s: %v", step.segment, err)
				}
				if step.manifest != "" {
					if err := RecordManifested(options, step.manifest); err != nil {
						t.Fatalf("manifest %s: %v", step.manifest, err)
					}
				}
			}
		})
	}
}

func TestPhaseGraphAndRendezvousRegistriesCannotDrift(t *testing.T) {
	t.Parallel()
	contracts := rendezvous.Contracts()
	if len(contracts) != 16 {
		t.Fatalf("rendezvous contract count = %d, want 16", len(contracts))
	}
	seen := make(map[string]bool, len(contracts))
	for _, contract := range contracts {
		if seen[contract.Checkpoint] {
			t.Fatalf("duplicate rendezvous checkpoint %s", contract.Checkpoint)
		}
		seen[contract.Checkpoint] = true
		producer, err := phase.RequiredManifestPhase(contract.ProducerDomain, contract.Checkpoint)
		if err != nil {
			t.Fatalf("rendezvous checkpoint %s is absent from the phase graph: %v", contract.Checkpoint, err)
		}
		graph, _ := phase.ResolveGraph(contract.ProducerDomain)
		if _, err := graph.Definition(producer); err != nil {
			t.Fatalf("rendezvous checkpoint %s has invalid producer phase: %v", contract.Checkpoint, err)
		}
	}
}

func TestPhaseFailurePathsMatchTheUploadedDiagnosticsTree(t *testing.T) {
	t.Parallel()
	failure := classifyFailure(
		execx.Outcome{ExitCode: 1, Err: errors.New("exit status 1")},
		phase.Definition{Name: "prepare", Sequence: 10, Timeout: time.Minute},
		time.Now().Add(time.Minute),
		"010-prepare",
	)
	want := []string{
		"harness/runtime/logs/010-prepare.stdout.log",
		"harness/runtime/logs/010-prepare.stderr.log",
	}
	if strings.Join(failure.DiagnosticPaths, "\n") != strings.Join(want, "\n") {
		t.Fatalf("diagnostic paths = %v, want %v", failure.DiagnosticPaths, want)
	}
}

type graphStep struct {
	consume  string
	segment  string
	manifest string
}
