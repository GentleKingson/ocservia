package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/GentleKingson/ocservia/tools/g6-harness/internal/cleanup"
	"github.com/GentleKingson/ocservia/tools/g6-harness/internal/execx"
	"github.com/GentleKingson/ocservia/tools/g6-harness/internal/phase"
	resultmodel "github.com/GentleKingson/ocservia/tools/g6-harness/internal/result"
	"github.com/GentleKingson/ocservia/tools/g6-harness/internal/state"
)

type Options struct {
	Profile     string
	Domain      string
	DomainRunID string
	RunnerTemp  string
	Workspace   string
	Binding     state.Binding
	Environment []string
	Now         func() time.Time
}

func RunSegment(ctx context.Context, options Options, segmentName string) (runErr error) {
	if err := options.validate(); err != nil {
		return err
	}
	graph, segment, err := phase.ResolveProfileSegment(options.Profile, options.Domain, segmentName)
	if err != nil {
		return err
	}
	if err := ensureRegistry(options); err != nil {
		contractErr := fmt.Errorf("prepare durable cleanup registry: %w", err)
		return errors.Join(contractErr, writeRejection(options, segmentName, segmentFirstDefinition(graph, segment), "cleanup_registry_rejected", contractErr))
	}
	store, err := state.Open(options.stateRoot(), options.Domain, options.Binding, graph, options.Now)
	if err != nil {
		return errors.Join(err, writeRejection(options, segmentName, segmentFirstDefinition(graph, segment), "runtime_state_rejected", err))
	}
	defer func() { runErr = errors.Join(runErr, store.Close()) }()
	for _, name := range segment.Phases {
		definition, err := graph.Definition(name)
		if err != nil {
			return err
		}
		if err := runPhase(ctx, options, store, segmentName, definition); err != nil {
			return err
		}
	}
	return nil
}

func RecordManifested(options Options, checkpoint string) (recordErr error) {
	if err := options.validate(); err != nil {
		return err
	}
	graph, err := phase.ResolveProfileGraph(options.Profile, options.Domain)
	if err != nil {
		return err
	}
	required, err := phase.RequiredManifestPhaseForProfile(options.Profile, options.Domain, checkpoint)
	if err != nil {
		return err
	}
	store, err := state.Open(options.stateRoot(), options.Domain, options.Binding, graph, options.Now)
	if err != nil {
		return err
	}
	defer func() { recordErr = errors.Join(recordErr, store.Close()) }()
	return store.RecordManifested(checkpoint, required)
}

func RecordConsumed(options Options, checkpoint string) (recordErr error) {
	if err := options.validate(); err != nil {
		return err
	}
	graph, err := phase.ResolveProfileGraph(options.Profile, options.Domain)
	if err != nil {
		return err
	}
	store, err := state.Open(options.stateRoot(), options.Domain, options.Binding, graph, options.Now)
	if err != nil {
		return err
	}
	defer func() { recordErr = errors.Join(recordErr, store.Close()) }()
	return store.RecordConsumed(checkpoint)
}

func Cleanup(ctx context.Context, options Options, timeout time.Duration) error {
	if err := options.validate(); err != nil {
		return err
	}
	if timeout <= 0 || timeout > 10*time.Minute {
		return errors.New("cleanup timeout is outside the bounded contract")
	}
	expected, err := expectedRegistry(options)
	if err != nil {
		return err
	}
	registry, err := cleanup.LoadAndValidate(options.registryPath(), expected)
	if err != nil {
		return err
	}
	if registry.CleanupStatus == "passed" {
		return nil
	}
	diagnostics := options.diagnosticsRoot()
	if err := os.MkdirAll(diagnostics, 0o700); err != nil {
		return err
	}
	started := options.Now().UTC()
	deadline := started.Add(timeout)
	logBase := fmt.Sprintf("cleanup-%d", started.UnixNano())
	resultPath := filepath.Join(diagnostics, "cleanup-results", logBase+".json")
	latestResultPath := filepath.Join(diagnostics, "cleanup-result.json")
	stdoutPath := filepath.Join(diagnostics, logBase+".stdout.log")
	stderrPath := filepath.Join(diagnostics, logBase+".stderr.log")
	initial := resultmodel.Phase{
		Domain: options.Domain, Binding: options.Binding, Segment: "cleanup", Phase: "cleanup",
		Sequence: 1000, Status: "running", StartedAt: started, CompletedAt: started, Deadline: deadline,
	}
	if err := writeCleanupResult(resultPath, latestResultPath, initial); err != nil {
		return err
	}
	cleanupContext, cancel := context.WithTimeoutCause(ctx, timeout, errors.New("cleanup deadline exceeded"))
	outcome := execx.Run(cleanupContext, execx.Spec{
		Executable: options.adapterPath(), Arguments: []string{"cleanup"}, Directory: options.Workspace,
		Environment: options.Environment, StdoutPath: stdoutPath, StderrPath: stderrPath, KillGrace: 15 * time.Second,
	})
	cancel()
	completed := options.Now().UTC()
	final := initial
	final.CompletedAt = completed
	final.ExitCode = &outcome.ExitCode
	if outcome.Err == nil {
		final.Status = "passed"
		if err := writeCleanupResult(resultPath, latestResultPath, final); err != nil {
			return err
		}
		return cleanup.Mark(options.registryPath(), registry, "passed", nil, completed)
	}
	final.Status = "failed"
	final.Failure = &resultmodel.Failure{
		Class: "cleanup_failed", Code: "cleanup_adapter_failed", Message: execx.ExitDescription(outcome),
		Expected: "exit code 0 before " + deadline.Format(time.RFC3339Nano), Actual: fmt.Sprintf("exit code %d", outcome.ExitCode),
		DiagnosticPaths: []string{"harness/" + logBase + ".stdout.log", "harness/" + logBase + ".stderr.log"},
	}
	writeErr := writeCleanupResult(resultPath, latestResultPath, final)
	markErr := cleanup.Mark(options.registryPath(), registry, "failed", outcome.Err, completed)
	return errors.Join(outcome.Err, writeErr, markErr)
}

func writeCleanupResult(attemptPath, latestPath string, result resultmodel.Phase) error {
	return errors.Join(resultmodel.Write(attemptPath, result), resultmodel.Write(latestPath, result))
}

func runPhase(parent context.Context, options Options, store *state.Store, segment string, definition phase.Definition) error {
	started, err := store.Begin(definition)
	if err != nil {
		return errors.Join(err, writeRejection(options, segment, definition, "phase_transition_rejected", err))
	}
	deadline := started.Add(definition.Timeout)
	resultsRoot := filepath.Join(options.stateRoot(), "phase-results")
	logsRoot := filepath.Join(options.stateRoot(), "logs")
	if err := os.MkdirAll(resultsRoot, 0o700); err != nil {
		return errors.Join(err, store.Fail(definition, err.Error()))
	}
	if err := os.MkdirAll(logsRoot, 0o700); err != nil {
		return errors.Join(err, store.Fail(definition, err.Error()))
	}
	basename := fmt.Sprintf("%03d-%s", definition.Sequence, definition.Name)
	resultPath := filepath.Join(resultsRoot, basename+".json")
	stdoutPath := filepath.Join(logsRoot, basename+".stdout.log")
	stderrPath := filepath.Join(logsRoot, basename+".stderr.log")
	phaseResult := resultmodel.Phase{
		Domain: options.Domain, Binding: options.Binding, Segment: segment, Phase: definition.Name,
		Sequence: definition.Sequence, Status: "running", StartedAt: started, CompletedAt: started, Deadline: deadline,
	}
	if err := resultmodel.Write(resultPath, phaseResult); err != nil {
		return errors.Join(err, store.Fail(definition, err.Error()))
	}
	phaseContext, cancel := context.WithTimeoutCause(parent, definition.Timeout, fmt.Errorf("phase %s deadline exceeded", definition.Name))
	arguments, err := adapterArguments(options, definition.Name)
	if err != nil {
		cancel()
		phaseResult.Status = "failed"
		phaseResult.CompletedAt = options.Now().UTC()
		phaseResult.Failure = &resultmodel.Failure{
			Class: "harness_contract_failed", Code: "leaf_adapter_undefined", Message: err.Error(),
			Expected: "an exact fixed leaf adapter for every phase", Actual: definition.Name,
			DiagnosticPaths: []string{},
		}
		resultErr := resultmodel.Write(resultPath, phaseResult)
		stateErr := store.Fail(definition, err.Error())
		return errors.Join(err, resultErr, stateErr)
	}
	outcome := execx.Run(phaseContext, execx.Spec{
		Executable: options.adapterPath(), Arguments: arguments, Directory: options.Workspace,
		Environment: options.Environment, StdoutPath: stdoutPath, StderrPath: stderrPath, KillGrace: 15 * time.Second,
	})
	cancel()
	phaseResult.CompletedAt = options.Now().UTC()
	phaseResult.ExitCode = &outcome.ExitCode
	if outcome.Err == nil {
		phaseResult.Status = "passed"
		if err := resultmodel.Write(resultPath, phaseResult); err != nil {
			return errors.Join(err, store.Fail(definition, err.Error()))
		}
		return store.Complete(definition)
	}
	phaseResult.Status = "failed"
	phaseResult.Failure = classifyFailure(outcome, definition, deadline, basename)
	resultErr := resultmodel.Write(resultPath, phaseResult)
	stateErr := store.Fail(definition, phaseResult.Failure.Message)
	return errors.Join(outcome.Err, resultErr, stateErr)
}

func segmentFirstDefinition(graph phase.Graph, segment phase.Segment) phase.Definition {
	definition, _ := graph.Definition(segment.Phases[0])
	return definition
}

func writeRejection(options Options, segment string, definition phase.Definition, code string, cause error) error {
	now := options.Now().UTC()
	path := filepath.Join(options.diagnosticsRoot(), "rejections", fmt.Sprintf("%d-%03d-%s.json", now.UnixNano(), definition.Sequence, definition.Name))
	rejection := resultmodel.Phase{
		Domain: options.Domain, Binding: options.Binding, Segment: segment, Phase: definition.Name,
		Sequence: definition.Sequence, Status: "failed", StartedAt: now, CompletedAt: now, Deadline: now,
		Failure: &resultmodel.Failure{
			Class: "harness_contract_failed", Code: code, Message: cause.Error(),
			Expected: "the exact next transition for this candidate-bound runtime", Actual: cause.Error(),
			DiagnosticPaths: []string{},
		},
	}
	return resultmodel.Write(path, rejection)
}

func classifyFailure(outcome execx.Outcome, definition phase.Definition, deadline time.Time, basename string) *resultmodel.Failure {
	failure := &resultmodel.Failure{
		Class: "product_assertion_failed", Code: "leaf_adapter_failed", Message: execx.ExitDescription(outcome),
		Expected: "exit code 0", Actual: fmt.Sprintf("exit code %d", outcome.ExitCode),
		DiagnosticPaths: []string{"harness/runtime/logs/" + basename + ".stdout.log", "harness/runtime/logs/" + basename + ".stderr.log"},
	}
	if outcome.Cause != nil {
		if errors.Is(outcome.Cause, context.Canceled) {
			failure.Class = "phase_cancelled"
			failure.Code = "phase_context_cancelled"
			failure.Expected = "phase context remains active"
		} else {
			failure.Class = "phase_timeout"
			failure.Code = "phase_deadline_exceeded"
			failure.Expected = "completion before " + deadline.Format(time.RFC3339Nano)
		}
		failure.Actual = outcome.Cause.Error()
		return failure
	}
	var execError *exec.Error
	if outcome.Infrastructure || errors.As(outcome.Err, &execError) {
		failure.Class = "runner_infrastructure_failed"
		failure.Code = "leaf_adapter_start_failed"
	}
	return failure
}

func adapterArguments(options Options, name string) ([]string, error) {
	prefix := "g6-rd-"
	if options.Profile == "smoke" {
		prefix = "g6-smoke-"
	}
	peer := func(checkpoint string) string { return filepath.Join(options.RunnerTemp, prefix+checkpoint) }
	if options.Profile == "smoke" {
		arguments := map[string][]string{
			"import-peer-tunnel-nodes": {"import-peer-tunnel-nodes", peer(map[string]string{"fd-a": "tunnel-fd-b", "fd-b": "tunnel-fd-a"}[options.Domain])},
			"materialize-runtime":      {"materialize-runtime", peer("shared")},
			"standby-bootstrap":        {"standby-bootstrap", peer("primary-up")},
			"agents-enroll":            map[string][]string{"fd-a": {"agents-enroll"}, "fd-b": {"agents-enroll", filepath.Join(peer("agents"), "nodes.tsv")}}[options.Domain],
			"transport-trust-reload":   {"transport-trust-reload", peer("agents-enrolled-fd-b")},
			"agents-start":             map[string][]string{"fd-a": {"agents-start"}, "fd-b": {"agents-start", peer("trust-ready")}}[options.Domain],
			"smoke-isolate":            {"smoke-isolate"}, "promote": {"promote", peer("isolation")},
			"smoke-evidence": map[string][]string{"fd-a": {"smoke-evidence", peer("promotion")}, "fd-b": {"smoke-evidence"}}[options.Domain],
		}
		if value, ok := arguments[name]; ok {
			return value, nil
		}
		for _, allowed := range []string{"prepare", "build-images", "tunnel-up", "publish-shared-secrets", "primary-up", "relay-up", "smoke-session"} {
			if name == allowed {
				return []string{name}, nil
			}
		}
		return nil, fmt.Errorf("smoke phase %s has no fixed leaf adapter", name)
	}
	if name == "window-barrier-arm" {
		if options.Domain == "fd-a" {
			return []string{"window-barrier-arm", peer("window-barrier-arm-request")}, nil
		}
		return []string{"window-barrier-arm"}, nil
	}
	arguments := map[string][]string{
		"import-peer-tunnel-nodes": {"import-peer-tunnel-nodes", peer(map[string]string{"fd-a": "tunnel-fd-b", "fd-b": "tunnel-fd-a"}[options.Domain])},
		"transport-trust-reload":   {"transport-trust-reload", peer("agents-enrolled-fd-b")},
		"dual-primary-probes":      {"dual-primary-probes", peer("new-primary")},
		"relay-a-stop":             {"relay-a-stop", peer("relay-pre-fault")},
		"evidence":                 {"evidence", peer("final-freeze")},
		"materialize-runtime":      {"materialize-runtime", peer("shared")},
		"standby-bootstrap":        {"standby-bootstrap", peer("primary-up")},
		"agents-enroll":            map[string][]string{"fd-a": {"agents-enroll"}, "fd-b": {"agents-enroll", filepath.Join(peer("agents"), "nodes.tsv")}}[options.Domain],
		"agents-start":             map[string][]string{"fd-a": {"agents-start"}, "fd-b": {"agents-start", peer("trust-ready")}}[options.Domain],
		"promote":                  {"promote", peer("isolation")},
		"relay-pre-fault":          {"relay-pre-fault", peer("relay-rejoin-ready")},
		"merge-peer-evidence":      {"merge-peer-evidence", peer("fd-a-ready")},
		"scenario-relay":           {"scenario-relay", peer("fd-a-ready")},
		"window":                   {"window", peer("window-barrier-armed-fd-a")},
	}
	if value, ok := arguments[name]; ok {
		return value, nil
	}
	allowedWithoutArguments := map[string]bool{
		"prepare": true, "build-images": true, "tunnel-up": true, "publish-shared-secrets": true,
		"primary-up": true, "pitr-prepare": true, "isolate": true, "pitr-restore": true, "rejoin": true,
		"relay-rejoin-ready": true, "ready": true, "window-barrier-release-after-proof": true,
		"relay-up": true, "load-start": true, "scenario-scheduler": true, "scenario-owner": true,
		"scenario-path": true, "outbox-claim-before-send": true, "outbox-send-before-mark": true,
		"outbox-result-before-commit": true, "resource-preflight": true, "evidence-collect": true, "final-freeze": true,
	}
	if allowedWithoutArguments[name] {
		return []string{name}, nil
	}
	return nil, fmt.Errorf("phase %s has no fixed leaf adapter", name)
}

func (options Options) validate() error {
	if options.Domain != "fd-a" && options.Domain != "fd-b" {
		return errors.New("failure domain must be fd-a or fd-b")
	}
	if options.DomainRunID == "" || strings.ContainsAny(options.DomainRunID, `/\\`) {
		return errors.New("domain run ID is invalid")
	}
	if !filepath.IsAbs(options.RunnerTemp) || !filepath.IsAbs(options.Workspace) {
		return errors.New("runner temp and workspace must be absolute")
	}
	if options.Now == nil {
		return errors.New("runtime clock is required")
	}
	if len(options.Environment) == 0 {
		return errors.New("runtime environment is required")
	}
	return nil
}

func (options Options) stateRoot() string {
	return filepath.Join(options.RunnerTemp, "g6-readiness-"+options.DomainRunID, "harness")
}

func (options Options) diagnosticsRoot() string {
	return filepath.Join(options.RunnerTemp, "artifacts", "g6-readiness-"+options.Domain, "harness")
}

func (options Options) registryPath() string {
	return filepath.Join(options.diagnosticsRoot(), "resources.json")
}

func (options Options) adapterPath() string {
	return filepath.Join(options.Workspace, "scripts", "g6-readiness-"+options.Domain+".sh")
}

func ensureRegistry(options Options) error {
	expected, err := expectedRegistry(options)
	if err != nil {
		return err
	}
	return cleanup.Ensure(options.registryPath(), expected)
}

func expectedRegistry(options Options) (cleanup.Registry, error) {
	environment := make(map[string]string)
	for _, entry := range options.Environment {
		name, value, found := strings.Cut(entry, "=")
		if found {
			environment[name] = value
		}
	}
	return cleanup.Expected(options.Domain, options.DomainRunID, options.RunnerTemp, options.Binding, environment)
}
