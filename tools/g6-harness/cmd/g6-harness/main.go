package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/GentleKingson/ocservia/tools/g6-harness/internal/rendezvous"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("usage: g6-harness <checkpoint-manifest|wait-download> [options]")
	}
	switch arguments[0] {
	case "checkpoint-manifest":
		return runManifest(arguments[1:])
	case "wait-download":
		return runWait(arguments[1:])
	default:
		return fmt.Errorf("unknown g6-harness command %q", arguments[0])
	}
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
	_, err = rendezvous.CreateManifest(*root, *name, producer, binding, time.Now(), *ttl)
	return err
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
	result, waitErr := rendezvous.WaitDownload(context.Background(), options)
	if writeErr := writeResult(*resultPath, result); writeErr != nil {
		return errors.Join(waitErr, fmt.Errorf("write structured rendezvous result: %w", writeErr))
	}
	return waitErr
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
