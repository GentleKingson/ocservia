package commandauth

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type strictWireFixtures struct {
	Commands               map[string]string `json:"commands"`
	TimestampDescriptorHex string            `json:"timestamp_descriptor_hex"`
}

// These Go-encoded frames are also decoded by the Rust production strict decoder.
func TestStrictWireCommandFixtures(t *testing.T) {
	data, err := os.ReadFile("../../../testdata/command-strict-wire.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures strictWireFixtures
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	secret := &agentv1.SealedSecretV1{Version: 1, Purpose: 1, KeyId: "test-key", Ciphertext: []byte("sealed-test-data")}
	p12Secret := &agentv1.SealedSecretV1{Version: 1, Purpose: 2, KeyId: "test-key", Ciphertext: []byte("sealed-test-data")}
	commands := map[string]*agentv1.CommandEnvelope{
		"user_create":          {Payload: &agentv1.CommandEnvelope_UserCreate{UserCreate: &agentv1.UserCreate{Username: "alice", DesiredRevision: 42, SealedPasswordV1: secret}}},
		"password_rotate":      {Payload: &agentv1.CommandEnvelope_UserPasswordRotate{UserPasswordRotate: &agentv1.UserPasswordRotate{Username: "alice", DesiredRevision: 42, SealedPasswordV1: secret}}},
		"config_plan_zero":     {Payload: &agentv1.CommandEnvelope_ConfigPlan{ConfigPlan: &agentv1.ConfigPlan{Candidate: []byte("config"), CandidateHash: []byte("hash")}}},
		"config_plan_revision": {Payload: &agentv1.CommandEnvelope_ConfigPlan{ConfigPlan: &agentv1.ConfigPlan{Candidate: []byte("config"), CandidateHash: []byte("hash"), ExpectedRevision: 42}}},
		"certificate_p12":      {Payload: &agentv1.CommandEnvelope_CertificateP12{CertificateP12: &agentv1.CertificateP12{CertificateId: []byte("certificate"), ArtifactId: []byte("artifact"), SealedPasswordV1: p12Secret, CertificateVersion: 42, ArtifactExpiresAt: &timestamppb.Timestamp{Seconds: 1700000060, Nanos: 123}}}},
		"certificate_revoke":   {Payload: &agentv1.CommandEnvelope_CertificateRevoke{CertificateRevoke: &agentv1.CertificateRevoke{CertificateId: []byte("certificate"), Reason: "rotation", CertificateVersion: 42}}},
	}
	for name, command := range fullStrictWireCommands(secret, p12Secret) {
		commands["full_"+name] = command
	}
	for _, command := range commands {
		command.ProtocolVersion = "1.1"
	}
	assertStrictWireFieldCoverage(t, commands)
	// The generated Rust package descriptor omits imported well-known types.
	timestampDescriptor, err := proto.MarshalOptions{Deterministic: true}.Marshal(
		protodesc.ToDescriptorProto((&timestamppb.Timestamp{}).ProtoReflect().Descriptor()))
	if err != nil {
		t.Fatal(err)
	}
	// Explicit regeneration still requires independent Rust value/policy checks.
	if os.Getenv("UPDATE_STRICT_WIRE_FIXTURES") == "1" {
		generated := make(map[string]string, len(commands))
		for name, command := range commands {
			wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(command)
			if err != nil {
				t.Fatal(err)
			}
			generated[name] = hex.EncodeToString(wire)
		}
		fixtures = strictWireFixtures{Commands: generated, TimestampDescriptorHex: hex.EncodeToString(timestampDescriptor)}
		data, err := json.MarshalIndent(fixtures, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile("../../../testdata/command-strict-wire.json", append(data, '\n'), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if fixtures.TimestampDescriptorHex != hex.EncodeToString(timestampDescriptor) {
		t.Error("Go Timestamp descriptor fixture drift")
	}
	if len(fixtures.Commands) != len(commands) {
		t.Errorf("fixture count = %d, want %d", len(fixtures.Commands), len(commands))
	}
	for name, command := range commands {
		t.Run(name, func(t *testing.T) {
			wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(command)
			if err != nil {
				t.Fatal(err)
			}
			if actual := hex.EncodeToString(wire); actual != fixtures.Commands[name] {
				t.Errorf("Go wire fixture %s: got %s, want %s", name, actual, fixtures.Commands[name])
			}
		})
	}
}

// These are raw-wire fixtures, not executable commands: deprecated fields and
// dummy signatures deliberately exercise all accepted tags without authorizing effects.
func fullStrictWireCommands(secret, p12Secret *agentv1.SealedSecretV1) map[string]*agentv1.CommandEnvelope {
	commands := map[string]*agentv1.CommandEnvelope{
		"session_disconnect": {Payload: &agentv1.CommandEnvelope_SessionDisconnect{SessionDisconnect: &agentv1.SessionDisconnect{SessionId: "session", BootId: "boot"}}},
		"session_terminate":  {Payload: &agentv1.CommandEnvelope_SessionTerminate{SessionTerminate: &agentv1.SessionTerminate{SessionId: "session", BootId: "boot"}}},
		"ip_ban_remove":      {Payload: &agentv1.CommandEnvelope_IpBanRemove{IpBanRemove: &agentv1.IpBanRemove{Ip: "192.0.2.1"}}},
		"user_create":        {Payload: &agentv1.CommandEnvelope_UserCreate{UserCreate: &agentv1.UserCreate{Username: "alice", SealedPassword: []byte("legacy"), SecretKeyId: "legacy-key", DesiredRevision: 42, SealedPasswordV1: secret}}},
		"user_disable":       {Payload: &agentv1.CommandEnvelope_UserDisable{UserDisable: &agentv1.UserDisable{Username: "alice", DesiredRevision: 42}}},
		"user_enable":        {Payload: &agentv1.CommandEnvelope_UserEnable{UserEnable: &agentv1.UserEnable{Username: "alice", DesiredRevision: 42}}},
		"password_rotate":    {Payload: &agentv1.CommandEnvelope_UserPasswordRotate{UserPasswordRotate: &agentv1.UserPasswordRotate{Username: "alice", SealedPassword: []byte("legacy"), SecretKeyId: "legacy-key", DesiredRevision: 42, SealedPasswordV1: secret}}},
		"group_apply":        {Payload: &agentv1.CommandEnvelope_GroupApply{GroupApply: &agentv1.GroupApply{GroupName: "admins", Members: []string{"alice", "bob"}, DesiredRevision: 42}}},
		"config_plan":        {Payload: &agentv1.CommandEnvelope_ConfigPlan{ConfigPlan: &agentv1.ConfigPlan{Candidate: []byte("config"), CandidateHash: []byte("hash"), ExpectedRevision: 42}}},
		"config_apply":       {Payload: &agentv1.CommandEnvelope_ConfigApply{ConfigApply: &agentv1.ConfigApply{CandidateHash: []byte("hash"), Candidate: []byte("config"), ExpectedCurrentHash: []byte("current"), DesiredRevision: 42}}},
		"certificate_csr":    {Payload: &agentv1.CommandEnvelope_CertificateCsr{CertificateCsr: &agentv1.CertificateCsr{CertificateId: []byte("certificate"), CommonName: "vpn.example.test", DnsNames: []string{"vpn.example.test", "alt.example.test"}, KeyBits: 3072}}},
		"certificate_p12":    {Payload: &agentv1.CommandEnvelope_CertificateP12{CertificateP12: &agentv1.CertificateP12{CertificateId: []byte("certificate"), CertificateChainPem: []byte("chain"), SealedPassword: []byte("legacy"), SecretKeyId: "legacy-key", ArtifactId: []byte("artifact"), SealedPasswordV1: p12Secret, CertificateVersion: 42, ArtifactExpiresAt: &timestamppb.Timestamp{Seconds: 1700000060, Nanos: 123}}}},
		"certificate_revoke": {Payload: &agentv1.CommandEnvelope_CertificateRevoke{CertificateRevoke: &agentv1.CertificateRevoke{CertificateId: []byte("certificate"), Reason: "rotation", CertificateVersion: 42}}},
		"agent_upgrade":      {Payload: &agentv1.CommandEnvelope_AgentUpgrade{AgentUpgrade: &agentv1.AgentUpgrade{TargetVersion: "1.2.3", PackageSha256: []byte("package-hash"), Architecture: "arm64"}}},
		"service_reload":     {Payload: &agentv1.CommandEnvelope_ServiceReload{ServiceReload: &agentv1.ServiceReload{}}},
		"simulation_probe":   {Payload: &agentv1.CommandEnvelope_SimulationProbe{SimulationProbe: &agentv1.SimulationProbe{HeartbeatCount: 3, DelayMillis: 17, DuplicateEvent: true, ReturnError: true, DisconnectAfter: true}}},
		"synthetic_noop":     {Payload: &agentv1.CommandEnvelope_SyntheticNoop{SyntheticNoop: &agentv1.SyntheticNoop{}}},
		"synthetic_echo":     {Payload: &agentv1.CommandEnvelope_SyntheticEcho{SyntheticEcho: &agentv1.SyntheticEcho{Message: "hello"}}},
	}
	command := commands["synthetic_echo"]
	command.MessageId = []byte("message")
	command.CommandId = []byte("command")
	command.IdempotencyKey = []byte("idempotency")
	command.NodeId = []byte("node")
	command.Sequence = 17
	command.IssuedAt = &timestamppb.Timestamp{Seconds: 1700000000, Nanos: 123}
	command.ExpiresAt = &timestamppb.Timestamp{Seconds: 1700000060, Nanos: 456}
	command.ExpectedRevision = 42
	command.Traceparent = "trace"
	command.ActorId = "actor"
	command.Reason = "reason"
	command.DeliveryMode = agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY
	command.SemanticPayloadHashVersion = agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V2
	command.SemanticPayloadSha256 = []byte("semantic-hash")
	command.OperationId = []byte("operation")
	command.Action = "synthetic.echo"
	command.RequiredCapability = "synthetic.v1"
	command.ApprovalId = []byte("approval")
	command.ApprovalRequestSha256 = []byte("approval-hash")
	command.Authorization = &agentv1.CommandAuthorizationProof{Version: 1, KeyId: "auth-key", Signature: []byte("auth-signature")}
	command.ConnectionFence = &agentv1.ConnectionFenceV2{
		SignatureVersion: 1, KeyId: "fence-key", FenceId: []byte("fence"), NodeId: []byte("node"), EndpointId: []byte("endpoint"), OwnerInstanceId: []byte("owner"),
		OwnerIncarnation: 11, OwnerEpoch: 12, ConnectionId: []byte("connection"), AuthorizationRevision: 13,
		Capabilities: []string{"ocserv.fencing.v2", "synthetic.v1"}, LeaseUntil: &timestamppb.Timestamp{Seconds: 1700000030, Nanos: 789},
		IssuedAt: command.IssuedAt, ExpiresAt: command.ExpiresAt, Signature: []byte("fence-signature"),
	}
	command.FenceBinding = &agentv1.FenceBindingV2{
		SignatureVersion: 1, KeyId: "binding-key", OperationKind: 1, OperationId: []byte("operation"), FenceId: []byte("fence"), NodeId: []byte("node"), EndpointId: []byte("endpoint"), OwnerInstanceId: []byte("owner"),
		OwnerIncarnation: 11, OwnerEpoch: 12, ConnectionId: []byte("connection"), AuthorizationRevision: 13,
		Capability: "synthetic.v1", IssuedAt: command.IssuedAt, ExpiresAt: command.ExpiresAt, Signature: []byte("binding-signature"),
	}
	return commands
}

func assertStrictWireFieldCoverage(t *testing.T, commands map[string]*agentv1.CommandEnvelope) {
	t.Helper()
	populated := make(map[protoreflect.FullName]bool)
	var record func(protoreflect.Message)
	record = func(message protoreflect.Message) {
		message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
			populated[field.FullName()] = true
			if field.Message() != nil {
				if field.IsList() {
					for i := 0; i < value.List().Len(); i++ {
						record(value.List().Get(i).Message())
					}
				} else {
					record(value.Message())
				}
			}
			return true
		})
	}
	for _, command := range commands {
		record(command.ProtoReflect())
	}
	visited := make(map[protoreflect.FullName]bool)
	var check func(protoreflect.MessageDescriptor)
	check = func(message protoreflect.MessageDescriptor) {
		if visited[message.FullName()] {
			return
		}
		visited[message.FullName()] = true
		for i := 0; i < message.Fields().Len(); i++ {
			field := message.Fields().Get(i)
			if !populated[field.FullName()] {
				t.Errorf("missing non-default Go wire fixture for %s; review protocol/capability/strict policy before adding it", field.FullName())
			}
			if field.Message() != nil {
				check(field.Message())
			}
		}
	}
	check((&agentv1.CommandEnvelope{}).ProtoReflect().Descriptor())
}
