package telemetry

import "testing"

func TestClassifyAgentVersion(t *testing.T) {
	tests := []struct {
		name        string
		observed    string
		recommended string
		want        string
	}{
		{name: "equal", observed: "0.2.0", recommended: "0.2.0", want: AgentVersionStateCurrent},
		{name: "equal with v prefix", observed: "v0.2.0", recommended: "0.2.0", want: AgentVersionStateCurrent},
		{name: "older major", observed: "0.1.9", recommended: "0.2.0", want: AgentVersionStateUpgradeAvailable},
		{name: "older patch", observed: "0.2.1", recommended: "0.2.2", want: AgentVersionStateUpgradeAvailable},
		{name: "numeric minor comparison", observed: "0.2.9", recommended: "0.2.10", want: AgentVersionStateUpgradeAvailable},
		{name: "ahead", observed: "0.3.0", recommended: "0.2.0", want: AgentVersionStateAhead},
		{name: "observed missing", observed: "", recommended: "0.2.0", want: AgentVersionStateUnknown},
		{name: "observed invalid", observed: "test", recommended: "0.2.0", want: AgentVersionStateUnknown},
		{name: "recommendation missing", observed: "0.2.0", recommended: "", want: AgentVersionStateUnknown},
		{name: "recommendation invalid", observed: "0.2.0", recommended: "latest", want: AgentVersionStateUnknown},
		{name: "prerelease below release", observed: "0.2.0-rc.1", recommended: "0.2.0", want: AgentVersionStateUpgradeAvailable},
		{name: "release ahead of prerelease", observed: "0.2.0", recommended: "0.2.0-rc.1", want: AgentVersionStateAhead},
		{name: "equal prereleases", observed: "0.2.0-rc.1", recommended: "0.2.0-rc.1", want: AgentVersionStateCurrent},
		{name: "newer prerelease", observed: "0.2.0-rc.1", recommended: "0.2.0-rc.2", want: AgentVersionStateUpgradeAvailable},
		{name: "build metadata ignored", observed: "0.2.0+build.7", recommended: "0.2.0", want: AgentVersionStateCurrent},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ClassifyAgentVersion(test.observed, test.recommended); got != test.want {
				t.Fatalf("ClassifyAgentVersion(%q, %q) = %q, want %q", test.observed, test.recommended, got, test.want)
			}
		})
	}
}

func TestApplyAgentVersionState(t *testing.T) {
	service := NewWithRecommendedAgentVersion(nil, "0.2.0")
	node := Node{AgentVersion: "0.1.1"}
	service.applyAgentVersionState(&node)
	if node.AgentVersionState != AgentVersionStateUpgradeAvailable || node.RecommendedAgentVersion != "0.2.0" {
		t.Fatalf("unexpected derived node state: %#v", node)
	}
	unconfigured := New(nil)
	unconfigured.applyAgentVersionState(&node)
	if node.AgentVersionState != AgentVersionStateUnknown || node.RecommendedAgentVersion != "" {
		t.Fatalf("missing recommendation must classify unknown: %#v", node)
	}
}
