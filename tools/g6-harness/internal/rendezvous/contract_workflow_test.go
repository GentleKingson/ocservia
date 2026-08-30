package rendezvous

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestPeerJobNamesMatchWorkflowPins keeps the frozen rendezvous contract and
// the workflow job display names in lockstep. A renamed FD job must update
// peerJobName in the same commit, otherwise both FD pairs fail at the first
// cross-domain checkpoint wait because the typed contract rejects the
// producer job name reported by the Actions API.
func TestPeerJobNamesMatchWorkflowPins(t *testing.T) {
	t.Parallel()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve test file location")
	}
	workflow := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "..", ".github", "workflows", "g6-harness-core.yml")
	content, err := os.ReadFile(workflow)
	if err != nil {
		t.Fatal(err)
	}
	pinned := map[string]bool{}
	for _, line := range strings.Split(string(content), "\n") {
		const marker = `--peer-job "`
		if index := strings.Index(line, marker); index >= 0 {
			rest := line[index+len(marker):]
			end := strings.Index(rest, `"`)
			if end < 0 {
				t.Fatalf("unterminated --peer-job value: %q", line)
			}
			pinned[rest[:end]] = true
		}
	}
	expected := map[string]bool{}
	for _, contract := range Contracts() {
		job, err := peerJobName(contract)
		if err != nil {
			t.Fatal(err)
		}
		expected[job] = true
	}
	if len(expected) != 4 {
		t.Fatalf("expected four distinct peer job names, got %d: %v", len(expected), expected)
	}
	for job := range expected {
		if !pinned[job] {
			t.Errorf("contract peer job %q is not referenced by any --peer-job in g6-harness-core.yml", job)
		}
	}
	for job := range pinned {
		if !expected[job] {
			t.Errorf("workflow --peer-job %q does not match any peerJobName() contract value", job)
		}
	}
}
