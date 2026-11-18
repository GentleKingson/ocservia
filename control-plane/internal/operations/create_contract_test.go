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
// and signed authorization for each operation family.
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
		{Payload: ConfigPlanPayload{Candidate: candidate, CandidateHash: digest[:], PlanRevision: 3, OcservVersion: "1.2.3", PlanCapabilities: []string{"ocserv.config.plan"}, PlanMetadata: &ConfigPlanMetadata{TemplateName: "default", CandidateRedacted: "config"}}},
		{Payload: ConfigApplyPayload{Candidate: candidate, CandidateHash: digest[:], ExpectedCurrentHash: bytes.Repeat([]byte{2}, 32), PlanRevision: 3, DesiredRevision: 4, ApplyMetadata: &ConfigApplyMetadata{PlanID: id}}},
		{Payload: CertificateCSRPayload{CertificateID: id, CommonName: "node.example.com", DNSNames: []string{"node.example.com"}, KeyBits: 2048}},
		{Payload: CertificateP12Payload{CertificateID: id, CertificateVersion: 2, CertificateChain: bytes.Repeat([]byte{3}, 64), SealedPassword: &agentv1.SealedSecretV1{Version: agentv1.SealedSecretVersion_SEALED_SECRET_VERSION_V1, Purpose: agentv1.SealedSecretPurpose_SEALED_SECRET_PURPOSE_CERTIFICATE_P12_PASSWORD, KeyId: "secret", Ciphertext: bytes.Repeat([]byte{4}, 32)}, ArtifactID: id, ArtifactMetadata: &ArtifactMetadata{TokenSHA256: digest[:], RequestHash: digest[:], ExpiresAt: now.Add(15 * time.Minute)}}},
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
	canonical, err := configprofile.Canonical(&complete)
	if err != nil {
		t.Fatal(err)
	}
	completeHash := sha256.Sum256(canonical)
	cases = append(cases,
		CreateRequest{Payload: ConfigPlanPayload{Candidate: canonical, CandidateHash: completeHash[:], CompleteCandidate: &complete, PlanRevision: 3, OcservVersion: "1.2.3", PlanCapabilities: []string{configprofile.PlanCapability}, PlanMetadata: &ConfigPlanMetadata{TemplateName: "complete", CandidateRedacted: "config"}}},
		CreateRequest{Payload: ConfigApplyPayload{Candidate: canonical, CandidateHash: completeHash[:], CompleteCandidate: &complete, PlanRevision: 3, ExpectedCurrentHash: bytes.Repeat([]byte{2}, 32), DesiredRevision: 4, MaterializedHash: bytes.Repeat([]byte{3}, 32), PlanExpiresAt: timestamppb.New(now.Add(time.Minute)), ApplyMetadata: &ConfigApplyMetadata{PlanID: id}}},
	)
	legacyUpgrade := cases[11]
	legacyPayload := legacyUpgrade.Payload.(AgentUpgradePayload)
	legacyPayload.upgradeCapability = "ocserv.agent.upgrade.v1"
	legacyUpgrade.Payload = legacyPayload
	cases = append(cases, legacyUpgrade)
	expected := map[string]string{"noop": "1e4d0ab8f43e50887489319786c0ba9b8836f1143c779b284fe9f2cc0881406d/synthetic_noop/88675966fd2517f294a3a57227e32fe1138a1e5ff487268809e424655bd3dd37/0acd5c2205abc574e6f658dadd8b74a32e5e3b4e3f383ec8b80aaba042fc9b326fb456b666149227a06f978ea355e411fcd7fd0e4186f43ee56bc8944af02703",
		"echo":                  "b64b25748fc3b1aea06211c456f2e7b3d2adfdf62972d61aa47580f3c24d08ab/synthetic_echo/504bbfb553df5ae9dac244e2e792511e4db87480f9bd1de6f3fc79d37afea7fe/a54ec6afcefb275685cc32cd748438c9c63b5e2ca9e8527140d758ab6caacec28e41de10edca068906653db17d62299187fc41e94af4be6c1d5ab3134718cd04",
		"session_disconnect":    "f904417356c972da2ba310754a58daa1f2fa4bb3ca582bffb96445c5c31be141/session_disconnect/4b1589f8152e3738971f45628ed042cb76328bdce025592a0872e1873f93bb41/b5960b9e3e85a66eefe667179d08605ec9e94412fbb44628c41ce671cc961ca28985f59c57a6e834ed001b9ab21b76cafa36b0c75812e5ae7479e1ecce51a609",
		"session_terminate":     "7029d41bcdbe7a071120e411935a26f5dff87cf7cbb5d4dceb1ee436a4f61604/session_terminate/d332c9b29f33405d0d78f904dc0e565edeacc3ccae2df7eb175d1ae3b684f7fd/c4e3bd0b1f419e53a4179cba354e753fd7d64f1a6bbcc7e4950462fc28a27382babffa964f75b253f2be01812a60b87ae77d9a13533d0b06678b85df70ccc809",
		"ip_ban_remove":         "df5007521c0bc61b15a4b91d51e1d9aa56eea6878edb4f5c3d2c02943bb6c0f3/ip_ban_remove/416a166b39ae82136774ac97f2cc75fa1d9bd2d3f34e9a01a8cbd50672b48dc2/61c788486029e4ebdb208b12e950ae3969f5f941f9ba7a7cf893369f17789648b491de5ef489c557f7ad46e917f8fa24d3b8a7cd44bc7b6871d8012157275006",
		"service_reload":        "8dcbf069ce285a721d0bf9d1b2b7c6a1481ebf6f3aaa41d118badb66ff78c3ec/service_reload/bbe6fac771f4e0dbe4ecd65a631c8955f37816081af59f47c70dcbc6d4dbae35/ed69069b835c1ae5e4b7324e3dde19e38a94d9a29c8bf753ab7f46a98feb3ccd18e690ff0273c70fd1095ddfd9ac144263cc2735a20989653047311dad644800",
		"config_plan":           "98938680fb0679e183acb8609068f73d31998c36393e50e74bd366f2444432e6/config_plan/2831c58be4ee2868514398402c7333055e5088f12fd35d6b1711d9a246ed3b00/2a53ba8026ee49052745976ad94a7134a59ef767fee307016c0e375373416eabea0ed14249773b4dec4a306795b6bd85a644727d449adbffbcd500345a1a4c02",
		"config_apply":          "ba1019230a66bc7760e9d990ce5d07fbd1e408e7ba4f101b4e303f5ada0e263f/config_apply/55fafc4b8df641561a37f8f12241c5eea5299d10ece06e027688f591e492b5df/73edae02a3b837ae2f6e753425833bd14e833442198e69555e5a686e4c1a8965d8754f8e42694d439ea3fd6b8d9c10c9cb7f5d6b9359c0572d5fd81834abc300",
		"certificate_csr":       "377d553acba7ed7bec93a5ce067808dc4f8945b461d272ee3a49270bca79229c/certificate_csr/8047784f5f5ccc753712924e6df3ae900ea1546baa3831500a3dcb40715c7003/6ceba27de68c5d5901e4a08d6a7ea1729e34f02582310681d2ec5b80ddcdd03641ca1eaebef58d32e189a67f49e17e555229e48a109d1426b85a927a9de12104",
		"certificate_p12":       "a11c1749bf407ccc97a4d89ed3e29978e78ac52fe423ba446d00439f6869eb32/certificate_p12/4dcb58827d08a060122fa2dc88766012beb1a2f619139cda955dec3fe1fa6bf8/4597d34aaf731c1f600ec2dc3d5c23e1ec5f918f50caea29917577854b415e1981b100b5de82ed5d0762c06c8406706a7c51ed97855906e7da09ce0133005b07",
		"certificate_revoke":    "e5539bd2bfc8c889828686ef3956e29fe880347b7db62e95d9cab638a3c0c735/certificate_revoke/a94171cbe3435a671f31a3fc0881430c2c0fe4bca7b1903f8a79c9d879c250eb/838f0ebcdc9ab018e874e84882ce208d1cd779f1b3147ec88fff78d4767ba46243f570890ea817c3570cdf1e385c0df74ffd080a0065c5c8c307414d135afa09",
		"agent_upgrade":         "1c150ba590da5e6c8361f42e34f95ce2a27f9f8471f8a0f2ae1d5a278db34f07/agent_upgrade/25e0aff9977f109cf10c9011df27b195dd7a69b36024577c54e7cbfc8facf525/1a1f42c141b947cb139bd7a07a0c7a1e7907fb25317928d28e34ba0c7a513dd0f9615afa5f90191ed09e919c462bbde74e1b8aae3886e3bede20f33135bc360e",
		"complete_config_plan":  "fb1598e0da6f8a9ee3ad7aabec50055dc714074eb9efbf2898cd0a3eb84721c8/config_plan/b7fe0ed26d3ad0115c29921006779eb3f35c25e98ee2b01fc17a3ec7a749c0be/1584808591a71cf766106727de5bc21d3c6e05d77d4fe06bc626879e28bc45d6ee8b1a926bf381ed6be45d9adf49c4b4168d0946b4c0b5c69d21361ca0c19806",
		"complete_config_apply": "b3e937dbe977e2a3db63f7e8c948fed0d38bbeb4b0186699f2743c80d069f2d0/config_apply/9bd82e62dca980b0f2d53c7b100b5fb0fdba1ab6b221495464c0a255c46f64dd/6ed949dc62ed182bb474e4da0e8a5076bf8ebf1f736d531dcf20e05b75bba10473aca03ed49f5400215876ef99988533938c2792fd4c78f0bd917f6051e80506",
		"legacy_agent_upgrade":  "1c150ba590da5e6c8361f42e34f95ce2a27f9f8471f8a0f2ae1d5a278db34f07/agent_upgrade/25e0aff9977f109cf10c9011df27b195dd7a69b36024577c54e7cbfc8facf525/212cb404ff2da7afc55a9c33f694e4aa425f453190f9e9eb193ae98d857cc82a4e012a7885e903d4346e1798818be74eae3694102c4ce7a2dfd9406d779f150a",
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
		got := fmt.Sprintf("%x/%s/%x/%x", requestHash(request), payloadType, envelope.SemanticPayloadSha256, envelope.Authorization.Signature)
		key := string(request.Payload.kind())
		switch p := request.Payload.(type) {
		case ConfigPlanPayload:
			if p.CompleteCandidate != nil {
				key = "complete_" + key
			}
		case ConfigApplyPayload:
			if p.CompleteCandidate != nil {
				key = "complete_" + key
			}
		case AgentUpgradePayload:
			if p.upgradeCapability != "" {
				key = "legacy_" + key
			}
		}

		if want := expected[key]; got != want {
			t.Fatalf("%s contract changed: %s", request.Payload.kind(), got)
		}
	}
}
