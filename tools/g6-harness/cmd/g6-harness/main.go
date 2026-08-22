package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/GentleKingson/ocservia/tools/g6-harness/internal/rendezvous"
	"github.com/GentleKingson/ocservia/tools/g6-harness/internal/runtime"
	"github.com/GentleKingson/ocservia/tools/g6-harness/internal/smoke"
	"github.com/GentleKingson/ocservia/tools/g6-harness/internal/state"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("usage: g6-harness <checkpoint-manifest|wait-download|run-segment|cleanup|smoke-domain|smoke-aggregate> [options]")
	}
	switch arguments[0] {
	case "checkpoint-manifest":
		return runManifest(arguments[1:])
	case "wait-download":
		return runWait(arguments[1:])
	case "run-segment":
		return runSegment(arguments[1:])
	case "cleanup":
		return runCleanup(arguments[1:])
	case "smoke-domain":
		return runSmokeDomain(arguments[1:])
	case "smoke-aggregate":
		return runSmokeAggregate(arguments[1:])
	default:
		return fmt.Errorf("unknown g6-harness command %q", arguments[0])
	}
}

func runSmokeDomain(arguments []string) error {
	flags := flag.NewFlagSet("smoke-domain", flag.ContinueOnError)
	domain := flags.String("domain", "", "smoke failure domain")
	expectedSHA := flags.String("expected-harness-sha", "", "frozen harness SHA-256")
	output := flags.String("output", "", "absolute domain result path")
	bootID := flags.String("boot-id", "/proc/sys/kernel/random/boot_id", "absolute runner boot ID path")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *domain == "" || *expectedSHA == "" || *output == "" {
		return errors.New("smoke-domain requires --domain, --expected-harness-sha, and --output")
	}
	binding, err := rendezvous.BindingFromEnvironment()
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	result, smokeErr := smoke.RunDomain(smoke.DomainOptions{
		Binding: binding, Domain: *domain, RunnerName: os.Getenv("G6_SMOKE_RUNNER_NAME"), BootIDPath: *bootID,
		ExecutablePath: executable, ExpectedSHA256: *expectedSHA, Now: time.Now,
	})
	return errors.Join(smokeErr, smoke.Write(*output, result))
}

func runSmokeAggregate(arguments []string) error {
	flags := flag.NewFlagSet("smoke-aggregate", flag.ContinueOnError)
	fdA := flags.String("fd-a", "", "absolute FD-A domain result path")
	fdB := flags.String("fd-b", "", "absolute FD-B domain result path")
	expectedSHA := flags.String("expected-harness-sha", "", "frozen harness SHA-256")
	output := flags.String("output", "", "absolute aggregate result path")
	releaseID := flags.String("release-artifact-id", "", "frozen harness artifact ID")
	releaseDigest := flags.String("release-artifact-digest", "", "frozen harness artifact digest")
	fdAID := flags.String("fd-a-artifact-id", "", "FD-A result artifact ID")
	fdADigest := flags.String("fd-a-artifact-digest", "", "FD-A result artifact digest")
	fdBID := flags.String("fd-b-artifact-id", "", "FD-B result artifact ID")
	fdBDigest := flags.String("fd-b-artifact-digest", "", "FD-B result artifact digest")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *fdA == "" || *fdB == "" || *expectedSHA == "" || *output == "" {
		return errors.New("smoke-aggregate requires both domain results, the harness digest, and output")
	}
	binding, err := rendezvous.BindingFromEnvironment()
	if err != nil {
		return err
	}
	release, err := smoke.ParseArtifactReference(*releaseID, *releaseDigest)
	parseErr := err
	artifactA, err := smoke.ParseArtifactReference(*fdAID, *fdADigest)
	parseErr = errors.Join(parseErr, err)
	artifactB, err := smoke.ParseArtifactReference(*fdBID, *fdBDigest)
	parseErr = errors.Join(parseErr, err)
	result, aggregateErr := smoke.Aggregate(smoke.AggregateOptions{
		Binding: binding, FDAPath: *fdA, FDBPath: *fdB, ReleaseArtifact: release,
		FDAArtifact: artifactA, FDBArtifact: artifactB, ExpectedHarnessSHA: *expectedSHA, Now: time.Now,
	})
	return errors.Join(parseErr, aggregateErr, smoke.Write(*output, result))
}

func runManifest(arguments []string) error {
	flags := flag.NewFlagSet("checkpoint-manifest", flag.ContinueOnError)
	name := flags.String("name", "", "run-scoped artifact name")
	root := flags.String("root", "", "checkpoint payload directory")
	ttl := flags.Duration("ttl", 2*time.Hour, "checkpoint lifetime")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *name == "" || *root == "" {
		return errors.New("checkpoint-manifest requires --name and --root")
	}
	binding, err := rendezvous.BindingFromEnvironment()
	if err != nil {
		return err
	}
	producer := os.Getenv("FD_ID")
	if producer == "" {
		return errors.New("FD_ID is required to produce a checkpoint")
	}
	manifest, err := rendezvous.CreateManifest(*root, *name, producer, binding, time.Now(), *ttl)
	if err != nil {
		return err
	}
	options, err := runtimeOptions(binding)
	if err != nil {
		return err
	}
	return runtime.RecordManifested(options, manifest.Checkpoint)
}

func runWait(arguments []string) error {
	flags := flag.NewFlagSet("wait-download", flag.ContinueOnError)
	name := flags.String("name", "", "run-scoped artifact name")
	destination := flags.String("destination", "", "empty absolute destination")
	peerJob := flags.String("peer-job", "", "exact peer job display name")
	timeout := flags.Duration("timeout", 10*time.Minute, "aggregate wait timeout")
	resultPath := flags.String("result", "", "structured result path")
	statePath := flags.String("state", "", "durable consumer state path")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *name == "" || *destination == "" || *peerJob == "" {
		return errors.New("wait-download requires --name, --destination, and --peer-job")
	}
	binding, err := rendezvous.BindingFromEnvironment()
	if err != nil {
		return err
	}
	if *resultPath == "" || *statePath == "" {
		defaults, defaultErr := defaultPaths(*name)
		if defaultErr != nil {
			return defaultErr
		}
		if *resultPath == "" {
			*resultPath = defaults.result
		}
		if *statePath == "" {
			*statePath = defaults.state
		}
	}
	options := rendezvous.Options{
		BaseURL:              os.Getenv("GITHUB_API_URL"),
		Repository:           os.Getenv("GITHUB_REPOSITORY"),
		Token:                os.Getenv("GITHUB_TOKEN"),
		Binding:              binding,
		ArtifactName:         *name,
		Destination:          *destination,
		PeerJob:              *peerJob,
		StatePath:            *statePath,
		Timeout:              *timeout,
		PollInterval:         durationEnvironment("G6_RENDEZVOUS_POLL_INTERVAL", 5*time.Second),
		PropagationGrace:     durationEnvironment("G6_RENDEZVOUS_PROPAGATION_GRACE", 30*time.Second),
		RequestTimeout:       durationEnvironment("G6_RENDEZVOUS_REQUEST_TIMEOUT", 20*time.Second),
		DownloadRetryTotal:   durationEnvironment("G6_RENDEZVOUS_DOWNLOAD_RETRY_TOTAL", 90*time.Second),
		MaxConsecutiveErrors: integerEnvironment("G6_RENDEZVOUS_MAX_CONSECUTIVE_ERRORS", 3),
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	result, waitErr := rendezvous.WaitDownload(ctx, options)
	if waitErr == nil {
		runtimeConfig, runtimeErr := runtimeOptions(binding)
		if runtimeErr == nil {
			runtimeErr = runtime.RecordConsumed(runtimeConfig, result.Checkpoint)
		}
		if runtimeErr != nil {
			result.Status = "failed"
			result.Artifact = nil
			result.ManifestSHA256 = ""
			result.Failure = &rendezvous.Failure{Class: "harness_contract_failed", Code: "runtime_checkpoint_rejected", Message: runtimeErr.Error()}
			waitErr = runtimeErr
		}
	}
	if writeErr := writeResult(*resultPath, result); writeErr != nil {
		return errors.Join(waitErr, fmt.Errorf("write structured rendezvous result: %w", writeErr))
	}
	return waitErr
}

func runSegment(arguments []string) error {
	flags := flag.NewFlagSet("run-segment", flag.ContinueOnError)
	domain := flags.String("domain", "", "failure domain")
	segment := flags.String("segment", "", "typed segment name")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *domain == "" || *segment == "" {
		return errors.New("run-segment requires --domain and --segment")
	}
	binding, err := rendezvous.BindingFromEnvironment()
	if err != nil {
		return err
	}
	options, err := runtimeOptions(binding)
	if err != nil {
		return err
	}
	if options.Domain != *domain {
		return fmt.Errorf("--domain %s does not match FD_ID %s", *domain, options.Domain)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runtime.RunSegment(ctx, options, *segment)
}

func runCleanup(arguments []string) error {
	flags := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	domain := flags.String("domain", "", "failure domain")
	timeout := flags.Duration("timeout", 3*time.Minute, "cleanup deadline")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *domain == "" {
		return errors.New("cleanup requires --domain")
	}
	binding, err := rendezvous.BindingFromEnvironment()
	if err != nil {
		return err
	}
	options, err := runtimeOptions(binding)
	if err != nil {
		return err
	}
	if options.Domain != *domain {
		return fmt.Errorf("--domain %s does not match FD_ID %s", *domain, options.Domain)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runtime.Cleanup(ctx, options, *timeout)
}

func runtimeOptions(binding rendezvous.Binding) (runtime.Options, error) {
	workspace := os.Getenv("GITHUB_WORKSPACE")
	if workspace == "" {
		var err error
		workspace, err = os.Getwd()
		if err != nil {
			return runtime.Options{}, err
		}
	}
	workspace, err := filepath.Abs(workspace)
	if err != nil {
		return runtime.Options{}, err
	}
	return runtime.Options{
		Domain: os.Getenv("FD_ID"), DomainRunID: os.Getenv("RUN_ID"), RunnerTemp: os.Getenv("RUNNER_TEMP"),
		Workspace: workspace,
		Binding: state.Binding{
			CandidateSHA: binding.CandidateSHA, RunID: binding.RunID, RunAttempt: binding.RunAttempt,
			EnvironmentID: binding.EnvironmentID, Authority: binding.Authority,
		},
		Environment: os.Environ(), Now: time.Now,
	}, nil
}

type paths struct {
	result string
	state  string
}

func defaultPaths(name string) (paths, error) {
	runnerTemp := os.Getenv("RUNNER_TEMP")
	domain := os.Getenv("FD_ID")
	if !filepath.IsAbs(runnerTemp) || (domain != "fd-a" && domain != "fd-b") {
		return paths{}, errors.New("RUNNER_TEMP and FD_ID are required for default rendezvous result paths")
	}
	root := filepath.Join(runnerTemp, "artifacts", "g6-readiness-"+domain, "rendezvous")
	return paths{
		result: filepath.Join(root, name+".result.json"),
		state:  filepath.Join(root, "consumer-state.json"),
	}, nil
}

func writeResult(path string, result rendezvous.Result) error {
	if !filepath.IsAbs(path) {
		return errors.New("structured result path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".rendezvous-result-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func durationEnvironment(name string, fallback time.Duration) time.Duration {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return -1
	}
	return parsed
}

func integerEnvironment(name string, fallback int) int {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return -1
	}
	return parsed
}
