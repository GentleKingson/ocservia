package configprofile_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"sort"
	"testing"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/configprofile"
	"github.com/GentleKingson/ocservia/control-plane/internal/semanticpayload"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func fixture() *agentv1.CompleteConfigCandidate {
	node := uuid.MustParse("019f6400-0000-7000-8000-000000000001")
	ref := uuid.MustParse("019f6400-0000-7000-8000-000000000002")
	candidate := &agentv1.CompleteConfigCandidate{NodeId: node[:], ExpectedRevision: 7}
	for name, value := range map[string]string{"auth": "plain[passwd=/etc/ocserv/ocpasswd]", "cookie-timeout": "600", "device": "vpns", "dns": "1.1.1.1", "ipv4-network": "10.208.0.0/24", "max-clients": "64", "max-same-clients": "2", "socket-file": "/run/ocserv.socket", "tcp-port": "44443", "udp-port": "0"} {
		candidate.Directives = append(candidate.Directives, &agentv1.CompleteConfigDirective{Name: name, Value: &agentv1.CompleteConfigDirective_Literal{Literal: value}})
	}
	for _, name := range []string{"server-cert", "server-key"} {
		candidate.Directives = append(candidate.Directives, &agentv1.CompleteConfigDirective{Name: name, Value: &agentv1.CompleteConfigDirective_Tls{Tls: &agentv1.NodeLocalTlsReference{SecretRefId: ref[:], Version: "2026-09-21", CertificateSha256: bytes.Repeat([]byte{0x31}, 32), SpkiSha256: bytes.Repeat([]byte{0x42}, 32)}}})
	}
	sort.Slice(candidate.Directives, func(i, j int) bool { return candidate.Directives[i].Name < candidate.Directives[j].Name })
	return candidate
}

func TestCompleteProfileVectors(t *testing.T) {
	candidate := fixture()
	canonical, err := configprofile.Canonical(candidate)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := configprofile.Hash(candidate)
	if err != nil {
		t.Fatal(err)
	}
	planID := uuid.MustParse("019f6400-0000-7000-8000-000000000003")
	nodeID := uuid.Must(uuid.FromBytes(candidate.NodeId))
	expiry := &timestamppb.Timestamp{Seconds: 1790000000, Nanos: 123000}
	materialized, current := bytes.Repeat([]byte{0x53}, 32), bytes.Repeat([]byte{0x64}, 32)
	approval, err := configprofile.ApprovalHash(planID, nodeID, hash[:], materialized, current, 7, expiry)
	if err != nil {
		t.Fatal(err)
	}
	plan := &agentv1.CommandEnvelope{NodeId: candidate.NodeId, ExpectedRevision: 42, SemanticPayloadHashVersion: 2, Payload: &agentv1.CommandEnvelope_CompleteConfigPlan{CompleteConfigPlan: &agentv1.CompleteConfigPlan{Candidate: candidate, CandidateHash: hash[:]}}}
	apply := &agentv1.CommandEnvelope{NodeId: candidate.NodeId, ExpectedRevision: 42, SemanticPayloadHashVersion: 2, ApprovalId: planID[:], ApprovalRequestSha256: approval, Payload: &agentv1.CommandEnvelope_CompleteConfigApply{CompleteConfigApply: &agentv1.CompleteConfigApply{Candidate: candidate, CandidateHash: hash[:], MaterializedHash: materialized, ExpectedCurrentHash: current, DesiredRevision: 8, PlanId: planID[:], PlanExpiresAt: expiry}}}
	wire := func(message proto.Message) string {
		b, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		return hex.EncodeToString(b)
	}
	semantic := func(envelope *agentv1.CommandEnvelope) string {
		h, err := semanticpayload.HashV2(envelope)
		if err != nil {
			t.Fatal(err)
		}
		return hex.EncodeToString(h[:])
	}
	values := map[string]string{"candidate_proto": wire(candidate), "canonical": hex.EncodeToString(canonical), "candidate_hash": hex.EncodeToString(hash[:]), "approval_hash": hex.EncodeToString(approval), "plan_proto": wire(plan), "plan_semantic_hash": semantic(plan), "apply_proto": wire(apply), "apply_semantic_hash": semantic(apply)}
	path := "../../../testdata/complete-config-v1.json"
	if os.Getenv("UPDATE_COMPLETE_CONFIG_FIXTURES") == "1" {
		encoded, err := json.MarshalIndent(values, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, append(encoded, '\n'), 0644); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var expected map[string]string
	if err = json.Unmarshal(data, &expected); err != nil {
		t.Fatal(err)
	}
	for name, actual := range values {
		if actual != expected[name] {
			t.Fatalf("%s mismatch: %s", name, actual)
		}
	}
	if _, err = semanticpayload.HashV1(plan); err == nil {
		t.Fatal("complete profile must not enter hash v1")
	}
	if _, err = semanticpayload.HashV1(apply); err == nil {
		t.Fatal("complete apply must not enter hash v1")
	}
	original := semantic(apply)
	for _, mutate := range []func(*agentv1.CommandEnvelope){
		func(v *agentv1.CommandEnvelope) { v.GetCompleteConfigApply().MaterializedHash[0] ^= 1 },
		func(v *agentv1.CommandEnvelope) { v.GetCompleteConfigApply().ExpectedCurrentHash[0] ^= 1 },
		func(v *agentv1.CommandEnvelope) { v.GetCompleteConfigApply().DesiredRevision++ },
		func(v *agentv1.CommandEnvelope) { v.GetCompleteConfigApply().PlanId[0] ^= 1 },
		func(v *agentv1.CommandEnvelope) { v.GetCompleteConfigApply().PlanExpiresAt.Seconds++ },
		func(v *agentv1.CommandEnvelope) { v.ApprovalId[0] ^= 1 },
		func(v *agentv1.CommandEnvelope) { v.ApprovalRequestSha256[0] ^= 1 },
	} {
		altered := proto.Clone(apply).(*agentv1.CommandEnvelope)
		mutate(altered)
		if semantic(altered) == original {
			t.Fatal("unbound apply field")
		}
	}
	apply.ExpiresAt = &timestamppb.Timestamp{Seconds: expiry.Seconds + 3600}
	if semantic(apply) != original {
		t.Fatal("authorization renewal changed command identity")
	}
	if hash == sha256.Sum256(materialized) {
		t.Fatal("logical identity must not become a physical hash")
	}
}

func TestFiniteProfileRejectsUnsafeOrIncompleteContent(t *testing.T) {
	for name, change := range map[string]func(*agentv1.CompleteConfigCandidate){
		"missing-device": func(c *agentv1.CompleteConfigCandidate) { c.Directives = append(c.Directives[:2], c.Directives[3:]...) },
		"duplicate": func(c *agentv1.CompleteConfigCandidate) {
			c.Directives[1] = proto.Clone(c.Directives[0]).(*agentv1.CompleteConfigDirective)
		},
		"unsorted": func(c *agentv1.CompleteConfigCandidate) {
			c.Directives[0], c.Directives[1] = c.Directives[1], c.Directives[0]
		},
		"arbitrary-auth": func(c *agentv1.CompleteConfigCandidate) {
			c.Directives[0].Value = &agentv1.CompleteConfigDirective_Literal{Literal: "plain[passwd=/tmp/passwords]"}
		},
		"newline": func(c *agentv1.CompleteConfigCandidate) {
			c.Directives[2].Value = &agentv1.CompleteConfigDirective_Literal{Literal: "vpns\ninclude=/tmp/config"}
		},
		"null-node":             func(c *agentv1.CompleteConfigCandidate) { c.NodeId = make([]byte, 16) },
		"old-revision-overflow": func(c *agentv1.CompleteConfigCandidate) { c.ExpectedRevision = 1 << 63 },
		"tls-version-path":      func(c *agentv1.CompleteConfigCandidate) { c.Directives[7].GetTls().Version = "../x" },
		"mismatched-version":    func(c *agentv1.CompleteConfigCandidate) { c.Directives[7].GetTls().Version = "v2" },
		"unbound-certificate":   func(c *agentv1.CompleteConfigCandidate) { c.Directives[7].GetTls().CertificateSha256 = nil },
		"literal-tls-path": func(c *agentv1.CompleteConfigCandidate) {
			c.Directives[7].Value = &agentv1.CompleteConfigDirective_Literal{Literal: "/etc/ocserv/cert.pem"}
		},
		"missing-dns": func(c *agentv1.CompleteConfigCandidate) {
			c.Directives[3].Value = &agentv1.CompleteConfigDirective_Literal{Literal: ""}
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := fixture()
			change(c)
			if _, err := configprofile.Canonical(c); err == nil {
				t.Fatal("unsafe candidate accepted")
			}
		})
	}
}
