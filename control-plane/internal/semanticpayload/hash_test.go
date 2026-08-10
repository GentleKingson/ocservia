package semanticpayload

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
)

func TestHashV2MatchesSharedConfigPlanVector(t *testing.T) {
	type vector struct {
		NodeIDHex             string `json:"node_id_hex"`
		AuthorizationRevision uint64 `json:"authorization_revision"`
		CandidateHashHex      string `json:"candidate_hash_hex"`
		ConfigExpected        uint64 `json:"config_expected_revision"`
		ExpectedSHA256        string `json:"expected_sha256"`
	}
	var fixture struct {
		Vector vector `json:"vector"`
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "semantic-payload-hash-v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	nodeID, err := hex.DecodeString(fixture.Vector.NodeIDHex)
	if err != nil {
		t.Fatal(err)
	}
	candidateHash, err := hex.DecodeString(fixture.Vector.CandidateHashHex)
	if err != nil {
		t.Fatal(err)
	}
	envelope := &agentv1.CommandEnvelope{
		NodeId:           nodeID,
		ExpectedRevision: fixture.Vector.AuthorizationRevision,
		Payload: &agentv1.CommandEnvelope_ConfigPlan{ConfigPlan: &agentv1.ConfigPlan{
			CandidateHash:    candidateHash,
			ExpectedRevision: fixture.Vector.ConfigExpected,
		}},
	}
	digest, err := HashV2(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(digest[:]) != fixture.Vector.ExpectedSHA256 {
		t.Fatalf("v2 digest=%x want=%s", digest, fixture.Vector.ExpectedSHA256)
	}
	changed := *envelope.GetConfigPlan()
	changed.ExpectedRevision++
	envelope.Payload = &agentv1.CommandEnvelope_ConfigPlan{ConfigPlan: &changed}
	changedDigest, err := HashV2(envelope)
	if err != nil || changedDigest == digest {
		t.Fatalf("ConfigPlan revision was not bound: digest=%x err=%v", changedDigest, err)
	}
}
