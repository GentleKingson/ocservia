package cleanup

import (
	"path/filepath"
	"testing"

	"github.com/GentleKingson/ocservia/tools/g6-harness/internal/state"
)

func TestRegistryRejectsCrossCandidateAndResourceChanges(t *testing.T) {
	t.Parallel()
	environment := map[string]string{
		"G6RD_AGENT_IMAGE": "agent:1", "G6RD_CONTROL_PLANE_IMAGE": "control:1",
		"G6RD_TRANSPORTD_IMAGE": "transport:1", "G6RD_RELAY_IMAGE": "relay:1", "G6RD_PROBE_IMAGE": "probe:1",
	}
	binding := state.Binding{CandidateSHA: "0123456789abcdef0123456789abcdef01234567", RunID: "42", RunAttempt: 1, EnvironmentID: "g6-01234567", Authority: "engineering"}
	expected, err := Expected("fd-a", "42-1-fd-a", t.TempDir(), binding, environment)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "resources.json")
	if err := Ensure(path, expected); err != nil {
		t.Fatal(err)
	}
	other := expected
	other.Binding.CandidateSHA = "1123456789abcdef0123456789abcdef01234567"
	if err := Ensure(path, other); err == nil {
		t.Fatal("cross-candidate registry was accepted")
	}
	other = expected
	other.Resources[0].ID += "-tampered"
	if err := Ensure(path, other); err == nil {
		t.Fatal("changed resource set was accepted")
	}
	other = expected
	other.CleanupStatus = "passed"
	if err := Write(path, other); err != nil {
		t.Fatal(err)
	}
	if err := Ensure(path, expected); err == nil {
		t.Fatal("invalid cleanup outcome was accepted")
	}
}
