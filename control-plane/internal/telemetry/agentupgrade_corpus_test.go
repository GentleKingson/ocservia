package telemetry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/mod/semver"
)

// The execution-time downgrade fence compares versions on both sides of the
// wire: Go decides admission with this ordering and the Rust runner refuses
// to execute with its mirror. The shared corpus pins both answers, including
// numeric prerelease identifiers beyond any fixed-width integer, so the two
// languages can never drift apart.
func TestAgentUpgradeStrictUpgradeCorpusMatchesGoOrdering(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "agent-upgrade-strict-upgrade-v1.json"))
	if err != nil {
		t.Fatalf("read shared corpus: %v", err)
	}
	var corpus struct {
		Cases []struct {
			Name          string `json:"name"`
			Current       string `json:"current"`
			Target        string `json:"target"`
			StrictUpgrade bool   `json:"strict_upgrade"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("parse shared corpus: %v", err)
	}
	if len(corpus.Cases) == 0 {
		t.Fatal("shared corpus has no cases")
	}
	for _, tc := range corpus.Cases {
		current, ok := canonicalSemver(tc.Current)
		if !ok {
			t.Fatalf("case %s: current %q is not a valid semver", tc.Name, tc.Current)
		}
		target, ok := canonicalSemver(tc.Target)
		if !ok {
			t.Fatalf("case %s: target %q is not a valid semver", tc.Name, tc.Target)
		}
		if ordered := semver.Compare(target, current) > 0; ordered != tc.StrictUpgrade {
			t.Fatalf("case %s: Go ordering says %v, corpus expects %v (%s -> %s)", tc.Name, ordered, tc.StrictUpgrade, tc.Current, tc.Target)
		}
	}
}
