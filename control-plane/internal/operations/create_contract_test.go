package operations

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/GentleKingson/ocservia/control-plane/internal/configprofile"
	"google.golang.org/protobuf/types/known/timestamppb"
	"os"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

// These fingerprints freeze the pre-refactor idempotency JSON, semantic hash,
// and signed authorization for each operation family. The P12 receipt comes
// from the previous artifact record, which the migration copies to operations.
func TestCreateContractFingerprints(t *testing.T) {
	id := uuid.MustParse("01900000-0000-7000-8000-000000000001")
	candidate := []byte("config")
	digest := sha256.Sum256(candidate)
	now := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	cases := []CreateRequest{
		{Payload: SyntheticNoopPayload{}},
		{Payload: SyntheticEchoPayload{Message: "hello"}},
		{Payload: SessionDisconnectPayload{SessionID: "42", BootID: id.String()}},
		{Payload: SessionTerminatePayload{SessionID: "42", BootID: id.String()}},
		{Payload: IPBanRemovePayload{IP: "192.0.2.1"}},
		{Payload: ServiceReloadPayload{}},
		{Payload: CertificateCSRPayload{CertificateID: id, CommonName: "node.example.com", DNSNames: []string{"node.example.com"}, KeyBits: 2048}},
		{Payload: CertificateP12Payload{CertificateID: id, CertificateVersion: 2, CertificateChain: bytes.Repeat([]byte{3}, 64), SealedPassword: &agentv1.SealedSecretV1{Version: 1, Purpose: agentv1.SealedSecretPurpose_SEALED_SECRET_PURPOSE_CERTIFICATE_P12_PASSWORD, KeyId: "secret", Ciphertext: bytes.Repeat([]byte{4}, 32)}, ArtifactID: id, ArtifactMetadata: &ArtifactMetadata{TokenSHA256: digest[:], ExpiresAt: now.Add(15 * time.Minute)}}},
		{Payload: CertificateRevokePayload{CertificateID: id, CertificateVersion: 2, RevocationReason: "rotate", HoldDispatch: true}},
		{Payload: AgentUpgradePayload{TargetVersion: "1.2.3", PackageSHA256: digest[:], Architecture: "arm64"}},
	}
	fixtureData, err := os.ReadFile("../../../testdata/complete-config-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture map[string]string
	if err := json.Unmarshal(fixtureData, &fixture); err != nil {
		t.Fatal(err)
	}
	encodedCandidate, err := hex.DecodeString(fixture["candidate_proto"])
	if err != nil {
		t.Fatal(err)
	}
	var complete agentv1.CompleteConfigCandidate
	if err := proto.Unmarshal(encodedCandidate, &complete); err != nil {
		t.Fatal(err)
	}
	complete.NodeId, complete.ExpectedRevision = id[:], 3
	normalized, err := configprofile.Normalize(&complete)
	if err != nil {
		t.Fatal(err)
	}
	cases = append(cases,
		CreateRequest{Payload: ConfigPlanPayload{Candidate: normalized, OcservVersion: "1.2.3", PlanMetadata: &ConfigPlanMetadata{TemplateName: "complete", CandidateRedacted: "config"}}},
		CreateRequest{Payload: ConfigApplyPayload{Candidate: normalized, ExpectedCurrentHash: bytes.Repeat([]byte{2}, 32), DesiredRevision: 4, MaterializedHash: bytes.Repeat([]byte{3}, 32), PlanExpiresAt: timestamppb.New(now.Add(time.Minute)), ApplyMetadata: &ConfigApplyMetadata{PlanID: id}}},
	)
	legacyUpgrade := cases[9]
	legacyPayload := legacyUpgrade.Payload.(AgentUpgradePayload)
	legacyPayload.upgradeCapability = "ocserv.agent.upgrade.v1"
	legacyUpgrade.Payload = legacyPayload
	cases = append(cases, legacyUpgrade)
	expected := map[string]string{
		"noop":                  "synthetic_noop/573987eed37239bc16f39149f4198cf0cacc0a9ba89a3621b4ab54ce9d06314e/c7b6c8496c488b80c85be1d07855eb8c1cea2ba5de0c6eec0609d6d1b53a31c4245668cd51c4598eb55b4b75b9e0d10ea5c0eb847ed9dd6aca56b1e82c2a7806",
		"echo":                  "synthetic_echo/9a20c470875e96c14a983fce6e74806f9c23bd1176331e2e2c91fc5cafee61dc/d3942a346f2b3f8362b856025ea061ec2795b0cb225b6fad816ce51ddd26a20ea58c10fee5439fc15df7d44aacc371c8fe7cd72b0c8726c2afbd43dadc2b9806",
		"session_disconnect":    "session_disconnect/60e77f69620bea0b35027ed3ed129a67311275cef164eb48fbda4862738f0f3f/c64b044d59072ad5eba577847b08e708dd4d945d778269028fe0ecdf57101873b65bf5dd78e5f0afdfc6f95c44fc8431e25c99b4b411b08b18c571007408530b",
		"session_terminate":     "session_terminate/5a27b9e03490becb06af89e8b9a5d41da7a18da188c1d02f8a991f09806b7a3c/76147366d2e0c9bc7c48d28819b89e98343fd52acc0678c76587c4f7b39eb0d29dd9c2f19ae41f21e36d9a853d0868fa4085bd028cc16a52f830586aadd80308",
		"ip_ban_remove":         "ip_ban_remove/1a14084db10e18b71a81b28281c3b571f46074f809550c57974e22c4641b66db/a00a63dbda4cb58b506a068390f74532c5b0e439478f113b6702db8ecd78fe54bfd359e0d8ce3b258fcebe3ada3628cd32c52d82fd474d1869dd82301c372e05",
		"service_reload":        "service_reload/ab65f2d6b56952c7be1c198a82a1899d4037c2d756f7a880a48113d19364e8c5/246e120d025537a670fb07656f5562d8acf90b60218dd1a4ccbc4baef1b7e4551ef952b7cc4512702d6e02706ecd8c1f9f4d96c1a4233d237d61b0c4fdef390a",
		"certificate_csr":       "certificate_csr/6cd092c14db95746118a07ad3bf9d070e3a0cc3a7f8a6d7973a6e472fbd28f1d/d87ad0c601504c6cfb9a4dedfb913d93d770b17793dce9b6990b975faff69e938bcffb7872f26d84a8a484f2098350686223322e122ba8c5e57deec26115e40b",
		"certificate_p12":       "certificate_p12/01c964a549c7d845da7b1d3f580e43a67db73e87e36dd2c7f259d9bba5436f7f/8724e2a42bd04208aa16504004afe8639e2a968f6b3f2efbccfacc5c765153983f423952e31298ad0a21bd0bc060fbcbe2c6ca8e25380579cf94dca84d32620c",
		"certificate_revoke":    "certificate_revoke/eb1b44684a03ca61b7e83968c8153fd522d682ad77947c3eb0359c0ba8914b72/5b37e70a0bf0a6cd13f71873f50fb64780ff0dffb624fc8f120b908b940a357fc2d99c3625bf4ed7c25d2ea8106974b96748520dcaa938f225dc0bf291e1b100",
		"agent_upgrade":         "agent_upgrade/0ebc35874ec113db09684e3a6e6984fb4bd19625bc31d5b1172e70e3cda6b7fc/3061c87bac685a45a0658611d33a0900d4b24463e6da486dc5d7ee8264eb04769683ab5a537d5519feeb5971e6bbe587e738a2c54a6f770bc811aba23a52d401",
		"complete_config_plan":  "config_plan/57ded24c4210f7060e5bd79cad781fabc9769466048db6d41340c4171f77922f/e50842778e032fdd4129726a878fa1ca1c43e88ee5f7efa9d4bb0b370b533c6703721185fb50ac2acb126900b921c7974b2ca2185e9fd9fee288e4ab6ed2d509",
		"complete_config_apply": "config_apply/549a11ee7f2daef53be87697baeedae48356db76ef4c560d4fbd357172cb5819/cf398e0ca6ff07bd69f3c572e9538003649d139dace3880d85cef5da4bc15a5e495db0d25ba69f59763cf2076e056754613d8aa54ef5ec44f1fdaf2edd29170c",
		"legacy_agent_upgrade":  "agent_upgrade/0ebc35874ec113db09684e3a6e6984fb4bd19625bc31d5b1172e70e3cda6b7fc/2170720b79775f28f7c91d454956373bbd7cada99b8e6efd30a936ece7ce7b73162a3ae20f047502c30591e587b8ef879d14bc0f677ca65dea9f226c4533d503",
	}
	for _, request := range cases {
		request.NodeID, request.ExpectedVersion, request.TTL = id, 3, time.Minute
		request.IdempotencyKey, request.RequestID, request.Traceparent = "stable", "attempt", testTraceparent
		request.ActorID, request.ActorIdentityID, request.ActorSessionID = "operator", id, id
		request.Action, request.Reason = "operation.create", "support"
		switch request.Payload.kind() {
		case SessionDisconnect:
			request.Action = "session.disconnect"
		case SessionTerminate:
			request.Action = "session.terminate"
		case IPBanRemove:
			request.Action = "ip_ban.remove"
		case ServiceReload:
			request.Action = "service.reload"
		case ConfigPlan:
			request.Action = "config.plan"
		case ConfigApply:
			request.Action = "config.apply"
		case CertificateCSR:
			request.Action = "certificate.issue"
		case CertificateP12:
			request.Action = "certificate.private_key.export"
		case CertificateRevoke:
			request.Action = "certificate.revoke"
		case AgentUpgrade:
			request.Action = "agent.upgrade"
		}
		data, payloadType, err := marshalEnvelope(request, id, id, 7, now, now.Add(time.Minute), testCommandSigner(t))
		if err != nil {
			t.Fatal(request.Payload.kind(), err)
		}
		var envelope agentv1.CommandEnvelope
		if err := proto.Unmarshal(data, &envelope); err != nil {
			t.Fatal(err)
		}
		got := fmt.Sprintf("%s/%x/%x", payloadType, envelope.SemanticPayloadSha256, envelope.Authorization.Signature)
		key := string(request.Payload.kind())
		switch p := request.Payload.(type) {
		case ConfigPlanPayload, ConfigApplyPayload:
			key = "complete_" + key
		case AgentUpgradePayload:
			if p.upgradeCapability != "" {
				key = "legacy_" + key
			}
		}

		want := expected[key]
		if got != want {
			t.Errorf("%s contract changed: %s", key, got)
		}
	}
}
