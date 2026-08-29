package operations

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/releasecatalog"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetry"
	"github.com/google/uuid"
)

func rolloutTestCatalog(t *testing.T) *releasecatalog.Catalog {
	t.Helper()
	digestA := make([]byte, 32)
	digestB := make([]byte, 32)
	for index := range digestB {
		digestB[index] = byte(index + 1)
	}
	manifest := filepath.Join(t.TempDir(), "agent-releases.json")
	manifestJSON := `{"releases":[{"version":"2.0.0","architecture":"amd64","package_sha256":"` +
		hex.EncodeToString(digestA) +
		`"},{"version":"2.0.0","architecture":"arm64","package_sha256":"` +
		hex.EncodeToString(digestB) +
		`"},{"version":"1.9.0","architecture":"amd64","package_sha256":"` +
		hex.EncodeToString(digestA) +
		`"}]}`
	if err := os.WriteFile(manifest, []byte(manifestJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, err := releasecatalog.Load(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func TestRolloutBatchForOrdinal(t *testing.T) {
	cases := []struct {
		ordinal   int
		batchSize int
		want      int
	}{
		{0, 5, 0},
		{1, 5, 1},
		{5, 5, 1},
		{6, 5, 2},
		{1, 1, 1},
		{2, 1, 2},
	}
	for _, testCase := range cases {
		if got := rolloutBatchForOrdinal(testCase.ordinal, testCase.batchSize); got != testCase.want {
			t.Fatalf("rolloutBatchForOrdinal(%d,%d) = %d, want %d", testCase.ordinal, testCase.batchSize, got, testCase.want)
		}
	}
}

func TestSortAndDedupeNodeIDs(t *testing.T) {
	first := uuid.Must(uuid.NewV7())
	second := uuid.Must(uuid.NewV7())
	unsorted := []uuid.UUID{second, first, second, first}
	sorted, err := sortAndDedupeNodeIDs(unsorted)
	if err != nil {
		t.Fatal(err)
	}
	if len(sorted) != 2 {
		t.Fatalf("deduped length = %d, want 2", len(sorted))
	}
	for index := 1; index < len(sorted); index++ {
		if sorted[index-1].String() >= sorted[index].String() {
			t.Fatalf("node ids are not in stable sorted order: %v", sorted)
		}
	}
	if _, err := sortAndDedupeNodeIDs([]uuid.UUID{uuid.Nil}); err != ErrRolloutInvalid {
		t.Fatalf("uuid.Nil rejection = %v, want ErrRolloutInvalid", err)
	}
	uuidV4 := uuid.Must(uuid.NewRandom())
	if _, err := sortAndDedupeNodeIDs([]uuid.UUID{uuidV4}); err != ErrRolloutInvalid {
		t.Fatalf("uuid v4 rejection = %v, want ErrRolloutInvalid", err)
	}
}

func TestRolloutNodeEligibility(t *testing.T) {
	service := &Service{releaseCatalog: rolloutTestCatalog(t)}
	now := time.Now().UTC()
	nodeID := uuid.Must(uuid.NewV7())
	base := rolloutNodeObservation{
		NodeID:          nodeID,
		Status:          "active",
		Architecture:    "amd64",
		AgentVersion:    "1.2.0",
		LastHeartbeatAt: now.Add(-10 * time.Second),
		CapabilityOK:    true,
	}
	cases := []struct {
		name       string
		mutate     func(*rolloutNodeObservation)
		target     string
		wantReason string
		wantOK     bool
	}{
		{"eligible", func(*rolloutNodeObservation) {}, "2.0.0", "", true},
		{"not trusted", func(node *rolloutNodeObservation) { node.Status = "revoked" }, "2.0.0", "not_trusted", false},
		{"offline", func(node *rolloutNodeObservation) { node.Status = "offline" }, "2.0.0", "offline", false},
		{"stale", func(node *rolloutNodeObservation) {
			node.LastHeartbeatAt = now.Add(-telemetry.OfflineAfter - time.Minute)
		}, "2.0.0", "stale", false},
		{"missing release metadata", func(node *rolloutNodeObservation) { node.Architecture = "" }, "2.0.0", "missing_release_metadata", false},
		{"unknown version", func(node *rolloutNodeObservation) { node.AgentVersion = "" }, "2.0.0", "unknown_version", false},
		{"already current", func(node *rolloutNodeObservation) { node.AgentVersion = "2.0.0" }, "2.0.0", "already_current", false},
		{"ahead", func(node *rolloutNodeObservation) { node.AgentVersion = "3.0.0" }, "2.0.0", "ahead", false},
		{"missing capability", func(node *rolloutNodeObservation) { node.CapabilityOK = false }, "2.0.0", "missing_capability", false},
		{"upgrade in progress", func(node *rolloutNodeObservation) { node.UpgradeActive = true }, "2.0.0", "upgrade_in_progress", false},
	}
	for _, testCase := range cases {
		node := base
		testCase.mutate(&node)
		reason, _, ok := service.rolloutNodeEligibility(now, node, testCase.target)
		if ok != testCase.wantOK || reason != testCase.wantReason {
			t.Fatalf("%s: reason=%q ok=%v, want %q/%v", testCase.name, reason, ok, testCase.wantReason, testCase.wantOK)
		}
	}
}

func TestRolloutTraceparentIsValid(t *testing.T) {
	rolloutID, nodeID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	traceparent := rolloutTraceparent(rolloutID, nodeID)
	pattern := regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-01$`)
	if !pattern.MatchString(traceparent) {
		t.Fatalf("traceparent %q is not a valid traceparent", traceparent)
	}
	if again := rolloutTraceparent(rolloutID, nodeID); again != traceparent {
		t.Fatalf("traceparent must be stable per rollout and node")
	}
	if other := rolloutTraceparent(rolloutID, uuid.Must(uuid.NewV7())); other == traceparent {
		t.Fatalf("different nodes must derive different traceparents")
	}
}
