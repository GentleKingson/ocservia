package mysql

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/attestationtest"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/localslice"
	"github.com/GentleKingson/ocservia/control-plane/internal/privdattestation"
	"github.com/GentleKingson/ocservia/control-plane/internal/semanticpayload"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Result projection fixtures seed already-dispatched intents. All result
// validation, proof verification, projection writes and commits use ingress
// with the restricted runtime principal, not direct store calls.
func runResultProjectionWorkflows(t *testing.T, owner, backend *Backend, workspace uuid.UUID, signer *commandauth.Signer) {
	ctx := context.Background()
	id := func() uuid.UUID { return uuid.Must(uuid.NewV7()) }
	now := time.Now().UTC().Truncate(time.Microsecond)
	stamp, expires := fixtureTimestamp(t, now), fixtureTimestamp(t, now.Add(time.Hour))
	trace := "00-11111111111111111111111111111111-2222222222222222-01"
	run := func(q string, args ...any) {
		t.Helper()
		if _, err := owner.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	actor, session := id(), id()
	run(`INSERT INTO identities(id,issuer,subject,created_at,updated_at)VALUES(?,'result',?,?,?)`, UUIDBytes(actor), actor.String(), stamp, stamp)
	run(`INSERT INTO auth_sessions(id,identity_id,expires_at,created_at)VALUES(?,?,?,?)`, UUIDBytes(session), UUIDBytes(actor), expires, stamp)
	digest, previous := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	service := localslice.NewBackend(backend, signer)
	for _, kind := range []string{"certificate_csr", "certificate_p12", "certificate_revoke", "agent_upgrade", "config_success", "config_rollback", "config_critical"} {
		t.Run(kind, func(t *testing.T) {
			run := func(q string, args ...any) {
				t.Helper()
				if _, err := owner.Exec(ctx, q, args...); err != nil {
					t.Fatal(err)
				}
			}
			node, operation, command, message, key, credential, certificate, artifact := id(), id(), id(), id(), id(), id(), id(), id()
			endpoint := sha256.Sum256(node[:])
			private := ed25519.NewKeyFromSeed(endpoint[:])
			public := private.Public().(ed25519.PublicKey)
			keyID := privdattestation.PublicKeyID(public)
			run(`INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at)VALUES(?,?,?,'active',?,?)`, UUIDBytes(node), UUIDBytes(workspace), node.String(), fixtureTimestamp(t, now), fixtureTimestamp(t, now))
			run(`INSERT INTO node_endpoint_keys(node_id,endpoint_id,state,bound_at)VALUES(?,?,'active',?)`, UUIDBytes(node), endpoint[:], fixtureTimestamp(t, now))
			run(`INSERT INTO privd_attestation_enrollment_credentials(id,node_id,secret_sha256,controller_nonce,credential_context_sha256,expires_at,consumed_at,created_by_identity_id,created_by_session_id,created_at)VALUES(?,?,?,?,?,?,?,?,?,?)`, UUIDBytes(credential), UUIDBytes(node), endpoint[:], endpoint[:], endpoint[:], expires, stamp, UUIDBytes(actor), UUIDBytes(session), stamp)
			run(`INSERT INTO node_privd_attestation_keys(node_id,key_id,algorithm,public_key,state,created_at,approved_at,activated_at,registration_credential_id)VALUES(?,?,'ed25519',?,'active',?,?,?,?)`, UUIDBytes(node), keyID, []byte(public), stamp, stamp, stamp, UUIDBytes(credential))
			envelope := &agentv1.CommandEnvelope{ProtocolVersion: commandauth.ProtocolVersion, MessageId: message[:], CommandId: command[:], OperationId: operation[:], NodeId: node[:], IdempotencyKey: key[:], IssuedAt: timestamppb.New(now), ExpiresAt: timestamppb.New(now.Add(time.Hour)), Sequence: 1, ExpectedRevision: 1, Traceparent: trace, ActorId: actor.String(), Reason: "result projection", DeliveryMode: agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_EXECUTE_OR_REPLAY}
			payloadType, want := kind, "succeeded"
			var resultPayload proto.Message
			switch kind {
			case "certificate_csr":
				envelope.Action, envelope.RequiredCapability = "certificate.issue", "ocserv.certificate.issue"
				envelope.Payload = &agentv1.CommandEnvelope_CertificateCsr{CertificateCsr: &agentv1.CertificateCsr{CertificateId: certificate[:], CommonName: "result.example.test", DnsNames: []string{"result.example.test"}, KeyBits: 2048}}
				rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
				if err != nil {
					t.Fatal(err)
				}
				csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "result.example.test"}, DNSNames: []string{"result.example.test"}}, rsaKey)
				if err != nil {
					t.Fatal(err)
				}
				pkixKey, err := x509.MarshalPKIXPublicKey(&rsaKey.PublicKey)
				if err != nil {
					t.Fatal(err)
				}
				hash := sha256.Sum256(pkixKey)
				resultPayload = &agentv1.CertificateCsrResult{CertificateId: certificate[:], CsrDer: csr, PublicKeySha256: hash[:]}
			case "certificate_p12":
				envelope.Action, envelope.RequiredCapability = "certificate.private_key.export", "ocserv.certificate.issue"
				envelope.Payload = &agentv1.CommandEnvelope_CertificateP12{CertificateP12: &agentv1.CertificateP12{CertificateId: certificate[:], ArtifactId: artifact[:], CertificateVersion: 1, ArtifactExpiresAt: timestamppb.New(now.Add(time.Hour)), SealedPasswordV1: &agentv1.SealedSecretV1{Version: agentv1.SealedSecretVersion_SEALED_SECRET_VERSION_V1, Purpose: agentv1.SealedSecretPurpose_SEALED_SECRET_PURPOSE_CERTIFICATE_P12_PASSWORD, KeyId: "isolated-result-key", Ciphertext: bytes.Repeat([]byte{1}, 64)}}}
				resultPayload = &agentv1.CertificateArtifactResult{CertificateId: certificate[:], ArtifactId: artifact[:], ArtifactSha256: digest, ArtifactSize: 4096}
			case "certificate_revoke":
				envelope.Action, envelope.RequiredCapability = "certificate.revoke", "ocserv.certificate.revoke"
				envelope.Payload = &agentv1.CommandEnvelope_CertificateRevoke{CertificateRevoke: &agentv1.CertificateRevoke{CertificateId: certificate[:], CertificateVersion: 1, Reason: "result revocation"}}
				resultPayload = &agentv1.CertificateRevokeResult{CertificateId: certificate[:], KeyRemoved: true}
			case "agent_upgrade":
				envelope.Action, envelope.RequiredCapability = "agent.upgrade", "ocserv.agent.upgrade.v2"
				envelope.Payload = &agentv1.CommandEnvelope_AgentUpgrade{AgentUpgrade: &agentv1.AgentUpgrade{TargetVersion: "2.0.0", Architecture: "amd64", PackageSha256: digest}}
				resultPayload = &agentv1.AgentUpgradeScheduledResult{OperationId: operation[:], TargetVersion: "2.0.0", PackageSha256: digest}
				want = "accepted"
			default:
				payloadType = "config_apply"
				envelope.Action, envelope.RequiredCapability = "config.apply", "ocserv.config.apply"
				envelope.Payload = &agentv1.CommandEnvelope_ConfigApply{ConfigApply: &agentv1.ConfigApply{CandidateHash: digest, ExpectedCurrentHash: previous, DesiredRevision: 2}}
				v := &agentv1.ConfigApplyResult{CandidateHash: digest, PreviousHash: previous, ObservedHash: digest, AppliedRevision: 2, Healthy: true}
				if kind == "config_rollback" {
					v.ObservedHash, v.AppliedRevision, v.RolledBack, v.FailureCode = previous, 0, true, "health_check_failed"
					want = "rolled_back"
				} else if kind == "config_critical" {
					v.ObservedHash, v.AppliedRevision, v.Healthy, v.FailedCritical, v.FailureCode = nil, 0, false, true, "rollback_failed"
					want = "failed"
				}
				resultPayload = v
			}
			if err := semanticpayload.PopulateV1(envelope); err != nil {
				t.Fatal(err)
			}
			if err := signer.Authorize(envelope); err != nil {
				t.Fatal(err)
			}
			encoded, err := proto.Marshal(envelope)
			if err != nil {
				t.Fatal(err)
			}
			run(`INSERT INTO operations(id,workspace_id,node_id,command_id,state,request_id,trace_id,created_at,updated_at)VALUES(?,?,?,?,'dispatched',?,?,?,?)`, UUIDBytes(operation), UUIDBytes(workspace), UUIDBytes(node), UUIDBytes(command), operation.String(), trace[3:35], stamp, stamp)
			run(`INSERT INTO commands(id,operation_id,workspace_id,node_id,state,payload_type,envelope,idempotency_key,expected_version,sequence,traceparent,expires_at,created_at,updated_at)VALUES(?,?,?,?,'dispatched',?,?,?,1,1,?,?,?,?)`, UUIDBytes(command), UUIDBytes(operation), UUIDBytes(workspace), UUIDBytes(node), payloadType, encoded, key.String(), trace, expires, stamp, stamp)
			switch payloadType {
			case "certificate_csr", "certificate_p12", "certificate_revoke":
				run(`INSERT INTO certificates(id,workspace_id,node_id,operation_id,common_name,dns_names,key_bits,state,created_at,updated_at)VALUES(?,?,?,?,'result.example.test','["result.example.test"]',2048,'csr_pending',?,?)`, UUIDBytes(certificate), UUIDBytes(workspace), UUIDBytes(node), UUIDBytes(operation), stamp, stamp)
				if payloadType != "certificate_csr" {
					// These result-only fixtures represent a previously issued legacy
					// certificate, not a new CSR lacking its required receipt.
					run(`UPDATE certificates SET state='issued',csr_receipt_legacy=true WHERE id=?`, UUIDBytes(certificate))
				}
				if payloadType == "certificate_p12" {
					run(`INSERT INTO artifact_operations(id,workspace_id,node_id,certificate_id,certificate_version,operation_id,purpose,state,token_sha256,request_hash,expires_at,created_at,updated_at)VALUES(?,?,?,?,1,?,'certificate_p12','pending',?,?,?,?,?)`, UUIDBytes(artifact), UUIDBytes(workspace), UUIDBytes(node), UUIDBytes(certificate), UUIDBytes(operation), digest, digest, expires, stamp, stamp)
				}
			case "agent_upgrade":
				run(`INSERT INTO agent_upgrade_operations(operation_id,workspace_id,node_id,target_version,package_sha256,architecture,state,created_at,updated_at)VALUES(?,?,?,'2.0.0',?,'amd64','queued',?,?)`, UUIDBytes(operation), UUIDBytes(workspace), UUIDBytes(node), digest, stamp, stamp)
			case "config_apply":
				approval := id()
				run(`INSERT INTO approval_requests(id,workspace_id,requester_id,action,resource_type,resource_id,reason,status,expires_at,created_at)VALUES(?,?,?,'config.apply','config_plan',?,'result','pending',?,?)`, UUIDBytes(approval), UUIDBytes(workspace), UUIDBytes(actor), UUIDBytes(operation), expires, stamp)
				run(`INSERT INTO config_plans(id,workspace_id,node_id,operation_id,template_name,expected_revision,candidate_hash,candidate_redacted,warnings,expires_at,created_at)VALUES(?,?,?,?,'result',1,?,'safe','[]',?,?)`, UUIDBytes(operation), UUIDBytes(workspace), UUIDBytes(node), UUIDBytes(operation), digest, expires, stamp)
				run(`INSERT INTO config_apply_operations(operation_id,workspace_id,node_id,plan_id,approval_id,expected_revision,desired_revision,candidate_hash,previous_hash,state,created_at,updated_at)VALUES(?,?,?,?,?,1,2,?,?,'dispatched',?,?)`, UUIDBytes(operation), UUIDBytes(workspace), UUIDBytes(node), UUIDBytes(operation), UUIDBytes(approval), digest, previous, stamp, stamp)
				// Exercise the duplicate-update path while preserving a newer intent.
				run(`INSERT INTO node_config_state(node_id,revision,desired_revision,updated_at)VALUES(?,1,3,?)`, UUIDBytes(node), stamp)
			}
			encodedResult, err := proto.Marshal(resultPayload)
			if err != nil {
				t.Fatal(err)
			}
			result := &agentv1.CommandResult{CommandId: command[:], IdempotencyKey: key[:], PayloadSha256: envelope.GetSemanticPayloadSha256(), SemanticPayloadHashVersion: envelope.GetSemanticPayloadHashVersion(), State: agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED, Result: encodedResult, AcceptedAt: timestamppb.Now(), CompletedAt: timestamppb.Now()}
			if err := attestationtest.AttachProof(envelope, result, private, 1); err != nil {
				t.Fatal(err)
			}
			payload, err := proto.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			eventID := id()
			event := &transportv1.TransportEvent{EventId: eventID[:], NodeId: node[:], EndpointId: endpoint[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT, OccurredAt: timestamppb.Now(), Traceparent: trace, Payload: payload}
			if err := service.Ingest(ctx, event); err != nil {
				t.Fatal(err)
			}
			var state, verified string
			if err := owner.QueryRow(ctx, `SELECT o.state,r.receipt_verification_status FROM operations o JOIN agent_command_results r ON r.command_id=o.command_id WHERE o.id=?`, UUIDBytes(operation)).Scan(&state, &verified); err != nil || state != want || verified != "verified" {
				t.Fatal("verified result outcome", state, verified, err)
			}
			switch payloadType {
			case "certificate_csr":
				var at value.Timestamp
				var der, receipt, subject []byte
				if err := owner.QueryRow(ctx, `SELECT state,csr_der,csr_receipt_verified_at,csr_receipt_sha256,csr_requested_subject_sha256 FROM certificates WHERE id=?`, UUIDBytes(certificate)).Scan(&state, &der, &at, &receipt, &subject); err != nil || state != "csr_ready" || !at.Valid || !bytes.Equal(der, resultPayload.(*agentv1.CertificateCsrResult).GetCsrDer()) || len(receipt) != 32 || len(subject) != 32 {
					t.Fatal("CSR projection", state, at, err)
				}
			case "certificate_p12":
				var hash []byte
				var size int64
				if err := owner.QueryRow(ctx, `SELECT state,content_sha256,content_size FROM artifact_operations WHERE id=?`, UUIDBytes(artifact)).Scan(&state, &hash, &size); err != nil || state != "ready" || !bytes.Equal(hash, digest) || size != 4096 {
					t.Fatal("artifact projection", state, size, err)
				}
			case "certificate_revoke":
				var at value.Timestamp
				var reason string
				if err := owner.QueryRow(ctx, `SELECT state,revoked_at,revocation_reason FROM certificates WHERE id=?`, UUIDBytes(certificate)).Scan(&state, &at, &reason); err != nil || state != "revoked" || !at.Valid || reason != "result revocation" {
					t.Fatal("revoke projection", state, at, reason, err)
				}
			case "agent_upgrade":
				var scheduled, completed value.Timestamp
				if err := owner.QueryRow(ctx, `SELECT u.state,u.scheduled_at,o.completed_at FROM agent_upgrade_operations u JOIN operations o ON o.id=u.operation_id WHERE o.id=?`, UUIDBytes(operation)).Scan(&state, &scheduled, &completed); err != nil || state != "accepted" || !scheduled.Valid || completed.Valid {
					t.Fatal("upgrade acknowledgement became terminal", state, scheduled, completed, err)
				}
			case "config_apply":
				projection := want
				if kind == "config_critical" {
					projection = "failed_critical"
				}
				var revision, desired int64
				var locked bool
				if err := owner.QueryRow(ctx, `SELECT x.state,s.revision,s.desired_revision,s.automation_locked FROM config_apply_operations x JOIN node_config_state s ON s.node_id=x.node_id WHERE x.operation_id=?`, UUIDBytes(operation)).Scan(&state, &revision, &desired, &locked); err != nil || state != projection || desired != 3 || locked != (kind == "config_critical") || (kind == "config_success" && revision != 2) || (kind != "config_success" && revision != 1) {
					t.Fatal("configuration projection", state, revision, desired, locked, err)
				}
			}
			// The same signed receipt under another transport event is evidence
			// replay, not another projection transition or version increment.
			duplicate := id()
			event.EventId = duplicate[:]
			if err := service.Ingest(ctx, event); err != nil {
				t.Fatal("receipt replay", err)
			}
			var version, count int
			if err := owner.QueryRow(ctx, `SELECT version,(SELECT count(*) FROM agent_command_results WHERE command_id=?) FROM operations WHERE id=?`, UUIDBytes(command), UUIDBytes(operation)).Scan(&version, &count); err != nil || version != 2 || count != 1 {
				t.Fatal("receipt replay reapplied projection", version, count, err)
			}
			if kind == "certificate_csr" {
				forged := proto.Clone(result).(*agentv1.CommandResult)
				forged.PrivilegedResultProof.Signature[0] ^= 1
				event.Payload, err = proto.Marshal(forged)
				if err != nil {
					t.Fatal(err)
				}
				forgedID := id()
				event.EventId = forgedID[:]
				if err := service.Ingest(ctx, event); err != nil {
					t.Fatal("forged terminal evidence", err)
				}
				if err := owner.QueryRow(ctx, `SELECT state,version FROM operations WHERE id=?`, UUIDBytes(operation)).Scan(&state, &version); err != nil || state != "succeeded" || version != 2 {
					t.Fatal("forged evidence changed terminal state", state, version, err)
				}
				if err := owner.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE command_id=? AND action='privd.result.verify' AND result='failed'`, UUIDBytes(command)).Scan(&count); err != nil || count != 1 {
					t.Fatal("forged receipt lost verification audit", count, err)
				}
			}
		})
	}
}
