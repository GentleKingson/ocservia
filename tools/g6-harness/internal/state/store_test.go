package state

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/tools/g6-harness/internal/phase"
)

func TestStoreRejectsDuplicateOutOfOrderAndCrossBindingState(t *testing.T) {
	t.Parallel()
	graph, err := phase.ResolveGraph("fd-a")
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "state")
	binding := testBinding()
	store, err := Open(root, "fd-a", binding, graph, fixedClock())
	if err != nil {
		t.Fatal(err)
	}
	prepare, _ := graph.Definition("prepare")
	build, _ := graph.Definition("build-images")
	if _, err := store.Begin(build); err == nil {
		t.Fatal("out-of-order phase was accepted")
	}
	if _, err := store.Begin(prepare); err != nil {
		t.Fatal(err)
	}
	if err := store.Complete(prepare); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Begin(prepare); err == nil {
		t.Fatal("duplicate phase was accepted")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	other := binding
	other.CandidateSHA = "1123456789abcdef0123456789abcdef01234567"
	if _, err := Open(root, "fd-a", other, graph, fixedClock()); err == nil {
		t.Fatal("cross-candidate runtime state was accepted")
	}
}

func TestStoreRejectsInterruptedActivePhase(t *testing.T) {
	t.Parallel()
	graph, _ := phase.ResolveGraph("fd-a")
	root := filepath.Join(t.TempDir(), "state")
	store, err := Open(root, "fd-a", testBinding(), graph, fixedClock())
	if err != nil {
		t.Fatal(err)
	}
	prepare, _ := graph.Definition("prepare")
	if _, err := store.Begin(prepare); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, "fd-a", testBinding(), graph, fixedClock()); err == nil {
		t.Fatal("interrupted active phase was silently resumed")
	}
}

func TestStoreBindsCheckpointProgressToCompletedPhase(t *testing.T) {
	t.Parallel()
	graph, _ := phase.ResolveGraph("fd-a")
	store, err := Open(filepath.Join(t.TempDir(), "state"), "fd-a", testBinding(), graph, fixedClock())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.RecordManifested("tunnel-fd-a", "prepare"); err == nil {
		t.Fatal("checkpoint manifested before its producer phase completed")
	}
	prepare, _ := graph.Definition("prepare")
	if _, err := store.Begin(prepare); err != nil {
		t.Fatal(err)
	}
	if err := store.Complete(prepare); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordManifested("tunnel-fd-a", "prepare"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordManifested("tunnel-fd-a", "prepare"); err == nil {
		t.Fatal("duplicate manifested checkpoint was accepted")
	}
	if err := store.RecordConsumed("tunnel-fd-b"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordConsumed("tunnel-fd-b"); err == nil {
		t.Fatal("duplicate consumed checkpoint was accepted")
	}
	if err := store.RecordConsumed("not-in-the-graph"); err == nil {
		t.Fatal("unknown consumed checkpoint was accepted")
	}
}

func TestFailedStateRejectsCheckpointProgress(t *testing.T) {
	t.Parallel()
	graph, _ := phase.ResolveGraph("fd-a")
	store, err := Open(filepath.Join(t.TempDir(), "state"), "fd-a", testBinding(), graph, fixedClock())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	prepare, _ := graph.Definition("prepare")
	if _, err := store.Begin(prepare); err != nil {
		t.Fatal(err)
	}
	if err := store.Fail(prepare, "injected failure"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordManifested("tunnel-fd-a", "prepare"); err == nil {
		t.Fatal("failed state manifested a checkpoint")
	}
	if err := store.RecordConsumed("tunnel-fd-b"); err == nil {
		t.Fatal("failed state consumed a checkpoint")
	}
}

func TestStoreRejectsTamperedPhasePrefix(t *testing.T) {
	t.Parallel()
	graph, _ := phase.ResolveGraph("fd-a")
	root := filepath.Join(t.TempDir(), "state")
	store, err := Open(root, "fd-a", testBinding(), graph, fixedClock())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	tampered := State{
		SchemaVersion: SchemaVersion, Domain: "fd-a", Binding: testBinding(), Status: "running",
		CurrentSequence: 30,
		CompletedPhases: []PhaseRecord{{Name: "build-images", Sequence: 30, CompletedAt: time.Now().UTC()}},
		UpdatedAt:       time.Now().UTC(),
	}
	if err := writeJSONAtomic(filepath.Join(root, "state.json"), tampered); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, "fd-a", testBinding(), graph, fixedClock()); err == nil {
		t.Fatal("tampered phase prefix was accepted")
	}
}

func testBinding() Binding {
	return Binding{
		CandidateSHA: "0123456789abcdef0123456789abcdef01234567", RunID: "424242", RunAttempt: 3,
		EnvironmentID: "g6-0123456789abcdef", Authority: "engineering",
	}
}

func fixedClock() func() time.Time {
	value := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	return func() time.Time {
		value = value.Add(time.Second)
		return value
	}
}
