package approvals

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestAgentUpgradeBindingBindsExactReleaseIdentity(t *testing.T) {
	nodeID := uuid.Must(uuid.NewV7())
	packageSHA256 := bytes.Repeat([]byte{0x43}, 32)
	baseHash, baseSummary := AgentUpgradeBinding(nodeID, "1.2.3", packageSHA256, "amd64")
	var summary map[string]any
	if err := json.Unmarshal(baseSummary, &summary); err != nil {
		t.Fatal(err)
	}
	if summary["action"] != "agent.upgrade" || summary["node_id"] != nodeID.String() ||
		summary["target_version"] != "1.2.3" || summary["package_sha256"] != hex.EncodeToString(packageSHA256) ||
		summary["architecture"] != "amd64" {
		t.Fatalf("upgrade approval summary omitted part of the release identity: %s", baseSummary)
	}
	repeated, repeatedSummary := AgentUpgradeBinding(nodeID, "1.2.3", packageSHA256, "amd64")
	if !bytes.Equal(repeated, baseHash) || !bytes.Equal(repeatedSummary, baseSummary) {
		t.Fatal("upgrade approval binding is not deterministic")
	}
	bound := func(nodeID uuid.UUID, targetVersion string, digest []byte, architecture string) []byte {
		hash, _ := AgentUpgradeBinding(nodeID, targetVersion, digest, architecture)
		return hash
	}
	for _, tc := range []struct {
		name  string
		bound []byte
	}{
		{"target version", bound(nodeID, "1.2.4", packageSHA256, "amd64")},
		{"package digest", bound(nodeID, "1.2.3", bytes.Repeat([]byte{0x44}, 32), "amd64")},
		{"architecture", bound(nodeID, "1.2.3", packageSHA256, "arm64")},
		{"node", bound(uuid.Must(uuid.NewV7()), "1.2.3", packageSHA256, "amd64")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if bytes.Equal(tc.bound, baseHash) {
				t.Fatalf("upgrade approval hash ignored %s", tc.name)
			}
		})
	}
	// The upgrade binding is domain separated from the action-level generic
	// binding, so a generic "agent.upgrade" approval can never satisfy it.
	genericHash, _ := GenericBinding("agent.upgrade", "node", nodeID)
	if bytes.Equal(genericHash, baseHash) {
		t.Fatal("upgrade approval hash collided with the generic action-level binding")
	}
	if !strings.HasPrefix(string(baseSummary), "{") {
		t.Fatal("upgrade approval summary is not a JSON object")
	}
}
