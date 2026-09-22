package app

import (
	"slices"
	"testing"
)

// Locks the finite published-node admission of the real-process database E2E
// and the capability profile each registered version verifies. The 1.0.0
// profile must grant the complete-config plan/apply capabilities its
// published binaries declare; no other version may be admitted.
func TestE2ETrustCapabilities(t *testing.T) {
	local, err := e2eTrustCapabilities("")
	if err != nil {
		t.Fatal(err)
	}
	v060, err := e2eTrustCapabilities("0.6.0")
	if err != nil {
		t.Fatal(err)
	}
	v061, err := e2eTrustCapabilities("0.6.1")
	if err != nil {
		t.Fatal(err)
	}
	v100, err := e2eTrustCapabilities("1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(v060, v061) {
		t.Fatal("0.6.0 and 0.6.1 profiles diverged")
	}
	if slices.Contains(local, "config.network") ||
		slices.Contains(local, "ocserv.config.complete.plan") ||
		slices.Contains(local, "ocserv.config.complete.apply") {
		t.Fatal("local candidate profile grants published-only capabilities")
	}
	if !slices.Contains(v060, "config.network") ||
		slices.Contains(v060, "ocserv.config.complete.plan") ||
		slices.Contains(v060, "ocserv.config.complete.apply") {
		t.Fatal("0.6.x profile must grant config.network only")
	}
	if !slices.Contains(v100, "config.network") ||
		!slices.Contains(v100, "ocserv.config.complete.plan") ||
		!slices.Contains(v100, "ocserv.config.complete.apply") {
		t.Fatal("1.0.0 profile must grant the 1.x complete-config capabilities")
	}
	if len(v100) != len(v060)+2 {
		t.Fatal("1.0.0 profile must extend the published baseline by exactly the complete-config capabilities")
	}
	for _, profile := range [][]string{local, v060, v100} {
		unique := slices.Clone(profile)
		slices.Sort(unique)
		if len(slices.Compact(unique)) != len(unique) {
			t.Fatal("profile contains duplicate capabilities")
		}
	}
	for _, version := range []string{"0.5.2", "0.6.2", "0.6.1 ", "1.0.0-rc.1", "1.0.1", "1.1.0", "2.0.0", "latest"} {
		if _, err := e2eTrustCapabilities(version); err == nil {
			t.Fatalf("unregistered published node version admitted: %s", version)
		}
	}
}
