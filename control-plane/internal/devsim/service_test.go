package devsim

import "testing"

func TestScenarioDefaultsPreservePresence(t *testing.T) {
	heartbeats, delay, err := normalizeScenario(SimulationScenario{})
	if err != nil || heartbeats != 3 || delay != 100 {
		t.Fatalf("defaults = (%d, %d, %v)", heartbeats, delay, err)
	}
	zero := uint32(0)
	if _, _, err := normalizeScenario(SimulationScenario{HeartbeatCount: &zero}); err == nil {
		t.Fatal("explicit zero heartbeat_count was accepted")
	}
	if heartbeats, delay, err := normalizeScenario(SimulationScenario{DelayMillis: &zero}); err != nil || heartbeats != 3 || delay != 0 {
		t.Fatalf("explicit zero delay = (%d, %d, %v)", heartbeats, delay, err)
	}
	thirtySeconds := uint32(30_000)
	if _, delay, err := normalizeScenario(SimulationScenario{DelayMillis: &thirtySeconds}); err != nil || delay != thirtySeconds {
		t.Fatalf("30-second heartbeat delay = (%d, %v)", delay, err)
	}
	overLimit := uint32(30_001)
	if _, _, err := normalizeScenario(SimulationScenario{DelayMillis: &overLimit}); err == nil {
		t.Fatal("delay above 30 seconds was accepted")
	}
}
