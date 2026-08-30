package rendezvous

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
)

const (
	CheckpointSchemaVersion = "ocservia.g6-checkpoint.v1"
	ResultSchemaVersion     = "ocservia.g6-rendezvous-result.v1"
	StateSchemaVersion      = "ocservia.g6-rendezvous-state.v1"
)

var (
	shaPattern         = regexp.MustCompile(`^[0-9a-f]{40}$`)
	digestPattern      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	runIDPattern       = regexp.MustCompile(`^[1-9][0-9]*$`)
	environmentPattern = regexp.MustCompile(`^g6-[a-z0-9]{8,32}$`)
)

type Contract struct {
	Profile        string
	Prefix         string
	Checkpoint     string
	Sequence       int
	ProducerDomain string
}

var checkpointContracts = []Contract{
	{Profile: "formal", Prefix: "g6-rd-tunnel-fd-a", Checkpoint: "tunnel-fd-a", Sequence: 10, ProducerDomain: "fd-a"},
	{Prefix: "g6-rd-tunnel-fd-b", Checkpoint: "tunnel-fd-b", Sequence: 20, ProducerDomain: "fd-b"},
	{Prefix: "g6-rd-shared-recipient-key", Checkpoint: "shared-recipient-key", Sequence: 25, ProducerDomain: "fd-b"},
	{Prefix: "g6-rd-shared", Checkpoint: "shared-trust-ready", Sequence: 30, ProducerDomain: "fd-a"},
	{Prefix: "g6-rd-primary-up", Checkpoint: "primary-ready", Sequence: 40, ProducerDomain: "fd-a"},
	{Prefix: "g6-rd-agents", Checkpoint: "fd-a-agent-inventory", Sequence: 50, ProducerDomain: "fd-a"},
	{Prefix: "g6-rd-agents-enrolled-fd-b", Checkpoint: "fd-b-agents-enrolled", Sequence: 60, ProducerDomain: "fd-b"},
	{Prefix: "g6-rd-trust-ready", Checkpoint: "transport-trust-ready", Sequence: 70, ProducerDomain: "fd-a"},
	{Prefix: "g6-rd-isolation", Checkpoint: "primary-isolated", Sequence: 80, ProducerDomain: "fd-a"},
	{Prefix: "g6-rd-load-active", Checkpoint: "production-load-active", Sequence: 90, ProducerDomain: "fd-b"},
	{Prefix: "g6-rd-new-primary", Checkpoint: "promotion-complete", Sequence: 120, ProducerDomain: "fd-b"},
	{Prefix: "g6-rd-relay-rejoin-ready", Checkpoint: "relay-rejoin-ready", Sequence: 130, ProducerDomain: "fd-a"},
	{Prefix: "g6-rd-fd-a-ready", Checkpoint: "fd-a-scenarios-ready", Sequence: 140, ProducerDomain: "fd-a"},
	{Prefix: "g6-rd-relay-pre-fault", Checkpoint: "relay-pre-fault-observed", Sequence: 150, ProducerDomain: "fd-b"},
	{Prefix: "g6-rd-window-barrier-arm-request", Checkpoint: "window-barrier-arm-request", Sequence: 180, ProducerDomain: "fd-b"},
	{Prefix: "g6-rd-window-barrier-armed-fd-a", Checkpoint: "window-barrier-armed-fd-a", Sequence: 190, ProducerDomain: "fd-a"},
	{Prefix: "g6-rd-final-freeze", Checkpoint: "final-freeze-request", Sequence: 210, ProducerDomain: "fd-b"},
}

func init() {
	for index := 1; index < len(checkpointContracts); index++ {
		checkpointContracts[index].Profile = "formal"
	}
	checkpointContracts = append(checkpointContracts,
		Contract{Profile: "smoke", Prefix: "g6-smoke-tunnel-fd-a", Checkpoint: "smoke-tunnel-fd-a", Sequence: 10, ProducerDomain: "fd-a"},
		Contract{Profile: "smoke", Prefix: "g6-smoke-tunnel-fd-b", Checkpoint: "smoke-tunnel-fd-b", Sequence: 20, ProducerDomain: "fd-b"},
		Contract{Profile: "smoke", Prefix: "g6-smoke-shared-recipient-key", Checkpoint: "smoke-shared-recipient-key", Sequence: 25, ProducerDomain: "fd-b"},
		Contract{Profile: "smoke", Prefix: "g6-smoke-shared", Checkpoint: "smoke-shared-trust-ready", Sequence: 30, ProducerDomain: "fd-a"},
		Contract{Profile: "smoke", Prefix: "g6-smoke-primary-up", Checkpoint: "smoke-primary-ready", Sequence: 40, ProducerDomain: "fd-a"},
		Contract{Profile: "smoke", Prefix: "g6-smoke-agents", Checkpoint: "smoke-fd-a-agent-inventory", Sequence: 50, ProducerDomain: "fd-a"},
		Contract{Profile: "smoke", Prefix: "g6-smoke-agents-enrolled-fd-b", Checkpoint: "smoke-fd-b-agents-enrolled", Sequence: 60, ProducerDomain: "fd-b"},
		Contract{Profile: "smoke", Prefix: "g6-smoke-trust-ready", Checkpoint: "smoke-transport-trust-ready", Sequence: 70, ProducerDomain: "fd-a"},
		Contract{Profile: "smoke", Prefix: "g6-smoke-session", Checkpoint: "smoke-session", Sequence: 80, ProducerDomain: "fd-b"},
		Contract{Profile: "smoke", Prefix: "g6-smoke-isolation", Checkpoint: "smoke-primary-isolated", Sequence: 90, ProducerDomain: "fd-a"},
		Contract{Profile: "smoke", Prefix: "g6-smoke-promotion", Checkpoint: "smoke-promotion-complete", Sequence: 100, ProducerDomain: "fd-b"},
	)
}

// Contracts returns a copy of the frozen checkpoint registry.
func Contracts() []Contract {
	return slices.Clone(checkpointContracts)
}

type Binding struct {
	CandidateSHA  string
	RunID         string
	RunAttempt    int
	EnvironmentID string
	Authority     string
}

func BindingFromEnvironment() (Binding, error) {
	attempt, err := strconv.Atoi(os.Getenv("GITHUB_RUN_ATTEMPT"))
	if err != nil {
		return Binding{}, fmt.Errorf("GITHUB_RUN_ATTEMPT must be a positive integer: %w", err)
	}
	binding := Binding{
		CandidateSHA:  os.Getenv("GITHUB_SHA"),
		RunID:         os.Getenv("GITHUB_RUN_ID"),
		RunAttempt:    attempt,
		EnvironmentID: os.Getenv("G6RD_ENVIRONMENT_ID"),
		Authority:     os.Getenv("G6_AUTHORITY"),
	}
	if binding.EnvironmentID == "" {
		binding.EnvironmentID = EnvironmentID(binding.RunID, binding.RunAttempt)
	}
	if err := binding.Validate(); err != nil {
		return Binding{}, err
	}
	return binding, nil
}

func EnvironmentID(runID string, attempt int) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s-%d", runID, attempt)))
	return "g6-" + hex.EncodeToString(digest[:8])
}

func (b Binding) Validate() error {
	if !shaPattern.MatchString(b.CandidateSHA) {
		return errors.New("candidate SHA must be 40 lowercase hexadecimal characters")
	}
	if !runIDPattern.MatchString(b.RunID) {
		return errors.New("run ID must be a positive decimal integer")
	}
	if b.RunAttempt < 1 {
		return errors.New("run attempt must be positive")
	}
	if !environmentPattern.MatchString(b.EnvironmentID) {
		return errors.New("environment ID violates the g6-[a-z0-9]{8,32} contract")
	}
	if b.EnvironmentID != EnvironmentID(b.RunID, b.RunAttempt) {
		return errors.New("environment ID does not match the exact run and attempt")
	}
	if b.Authority != "engineering" && b.Authority != "production_readiness" {
		return errors.New("authority must be engineering or production_readiness")
	}
	return nil
}

func ResolveContract(name string, binding Binding) (Contract, error) {
	if err := binding.Validate(); err != nil {
		return Contract{}, err
	}
	for _, contract := range checkpointContracts {
		expected := fmt.Sprintf("%s-%s-%d", contract.Prefix, binding.RunID, binding.RunAttempt)
		if name == expected {
			return contract, nil
		}
	}
	return Contract{}, fmt.Errorf("artifact name %q is not a G6 checkpoint for this run attempt", name)
}

func peerJobName(contract Contract) (string, error) {
	if contract.Profile == "smoke" {
		switch contract.ProducerDomain {
		case "fd-a":
			return "G6 Harness Smoke Core / G6 Smoke FD-A", nil
		case "fd-b":
			return "G6 Harness Smoke Core / G6 Smoke FD-B", nil
		}
	}
	switch contract.ProducerDomain {
	case "fd-a":
		return "G6 Formal Readiness / G6 Formal FD-A: Primary & PITR", nil
	case "fd-b":
		return "G6 Formal Readiness / G6 Formal FD-B: Standby, Promotion & Faults", nil
	default:
		return "", fmt.Errorf("unsupported producer domain %q", contract.ProducerDomain)
	}
}
