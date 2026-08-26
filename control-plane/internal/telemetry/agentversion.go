package telemetry

import (
	"strings"

	"golang.org/x/mod/semver"
)

// Derived agent version states reported by the node read model. The
// "unsupported" state is part of the API contract but is never derived until
// the repository defines an explicit agent compatibility floor.
const (
	AgentVersionStateCurrent          = "current"
	AgentVersionStateUpgradeAvailable = "upgrade_available"
	AgentVersionStateAhead            = "ahead"
	AgentVersionStateUnknown          = "unknown"
)

// ClassifyAgentVersion derives the fleet version state of an observed agent
// version against the operator-configured recommended version. Both values
// must be valid semantic versions; anything missing or unparsable classifies
// as unknown instead of failing the node read.
func ClassifyAgentVersion(observed, recommended string) string {
	observedVersion, ok := canonicalSemver(observed)
	if !ok {
		return AgentVersionStateUnknown
	}
	recommendedVersion, ok := canonicalSemver(recommended)
	if !ok {
		return AgentVersionStateUnknown
	}
	switch semver.Compare(observedVersion, recommendedVersion) {
	case 0:
		return AgentVersionStateCurrent
	case -1:
		return AgentVersionStateUpgradeAvailable
	default:
		return AgentVersionStateAhead
	}
}

// canonicalSemver accepts the agent-reported plain "0.1.0" form as well as the
// "v0.1.0" form required by golang.org/x/mod/semver.
func canonicalSemver(value string) (string, bool) {
	if !strings.HasPrefix(value, "v") {
		value = "v" + value
	}
	if !semver.IsValid(value) {
		return "", false
	}
	return value, true
}
