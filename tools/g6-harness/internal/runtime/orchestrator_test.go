package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/tools/g6-harness/internal/cleanup"
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

type graphStep struct {
	consume  string
	segment  string
	manifest string
}
