package rendezvous

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testBinding() Binding {
	return Binding{
		CandidateSHA:  "0123456789abcdef0123456789abcdef01234567",
		RunID:         "424242",
		RunAttempt:    3,
		EnvironmentID: EnvironmentID("424242", 3),
		Authority:     "engineering",
	}
}

func TestCreateAndValidateManifest(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "candidate-sha"), []byte("candidate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	name := "g6-rd-tunnel-fd-a-424242-3"
	now := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	created, err := CreateManifest(root, name, "fd-a", testBinding(), now, 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if created.Checkpoint != "tunnel-fd-a" || created.Sequence != 10 || len(created.Payloads) != 1 {
		t.Fatalf("unexpected manifest: %+v", created)
	}
	validated, err := ReadAndValidateManifest(root, name, testBinding(), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if validated.Payloads[0].Path != "candidate-sha" {
		t.Fatalf("unexpected payload: %+v", validated.Payloads)
	}
}

func TestSmokeContractUsesSeparateNamespaceAndPeerJob(t *testing.T) {
	contract, err := ResolveContract("g6-smoke-session-424242-3", testBinding())
	if err != nil {
		t.Fatal(err)
	}
	if contract.Profile != "smoke" || contract.Checkpoint != "smoke-session" || contract.ProducerDomain != "fd-b" {
		t.Fatalf("unexpected smoke contract: %+v", contract)
	}
	job, err := peerJobName(contract)
	if err != nil {
		t.Fatal(err)
	}
	if job != "G6 Harness Smoke Core / G6 Smoke FD-B" {
		t.Fatalf("smoke peer job = %q", job)
	}
	if _, err := ResolveContract("g6-rd-load-active-424242-3", testBinding()); err != nil {
		t.Fatalf("formal contract regressed: %v", err)
	}
}

func TestManifestRejectsDigestAndFileSetChanges(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	payload := filepath.Join(root, "payload.txt")
	if err := os.WriteFile(payload, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	name := "g6-rd-tunnel-fd-b-424242-3"
	now := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	if _, err := CreateManifest(root, name, "fd-b", testBinding(), now, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(payload, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadAndValidateManifest(root, name, testBinding(), now); err == nil {
		t.Fatal("changed payload digest was accepted")
	}
	if err := os.WriteFile(filepath.Join(root, "undeclared.txt"), []byte("extra"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadAndValidateManifest(root, name, testBinding(), now); err == nil {
		t.Fatal("undeclared payload was accepted")
	}
}

func TestManifestRejectsWrongProducerAndExpiry(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "payload"), []byte("value"), 0o600); err != nil {
		t.Fatal(err)
	}
	name := "g6-rd-load-active-424242-3"
	now := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	if _, err := CreateManifest(root, name, "fd-a", testBinding(), now, time.Hour); err == nil {
		t.Fatal("wrong producer was accepted")
	}
	if _, err := CreateManifest(root, name, "fd-b", testBinding(), now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadAndValidateManifest(root, name, testBinding(), now.Add(2*time.Minute)); err == nil {
		t.Fatal("expired checkpoint was accepted")
	}
}

func TestManifestRejectsSymlink(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "target"), []byte("value"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateManifest(root, "g6-rd-tunnel-fd-a-424242-3", "fd-a", testBinding(), time.Now(), time.Hour); err == nil {
		t.Fatal("symlink payload was accepted")
	}
}
