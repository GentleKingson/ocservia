package semanticpayload

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

func TestHashV2MatchesSharedAgentUpgradeVector(t *testing.T) {
	type vector struct {
		NodeIDHex             string `json:"node_id_hex"`
		AuthorizationRevision uint64 `json:"authorization_revision"`
		PayloadKind           uint32 `json:"payload_kind"`
		TargetVersion         string `json:"target_version"`
		Architecture          string `json:"architecture"`
		PackageSHA256Hex      string `json:"package_sha256_hex"`
		ExpectedSHA256        string `json:"expected_sha256"`
	}
	var fixture struct {
		Vector vector `json:"vector"`
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "semantic-payload-hash-v2-agent-upgrade.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Vector.PayloadKind != 128 {
		t.Fatalf("fixture payload kind = %d", fixture.Vector.PayloadKind)
	}
	nodeID, err := hex.DecodeString(fixture.Vector.NodeIDHex)
	if err != nil {
		t.Fatal(err)
	}
	packageSHA256, err := hex.DecodeString(fixture.Vector.PackageSHA256Hex)
	if err != nil {
		t.Fatal(err)
	}
	envelope := &agentv1.CommandEnvelope{
		NodeId:           nodeID,
		ExpectedRevision: fixture.Vector.AuthorizationRevision,
		Payload:          &agentv1.CommandEnvelope_AgentUpgrade{AgentUpgrade: &agentv1.AgentUpgrade{TargetVersion: fixture.Vector.TargetVersion, PackageSha256: packageSHA256, Architecture: fixture.Vector.Architecture}},
	}
	digest, err := HashV2(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(digest[:]) != fixture.Vector.ExpectedSHA256 {
		t.Fatalf("v2 agent upgrade digest=%x want=%s", digest, fixture.Vector.ExpectedSHA256)
	}
	pristine := func() *agentv1.AgentUpgrade {
		return &agentv1.AgentUpgrade{
			TargetVersion: fixture.Vector.TargetVersion,
			PackageSha256: append([]byte(nil), packageSHA256...),
			Architecture:  fixture.Vector.Architecture,
		}
	}
	for _, tc := range []struct {
		name   string
		mutate func(*agentv1.AgentUpgrade)
	}{
		{"target version", func(p *agentv1.AgentUpgrade) { p.TargetVersion = "1.2.4" }},
		{"package digest", func(p *agentv1.AgentUpgrade) { p.PackageSha256[0] ^= 0xff }},
		{"architecture", func(p *agentv1.AgentUpgrade) { p.Architecture = "amd64" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := pristine()
			tc.mutate(changed)
			envelope.Payload = &agentv1.CommandEnvelope_AgentUpgrade{AgentUpgrade: changed}
			changedDigest, err := HashV2(envelope)
			if err != nil || changedDigest == digest {
				t.Fatalf("agent upgrade %s was not bound: digest=%x err=%v", tc.name, changedDigest, err)
			}
		})
	}
	for _, tc := range []struct {
		name   string
		mutate func(*agentv1.AgentUpgrade)
	}{
		{"invalid semver", func(p *agentv1.AgentUpgrade) { p.TargetVersion = "1.2" }},
		{"v-prefixed semver", func(p *agentv1.AgentUpgrade) { p.TargetVersion = "v1.2.3" }},
		{"leading zero", func(p *agentv1.AgentUpgrade) { p.TargetVersion = "1.02.3" }},
		{"short digest", func(p *agentv1.AgentUpgrade) { p.PackageSha256 = make([]byte, 31) }},
		{"unsupported arch", func(p *agentv1.AgentUpgrade) { p.Architecture = "x86_64" }},
		{"empty version", func(p *agentv1.AgentUpgrade) { p.TargetVersion = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := pristine()
			tc.mutate(changed)
			envelope.Payload = &agentv1.CommandEnvelope_AgentUpgrade{AgentUpgrade: changed}
			if _, err := HashV2(envelope); err == nil {
				t.Fatalf("malformed agent upgrade payload was hashed: %s", tc.name)
			}
		})
	}
}

func TestAgentUpgradeReleaseIdentityGrammar(t *testing.T) {
	for _, version := range []string{"0.0.0", "1.2.3", "10.20.30", "1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-0.3.7", "1.0.0-x.7.z.92", "1.0.0+build.01", "1.0.0-rc.1+build.5"} {
		if !ValidAgentUpgradeTargetVersion(version) {
			t.Fatalf("expected valid target version: %q", version)
		}
	}
	for _, version := range []string{"", "1", "1.2", "1.2.3.4", "01.2.3", "1.02.3", "1.2.03", "v1.2.3", "1.2.3-01", "1.2.3-", "1.2.3+", "1.2.3-alpha..1", "1.2.3-alpha_1", "latest", "1.2.3 ", strings.Repeat("1", 200)} {
		if ValidAgentUpgradeTargetVersion(version) {
			t.Fatalf("expected invalid target version: %q", version)
		}
	}
	if !ValidAgentUpgradeArchitecture("amd64") || !ValidAgentUpgradeArchitecture("arm64") {
		t.Fatal("supported architectures were rejected")
	}
	for _, architecture := range []string{"", "x86_64", "aarch64", "AMD64", "arm64 "} {
		if ValidAgentUpgradeArchitecture(architecture) {
			t.Fatalf("expected invalid architecture: %q", architecture)
		}
	}
}
