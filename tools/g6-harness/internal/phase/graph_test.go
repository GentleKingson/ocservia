package phase

import (
	"testing"
	"time"
)

func TestSegmentsCoverEachPhaseExactlyOnceInOrder(t *testing.T) {
	t.Parallel()
	for _, domain := range []string{"fd-a", "fd-b"} {
		graph, err := ResolveGraph(domain)
		if err != nil {
			t.Fatal(err)
		}
		var flattened []string
		phaseNames := make(map[string]bool)
		segmentNames := make(map[string]bool)
		for _, segment := range graph.Segments {
			if segmentNames[segment.Name] {
				t.Fatalf("%s duplicates segment %s", domain, segment.Name)
			}
			segmentNames[segment.Name] = true
			flattened = append(flattened, segment.Phases...)
		}
		if len(flattened) != len(graph.Phases) {
			t.Fatalf("%s segments cover %d phases, graph contains %d", domain, len(flattened), len(graph.Phases))
		}
		for index, definition := range graph.Phases {
			if phaseNames[definition.Name] {
				t.Fatalf("%s duplicates phase %s", domain, definition.Name)
			}
			phaseNames[definition.Name] = true
			if definition.Timeout <= 0 || definition.Timeout > 35*time.Minute {
				t.Fatalf("%s phase %s has unbounded timeout %s", domain, definition.Name, definition.Timeout)
			}
			if flattened[index] != definition.Name {
				t.Fatalf("%s segment order at %d is %s, want %s", domain, index, flattened[index], definition.Name)
			}
			if index > 0 && definition.Sequence <= graph.Phases[index-1].Sequence {
				t.Fatalf("%s phase sequence does not increase at %s", domain, definition.Name)
			}
		}
	}
}

func TestEveryCheckpointHasExactProducerPhase(t *testing.T) {
	t.Parallel()
	if len(manifestedByCheckpoint) != 16 {
		t.Fatalf("checkpoint registry has %d entries, want 16", len(manifestedByCheckpoint))
	}
	for checkpoint, requirement := range manifestedByCheckpoint {
		graph, err := ResolveGraph(requirement.Domain)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := graph.Definition(requirement.Phase); err != nil {
			t.Fatalf("checkpoint %s: %v", checkpoint, err)
		}
	}
}

func TestEveryPhaseCheckpointPreconditionHasAProducer(t *testing.T) {
	t.Parallel()
	for _, domain := range []string{"fd-a", "fd-b"} {
		graph, _ := ResolveGraph(domain)
		for _, definition := range graph.Phases {
			for _, checkpoint := range append(append([]string{}, definition.RequiresConsumed...), definition.RequiresManifested...) {
				requirement, ok := manifestedByCheckpoint[checkpoint]
				if !ok {
					t.Fatalf("%s phase %s requires checkpoint %s without a producer", domain, definition.Name, checkpoint)
				}
				if _, err := graphs[requirement.Domain].Definition(requirement.Phase); err != nil {
					t.Fatalf("%s phase %s requires invalid checkpoint %s: %v", domain, definition.Name, checkpoint, err)
				}
			}
		}
	}
}

func TestSmokeSegmentsCoverEachPhaseExactlyOnceInOrder(t *testing.T) {
	for _, domain := range []string{"fd-a", "fd-b"} {
		graph, err := ResolveProfileGraph("smoke", domain)
		if err != nil {
			t.Fatal(err)
		}
		if graph.Profile != "smoke" {
			t.Fatalf("%s profile = %q", domain, graph.Profile)
		}
		var flattened []string
		for _, segment := range graph.Segments {
			flattened = append(flattened, segment.Phases...)
		}
		if len(flattened) != len(graph.Phases) {
			t.Fatalf("%s segments cover %d/%d phases", domain, len(flattened), len(graph.Phases))
		}
		for index, definition := range graph.Phases {
			if flattened[index] != definition.Name || (index > 0 && definition.Sequence <= graph.Phases[index-1].Sequence) {
				t.Fatalf("%s smoke graph is unordered at %s", domain, definition.Name)
			}
		}
	}
	if _, err := RequiredManifestPhaseForProfile("smoke", "fd-b", "smoke-session"); err != nil {
		t.Fatal(err)
	}
	if _, err := RequiredManifestPhaseForProfile("smoke", "fd-b", "production-load-active"); err == nil {
		t.Fatal("formal checkpoint accepted by smoke profile")
	}
}
