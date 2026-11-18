use std::net::SocketAddr;

use ed25519_dalek::{Signer as _, SigningKey};
use iroh::{RelayMap, Watcher as _, tls::CaTlsConfig};
use ocservia_command_authorization::{
    ConnectionFenceClaimsV2, FenceBindingClaimsV2, SessionGrantClaimsV1,
    canonical_connection_fence_v2, canonical_fence_binding_v2, canonical_session_grant_v1,
    connection_fence_claims_v2, verification_key_id,
};
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    ArtifactGrantV1, ArtifactGrantVersion, CommandAuthorizationProof, CommandAuthorizationVersion,
    CommandDeliveryMode, FenceSignatureVersion, GroupObservation, PrivdReceiptVersion,
    PrivdResultReceiptV1, PrivilegedCommandKind, PrivilegedResultKind, SealedSecretPurpose,
    SealedSecretVersion, SealingKeyDescriptorV1, SemanticPayloadHashVersion, ServiceReload,
    SessionGrantV1, SessionGrantVersion, UserObservation,
};
use ocservia_contracts::generated::ocserv::platform::transport::v1::{
    AuthorizeSessionRequest, CheckEndpointRequest, CheckEndpointResponse, ListNodeTrustRequest,
    ListNodeTrustResponse, NodeTrustBinding, ValidateEnrollmentRequest, ValidateEnrollmentResponse,
    trust_service_server::{TrustService, TrustServiceServer},
};
use ocservia_privd_attestation::{key_id, sign_receipt, verify_receipt};
use sha2::{Digest as _, Sha256};
use tokio::{
    net::{TcpListener, TcpStream},
    sync::oneshot,
    task::JoinHandle,
};
use tokio_stream::wrappers::TcpListenerStream;

use super::*;

const RELAYED_AUTHORIZATION_SIGNATURE: &[u8] = &[0xa5; 64];

fn test_service(event_capacity: usize) -> IrohTransportService {
    test_service_with_policy(event_capacity, IdentityPolicy::default())
}

fn test_service_with_policy(event_capacity: usize, policy: IdentityPolicy) -> IrohTransportService {
    let (_signing_key, keyring) = controller_keyring();
    IrohTransportService::new(event_capacity, policy, keyring)
}

struct TestTrustService {
    signing_key: SigningKey,
    bindings: Vec<(EndpointId, Vec<u8>, NodeTrustState, u64)>,
    override_fence: Option<ConnectionFenceV2>,
    next_epoch: AtomicU64,
}

#[tonic::async_trait]
impl TrustService for TestTrustService {
    async fn validate_enrollment(
        &self,
        _request: Request<ValidateEnrollmentRequest>,
    ) -> Result<Response<ValidateEnrollmentResponse>, Status> {
        Ok(Response::new(ValidateEnrollmentResponse {
            permitted: true,
        }))
    }

    async fn check_endpoint(
        &self,
        request: Request<CheckEndpointRequest>,
    ) -> Result<Response<CheckEndpointResponse>, Status> {
        let endpoint = request.into_inner().endpoint_id;
        let permitted = self.bindings.iter().any(|(bound, _, state, _)| {
            bound.as_bytes() == endpoint.as_slice() && *state == NodeTrustState::Active
        });
        Ok(Response::new(CheckEndpointResponse { permitted }))
    }

    async fn authorize_session(
        &self,
        request: Request<AuthorizeSessionRequest>,
    ) -> Result<Response<SessionHandshakeResponse>, Status> {
        let request = request.into_inner();
        let handshake = request
            .handshake
            .ok_or_else(|| Status::invalid_argument("handshake missing"))?;
        let node_id: [u8; 16] = handshake
            .node_id
            .as_slice()
            .try_into()
            .map_err(|_| Status::invalid_argument("node_id invalid"))?;
        let endpoint_id: [u8; 32] = request
            .remote_endpoint_id
            .as_slice()
            .try_into()
            .map_err(|_| Status::invalid_argument("endpoint_id invalid"))?;
        let mut capabilities = handshake.capabilities;
        capabilities.sort();
        capabilities.dedup();
        let now = unix_seconds();
        let key_id = verification_key_id(&self.signing_key.verifying_key());
        let grant_claims = SessionGrantClaimsV1 {
            version: u32::try_from(i32::from(SessionGrantVersion::V1))
                .map_err(|_| Status::internal("grant version"))?,
            key_id: key_id.clone(),
            protocol_major: PROTOCOL_MAJOR,
            protocol_minor: PROTOCOL_MINOR,
            node_id,
            endpoint_id,
            authorization_revision: 1,
            negotiated_capabilities: capabilities.clone(),
            issued_at_seconds: now - 1,
            issued_at_nanos: 0,
            expires_at_seconds: now + 300,
            expires_at_nanos: 0,
        };
        let canonical_grant = canonical_session_grant_v1(&grant_claims)
            .map_err(|_| Status::internal("canonical grant"))?;
        let session_grant = SessionGrantV1 {
            version: SessionGrantVersion::V1.into(),
            key_id,
            protocol_major: PROTOCOL_MAJOR,
            protocol_minor: PROTOCOL_MINOR,
            node_id: node_id.to_vec(),
            endpoint_id: endpoint_id.to_vec(),
            authorization_revision: 1,
            negotiated_capabilities: capabilities.clone(),
            issued_at: Some(prost_types::Timestamp {
                seconds: grant_claims.issued_at_seconds,
                nanos: 0,
            }),
            expires_at: Some(prost_types::Timestamp {
                seconds: grant_claims.expires_at_seconds,
                nanos: 0,
            }),
            signature: self.signing_key.sign(&canonical_grant).to_bytes().to_vec(),
        };
        let fence = if let Some(fence) = &self.override_fence {
            fence.clone()
        } else {
            let mut term = FenceTerm::new(
                node_id,
                endpoint_id,
                self.next_epoch.fetch_add(1, Ordering::Relaxed),
            );
            term.capabilities.clone_from(&capabilities);
            signed_controller_fence(&self.signing_key, &term, 300)
        };
        Ok(Response::new(SessionHandshakeResponse {
            result: HandshakeResult::Accepted.into(),
            protocol_major: PROTOCOL_MAJOR,
            protocol_minor: PROTOCOL_MINOR,
            max_message_size: handshake.max_message_size,
            controller_version: "test".to_owned(),
            negotiated_capabilities: capabilities,
            session_grant: Some(session_grant),
            connection_fence: Some(fence),
        }))
    }

    async fn enroll(
        &self,
        _request: Request<EnrollRequest>,
    ) -> Result<Response<EnrollResponse>, Status> {
        Ok(Response::new(EnrollResponse::default()))
    }

    async fn list_node_trust(
        &self,
        _request: Request<ListNodeTrustRequest>,
    ) -> Result<Response<ListNodeTrustResponse>, Status> {
        let bindings = self
            .bindings
            .iter()
            .map(|(endpoint, node_id, state, revision)| NodeTrustBinding {
                node_id: node_id.clone(),
                endpoint_id: endpoint.as_bytes().to_vec(),
                state: (*state).into(),
                revision: *revision,
            })
            .collect();
        Ok(Response::new(ListNodeTrustResponse { bindings }))
    }
}

async fn test_trust_with_options(
    signing_key: SigningKey,
    bindings: Vec<(EndpointId, Vec<u8>, NodeTrustState, u64)>,
    override_fence: Option<ConnectionFenceV2>,
) -> TrustAuthority {
    let listener = TcpListener::bind("127.0.0.1:0")
        .await
        .expect("bind test trust service");
    let addr = listener.local_addr().expect("test trust addr");
    let trust_service = Arc::new(TestTrustService {
        signing_key,
        bindings,
        override_fence,
        next_epoch: AtomicU64::new(1),
    });
    tokio::spawn(
        tonic::transport::Server::builder()
            .add_service(TrustServiceServer::from_arc(trust_service))
            .serve_with_incoming(TcpListenerStream::new(listener)),
    );
    let channel = tonic::transport::Endpoint::from_shared(format!("http://{addr}"))
        .expect("test trust endpoint")
        .connect()
        .await
        .expect("connect test trust service");
    TrustAuthority::new(channel)
}

async fn test_trust_for_key(
    key: &SecretKey,
    handshake: &SessionHandshake,
    override_fence: Option<ConnectionFenceV2>,
) -> TrustAuthority {
    let (signing_key, _) = controller_keyring();
    test_trust_with_options(
        signing_key,
        vec![(
            key.public(),
            handshake.node_id.clone(),
            NodeTrustState::Active,
            1,
        )],
        override_fence,
    )
    .await
}

async fn test_router(
    secret_key: SecretKey,
    relay_mode: RelayMode,
    policy: IdentityPolicy,
    trust: TrustAuthority,
    service: &IrohTransportService,
) -> Router {
    build_router_with_trust(secret_key, relay_mode, policy, trust, service)
        .await
        .expect("build test router")
}

async fn private_tls_relay() -> (
    RelayMap,
    Vec<rustls_pki_types::CertificateDer<'static>>,
    iroh_relay::server::Server,
) {
    use iroh_relay::server::{CertConfig, RelayConfig, Server, ServerConfig, TlsConfig};
    let (certs, server_config) = iroh_relay::server::testing::self_signed_tls_certs_and_config();
    let mut relay = RelayConfig::new(([127, 0, 0, 1], 0));
    relay.tls = Some(TlsConfig::new(
        ([127, 0, 0, 1], 0),
        CertConfig::Manual { server_config },
    ));
    let mut config = ServerConfig::default();
    config.relay = Some(relay);
    let server = Server::spawn(config)
        .await
        .expect("start private TLS relay");
    let url = format!("https://{}", server.https_addr().expect("TLS listener"));
    let map = RelayMap::try_from_iter([url.as_str()]).expect("private relay map");
    (map, certs, server)
}

struct RestartableTcpProxy {
    addr: SocketAddr,
    stop: oneshot::Sender<()>,
    task: JoinHandle<()>,
}

impl RestartableTcpProxy {
    async fn start(addr: SocketAddr, target: SocketAddr) -> Self {
        let listener = TcpListener::bind(addr).await.expect("bind relay proxy");
        let addr = listener.local_addr().expect("relay proxy address");
        let (stop, mut stop_rx) = oneshot::channel();
        let task = tokio::spawn(async move {
            let mut children = tokio::task::JoinSet::new();
            loop {
                tokio::select! {
                    _ = &mut stop_rx => break,
                    accepted = listener.accept() => {
                        let Ok((mut client, _)) = accepted else { break };
                        children.spawn(async move {
                            let Ok(mut upstream) = TcpStream::connect(target).await else {
                                return;
                            };
                            tokio::io::copy_bidirectional(&mut client, &mut upstream)
                                .await
                                .ok();
                        });
                    }
                    Some(_) = children.join_next(), if !children.is_empty() => {}
                }
            }
            children.abort_all();
            while children.join_next().await.is_some() {}
        });
        Self { addr, stop, task }
    }

    async fn stop(self) -> SocketAddr {
        self.stop.send(()).ok();
        self.task.await.expect("join relay proxy");
        self.addr
    }
}

fn sealing_keys() -> Vec<SealingKeyDescriptorV1> {
    vec![
        SealingKeyDescriptorV1 {
            version: SealedSecretVersion::V1 as i32,
            purpose: SealedSecretPurpose::UserPassword as i32,
            key_id: "fixture-user-key-v1".into(),
            public_key_sha256: vec![0x11; 32],
        },
        SealingKeyDescriptorV1 {
            version: SealedSecretVersion::V1 as i32,
            purpose: SealedSecretPurpose::CertificateP12Password as i32,
            key_id: "fixture-p12-key-v1".into(),
            public_key_sha256: vec![0x22; 32],
        },
    ]
}

fn handshake(key: &SecretKey) -> SessionHandshake {
    SessionHandshake {
        protocol_major: PROTOCOL_MAJOR,
        protocol_minor: PROTOCOL_MINOR,
        agent_version: "test".to_owned(),
        controller_version: String::new(),
        node_id: Uuid::now_v7().as_bytes().to_vec(),
        endpoint_id: key.public().as_bytes().to_vec(),
        capabilities: vec![
            "ocserv.fencing.v2".to_owned(),
            "ocserv.status.read".to_owned(),
        ],
        ocserv_version: "1.3".to_owned(),
        os_release: "test".to_owned(),
        boot_id: "boot".to_owned(),
        agent_instance_id: Uuid::now_v7().as_bytes().to_vec(),
        supported_compressions: Vec::new(),
        max_message_size: MAX_FRAME_BYTES_U32,
        time: Some(now_timestamp()),
        nonce: vec![7; 32],
        sealing_keys: sealing_keys(),
    }
}

fn identity_policy(key: &SecretKey, handshake: &SessionHandshake) -> IdentityPolicy {
    IdentityPolicy::new(
        HashMap::from([(key.public(), handshake.node_id.clone())]),
        HashSet::new(),
    )
}

fn command_envelope(node_id: &[u8], traceparent: &str, reason: String) -> Vec<u8> {
    CommandEnvelope {
        protocol_version: "1.1".to_owned(),
        message_id: Uuid::now_v7().as_bytes().to_vec(),
        command_id: Uuid::now_v7().as_bytes().to_vec(),
        idempotency_key: Uuid::now_v7().as_bytes().to_vec(),
        node_id: node_id.to_vec(),
        sequence: 1,
        issued_at: Some(now_timestamp()),
        expires_at: Some((SystemTime::now() + Duration::from_secs(30)).into()),
        expected_revision: 1,
        traceparent: traceparent.to_owned(),
        actor_id: "test".to_owned(),
        required_capability: "ocserv.status.read".to_owned(),
        reason,
        authorization: Some(CommandAuthorizationProof {
            version: CommandAuthorizationVersion::V1.into(),
            key_id: "ed25519-sha256:test-transport-relay-only".to_owned(),
            signature: RELAYED_AUTHORIZATION_SIGNATURE.to_vec(),
        }),
        delivery_mode: CommandDeliveryMode::ExecuteOrReplay.into(),
        payload: Some(command_envelope::Payload::ServiceReload(ServiceReload {})),
        semantic_payload_hash_version: SemanticPayloadHashVersion::V1 as i32,
        semantic_payload_sha256: vec![0x11; 32],
        operation_id: Uuid::now_v7().as_bytes().to_vec(),
        ..CommandEnvelope::default()
    }
    .encode_to_vec()
}

async fn send_handshake(
    connection: &Connection,
    handshake: &SessionHandshake,
) -> SessionHandshakeResponse {
    let bytes = handshake.encode_to_vec();
    let (mut send, mut recv) = connection.open_bi().await.expect("open handshake stream");
    let request_len = u32::try_from(bytes.len()).expect("test handshake length fits u32");
    send.write_all(&request_len.to_be_bytes())
        .await
        .expect("write handshake length");
    send.write_all(&bytes).await.expect("write handshake");
    send.finish().expect("finish handshake request");
    let mut length = [0_u8; 4];
    recv.read_exact(&mut length)
        .await
        .expect("read response length");
    let mut response = vec![0_u8; u32::from_be_bytes(length) as usize];
    recv.read_exact(&mut response).await.expect("read response");
    SessionHandshakeResponse::decode(response.as_slice()).expect("decode response")
}

fn privileged_command_result(command: &CommandEnvelope, include_receipt: bool) -> CommandResult {
    let now = now_timestamp();
    let result_bytes = b"completed".to_vec();
    let signing_key = SigningKey::from_bytes(&[7; 32]);
    let proof = include_receipt.then(|| {
        sign_receipt(
            PrivdResultReceiptV1 {
                receipt_version: PrivdReceiptVersion::V1.into(),
                node_id: command.node_id.clone(),
                privd_attestation_key_id: key_id(&signing_key.verifying_key()),
                command_id: command.command_id.clone(),
                operation_id: command.operation_id.clone(),
                idempotency_key: command.idempotency_key.clone(),
                semantic_payload_hash_version: command.semantic_payload_hash_version,
                semantic_payload_sha256: command.semantic_payload_sha256.clone(),
                command_kind: PrivilegedCommandKind::ServiceReload.into(),
                result_kind: PrivilegedResultKind::Mutation.into(),
                terminal_state: CommandResultState::Succeeded.into(),
                result_bytes_sha256: Sha256::digest(&result_bytes).to_vec(),
                error_code_sha256: Sha256::digest([]).to_vec(),
                effect_record_id: Uuid::now_v7().as_bytes().to_vec(),
                effect_sequence: 1,
                accepted_at: Some(now),
                completed_at: Some(now),
                replayed: false,
                certificate: None,
            },
            &signing_key,
        )
        .expect("sign root result receipt")
    });
    CommandResult {
        command_id: command.command_id.clone(),
        idempotency_key: command.idempotency_key.clone(),
        payload_sha256: command.semantic_payload_sha256.clone(),
        state: CommandResultState::Succeeded.into(),
        result: result_bytes,
        error_code: String::new(),
        accepted_at: Some(now),
        completed_at: Some(now),
        replayed: false,
        semantic_payload_hash_version: command.semantic_payload_hash_version,
        privileged_result_proof: proof,
    }
}

async fn respond_to_command(connection: Connection, include_receipt: bool, foreign_id: bool) {
    let (mut send, mut recv) = connection.accept_bi().await.expect("accept command");
    let mut length = [0_u8; 4];
    recv.read_exact(&mut length)
        .await
        .expect("read command length");
    let mut command = vec![0_u8; u32::from_be_bytes(length) as usize];
    recv.read_exact(&mut command).await.expect("read command");
    let decoded = CommandEnvelope::decode(command.as_slice()).expect("decode command");
    assert_eq!(
        decoded
            .authorization
            .as_ref()
            .expect("relayed authorization")
            .signature,
        RELAYED_AUTHORIZATION_SIGNATURE
    );
    let mut result = privileged_command_result(&decoded, include_receipt);
    if foreign_id {
        result.command_id = Uuid::now_v7().as_bytes().to_vec();
    }
    let response = AgentEvent {
        r#type: AgentEventType::CommandResult.into(),
        payload: result.encode_to_vec(),
    }
    .encode_to_vec();
    let response_len = u32::try_from(response.len()).expect("response length fits u32");
    send.write_all(&response_len.to_be_bytes())
        .await
        .expect("write response length");
    send.write_all(&response).await.expect("write response");
    send.finish().expect("finish response");
}

#[test]
fn privileged_terminal_result_requires_a_well_formed_receipt() {
    let node_id = Uuid::now_v7();
    let command = CommandEnvelope::decode(
        command_envelope(node_id.as_bytes(), &new_traceparent(), String::new()).as_slice(),
    )
    .expect("decode command");
    let valid = privileged_command_result(&command, true);
    assert!(valid_privileged_result_proof(&command, &valid));

    let missing = privileged_command_result(&command, false);
    assert!(!valid_privileged_result_proof(&command, &missing));

    let mut malformed = valid;
    malformed
        .privileged_result_proof
        .as_mut()
        .expect("proof")
        .version = PrivdReceiptVersion::Unspecified.into();
    assert!(!valid_privileged_result_proof(&command, &malformed));

    let mut foreign_receipt = privileged_command_result(&command, true);
    foreign_receipt
        .privileged_result_proof
        .as_mut()
        .and_then(|proof| proof.receipt_v1.as_mut())
        .expect("receipt")
        .command_id = Uuid::now_v7().as_bytes().to_vec();
    assert!(!valid_privileged_result_proof(&command, &foreign_receipt));

    // An unsigned Rejected must not end a privileged command; a signed one
    // and a non-privileged rejection keep their existing handling.
    let mut rejected = privileged_command_result(&command, false);
    rejected.state = CommandResultState::Rejected.into();
    assert!(!valid_privileged_result_proof(&command, &rejected));
    let mut signed_rejected = privileged_command_result(&command, true);
    signed_rejected.state = CommandResultState::Rejected.into();
    assert!(valid_privileged_result_proof(&command, &signed_rejected));
    let synthetic = CommandEnvelope {
        payload: Some(command_envelope::Payload::SyntheticNoop(
            ocservia_contracts::generated::ocserv::platform::agent::v1::SyntheticNoop::default(),
        )),
        ..command.clone()
    };
    assert!(valid_privileged_result_proof(&synthetic, &rejected));
}

/// Answers with a Heartbeat before an otherwise valid result. The first
/// frame must end the stream, so a later write may fail.
async fn respond_with_heartbeat_first(connection: Connection) {
    let (mut send, mut recv) = connection.accept_bi().await.expect("accept command");
    let mut length = [0_u8; 4];
    recv.read_exact(&mut length)
        .await
        .expect("read command length");
    let mut command = vec![0_u8; u32::from_be_bytes(length) as usize];
    recv.read_exact(&mut command).await.expect("read command");
    let decoded = CommandEnvelope::decode(command.as_slice()).expect("decode command");
    for event in [
        AgentEvent {
            r#type: AgentEventType::Heartbeat.into(),
            payload: Vec::new(),
        },
        AgentEvent {
            r#type: AgentEventType::CommandResult.into(),
            payload: privileged_command_result(&decoded, true).encode_to_vec(),
        },
    ] {
        let frame = event.encode_to_vec();
        let frame_len = u32::try_from(frame.len()).expect("frame length fits u32");
        if send.write_all(&frame_len.to_be_bytes()).await.is_err()
            || send.write_all(&frame).await.is_err()
        {
            return;
        }
    }
    let _ = send.finish();
}

async fn end_command_without_response(connection: Connection) {
    let (mut send, mut recv) = connection.accept_bi().await.expect("accept command");
    let mut length = [0_u8; 4];
    recv.read_exact(&mut length)
        .await
        .expect("read command length");
    let mut command = vec![0_u8; u32::from_be_bytes(length) as usize];
    recv.read_exact(&mut command).await.expect("read command");
    let decoded = CommandEnvelope::decode(command.as_slice()).expect("decode command");
    assert_eq!(
        decoded
            .authorization
            .expect("relayed authorization")
            .signature,
        RELAYED_AUTHORIZATION_SIGNATURE
    );
    send.finish().expect("finish empty response");
}

async fn next_event(events: &mut EventStream, event_type: TransportEventType) -> TransportEvent {
    tokio::time::timeout(Duration::from_secs(2), async {
        loop {
            let event = events
                .next()
                .await
                .expect("event stream remains open")
                .expect("event is valid");
            if event.r#type == i32::from(event_type) {
                break event;
            }
        }
    })
    .await
    .expect("event arrives")
}

async fn wait_until_registered(service: &IrohTransportService, node_id: &[u8]) {
    tokio::time::timeout(Duration::from_secs(2), async {
        loop {
            if service
                .shared
                .inner
                .connections
                .lock()
                .await
                .contains_key(node_id)
            {
                break;
            }
            tokio::task::yield_now().await;
        }
    })
    .await
    .expect("connection registered");
}

async fn last_seen(
    service: &IrohTransportService,
    node_id: &[u8],
) -> Option<prost_types::Timestamp> {
    service
        .get_node_connection(Request::new(GetNodeConnectionRequest {
            node_id: node_id.to_vec(),
        }))
        .await
        .expect("query connection")
        .into_inner()
        .last_seen
}

#[test]
fn policy_rejects_unknown_agent_and_revoked_enrollment() {
    let approved = SecretKey::generate().public();
    let revoked = SecretKey::generate().public();
    let policy = IdentityPolicy::new(
        HashMap::from([(approved, Uuid::now_v7().as_bytes().to_vec())]),
        HashSet::from([revoked]),
    );
    assert!(policy.permits(approved, AGENT_ALPN));
    assert!(!policy.permits(revoked, ENROLL_ALPN));
    assert!(!policy.permits(SecretKey::generate().public(), AGENT_ALPN));
    let dynamic = SecretKey::generate().public();
    let dynamic_node = Uuid::now_v7().as_bytes().to_vec();
    assert_eq!(
        policy.update(dynamic, dynamic_node.clone(), NodeTrustState::Active, 1),
        PolicyUpdateDisposition::Applied
    );
    assert!(policy.permits(dynamic, AGENT_ALPN));
    assert_eq!(
        policy.update(dynamic, dynamic_node.clone(), NodeTrustState::Revoked, 2),
        PolicyUpdateDisposition::Applied
    );
    assert_eq!(
        policy.update(dynamic, dynamic_node.clone(), NodeTrustState::Active, 1),
        PolicyUpdateDisposition::Stale
    );
    assert_eq!(
        policy.update(dynamic, dynamic_node, NodeTrustState::Active, 3),
        PolicyUpdateDisposition::Rejected
    );
    assert!(!policy.permits(dynamic, AGENT_ALPN));
    assert!(policy.revoked(dynamic));

    let restored_revocation = SecretKey::generate().public();
    let restored = IdentityPolicy::new(HashMap::new(), HashSet::from([restored_revocation]));
    assert_eq!(
        restored.update(
            restored_revocation,
            Uuid::now_v7().as_bytes().to_vec(),
            NodeTrustState::Active,
            u64::MAX,
        ),
        PolicyUpdateDisposition::Rejected
    );
    assert!(restored.revoked(restored_revocation));
}

#[tokio::test]
async fn trust_update_reports_stale_and_rejects_tombstone_reactivation() {
    let (signing_key, keyring) = controller_keyring();
    let service = IrohTransportService::new(8, IdentityPolicy::default(), keyring);
    let endpoint = SecretKey::generate().public();
    let node_id = Uuid::now_v7().as_bytes().to_vec();
    let term = FenceTerm::new(
        node_id.as_slice().try_into().expect("node id"),
        *endpoint.as_bytes(),
        1,
    );
    let fence = signed_controller_fence(&signing_key, &term, 300);
    service
        .register_owner_fence(Request::new(RegisterOwnerFenceRequest {
            fence: Some(fence.clone()),
        }))
        .await
        .expect("register owner fence");
    let update = |state: NodeTrustState, revision: u64| {
        let operation_id = ocservia_command_authorization::state_update_operation_id(
            node_id.as_slice().try_into().expect("node id"),
            endpoint.as_bytes(),
            state as u32,
            revision,
            "trust transition fixture",
        );
        let binding = signed_fence_binding(
            &signing_key,
            &fence,
            FenceOperationKind::StateUpdate,
            operation_id,
            "ocserv.fencing.v2",
        );
        UpdateNodeTrustRequest {
            node_id: node_id.clone(),
            endpoint_id: endpoint.as_bytes().to_vec(),
            state: state.into(),
            reason: "trust transition fixture".into(),
            revision,
            fence_binding: Some(binding),
            operation_id: operation_id.to_vec(),
        }
    };
    let active = service
        .update_node_trust(Request::new(update(NodeTrustState::Active, 2)))
        .await
        .expect("activate endpoint")
        .into_inner();
    assert_eq!(
        active.disposition,
        i32::from(TrustUpdateDisposition::Applied)
    );
    let stale = service
        .update_node_trust(Request::new(update(NodeTrustState::Active, 1)))
        .await
        .expect("report stale update")
        .into_inner();
    assert_eq!(stale.disposition, i32::from(TrustUpdateDisposition::Stale));
    assert_eq!(stale.retained_revision, 2);
    assert_eq!(stale.retained_state, i32::from(NodeTrustState::Active));

    let revoked = service
        .update_node_trust(Request::new(update(NodeTrustState::Revoked, 3)))
        .await
        .expect("revoke endpoint")
        .into_inner();
    assert_eq!(
        revoked.disposition,
        i32::from(TrustUpdateDisposition::Applied)
    );
    let reactivation = service
        .update_node_trust(Request::new(update(NodeTrustState::Active, 4)))
        .await
        .expect("report rejected reactivation")
        .into_inner();
    assert_eq!(
        reactivation.disposition,
        i32::from(TrustUpdateDisposition::Rejected)
    );
    assert_eq!(reactivation.retained_revision, 3);
    assert_eq!(
        reactivation.retained_state,
        i32::from(NodeTrustState::Revoked)
    );
}

#[tokio::test]
async fn close_attempt_invalidates_only_target_registration() {
    let (_signing_key, keyring) = controller_keyring();
    let shared = Shared::new(8, keyring);
    let target = Uuid::now_v7().as_bytes().to_vec();
    let unrelated = Uuid::now_v7().as_bytes().to_vec();
    let target_token = shared.registration_token(&target).await;
    let unrelated_token = shared.registration_token(&unrelated).await;
    assert!(shared.remove(&target, b"revoked").await.is_none());
    assert!(!Arc::ptr_eq(
        &shared.registration_token(&target).await,
        &target_token
    ));
    assert!(Arc::ptr_eq(
        &shared.registration_token(&unrelated).await,
        &unrelated_token
    ));
}

#[test]
fn enrollment_request_binds_the_authenticated_endpoint() {
    let key = SecretKey::generate();
    let mut request = EnrollRequest {
        token: "a".repeat(43),
        endpoint_id: key.public().as_bytes().to_vec(),
        agent_version: "test".to_owned(),
        os_release: "test".to_owned(),
        ocserv_version: "test".to_owned(),
        boot_id: "boot".to_owned(),
        agent_instance_id: Uuid::now_v7().as_bytes().to_vec(),
        capabilities: vec![
            "ocserv.fencing.v2".to_owned(),
            "ocserv.status.read".to_owned(),
        ],
        environment: "test".to_owned(),
        nonce: vec![0; 16],
        time: Some(now_timestamp()),
        enrollment_protocol_major: 0,
        enrollment_protocol_minor: 0,
        proof: None,
        sealing_keys: sealing_keys(),
    };
    ocservia_agent_identity::authorize_enrollment(&mut request, &key)
        .expect("sign enrollment request");
    assert!(validate_enrollment(&request, key.public()).is_ok());
    let mut bootstrap = request.clone();
    bootstrap.token = format!("obt1_{}", request.token);
    ocservia_agent_identity::authorize_enrollment(&mut bootstrap, &key)
        .expect("sign legacy root bootstrap request");
    assert!(validate_enrollment(&bootstrap, key.public()).is_err());
    bootstrap.token = format!("obt2_{}", request.token);
    ocservia_agent_identity::authorize_enrollment(&mut bootstrap, &key)
        .expect("sign derived installation enrollment request");
    assert!(validate_enrollment(&bootstrap, key.public()).is_ok());
    bootstrap.token = format!("oit1_{}", request.token);
    assert!(validate_enrollment(&bootstrap, key.public()).is_err());
    bootstrap.token = format!("obt2_{}a", request.token);
    assert!(validate_enrollment(&bootstrap, key.public()).is_err());
    assert!(validate_enrollment(&request, SecretKey::generate().public()).is_err());
    let mut unicode = request;
    unicode.capabilities = vec!["界".repeat(128)];
    assert!(validate_enrollment(&unicode, key.public()).is_ok());
    unicode.capabilities = vec!["界".repeat(129)];
    assert!(validate_enrollment(&unicode, key.public()).is_err());
}

#[test]
fn enrollment_identity_churn_refuses_new_identity_without_evicting_oldest() {
    let now = Instant::now();
    let mut enrollment_attempts = HashMap::new();
    let oldest = SecretKey::generate().public();
    record_attempt(
        &mut enrollment_attempts,
        oldest,
        now,
        MAX_ENROLLMENT_CONNECTIONS,
    )
    .expect("record oldest enrollment identity");
    for _ in 1..MAX_ENROLLMENT_CONNECTIONS {
        record_attempt(
            &mut enrollment_attempts,
            SecretKey::generate().public(),
            now + Duration::from_millis(1),
            MAX_ENROLLMENT_CONNECTIONS,
        )
        .expect("fill enrollment identity capacity");
    }
    let newcomer = SecretKey::generate().public();
    assert_eq!(
        record_attempt(
            &mut enrollment_attempts,
            newcomer,
            now + Duration::from_millis(2),
            MAX_ENROLLMENT_CONNECTIONS,
        ),
        Err(AttemptRejection::Capacity)
    );
    assert_eq!(enrollment_attempts.len(), MAX_ENROLLMENT_CONNECTIONS);
    assert!(enrollment_attempts.contains_key(&oldest));
    assert!(!enrollment_attempts.contains_key(&newcomer));

    let mut agent_attempts = HashMap::new();
    assert_eq!(
        record_attempt(
            &mut agent_attempts,
            SecretKey::generate().public(),
            now,
            MAX_AGENT_CONNECTIONS,
        ),
        Ok(())
    );
}

#[test]
fn trust_attempt_rate_is_global_and_non_evictable() {
    let now = Instant::now();
    let mut attempts = VecDeque::new();
    assert_eq!(record_global_attempt(&mut attempts, now, 2), Ok(()));
    assert_eq!(
        record_global_attempt(&mut attempts, now + Duration::from_millis(1), 2),
        Ok(())
    );
    assert_eq!(
        record_global_attempt(&mut attempts, now + Duration::from_millis(2), 2),
        Err(AttemptRejection::Rate)
    );
    assert_eq!(
        record_global_attempt(&mut attempts, now + Duration::from_mins(1), 2),
        Ok(())
    );
}

#[tokio::test]
async fn trust_authority_clones_share_the_global_limits() {
    let channel = tonic::transport::Endpoint::from_static("http://[::1]:50051").connect_lazy();
    let trust = TrustAuthority::new(channel);
    let clone = trust.clone();
    assert!(Arc::ptr_eq(&trust.budgets, &clone.budgets));
    for class in [
        TrustClass::KnownAgentHandshake,
        TrustClass::AgentAuthorization,
        TrustClass::AgentRegistrationRecheck,
        TrustClass::UnknownEnrollmentPreAuth,
        TrustClass::EnrollmentCompletion,
    ] {
        assert_eq!(trust.metrics(class), TrustMetricsSnapshot::default());
        assert!(!class.label().contains("endpoint"));
    }
}

#[tokio::test]
async fn unknown_enrollment_flood_cannot_consume_reserved_agent_budgets() {
    let config = TrustBudgetConfig::default()
        .with_limit(
            TrustClass::UnknownEnrollmentPreAuth,
            TrustClassLimit {
                attempts_per_minute: 20,
                concurrency: 20,
            },
        )
        .expect("valid test limit")
        .with_limit(
            TrustClass::KnownAgentHandshake,
            TrustClassLimit {
                attempts_per_minute: 3,
                concurrency: 3,
            },
        )
        .expect("valid test limit")
        .with_limit(
            TrustClass::AgentAuthorization,
            TrustClassLimit {
                attempts_per_minute: 3,
                concurrency: 3,
            },
        )
        .expect("valid test limit")
        .with_limit(
            TrustClass::AgentRegistrationRecheck,
            TrustClassLimit {
                attempts_per_minute: 3,
                concurrency: 3,
            },
        )
        .expect("valid test limit")
        .with_limit(
            TrustClass::EnrollmentCompletion,
            TrustClassLimit {
                attempts_per_minute: 3,
                concurrency: 3,
            },
        )
        .expect("valid test limit");
    let budgets = Arc::new(TrustBudgetSet::new(&config));
    let now = Instant::now();
    let mut enrollment = Vec::new();
    for _ in 0..20 {
        enrollment.push(
            budgets
                .acquire_at(TrustClass::UnknownEnrollmentPreAuth, now)
                .await
                .expect("unknown enrollment receives only its own permits"),
        );
    }
    assert_eq!(
        budgets
            .acquire_at(TrustClass::UnknownEnrollmentPreAuth, now)
            .await
            .err(),
        Some(AttemptRejection::Capacity)
    );
    for class in [
        TrustClass::KnownAgentHandshake,
        TrustClass::AgentAuthorization,
        TrustClass::AgentRegistrationRecheck,
        TrustClass::EnrollmentCompletion,
    ] {
        let permit = budgets
            .acquire_at(class, now)
            .await
            .expect("approved class retains independent capacity during enrollment flood");
        assert_eq!(budgets.snapshot(class).accepted, 1);
        drop(permit);
    }
    drop(enrollment);
    assert_eq!(
        budgets
            .acquire_at(TrustClass::UnknownEnrollmentPreAuth, now)
            .await
            .err(),
        Some(AttemptRejection::Rate)
    );
    let recovered = budgets
        .acquire_at(
            TrustClass::UnknownEnrollmentPreAuth,
            now + Duration::from_mins(1),
        )
        .await
        .expect("rolling window recovers deterministically");
    drop(recovered);
}

#[tokio::test]
async fn baseline_budget_parameters_remain_partitioned() {
    let shared_limit = TrustClassLimit {
        attempts_per_minute: 600,
        concurrency: 16,
    };
    let mut config = TrustBudgetConfig::default();
    for class in [
        TrustClass::KnownAgentHandshake,
        TrustClass::AgentAuthorization,
        TrustClass::AgentRegistrationRecheck,
        TrustClass::UnknownEnrollmentPreAuth,
        TrustClass::EnrollmentCompletion,
    ] {
        config = config
            .with_limit(class, shared_limit)
            .expect("baseline-compatible trust limit");
    }
    let budgets = Arc::new(TrustBudgetSet::new(&config));
    let now = Instant::now();
    for _ in 0..shared_limit.attempts_per_minute {
        drop(
            budgets
                .acquire_at(TrustClass::UnknownEnrollmentPreAuth, now)
                .await
                .expect("enrollment consumes only its own rolling window"),
        );
    }
    assert_eq!(
        budgets
            .acquire_at(TrustClass::UnknownEnrollmentPreAuth, now)
            .await
            .err(),
        Some(AttemptRejection::Rate)
    );
    for class in [
        TrustClass::KnownAgentHandshake,
        TrustClass::AgentAuthorization,
        TrustClass::AgentRegistrationRecheck,
    ] {
        drop(
            budgets
                .acquire_at(class, now)
                .await
                .expect("Agent reserve survives enrollment exhaustion"),
        );
    }
}

#[tokio::test]
async fn router_restart_retains_identity_and_direct_connectivity() {
    let controller_key = SecretKey::generate();
    let controller_id = controller_key.public();
    let agent_key = SecretKey::generate();
    let handshake = handshake(&agent_key);
    let node_id = handshake.node_id.clone();

    for _ in 0..2 {
        let service = test_service(8);
        let trust = test_trust_for_key(&agent_key, &handshake, None).await;
        let router = test_router(
            controller_key.clone(),
            RelayMode::Disabled,
            identity_policy(&agent_key, &handshake),
            trust,
            &service,
        )
        .await;
        assert_eq!(router.endpoint().id(), controller_id);
        let client = Endpoint::builder(presets::N0)
            .secret_key(agent_key.clone())
            .relay_mode(RelayMode::Disabled)
            .bind()
            .await
            .expect("build direct client");
        let connection = client
            .connect(router.endpoint().addr(), AGENT_ALPN)
            .await
            .expect("connect after router start");
        let response = send_handshake(&connection, &handshake).await;
        assert_eq!(response.result, i32::from(HandshakeResult::Accepted));
        wait_until_registered(&service, &node_id).await;
        connection.close(VarInt::from_u32(0), b"restart fixture");
        client.close().await;
        shutdown(&service, router)
            .await
            .expect("shutdown direct router");
    }
}

#[test]
fn command_response_wait_honors_envelope_expiration() {
    let command = CommandEnvelope {
        expires_at: Some((SystemTime::now() + Duration::from_secs(30)).into()),
        ..CommandEnvelope::default()
    };
    let deadline = command_response_deadline(&command).expect("valid command deadline");
    assert!(deadline > tokio::time::Instant::now() + STREAM_TIMEOUT);

    let expired = CommandEnvelope {
        expires_at: Some((SystemTime::now() - Duration::from_secs(1)).into()),
        ..CommandEnvelope::default()
    };
    assert_eq!(
        command_response_deadline(&expired)
            .expect_err("expired command rejected")
            .code(),
        tonic::Code::DeadlineExceeded
    );
    let reconcile = CommandEnvelope {
        delivery_mode: CommandDeliveryMode::ReconcileOnly.into(),
        ..expired
    };
    let before = tokio::time::Instant::now();
    let deadline = command_response_deadline(&reconcile).expect("expired read-only observation");
    assert!(deadline >= before + STREAM_TIMEOUT);
    assert!(deadline <= tokio::time::Instant::now() + STREAM_TIMEOUT);
}

#[test]
fn handshake_limits_reject_identity_and_version_mismatch() {
    let remote_key = SecretKey::generate();
    let mut handshake = SessionHandshake {
        protocol_major: PROTOCOL_MAJOR,
        protocol_minor: PROTOCOL_MINOR,
        agent_version: "test".to_owned(),
        controller_version: String::new(),
        node_id: Uuid::now_v7().as_bytes().to_vec(),
        endpoint_id: remote_key.public().as_bytes().to_vec(),
        capabilities: Vec::new(),
        ocserv_version: String::new(),
        os_release: String::new(),
        boot_id: "boot".to_owned(),
        agent_instance_id: Uuid::now_v7().as_bytes().to_vec(),
        supported_compressions: Vec::new(),
        max_message_size: MAX_FRAME_BYTES_U32,
        time: Some(now_timestamp()),
        nonce: vec![7; 32],
        sealing_keys: sealing_keys(),
    };
    assert!(validate_handshake(&handshake, remote_key.public()).is_ok());
    assert!(validate_protocol_version(&handshake).is_ok());
    handshake.protocol_minor = 0;
    assert!(validate_protocol_version(&handshake).is_err());
    handshake.protocol_minor = PROTOCOL_MINOR;
    handshake.protocol_major = 2;
    assert!(validate_handshake(&handshake, remote_key.public()).is_ok());
    assert!(validate_protocol_version(&handshake).is_err());
    handshake.protocol_major = PROTOCOL_MAJOR;
    assert!(validate_handshake(&handshake, SecretKey::generate().public()).is_err());
}

#[test]
fn negotiated_session_rejects_protocol_v1_0_and_grantless_v1_1() {
    let key = SecretKey::generate();
    let request = handshake(&key);
    let legacy = SessionHandshakeResponse {
        result: HandshakeResult::Accepted.into(),
        protocol_major: 1,
        protocol_minor: 0,
        max_message_size: request.max_message_size,
        controller_version: "test".to_owned(),
        negotiated_capabilities: vec!["ocserv.status.read".to_owned()],
        session_grant: None,
        connection_fence: None,
    };
    assert!(validate_negotiated_session(&request, key.public(), &legacy).is_err());

    let mut grantless_v1_1 = legacy;
    grantless_v1_1.protocol_minor = 1;
    assert!(validate_negotiated_session(&request, key.public(), &grantless_v1_1).is_err());
}

#[test]
fn queues_are_bounded() {
    let service = test_service(usize::MAX);
    assert_eq!(service.shared.inner.event_capacity, MAX_CONNECTIONS);
    assert_eq!(
        service
            .shared
            .inner
            .agent_connection_permits
            .available_permits(),
        MAX_AGENT_CONNECTIONS
    );
    assert_eq!(
        service
            .shared
            .inner
            .enrollment_connection_permits
            .available_permits(),
        MAX_ENROLLMENT_CONNECTIONS
    );
}

#[test]
fn enrollment_capacity_cannot_exhaust_agent_capacity() {
    let service = test_service(8);
    let enrollment_permits = service
        .shared
        .inner
        .enrollment_connection_permits
        .clone()
        .try_acquire_many_owned(
            u32::try_from(MAX_ENROLLMENT_CONNECTIONS).expect("enrollment capacity fits u32"),
        )
        .expect("reserve all enrollment permits");

    assert_eq!(
        service
            .shared
            .inner
            .enrollment_connection_permits
            .available_permits(),
        0
    );
    assert_eq!(
        service
            .shared
            .inner
            .agent_connection_permits
            .available_permits(),
        MAX_AGENT_CONNECTIONS
    );
    drop(enrollment_permits);
}

#[test]
fn transport_events_have_a_valid_root_traceparent() {
    let traceparent = event(
        Uuid::now_v7().as_bytes(),
        SecretKey::generate().public().as_bytes(),
        TransportEventType::Connected,
        Vec::new(),
    )
    .traceparent;
    let parts = traceparent.split('-').collect::<Vec<_>>();
    assert_eq!(parts.len(), 4);
    assert_eq!(parts[0], "00");
    assert_eq!(parts[1].len(), 32);
    assert_eq!(parts[2].len(), 16);
    assert_eq!(parts[3], "01");
    assert!(parts[1..=2].iter().all(|part| {
        part.bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
    }));
}

#[tokio::test]
async fn shutdown_terminates_event_streams() {
    let service = test_service(8);
    let mut stream = service
        .watch_events(Request::new(WatchEventsRequest {
            after_event_id: Vec::new(),
        }))
        .await
        .expect("open event stream")
        .into_inner();
    service.begin_shutdown().await;
    assert!(
        tokio::time::timeout(Duration::from_secs(1), stream.next())
            .await
            .expect("stream terminates promptly")
            .is_none()
    );
}

#[tokio::test]
async fn router_rejects_wrong_alpn_and_unknown_agent() {
    let service = test_service(8);
    let (signing_key, _) = controller_keyring();
    let trust = test_trust_with_options(signing_key, Vec::new(), None).await;
    let router = test_router(
        SecretKey::generate(),
        RelayMode::Disabled,
        IdentityPolicy::default(),
        trust,
        &service,
    )
    .await;
    let client = Endpoint::builder(presets::Minimal)
        .relay_mode(RelayMode::Disabled)
        .bind()
        .await
        .expect("build client");
    let address = router.endpoint().addr();

    assert!(
        client
            .connect(address.clone(), b"wrong/alpn/1")
            .await
            .is_err()
    );
    let unknown = client
        .connect(address, AGENT_ALPN)
        .await
        .expect("TLS identity is established before the endpoint hook rejects it");
    tokio::time::timeout(Duration::from_secs(2), unknown.closed())
        .await
        .expect("unknown endpoint closed before application data");

    client.close().await;
    shutdown(&service, router).await.expect("shutdown router");
}

#[tokio::test]
async fn newer_authority_revision_closes_the_retained_session() {
    let agent_key = SecretKey::generate();
    let handshake = handshake(&agent_key);
    let policy = identity_policy(&agent_key, &handshake);
    let service = test_service_with_policy(8, policy.clone());
    let trust = test_trust_for_key(&agent_key, &handshake, None).await;
    let router = test_router(
        SecretKey::generate(),
        RelayMode::Disabled,
        policy,
        trust,
        &service,
    )
    .await;
    let client = Endpoint::builder(presets::Minimal)
        .secret_key(agent_key.clone())
        .relay_mode(RelayMode::Disabled)
        .bind()
        .await
        .expect("build client");
    let connection = client
        .connect(router.endpoint().addr(), AGENT_ALPN)
        .await
        .expect("connect agent");
    let response = send_handshake(&connection, &handshake).await;
    assert_eq!(response.result, i32::from(HandshakeResult::Accepted));

    tokio::time::timeout(Duration::from_secs(2), async {
        loop {
            if service
                .shared
                .inner
                .connections
                .lock()
                .await
                .contains_key(&handshake.node_id)
            {
                break;
            }
            tokio::task::yield_now().await;
        }
    })
    .await
    .expect("connection registered");
    let metadata = service
        .get_node_connection(Request::new(GetNodeConnectionRequest {
            node_id: handshake.node_id.clone(),
        }))
        .await
        .expect("query connection")
        .into_inner();
    assert_eq!(metadata.endpoint_id, agent_key.public().as_bytes());
    assert_eq!(metadata.path, i32::from(ConnectionPath::Direct));

    let (signing_key, _) = controller_keyring();
    let fence = response.connection_fence.expect("session connection fence");
    let operation_id = ocservia_command_authorization::state_update_operation_id(
        metadata.node_id.as_slice().try_into().expect("node id"),
        agent_key.public().as_bytes(),
        NodeTrustState::Active as u32,
        2,
        "capability authority changed",
    );
    let binding = signed_fence_binding(
        &signing_key,
        &fence,
        FenceOperationKind::StateUpdate,
        operation_id,
        "ocserv.fencing.v2",
    );
    service
        .update_node_trust(Request::new(UpdateNodeTrustRequest {
            node_id: metadata.node_id.clone(),
            endpoint_id: agent_key.public().as_bytes().to_vec(),
            state: NodeTrustState::Active.into(),
            reason: "capability authority changed".to_owned(),
            revision: 2,
            fence_binding: Some(binding),
            operation_id: operation_id.to_vec(),
        }))
        .await
        .expect("advance connected node authority");
    tokio::time::timeout(Duration::from_secs(2), connection.closed())
        .await
        .expect("connection closed after authority revision changed");

    shutdown(&service, router).await.expect("shutdown router");
    client.close().await;
}

#[tokio::test]
async fn telemetry_uni_stream_is_identity_checked_and_published() {
    let agent_key = SecretKey::generate();
    let handshake = handshake(&agent_key);
    let service = test_service(8);
    let trust = test_trust_for_key(&agent_key, &handshake, None).await;
    let router = test_router(
        SecretKey::generate(),
        RelayMode::Disabled,
        identity_policy(&agent_key, &handshake),
        trust,
        &service,
    )
    .await;
    let client = Endpoint::builder(presets::Minimal)
        .secret_key(agent_key)
        .relay_mode(RelayMode::Disabled)
        .bind()
        .await
        .expect("build client");
    let connection = client
        .connect(router.endpoint().addr(), AGENT_ALPN)
        .await
        .expect("connect agent");
    send_handshake(&connection, &handshake).await;
    wait_until_registered(&service, &handshake.node_id).await;
    let mut events = service
        .watch_events(Request::new(WatchEventsRequest {
            after_event_id: Vec::new(),
        }))
        .await
        .expect("watch events")
        .into_inner();
    let maximum_name = |prefix: &str, index: usize| {
        format!("{prefix}{index:06}{}", "x".repeat(64 - prefix.len() - 6))
    };
    let users = (0..384)
        .map(|index| UserObservation {
            username: maximum_name("u", index),
            enabled: true,
            revision: 1,
            fingerprint_sha256: vec![1; 32],
        })
        .collect::<Vec<_>>();
    let groups = (0..768)
        .map(|index| GroupObservation {
            group_name: maximum_name("g", index),
            members: if index < 384 {
                vec![maximum_name("u", index)]
            } else {
                Vec::new()
            },
            revision: 1,
            fingerprint_sha256: vec![2; 32],
        })
        .collect::<Vec<_>>();
    let batch = TelemetryBatch {
        batch_id: Uuid::now_v7().as_bytes().to_vec(),
        node_id: handshake.node_id.clone(),
        sequence: 1,
        priority: 2,
        snapshot: None,
        sessions: Vec::new(),
        samples: Vec::new(),
        security_events: Vec::new(),
        ip_bans: Vec::new(),
        users,
        groups,
    };
    let payload = batch.encode_to_vec();
    assert!(payload.len() <= 512 * 1024);
    let mut send = connection.open_uni().await.expect("open telemetry stream");
    send.write_all(&u32::try_from(payload.len()).unwrap().to_be_bytes())
        .await
        .expect("write telemetry length");
    send.write_all(&payload)
        .await
        .expect("write telemetry batch");
    send.finish().expect("finish telemetry stream");

    let event = next_event(&mut events, TransportEventType::Telemetry).await;
    assert_eq!(event.node_id, handshake.node_id);
    assert_eq!(event.payload, payload);

    shutdown(&service, router).await.expect("shutdown router");
    client.close().await;
}

#[tokio::test]
async fn stalled_handshake_response_releases_the_connection_slot() {
    let agent_key = SecretKey::generate();
    let handshake = handshake(&agent_key);
    let service = test_service(8);
    let trust = test_trust_for_key(&agent_key, &handshake, None).await;
    let router = Router::builder(
        Endpoint::builder(presets::Minimal)
            .secret_key(SecretKey::generate())
            .relay_mode(RelayMode::Disabled)
            .bind()
            .await
            .expect("build router endpoint"),
    )
    .accept(
        AGENT_ALPN,
        SessionHandler {
            shared: service.shared.clone(),
            policy: identity_policy(&agent_key, &handshake),
            kind: ProtocolKind::Agent,
            trust,
            handshake_timeout: Duration::from_millis(500),
        },
    )
    .spawn();
    // A zero stream window never grants the server credit to write its
    // handshake response.
    let client = Endpoint::builder(presets::Minimal)
        .secret_key(agent_key)
        .relay_mode(RelayMode::Disabled)
        .transport_config(
            QuicTransportConfig::builder()
                .stream_receive_window(VarInt::from_u32(0))
                .build(),
        )
        .bind()
        .await
        .expect("build client");
    let connection = client
        .connect(router.endpoint().addr(), AGENT_ALPN)
        .await
        .expect("connect agent");
    let bytes = handshake.encode_to_vec();
    let (mut send, _recv) = connection.open_bi().await.expect("open handshake stream");
    send.write_all(&u32::try_from(bytes.len()).unwrap().to_be_bytes())
        .await
        .expect("write handshake length");
    send.write_all(&bytes).await.expect("write handshake");
    send.finish().expect("finish handshake request");

    let permits = &service.shared.inner.agent_connection_permits;
    tokio::time::timeout(Duration::from_secs(2), async {
        while permits.available_permits() == MAX_AGENT_CONNECTIONS {
            tokio::task::yield_now().await;
        }
    })
    .await
    .expect("handshake holds a connection slot");
    tokio::time::timeout(Duration::from_secs(5), connection.closed())
        .await
        .expect("stalled handshake is closed at the deadline");
    tokio::time::timeout(Duration::from_secs(2), async {
        while permits.available_permits() != MAX_AGENT_CONNECTIONS {
            tokio::task::yield_now().await;
        }
    })
    .await
    .expect("handshake slot is released");
    assert!(service.shared.inner.connections.lock().await.is_empty());

    shutdown(&service, router).await.expect("shutdown router");
    client.close().await;
}

fn telemetry_event(node_id: &[u8], bytes: usize) -> TransportEvent {
    event(
        node_id,
        &[9; 32],
        TransportEventType::Telemetry,
        vec![0; bytes],
    )
}

#[tokio::test]
async fn telemetry_flood_does_not_evict_consumers_or_starve_control_events() {
    let service = test_service(8);
    let mut events = service
        .watch_events(Request::new(WatchEventsRequest {
            after_event_id: Vec::new(),
        }))
        .await
        .expect("watch events")
        .into_inner();
    let (node_a, node_b) = (Uuid::now_v7(), Uuid::now_v7());
    // The consumer is not reading while node A floods.
    for _ in 0..64 {
        service
            .shared
            .publish_telemetry(telemetry_event(node_a.as_bytes(), 16), false)
            .await;
    }
    service
        .shared
        .publish(event(
            node_b.as_bytes(),
            &[8; 32],
            TransportEventType::Connected,
            Vec::new(),
        ))
        .await;
    {
        let state = service.shared.inner.events.lock().await;
        assert_eq!(state.subscribers.len(), 1, "flood evicted the consumer");
        assert!(state.telemetry_retained <= 4);
        assert!(state.retained.len() <= 8);
    }
    let event = next_event(&mut events, TransportEventType::Connected).await;
    assert_eq!(event.node_id, node_b.as_bytes());
}

#[tokio::test]
async fn forged_security_priority_cannot_keep_evicting_a_slow_consumer() {
    let service = test_service(8);
    let _events = service
        .watch_events(Request::new(WatchEventsRequest {
            after_event_id: Vec::new(),
        }))
        .await
        .expect("watch events")
        .into_inner();
    let (node_a, node_b) = (Uuid::now_v7(), Uuid::now_v7());
    // Every frame claims security priority while the consumer is not reading.
    let mut granted = 0;
    for _ in 0..32 {
        assert!(service.shared.admit_telemetry(node_a.as_bytes(), 16).await);
        let security = service
            .shared
            .claim_security_priority(node_a.as_bytes())
            .await;
        granted += usize::from(security);
        service
            .shared
            .publish_telemetry(telemetry_event(node_a.as_bytes(), 16), security)
            .await;
    }
    assert_eq!(granted, 4, "security priority allowance is not bounded");
    service
        .shared
        .publish(event(
            node_b.as_bytes(),
            &[8; 32],
            TransportEventType::Connected,
            Vec::new(),
        ))
        .await;
    assert_eq!(
        service.shared.inner.events.lock().await.subscribers.len(),
        1,
        "forged security telemetry evicted the consumer"
    );
    // Other nodes keep their own allowance.
    assert!(service.shared.admit_telemetry(node_b.as_bytes(), 16).await);
    assert!(
        service
            .shared
            .claim_security_priority(node_b.as_bytes())
            .await
    );
}

#[tokio::test]
async fn retained_telemetry_bytes_are_bounded_and_keep_control_events() {
    let service = test_service(4096);
    service
        .shared
        .publish(event(
            Uuid::now_v7().as_bytes(),
            &[8; 32],
            TransportEventType::Connected,
            Vec::new(),
        ))
        .await;
    for _ in 0..200 {
        service
            .shared
            .publish_telemetry(
                telemetry_event(Uuid::now_v7().as_bytes(), 512 * 1024),
                false,
            )
            .await;
    }
    let state = service.shared.inner.events.lock().await;
    assert!(state.telemetry_retained_bytes <= MAX_RETAINED_TELEMETRY_BYTES);
    assert_eq!(
        state
            .retained
            .iter()
            .map(|event| event.payload.len())
            .sum::<usize>(),
        state.telemetry_retained_bytes
    );
    assert!(
        state
            .retained
            .iter()
            .any(|event| event.r#type == i32::from(TransportEventType::Connected))
    );
}

#[tokio::test]
async fn telemetry_budget_allows_an_offline_burst_and_limits_one_node() {
    let service = test_service(8);
    let (node_a, node_b) = (Uuid::now_v7(), Uuid::now_v7());
    for frame in 0..32 {
        assert!(
            service
                .shared
                .admit_telemetry(node_a.as_bytes(), 512 * 1024)
                .await,
            "legitimate burst frame {frame} was rejected"
        );
    }
    assert!(
        !service
            .shared
            .admit_telemetry(node_a.as_bytes(), 512 * 1024)
            .await
    );
    assert!(
        service
            .shared
            .admit_telemetry(node_b.as_bytes(), 512 * 1024)
            .await,
        "another node shares node A's budget"
    );
    for _ in 0..40 {
        let _ = service.shared.admit_telemetry(node_b.as_bytes(), 1).await;
    }
    assert!(!service.shared.admit_telemetry(node_b.as_bytes(), 1).await);
}

#[tokio::test]
async fn replacement_connection_publishes_disconnect_before_connect() {
    let agent_key = SecretKey::generate();
    let handshake = handshake(&agent_key);
    let service = test_service(8);
    let trust = test_trust_for_key(&agent_key, &handshake, None).await;
    let router = test_router(
        SecretKey::generate(),
        RelayMode::Disabled,
        identity_policy(&agent_key, &handshake),
        trust,
        &service,
    )
    .await;
    let client = Endpoint::builder(presets::Minimal)
        .secret_key(agent_key)
        .relay_mode(RelayMode::Disabled)
        .bind()
        .await
        .expect("build client");
    let mut events = service
        .watch_events(Request::new(WatchEventsRequest {
            after_event_id: Vec::new(),
        }))
        .await
        .expect("watch events")
        .into_inner();

    let first = client
        .connect(router.endpoint().addr(), AGENT_ALPN)
        .await
        .expect("connect first agent session");
    send_handshake(&first, &handshake).await;
    wait_until_registered(&service, &handshake.node_id).await;
    let second = client
        .connect(router.endpoint().addr(), AGENT_ALPN)
        .await
        .expect("connect replacement agent session");
    send_handshake(&second, &handshake).await;

    let mut connectivity = Vec::new();
    tokio::time::timeout(Duration::from_secs(2), async {
        while connectivity.len() < 3 {
            let event = events
                .next()
                .await
                .expect("event stream remains open")
                .expect("event is valid");
            if event.r#type == i32::from(TransportEventType::Connected)
                || event.r#type == i32::from(TransportEventType::Disconnected)
            {
                connectivity.push(event.r#type);
            }
        }
    })
    .await
    .expect("replacement connectivity events arrive");
    assert_eq!(
        connectivity,
        vec![
            i32::from(TransportEventType::Connected),
            i32::from(TransportEventType::Disconnected),
            i32::from(TransportEventType::Connected),
        ]
    );
    tokio::time::timeout(Duration::from_secs(2), first.closed())
        .await
        .expect("superseded connection closes");

    shutdown(&service, router).await.expect("shutdown router");
    client.close().await;
}

#[tokio::test]
#[allow(clippy::too_many_lines)]
async fn command_responses_are_published_with_negotiated_limits() {
    let agent_key = SecretKey::generate();
    let mut handshake = handshake(&agent_key);
    handshake.max_message_size = 2048;
    let service = test_service(16);
    let trust = test_trust_for_key(&agent_key, &handshake, None).await;
    let router = test_router(
        SecretKey::generate(),
        RelayMode::Disabled,
        identity_policy(&agent_key, &handshake),
        trust,
        &service,
    )
    .await;
    let client = Endpoint::builder(presets::Minimal)
        .secret_key(agent_key.clone())
        .relay_mode(RelayMode::Disabled)
        .bind()
        .await
        .expect("build client");
    let connection = client
        .connect(router.endpoint().addr(), AGENT_ALPN)
        .await
        .expect("connect agent");
    let response = send_handshake(&connection, &handshake).await;
    assert_eq!(response.result, i32::from(HandshakeResult::Accepted));
    wait_until_registered(&service, &handshake.node_id).await;
    let metadata = service
        .get_node_connection(Request::new(GetNodeConnectionRequest {
            node_id: handshake.node_id.clone(),
        }))
        .await
        .expect("query negotiated session")
        .into_inner();
    assert_eq!(metadata.negotiated_capabilities, handshake.capabilities);
    let (signing_key, _) = controller_keyring();
    let session_fence = response.connection_fence.expect("session connection fence");
    let fenced_envelope = |traceparent: &str, reason: String| {
        let bare = command_envelope(&handshake.node_id, traceparent, reason);
        let op_id = binding_operation_id(&bare);
        let binding = signed_fence_binding(
            &signing_key,
            &session_fence,
            FenceOperationKind::Command,
            op_id,
            "ocserv.status.read",
        );
        let mut decoded = CommandEnvelope::decode(bare.as_slice()).expect("decode command");
        decoded.connection_fence = Some(session_fence.clone());
        decoded.fence_binding = Some(binding);
        decoded.encode_to_vec()
    };

    let mut unapproved = CommandEnvelope::decode(
        command_envelope(&handshake.node_id, &new_traceparent(), String::new()).as_slice(),
    )
    .expect("decode unapproved command");
    unapproved.required_capability = "ocserv.service.reload".to_owned();
    let error = service
        .send_command(Request::new(SendCommandRequest {
            node_id: handshake.node_id.clone(),
            command_envelope: unapproved.encode_to_vec(),
        }))
        .await
        .expect_err("unnegotiated capability rejected before transport");
    assert_eq!(error.code(), tonic::Code::PermissionDenied);

    {
        let mut connections = service.shared.inner.connections.lock().await;
        let registered = connections
            .get_mut(&handshake.node_id)
            .expect("registered session");
        registered.authorization_revision = 7;
    }
    let error = service
        .send_command(Request::new(SendCommandRequest {
            node_id: handshake.node_id.clone(),
            command_envelope: command_envelope(
                &handshake.node_id,
                &new_traceparent(),
                String::new(),
            ),
        }))
        .await
        .expect_err("stale command revision rejected before transport");
    assert_eq!(error.code(), tonic::Code::FailedPrecondition);
    {
        let mut connections = service.shared.inner.connections.lock().await;
        let registered = connections
            .get_mut(&handshake.node_id)
            .expect("registered session");
        registered.authorization_revision = 0;
        registered.session_expires_at = Some(SystemTime::UNIX_EPOCH);
    }
    let error = service
        .send_command(Request::new(SendCommandRequest {
            node_id: handshake.node_id.clone(),
            command_envelope: command_envelope(
                &handshake.node_id,
                &new_traceparent(),
                String::new(),
            ),
        }))
        .await
        .expect_err("expired session grant rejected before transport");
    assert_eq!(error.code(), tonic::Code::FailedPrecondition);
    service
        .shared
        .inner
        .connections
        .lock()
        .await
        .get_mut(&handshake.node_id)
        .expect("registered session")
        .session_expires_at = None;

    let traceparent = new_traceparent();
    let baseline_last_seen = last_seen(&service, &handshake.node_id).await;
    let oversized = command_envelope(&handshake.node_id, &traceparent, "x".repeat(2048));
    let error = service
        .send_command(Request::new(SendCommandRequest {
            node_id: handshake.node_id.clone(),
            command_envelope: oversized,
        }))
        .await
        .expect_err("negotiated limit enforced");
    assert_eq!(error.code(), tonic::Code::ResourceExhausted);
    assert_eq!(
        last_seen(&service, &handshake.node_id).await,
        baseline_last_seen
    );

    tokio::time::sleep(Duration::from_millis(2)).await;
    let agent = tokio::spawn(respond_to_command(connection.clone(), true, false));
    let mut events = service
        .watch_events(Request::new(WatchEventsRequest {
            after_event_id: Vec::new(),
        }))
        .await
        .expect("watch events")
        .into_inner();
    let command = fenced_envelope(&traceparent, String::new());
    let sent_command = CommandEnvelope::decode(command.as_slice()).expect("decode sent command");
    service
        .send_command(Request::new(SendCommandRequest {
            node_id: handshake.node_id.clone(),
            command_envelope: command,
        }))
        .await
        .expect("send command");
    agent.await.expect("agent response task");
    let event = next_event(&mut events, TransportEventType::CommandResult).await;
    assert_eq!(event.node_id, handshake.node_id);
    assert_eq!(event.traceparent, traceparent);
    let result = CommandResult::decode(event.payload.as_slice()).expect("command result");
    assert_eq!(
        CommandResultState::try_from(result.state).expect("result state"),
        CommandResultState::Succeeded
    );
    assert_eq!(result.result, b"completed");
    let proof = result
        .privileged_result_proof
        .as_ref()
        .expect("root result receipt");
    let signing_key = SigningKey::from_bytes(&[7; 32]);
    let receipt = verify_receipt(proof, &signing_key.verifying_key()).expect("valid root receipt");
    assert_eq!(receipt.node_id, sent_command.node_id);
    assert_eq!(receipt.command_id, sent_command.command_id);
    assert_eq!(receipt.operation_id, sent_command.operation_id);
    assert_eq!(receipt.idempotency_key, sent_command.idempotency_key);
    assert_eq!(
        receipt.result_bytes_sha256,
        Sha256::digest(b"completed").to_vec()
    );
    assert_ne!(
        last_seen(&service, &handshake.node_id).await,
        baseline_last_seen
    );

    let missing_receipt_traceparent = new_traceparent();
    let agent = tokio::spawn(respond_to_command(connection.clone(), false, false));
    let missing_receipt_command_bytes =
        fenced_envelope(&missing_receipt_traceparent, String::new());
    let missing_receipt_command = CommandEnvelope::decode(missing_receipt_command_bytes.as_slice())
        .expect("decode missing-receipt command");
    service
        .send_command(Request::new(SendCommandRequest {
            node_id: handshake.node_id.clone(),
            command_envelope: missing_receipt_command_bytes,
        }))
        .await
        .expect("send command with missing receipt");
    agent.await.expect("missing-receipt response task");
    let event = next_event(&mut events, TransportEventType::CommandResult).await;
    assert_eq!(event.traceparent, missing_receipt_traceparent);
    let result = CommandResult::decode(event.payload.as_slice()).expect("unknown result");
    assert_eq!(
        CommandResultState::try_from(result.state).expect("result state"),
        CommandResultState::Unknown
    );
    assert_eq!(result.error_code, "outcome_requires_reconciliation");
    assert_eq!(result.command_id, missing_receipt_command.command_id);
    assert_eq!(
        result.idempotency_key,
        missing_receipt_command.idempotency_key
    );

    let foreign_traceparent = new_traceparent();
    let agent = tokio::spawn(respond_to_command(connection.clone(), true, true));
    let foreign_command_bytes = fenced_envelope(&foreign_traceparent, String::new());
    let foreign_command = CommandEnvelope::decode(foreign_command_bytes.as_slice())
        .expect("decode foreign-result command");
    service
        .send_command(Request::new(SendCommandRequest {
            node_id: handshake.node_id.clone(),
            command_envelope: foreign_command_bytes,
        }))
        .await
        .expect("send command answered for another command");
    agent.await.expect("foreign-result response task");
    let event = next_event(&mut events, TransportEventType::CommandResult).await;
    assert_eq!(event.traceparent, foreign_traceparent);
    let result = CommandResult::decode(event.payload.as_slice()).expect("unknown result");
    assert_eq!(
        CommandResultState::try_from(result.state).expect("result state"),
        CommandResultState::Unknown
    );
    assert_eq!(result.command_id, foreign_command.command_id);

    let eof_traceparent = new_traceparent();
    let agent = tokio::spawn(end_command_without_response(connection.clone()));
    let eof_command_bytes = fenced_envelope(&eof_traceparent, String::new());
    let eof_command =
        CommandEnvelope::decode(eof_command_bytes.as_slice()).expect("decode sent EOF command");
    service
        .send_command(Request::new(SendCommandRequest {
            node_id: handshake.node_id.clone(),
            command_envelope: eof_command_bytes,
        }))
        .await
        .expect("send command with empty response");
    agent.await.expect("empty response task");
    let event = next_event(&mut events, TransportEventType::CommandResult).await;
    assert_eq!(event.traceparent, eof_traceparent);
    let result = CommandResult::decode(event.payload.as_slice()).expect("unknown result");
    assert_eq!(
        CommandResultState::try_from(result.state).expect("result state"),
        CommandResultState::Unknown
    );
    assert_eq!(result.error_code, "outcome_requires_reconciliation");
    assert_eq!(result.command_id, eof_command.command_id);
    assert_eq!(result.idempotency_key, eof_command.idempotency_key);
    assert_eq!(result.payload_sha256, eof_command.semantic_payload_sha256);
    assert_eq!(
        result.semantic_payload_hash_version,
        eof_command.semantic_payload_hash_version
    );

    // A Heartbeat on a command stream bypasses the telemetry quota, so it
    // is never published and leaves the command outcome unknown.
    let heartbeat_traceparent = new_traceparent();
    let agent = tokio::spawn(respond_with_heartbeat_first(connection.clone()));
    let heartbeat_command_bytes = fenced_envelope(&heartbeat_traceparent, String::new());
    let heartbeat_command = CommandEnvelope::decode(heartbeat_command_bytes.as_slice())
        .expect("decode heartbeat command");
    service
        .send_command(Request::new(SendCommandRequest {
            node_id: handshake.node_id.clone(),
            command_envelope: heartbeat_command_bytes,
        }))
        .await
        .expect("send command answered with heartbeat");
    agent.await.expect("heartbeat response task");
    let event = tokio::time::timeout(Duration::from_secs(2), async {
        loop {
            let event = events
                .next()
                .await
                .expect("event stream remains open")
                .expect("event is valid");
            assert_ne!(
                event.r#type,
                i32::from(TransportEventType::Heartbeat),
                "command-stream heartbeat published"
            );
            if event.r#type == i32::from(TransportEventType::CommandResult) {
                break event;
            }
        }
    })
    .await
    .expect("event arrives");
    assert_eq!(event.traceparent, heartbeat_traceparent);
    let result = CommandResult::decode(event.payload.as_slice()).expect("unknown result");
    assert_eq!(
        CommandResultState::try_from(result.state).expect("result state"),
        CommandResultState::Unknown
    );
    assert_eq!(result.command_id, heartbeat_command.command_id);

    shutdown(&service, router).await.expect("shutdown router");
    client.close().await;
}

#[tokio::test]
async fn relay_only_connection_and_disabled_relay_failure() {
    let (relay_map, relay_roots, relay) = private_tls_relay().await;
    let agent_key = SecretKey::generate();
    let handshake = handshake(&agent_key);
    let service = test_service(8);
    let trust = test_trust_for_key(&agent_key, &handshake, None).await;
    let router = build_router_with_direct(
        SecretKey::generate(),
        RelayMode::Custom(relay_map.clone()),
        identity_policy(&agent_key, &handshake),
        trust,
        &service,
        false,
        relay_roots.clone(),
    )
    .await
    .expect("build router");
    tokio::time::timeout(Duration::from_secs(30), router.endpoint().online())
        .await
        .expect("controller endpoint online");
    let full_address = router.endpoint().addr();
    let relay_url = full_address
        .relay_urls()
        .next()
        .expect("controller selected a home relay")
        .clone();
    let relay_only = iroh::EndpointAddr::new(full_address.id).with_relay_url(relay_url);

    let disabled = Endpoint::builder(presets::Minimal)
        .relay_mode(RelayMode::Disabled)
        .bind()
        .await
        .expect("build relay-disabled client");
    let disabled_attempt = tokio::time::timeout(
        Duration::from_secs(5),
        disabled.connect(relay_only.clone(), AGENT_ALPN),
    )
    .await;
    assert!(!matches!(disabled_attempt, Ok(Ok(_))));
    disabled.close().await;

    let client = Endpoint::builder(presets::N0)
        .secret_key(agent_key.clone())
        .relay_mode(RelayMode::Custom(relay_map))
        .ca_tls_config(CaTlsConfig::default().with_extra_roots(relay_roots))
        .clear_address_lookup()
        .clear_ip_transports()
        .bind()
        .await
        .expect("build relay client");
    let connection = tokio::time::timeout(
        Duration::from_secs(30),
        client.connect(relay_only, AGENT_ALPN),
    )
    .await
    .expect("relay connection completed")
    .expect("connect over relay");
    let response = send_handshake(&connection, &handshake).await;
    assert_eq!(response.result, i32::from(HandshakeResult::Accepted));
    tokio::time::timeout(Duration::from_secs(2), async {
        loop {
            let path = service
                .shared
                .inner
                .connections
                .lock()
                .await
                .get(&handshake.node_id)
                .map(|entry| entry.metadata.path);
            if path == Some(i32::from(ConnectionPath::Relay)) {
                break;
            }
            tokio::task::yield_now().await;
        }
    })
    .await
    .expect("relay path registered");

    shutdown(&service, router).await.expect("shutdown router");
    client.close().await;
    relay.shutdown().await.expect("stop private relay");
}

#[tokio::test]
async fn malformed_relay_batches_preserve_receive_progress() {
    iroh::test_utils::assert_relay_receive_progress().await;
}

#[test]
fn relay_mapping_bounds_preserve_live_addresses() {
    iroh::test_utils::assert_relay_mapping_bounds();
}

#[tokio::test]
async fn single_relay_recovers_without_replacing_endpoint() {
    let (_, roots, relay) = private_tls_relay().await;
    let relay_addr = relay.https_addr().expect("TLS listener");
    let proxy = RestartableTcpProxy::start("127.0.0.1:0".parse().unwrap(), relay_addr).await;
    let url: iroh::RelayUrl = format!("https://{}", proxy.addr).parse().unwrap();
    let map = RelayMap::from_iter([iroh_relay::RelayConfig::from(url.clone())]);
    let agent_key = SecretKey::generate();
    let handshake = handshake(&agent_key);
    let service = test_service(16);
    let trust = test_trust_for_key(&agent_key, &handshake, None).await;
    let router = build_router_with_direct(
        SecretKey::generate(),
        RelayMode::Custom(map.clone()),
        identity_policy(&agent_key, &handshake),
        trust,
        &service,
        false,
        roots.clone(),
    )
    .await
    .expect("single relay router");
    let identity = router.endpoint().id();
    let mut proxy = Some(proxy);
    for recovering in [false, true] {
        if recovering {
            let addr = proxy.take().unwrap().stop().await;
            tokio::time::timeout(Duration::from_secs(10), async {
                while router
                    .endpoint()
                    .home_relay_status()
                    .get()
                    .iter()
                    .any(iroh::endpoint::RelayStatus::is_connected)
                {
                    tokio::time::sleep(Duration::from_millis(20)).await;
                }
            })
            .await
            .expect("observe disconnected Relay");
            proxy = Some(RestartableTcpProxy::start(addr, relay_addr).await);
        }
        tokio::time::timeout(Duration::from_secs(90), router.endpoint().online())
            .await
            .expect("single relay online without endpoint restart");
        let client = Endpoint::builder(presets::Minimal)
            .secret_key(agent_key.clone())
            .relay_mode(RelayMode::Custom(map.clone()))
            .ca_tls_config(CaTlsConfig::default().with_extra_roots(roots.clone()))
            .clear_ip_transports()
            .bind()
            .await
            .expect("relay-only agent");
        let target = iroh::EndpointAddr::new(identity).with_relay_url(url.clone());
        let connection =
            tokio::time::timeout(Duration::from_secs(15), client.connect(target, AGENT_ALPN))
                .await
                .expect("connection deadline")
                .expect("authenticated ALPN connection");
        assert_eq!(
            send_handshake(&connection, &handshake).await.result,
            i32::from(HandshakeResult::Accepted)
        );
        assert_eq!(router.endpoint().id(), identity);
        client.close().await;
    }
    shutdown(&service, router).await.expect("shutdown router");
    proxy.unwrap().stop().await;
    relay.shutdown().await.expect("stop private relay");
}

#[tokio::test]
async fn unsigned_artifact_requests_fail_before_connection_lookup() {
    let service = test_service(8);
    let node_id = Uuid::now_v7();
    let artifact_id = Uuid::now_v7();
    let Err(fetch) = service
        .fetch_artifact(Request::new(FetchArtifactRequest {
            node_id: node_id.as_bytes().to_vec(),
            artifact_id: artifact_id.as_bytes().to_vec(),
            purpose: "certificate_p12".to_owned(),
            max_bytes: 32,
            grant: None,
            fence_binding: None,
        }))
        .await
    else {
        panic!("unsigned artifact fetch must fail");
    };
    assert_eq!(fetch.code(), tonic::Code::PermissionDenied);
    let consume = service
        .consume_artifact(Request::new(ConsumeArtifactRequest {
            node_id: node_id.as_bytes().to_vec(),
            grant: None,
            sha256: vec![0; 32],
            size: 32,
            confirm_only: false,
            fence_binding: None,
        }))
        .await
        .expect_err("unsigned artifact consume must fail");
    assert_eq!(consume.code(), tonic::Code::PermissionDenied);
}

/// Pins the streaming acceptance boundary contract: `fetch_artifact` may
/// not resolve — and therefore may not emit response headers or frames,
/// which the controller's ownership guard waits on — before fence
/// verification has run. Holding the fence registry lock keeps
/// verification from completing, so the handler future must stay pending
/// and finish only once verification is allowed to proceed.
#[tokio::test]
async fn artifact_fetch_acceptance_waits_for_binding_verification() {
    let (signing_key, keyring) = controller_keyring();
    let node_id = *Uuid::now_v7().as_bytes();
    let service = IrohTransportService::new(8, IdentityPolicy::default(), keyring);
    let (_term, fence, _router, _client, _connection) =
        fenced_agent_session(&service, &signing_key, node_id, 57).await;
    service
        .register_owner_fence(Request::new(RegisterOwnerFenceRequest {
            fence: Some(fence.clone()),
        }))
        .await
        .expect("register owner fence");
    let artifact_id = *Uuid::now_v7().as_bytes();
    let binding = signed_fence_binding(
        &signing_key,
        &fence,
        FenceOperationKind::Artifact,
        artifact_id,
        "ocserv.fencing.v2",
    );
    let mut pending_fetch = service.fetch_artifact(Request::new(FetchArtifactRequest {
        node_id: node_id.to_vec(),
        artifact_id: artifact_id.to_vec(),
        purpose: "certificate_p12".to_owned(),
        max_bytes: 32,
        grant: Some(shape_valid_grant(node_id, artifact_id)),
        fence_binding: Some(binding),
    }));
    let registry = service.shared.inner.fences.lock().await;
    let blocked = tokio::time::timeout(Duration::from_millis(250), &mut pending_fetch).await;
    assert!(
        blocked.is_err(),
        "fetch_artifact resolved before the fence binding was verified"
    );
    drop(registry);
    tokio::time::timeout(Duration::from_secs(5), pending_fetch)
        .await
        .expect("fetch_artifact completes once verification runs")
        .expect("verified artifact fetch accepted");
    service.begin_shutdown().await;
}

#[tokio::test]
#[allow(clippy::too_many_lines)]
async fn artifact_fence_carriers_cross_the_agent_boundary() {
    let (signing_key, keyring) = controller_keyring();
    let node_id = *Uuid::now_v7().as_bytes();
    let service = IrohTransportService::new(8, IdentityPolicy::default(), keyring);
    let (term, _handshake_fence, _router, _client, connection) =
        fenced_agent_session(&service, &signing_key, node_id, 58).await;
    // Keep the immutable session term but age its handshake carrier past
    // the lease deadline. Registration refreshes that same term, and the
    // refreshed wire proof is the one transportd must relay to the Agent.
    let expired_session_fence = signed_controller_fence_at(&signing_key, &term, -10, -1);
    let expired_verified = service
        .shared
        .keyring()
        .verify_connection_fence_v2(
            &expired_session_fence,
            &term.node_id,
            &term.endpoint_id,
            unix_now(),
        )
        .expect("verify expired-lease session proof");
    let mut connections = service.shared.inner.connections.lock().await;
    connections
        .get_mut(node_id.as_slice())
        .expect("registered session")
        .fence = RegisteredFence {
        verified: expired_verified,
        fence: expired_session_fence.clone(),
        endpoint_id: term.endpoint_id,
    };
    drop(connections);
    let fence = signed_controller_fence(&signing_key, &term, 60);
    service
        .register_owner_fence(Request::new(RegisterOwnerFenceRequest {
            fence: Some(fence.clone()),
        }))
        .await
        .expect("register owner fence");

    let artifact_id = *Uuid::now_v7().as_bytes();
    let grant = shape_valid_grant(node_id, artifact_id);
    let fetch_binding = signed_fence_binding(
        &signing_key,
        &fence,
        FenceOperationKind::Artifact,
        artifact_id,
        "ocserv.fencing.v2",
    );
    let mut fetched = service
        .fetch_artifact(Request::new(FetchArtifactRequest {
            node_id: node_id.to_vec(),
            artifact_id: artifact_id.to_vec(),
            purpose: "certificate_p12".to_owned(),
            max_bytes: 32,
            grant: Some(grant.clone()),
            fence_binding: Some(fetch_binding.clone()),
        }))
        .await
        .expect("fenced fetch accepted")
        .into_inner();
    let (mut fetch_send, mut fetch_recv) =
        tokio::time::timeout(Duration::from_secs(5), connection.accept_bi())
            .await
            .expect("artifact fetch stream opened")
            .expect("accept artifact fetch stream");
    let mut framed = [0_u8; 4];
    fetch_recv
        .read_exact(&mut framed)
        .await
        .expect("read artifact fetch length");
    assert_eq!(framed[0] & 0xc0, 0x80);
    let framed = u32::from_be_bytes(framed);
    let mut body = vec![0_u8; (framed & !ARTIFACT_FETCH_FRAME) as usize];
    fetch_recv
        .read_exact(&mut body)
        .await
        .expect("read artifact fetch request");
    let forwarded =
        ArtifactFetchRequest::decode(body.as_slice()).expect("decode forwarded artifact fetch");
    assert_eq!(forwarded.connection_fence.as_ref(), Some(&fence));
    assert_ne!(
        forwarded.connection_fence.as_ref(),
        Some(&expired_session_fence),
        "transportd must not relay the expired handshake carrier"
    );
    assert_eq!(forwarded.fence_binding.as_ref(), Some(&fetch_binding));
    let chunk = ArtifactChunk {
        artifact_id: artifact_id.to_vec(),
        offset: 0,
        data: vec![1],
        eof: true,
        sha256: vec![0; 32],
    }
    .encode_to_vec();
    fetch_send
        .write_all(
            &u32::try_from(chunk.len())
                .expect("chunk length")
                .to_be_bytes(),
        )
        .await
        .expect("write artifact chunk length");
    fetch_send
        .write_all(&chunk)
        .await
        .expect("write artifact chunk");
    fetch_send.finish().expect("finish artifact fetch response");
    assert!(fetched.next().await.expect("artifact stream item").is_ok());
    assert!(
        tokio::time::timeout(Duration::from_secs(5), fetch_send.stopped())
            .await
            .expect("artifact transport acknowledgement arrives")
            .expect("artifact transport acknowledgement succeeds")
            .is_none(),
        "the terminal artifact chunk must not cancel the Agent send stream"
    );

    let grant_id: [u8; 16] = grant.grant_id.as_slice().try_into().expect("grant id");
    let consume_binding = signed_fence_binding(
        &signing_key,
        &fence,
        FenceOperationKind::Artifact,
        grant_id,
        "ocserv.fencing.v2",
    );
    let consume = tokio::spawn({
        let service = service.clone();
        let grant = grant.clone();
        let consume_binding = consume_binding.clone();
        async move {
            service
                .consume_artifact(Request::new(ConsumeArtifactRequest {
                    node_id: node_id.to_vec(),
                    grant: Some(grant),
                    sha256: vec![0x11; 32],
                    size: 32,
                    confirm_only: false,
                    fence_binding: Some(consume_binding),
                }))
                .await
        }
    });
    let (mut consume_send, mut consume_recv) =
        tokio::time::timeout(Duration::from_secs(5), connection.accept_bi())
            .await
            .expect("artifact consume stream opened")
            .expect("accept artifact consume stream");
    let mut framed = [0_u8; 4];
    consume_recv
        .read_exact(&mut framed)
        .await
        .expect("read artifact consume length");
    assert_eq!(framed[0] & 0xc0, 0xc0);
    let framed = u32::from_be_bytes(framed);
    let mut body = vec![0_u8; (framed & !ARTIFACT_CONSUME_FRAME) as usize];
    consume_recv
        .read_exact(&mut body)
        .await
        .expect("read artifact consume request");
    let forwarded = AgentArtifactConsumeRequest::decode(body.as_slice())
        .expect("decode forwarded artifact consume");
    assert_eq!(forwarded.connection_fence.as_ref(), Some(&fence));
    assert_eq!(forwarded.fence_binding.as_ref(), Some(&consume_binding));
    let response = AgentArtifactConsumeResponse {
        artifact_id: artifact_id.to_vec(),
        grant_id: grant_id.to_vec(),
        consumed: true,
    }
    .encode_to_vec();
    consume_send
        .write_all(
            &u32::try_from(response.len())
                .expect("consume response length")
                .to_be_bytes(),
        )
        .await
        .expect("write consume response length");
    consume_send
        .write_all(&response)
        .await
        .expect("write consume response");
    consume_send.finish().expect("finish consume response");
    consume
        .await
        .expect("consume task")
        .expect("fenced consume accepted");

    service.begin_shutdown().await;
}

#[tokio::test]
async fn relay_and_direct_paths_converge_to_direct() {
    let (relay_map, relay_roots, relay) = private_tls_relay().await;
    let agent_key = SecretKey::generate();
    let handshake = handshake(&agent_key);
    let service = test_service(16);
    let trust = test_trust_for_key(&agent_key, &handshake, None).await;
    let router = build_router_with_trust_tls_roots(
        SecretKey::generate(),
        RelayMode::Custom(relay_map.clone()),
        identity_policy(&agent_key, &handshake),
        trust,
        relay_roots.clone(),
        &service,
    )
    .await
    .expect("build router");
    tokio::time::timeout(Duration::from_secs(30), router.endpoint().online())
        .await
        .expect("controller endpoint online");
    let client = Endpoint::builder(presets::N0)
        .secret_key(agent_key.clone())
        .relay_mode(RelayMode::Custom(relay_map))
        .ca_tls_config(CaTlsConfig::default().with_extra_roots(relay_roots))
        .clear_address_lookup()
        .bind()
        .await
        .expect("build client");
    let connection = tokio::time::timeout(
        Duration::from_secs(30),
        client.connect(router.endpoint().addr(), AGENT_ALPN),
    )
    .await
    .expect("connection completed")
    .expect("connect agent");
    let response = send_handshake(&connection, &handshake).await;
    assert_eq!(response.result, i32::from(HandshakeResult::Accepted));

    tokio::time::timeout(Duration::from_secs(30), async {
        loop {
            let paths = connection.paths();
            let has_relay = paths.iter().any(|path| path.is_relay());
            let direct_selected = paths.iter().any(|path| path.is_ip() && path.is_selected());
            if has_relay && direct_selected {
                break;
            }
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
    })
    .await
    .expect("relay fallback and selected direct path observed");
    let metadata = service
        .get_node_connection(Request::new(GetNodeConnectionRequest {
            node_id: handshake.node_id,
        }))
        .await
        .expect("query connection")
        .into_inner();
    assert_eq!(metadata.path, i32::from(ConnectionPath::Direct));

    shutdown(&service, router).await.expect("shutdown router");
    client.close().await;
    relay.shutdown().await.expect("stop private relay");
}

// ---------------------------------------------------------------------------
// Connection-owner fence enforcement
// ---------------------------------------------------------------------------

struct FenceTerm {
    node_id: [u8; 16],
    endpoint_id: [u8; 32],
    owner_instance_id: [u8; 16],
    owner_incarnation: u64,
    owner_epoch: u64,
    fence_id: [u8; 16],
    connection_id: [u8; 16],
    authorization_revision: u64,
    capabilities: Vec<String>,
}

impl FenceTerm {
    fn new(node_id: [u8; 16], endpoint_id: [u8; 32], owner_epoch: u64) -> Self {
        Self {
            node_id,
            endpoint_id,
            owner_instance_id: *Uuid::now_v7().as_bytes(),
            owner_incarnation: 1,
            owner_epoch,
            fence_id: *Uuid::now_v7().as_bytes(),
            connection_id: *Uuid::now_v7().as_bytes(),
            authorization_revision: 1,
            capabilities: vec![
                "ocserv.fencing.v2".to_owned(),
                "ocserv.service.reload".to_owned(),
            ],
        }
    }

    /// Advances to the next takeover term of the same ownership lineage.
    fn supersede(&mut self) {
        self.owner_epoch += 1;
        self.fence_id = *Uuid::now_v7().as_bytes();
        self.connection_id = *Uuid::now_v7().as_bytes();
    }
}

fn unix_seconds() -> i64 {
    i64::try_from(
        SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("clock after epoch")
            .as_secs(),
    )
    .expect("unix seconds fit")
}

fn signed_controller_fence(
    signing_key: &SigningKey,
    term: &FenceTerm,
    lease_seconds: i64,
) -> ConnectionFenceV2 {
    signed_controller_fence_at(signing_key, term, 0, lease_seconds)
}

fn signed_controller_fence_at(
    signing_key: &SigningKey,
    term: &FenceTerm,
    issued_offset_seconds: i64,
    lease_until_offset_seconds: i64,
) -> ConnectionFenceV2 {
    let now = unix_seconds() + issued_offset_seconds;
    let lease_until = unix_seconds() + lease_until_offset_seconds;
    let claims = ConnectionFenceClaimsV2 {
        signature_version: ocservia_command_authorization::FENCE_SIGNATURE_VERSION_ED25519_V1,
        key_id: verification_key_id(&signing_key.verifying_key()),
        fence_id: term.fence_id,
        node_id: term.node_id,
        endpoint_id: term.endpoint_id,
        owner_instance_id: term.owner_instance_id,
        owner_incarnation: term.owner_incarnation,
        owner_epoch: term.owner_epoch,
        connection_id: term.connection_id,
        authorization_revision: term.authorization_revision,
        capabilities: term.capabilities.clone(),
        lease_until_seconds: lease_until,
        lease_until_nanos: 0,
        issued_at_seconds: now,
        issued_at_nanos: 0,
        expires_at_seconds: lease_until.max(now) + 300,
        expires_at_nanos: 0,
    };
    let canonical = canonical_connection_fence_v2(&claims).expect("canonical fence");
    ConnectionFenceV2 {
        signature_version: FenceSignatureVersion::Ed25519V1.into(),
        key_id: claims.key_id,
        fence_id: term.fence_id.to_vec(),
        node_id: term.node_id.to_vec(),
        endpoint_id: term.endpoint_id.to_vec(),
        owner_instance_id: term.owner_instance_id.to_vec(),
        owner_incarnation: term.owner_incarnation,
        owner_epoch: term.owner_epoch,
        connection_id: term.connection_id.to_vec(),
        authorization_revision: term.authorization_revision,
        capabilities: term.capabilities.clone(),
        lease_until: Some(prost_types::Timestamp {
            seconds: claims.lease_until_seconds,
            nanos: 0,
        }),
        issued_at: Some(prost_types::Timestamp {
            seconds: claims.issued_at_seconds,
            nanos: 0,
        }),
        expires_at: Some(prost_types::Timestamp {
            seconds: claims.expires_at_seconds,
            nanos: 0,
        }),
        signature: signing_key.sign(&canonical).to_bytes().to_vec(),
    }
}

fn signed_fence_binding(
    signing_key: &SigningKey,
    fence: &ConnectionFenceV2,
    kind: FenceOperationKind,
    operation_id: [u8; 16],
    capability: &str,
) -> FenceBindingV2 {
    let fence_claims = connection_fence_claims_v2(fence).expect("fence claims");
    let now = unix_seconds();
    let claims = FenceBindingClaimsV2 {
        signature_version: ocservia_command_authorization::FENCE_SIGNATURE_VERSION_ED25519_V1,
        key_id: fence_claims.key_id.clone(),
        operation_kind: kind as u32,
        operation_id,
        fence_id: fence_claims.fence_id,
        node_id: fence_claims.node_id,
        endpoint_id: fence_claims.endpoint_id,
        owner_instance_id: fence_claims.owner_instance_id,
        owner_incarnation: fence_claims.owner_incarnation,
        owner_epoch: fence_claims.owner_epoch,
        connection_id: fence_claims.connection_id,
        authorization_revision: fence_claims.authorization_revision,
        capability: capability.to_owned(),
        issued_at_seconds: now,
        issued_at_nanos: 0,
        expires_at_seconds: now + 300,
        expires_at_nanos: 0,
    };
    let canonical = canonical_fence_binding_v2(&claims).expect("canonical binding");
    FenceBindingV2 {
        signature_version: FenceSignatureVersion::Ed25519V1.into(),
        key_id: claims.key_id,
        operation_kind: kind.into(),
        operation_id: operation_id.to_vec(),
        fence_id: fence_claims.fence_id.to_vec(),
        node_id: fence_claims.node_id.to_vec(),
        endpoint_id: fence_claims.endpoint_id.to_vec(),
        owner_instance_id: fence_claims.owner_instance_id.to_vec(),
        owner_incarnation: fence_claims.owner_incarnation,
        owner_epoch: fence_claims.owner_epoch,
        connection_id: fence_claims.connection_id.to_vec(),
        authorization_revision: fence_claims.authorization_revision,
        capability: capability.to_owned(),
        issued_at: Some(prost_types::Timestamp {
            seconds: claims.issued_at_seconds,
            nanos: 0,
        }),
        expires_at: Some(prost_types::Timestamp {
            seconds: claims.expires_at_seconds,
            nanos: 0,
        }),
        signature: signing_key.sign(&canonical).to_bytes().to_vec(),
    }
}

fn controller_keyring() -> (SigningKey, Arc<ControllerCommandKeyring>) {
    let signing_key = SigningKey::from_bytes(&[7; 32]);
    let keyring = Arc::new(
        ControllerCommandKeyring::new([signing_key.verifying_key()])
            .expect("test controller keyring"),
    );
    (signing_key, keyring)
}

/// Registers a real agent session that claims the supplied node and
/// promotes it to a fenced mutation session under one owner term,
/// mirroring what negotiation records for a fence-carrying authorized
/// handshake. The returned handles must stay alive for the session to
/// remain registered.
#[allow(clippy::type_complexity)]
async fn fenced_agent_session(
    service: &IrohTransportService,
    signing_key: &SigningKey,
    node_id: [u8; 16],
    owner_epoch: u64,
) -> (
    FenceTerm,
    ConnectionFenceV2,
    iroh::protocol::Router,
    Endpoint,
    Connection,
) {
    let agent_key = SecretKey::generate();
    let mut handshake = handshake(&agent_key);
    handshake.node_id = node_id.to_vec();
    handshake.capabilities = vec![
        "ocserv.fencing.v2".to_owned(),
        "ocserv.service.reload".to_owned(),
        "ocserv.status.read".to_owned(),
    ];
    let mut term = FenceTerm::new(node_id, *agent_key.public().as_bytes(), owner_epoch);
    term.capabilities.clone_from(&handshake.capabilities);
    let fence = signed_controller_fence(signing_key, &term, 120);
    let trust = test_trust_with_options(
        signing_key.clone(),
        vec![(
            agent_key.public(),
            handshake.node_id.clone(),
            NodeTrustState::Active,
            1,
        )],
        Some(fence.clone()),
    )
    .await;
    let router = test_router(
        SecretKey::generate(),
        RelayMode::Disabled,
        identity_policy(&agent_key, &handshake),
        trust,
        service,
    )
    .await;
    let client = Endpoint::builder(presets::Minimal)
        .secret_key(agent_key.clone())
        .relay_mode(RelayMode::Disabled)
        .bind()
        .await
        .expect("build client");
    let connection = client
        .connect(router.endpoint().addr(), AGENT_ALPN)
        .await
        .expect("connect agent");
    let response = send_handshake(&connection, &handshake).await;
    assert_eq!(response.result, i32::from(HandshakeResult::Accepted));
    wait_until_registered(service, &handshake.node_id).await;
    (term, fence, router, client, connection)
}

fn fenced_command_envelope(
    node_id: &[u8],
    traceparent: &str,
    fence: &ConnectionFenceV2,
    binding: &FenceBindingV2,
) -> Vec<u8> {
    let mut command =
        CommandEnvelope::decode(command_envelope(node_id, traceparent, String::new()).as_slice())
            .expect("decode base command");
    command.connection_fence = Some(fence.clone());
    command.fence_binding = Some(binding.clone());
    command.encode_to_vec()
}

fn binding_operation_id(command_bytes: &[u8]) -> [u8; 16] {
    let command = CommandEnvelope::decode(command_bytes).expect("decode fenced command");
    command.command_id.clone().try_into().expect("command id")
}

#[tokio::test]
#[allow(clippy::too_many_lines)]
async fn fenced_command_dispatch_requires_the_registered_owner_term() {
    let (signing_key, keyring) = controller_keyring();
    let node_id = *Uuid::now_v7().as_bytes();
    let service = IrohTransportService::new(8, IdentityPolicy::default(), keyring);
    let (mut term, fence, _router, _client, _connection) =
        fenced_agent_session(&service, &signing_key, node_id, 41).await;
    let first = service
        .register_owner_fence(Request::new(RegisterOwnerFenceRequest {
            fence: Some(fence.clone()),
        }))
        .await
        .expect("register owner fence")
        .into_inner();
    assert_eq!(
        first.disposition,
        i32::from(OwnerFenceDisposition::Refreshed)
    );
    assert_eq!(first.retained_epoch, 41);

    // A fenced session without its fence carriers is rejected.
    let bare = command_envelope(&node_id, &new_traceparent(), String::new());
    let error = service
        .send_command(Request::new(SendCommandRequest {
            node_id: node_id.to_vec(),
            command_envelope: bare,
        }))
        .await
        .expect_err("fenced session requires fence carriers");
    assert_eq!(error.code(), tonic::Code::PermissionDenied);

    // A binding for another command does not authorize this dispatch.
    let other_command = command_envelope(&node_id, &new_traceparent(), String::new());
    let other_id = binding_operation_id(&other_command);
    let misbound = signed_fence_binding(
        &signing_key,
        &fence,
        FenceOperationKind::Command,
        other_id,
        "ocserv.service.reload",
    );
    let this_command = command_envelope(&node_id, &new_traceparent(), String::new());
    let this_id = binding_operation_id(&this_command);
    let mismatched = fenced_command_envelope(&node_id, &new_traceparent(), &fence, &misbound);
    let error = service
        .send_command(Request::new(SendCommandRequest {
            node_id: node_id.to_vec(),
            command_envelope: mismatched,
        }))
        .await
        .expect_err("binding must match the dispatched command");
    assert_eq!(error.code(), tonic::Code::PermissionDenied);

    // A capability outside the fence cannot be bound to this command.
    let unheld = signed_fence_binding(
        &signing_key,
        &fence,
        FenceOperationKind::Command,
        this_id,
        "ocserv.tenant.admin",
    );
    let unheld_bytes = fenced_command_envelope(&node_id, &new_traceparent(), &fence, &unheld);
    let error = service
        .send_command(Request::new(SendCommandRequest {
            node_id: node_id.to_vec(),
            command_envelope: unheld_bytes,
        }))
        .await
        .expect_err("unheld capability rejected");
    assert_eq!(error.code(), tonic::Code::PermissionDenied);

    // A dispatch from a superseded owner term is rejected before the
    // Agent stream is opened.
    term.supersede();
    let successor = signed_controller_fence(&signing_key, &term, 30);
    let takeover = service
        .register_owner_fence(Request::new(RegisterOwnerFenceRequest {
            fence: Some(successor.clone()),
        }))
        .await
        .expect("register successor fence")
        .into_inner();
    assert_eq!(
        takeover.disposition,
        i32::from(OwnerFenceDisposition::Applied)
    );
    assert_eq!(takeover.retained_epoch, 42);
    let stale_binding = signed_fence_binding(
        &signing_key,
        &fence,
        FenceOperationKind::Command,
        this_id,
        "ocserv.service.reload",
    );
    let stale_bytes = fenced_command_envelope(&node_id, &new_traceparent(), &fence, &stale_binding);
    let error = service
        .send_command(Request::new(SendCommandRequest {
            node_id: node_id.to_vec(),
            command_envelope: stale_bytes,
        }))
        .await
        .expect_err("stale owner dispatch rejected");
    // The takeover already retired the live mutation session, so the
    // stale dispatch is refused either by the fence predicate or by the
    // missing retired session; neither reaches the Agent stream.
    assert!(matches!(
        error.code(),
        tonic::Code::PermissionDenied | tonic::Code::Unavailable
    ));

    // The takeover retired the live mutation session, so no Agent
    // stream remains open for the superseded owner.
    assert!(
        service
            .shared
            .inner
            .connections
            .lock()
            .await
            .get(&node_id.to_vec())
            .is_none()
    );

    service.begin_shutdown().await;
}

#[tokio::test]
async fn higher_epoch_registration_retires_the_superseded_session() {
    let (signing_key, keyring) = controller_keyring();
    let node_id = *Uuid::now_v7().as_bytes();
    let service = IrohTransportService::new(8, IdentityPolicy::default(), keyring);
    let (mut term, fence, _router, _client, _connection) =
        fenced_agent_session(&service, &signing_key, node_id, 7).await;
    service
        .register_owner_fence(Request::new(RegisterOwnerFenceRequest {
            fence: Some(fence.clone()),
        }))
        .await
        .expect("register initial fence");

    // Same term refreshes the lease deadline.
    let refreshed = signed_controller_fence(&signing_key, &term, 60);
    let response = service
        .register_owner_fence(Request::new(RegisterOwnerFenceRequest {
            fence: Some(refreshed),
        }))
        .await
        .expect("refresh fence")
        .into_inner();
    assert_eq!(
        response.disposition,
        i32::from(OwnerFenceDisposition::Refreshed)
    );
    assert_eq!(response.retained_epoch, 7);

    // A takeover with a strictly higher epoch retires the live session.
    term.supersede();
    let successor = signed_controller_fence(&signing_key, &term, 30);
    let response = service
        .register_owner_fence(Request::new(RegisterOwnerFenceRequest {
            fence: Some(successor),
        }))
        .await
        .expect("register successor fence")
        .into_inner();
    assert_eq!(
        response.disposition,
        i32::from(OwnerFenceDisposition::Applied)
    );
    assert!(
        service
            .shared
            .inner
            .connections
            .lock()
            .await
            .get(&node_id.to_vec())
            .is_none()
    );

    // The resumed previous owner is stale and cannot reclaim.
    let stale = signed_controller_fence(
        &signing_key,
        &FenceTerm::new(node_id, term.endpoint_id, 7),
        30,
    );
    let response = service
        .register_owner_fence(Request::new(RegisterOwnerFenceRequest {
            fence: Some(stale),
        }))
        .await
        .expect("stale fence disposition")
        .into_inner();
    assert_eq!(
        response.disposition,
        i32::from(OwnerFenceDisposition::Stale)
    );
    assert_eq!(response.retained_epoch, 8);
    let retained = service
        .get_owner_fence(Request::new(GetOwnerFenceRequest {
            node_id: node_id.to_vec(),
        }))
        .await
        .expect("retained fence readable")
        .into_inner();
    assert_eq!(retained.fence.expect("retained fence").owner_epoch, 8);

    // Two distinct fences may not claim one epoch.
    let collision = signed_controller_fence(
        &signing_key,
        &FenceTerm::new(node_id, term.endpoint_id, 8),
        30,
    );
    let error = service
        .register_owner_fence(Request::new(RegisterOwnerFenceRequest {
            fence: Some(collision),
        }))
        .await
        .expect_err("epoch collision rejected");
    assert_eq!(error.code(), tonic::Code::InvalidArgument);

    service.begin_shutdown().await;
}

#[tokio::test]
async fn owner_lease_expiry_closes_the_existing_agent_session() {
    let (signing_key, keyring) = controller_keyring();
    let node_id = *Uuid::now_v7().as_bytes();
    let service = IrohTransportService::new(8, IdentityPolicy::default(), keyring);
    let (term, _, _router, _client, connection) =
        fenced_agent_session(&service, &signing_key, node_id, 7).await;
    service
        .register_owner_fence(Request::new(RegisterOwnerFenceRequest {
            fence: Some(signed_controller_fence(&signing_key, &term, 2)),
        }))
        .await
        .expect("register short owner lease");
    tokio::time::timeout(Duration::from_secs(5), connection.closed())
        .await
        .expect("expired owner session closes without a new command");
    service.begin_shutdown().await;
}

#[tokio::test]
async fn owner_lease_refresh_keeps_the_existing_agent_session() {
    let (signing_key, keyring) = controller_keyring();
    let node_id = *Uuid::now_v7().as_bytes();
    let service = IrohTransportService::new(8, IdentityPolicy::default(), keyring);
    let (term, _, _router, _client, connection) =
        fenced_agent_session(&service, &signing_key, node_id, 7).await;
    let short = signed_controller_fence(&signing_key, &term, 2);
    {
        let mut connections = service.shared.inner.connections.lock().await;
        connections
            .get_mut(node_id.as_slice())
            .expect("live session")
            .fence = RegisteredFence {
            verified: service
                .shared
                .keyring()
                .verify_connection_fence_v2(&short, &term.node_id, &term.endpoint_id, unix_now())
                .expect("short session fence"),
            fence: short.clone(),
            endpoint_id: term.endpoint_id,
        };
    }
    for fence in [short, signed_controller_fence(&signing_key, &term, 120)] {
        service
            .register_owner_fence(Request::new(RegisterOwnerFenceRequest {
                fence: Some(fence),
            }))
            .await
            .expect("register or refresh the same owner term");
    }
    assert!(
        tokio::time::timeout(Duration::from_secs(3), connection.closed())
            .await
            .is_err(),
        "the registered renewal outlives the handshake snapshot"
    );
    service.begin_shutdown().await;
}

#[tokio::test]
async fn expired_lease_and_unknown_key_fences_are_rejected() {
    let (signing_key, keyring) = controller_keyring();
    let node_id = *Uuid::now_v7().as_bytes();
    let endpoint_id = [9_u8; 32];
    let service = IrohTransportService::new(8, IdentityPolicy::default(), keyring);
    let expired = signed_controller_fence_at(
        &signing_key,
        &FenceTerm::new(node_id, endpoint_id, 3),
        -120,
        -30,
    );
    let error = service
        .register_owner_fence(Request::new(RegisterOwnerFenceRequest {
            fence: Some(expired),
        }))
        .await
        .expect_err("expired lease rejected");
    assert_eq!(error.code(), tonic::Code::FailedPrecondition);

    let stranger = SigningKey::from_bytes(&[11; 32]);
    let unknown_key =
        signed_controller_fence(&stranger, &FenceTerm::new(node_id, endpoint_id, 4), 30);
    let error = service
        .register_owner_fence(Request::new(RegisterOwnerFenceRequest {
            fence: Some(unknown_key),
        }))
        .await
        .expect_err("unknown signing key rejected");
    assert_eq!(error.code(), tonic::Code::PermissionDenied);

    let error = service
        .get_owner_fence(Request::new(GetOwnerFenceRequest {
            node_id: node_id.to_vec(),
        }))
        .await
        .expect_err("no fence registered");
    assert_eq!(error.code(), tonic::Code::NotFound);
}

#[tokio::test]
async fn administrative_operations_require_the_registered_owner_binding() {
    let (signing_key, keyring) = controller_keyring();
    let node_id = *Uuid::now_v7().as_bytes();
    let service = IrohTransportService::new(8, IdentityPolicy::default(), keyring);
    let (term, fence, _router, _client, _connection) =
        fenced_agent_session(&service, &signing_key, node_id, 12).await;
    service
        .register_owner_fence(Request::new(RegisterOwnerFenceRequest {
            fence: Some(fence.clone()),
        }))
        .await
        .expect("register owner fence");

    // Without a registered fence the operation would pass (N-1 rollout);
    // with one, the binding is mandatory and must match exactly.
    let wrong_kind = signed_fence_binding(
        &signing_key,
        &fence,
        FenceOperationKind::StateUpdate,
        node_id,
        "ocserv.fencing.v2",
    );
    let error = service
        .close_node(Request::new(CloseNodeRequest {
            node_id: node_id.to_vec(),
            reason: "operator close".to_owned(),
            fence_binding: Some(wrong_kind),
        }))
        .await
        .expect_err("wrong binding kind rejected");
    assert_eq!(error.code(), tonic::Code::PermissionDenied);

    let close_binding = signed_fence_binding(
        &signing_key,
        &fence,
        FenceOperationKind::ConnectionClose,
        node_id,
        "ocserv.fencing.v2",
    );
    service
        .close_node(Request::new(CloseNodeRequest {
            node_id: node_id.to_vec(),
            reason: "operator close".to_owned(),
            fence_binding: Some(close_binding),
        }))
        .await
        .expect("owner-bound close accepted");

    let state_operation_id = ocservia_command_authorization::state_update_operation_id(
        &node_id,
        &term.endpoint_id,
        NodeTrustState::Active as u32,
        2,
        "owner bound state update",
    );
    let state_binding = signed_fence_binding(
        &signing_key,
        &fence,
        FenceOperationKind::StateUpdate,
        state_operation_id,
        "ocserv.fencing.v2",
    );
    let response = service
        .update_node_trust(Request::new(UpdateNodeTrustRequest {
            node_id: node_id.to_vec(),
            endpoint_id: term.endpoint_id.to_vec(),
            state: NodeTrustState::Active.into(),
            reason: "owner bound state update".to_owned(),
            revision: 2,
            fence_binding: Some(state_binding),
            operation_id: state_operation_id.to_vec(),
        }))
        .await
        .expect("owner-bound trust update accepted")
        .into_inner();
    assert_eq!(
        response.disposition,
        i32::from(TrustUpdateDisposition::Applied)
    );
}

#[tokio::test]
#[allow(clippy::too_many_lines)]
async fn revoked_update_retry_after_lost_ack_requires_the_same_live_fence() {
    let (signing_key, keyring) = controller_keyring();
    let node_id = *Uuid::now_v7().as_bytes();
    let service = IrohTransportService::new(8, IdentityPolicy::default(), keyring);
    let (term, fence, router, client, connection) =
        fenced_agent_session(&service, &signing_key, node_id, 12).await;
    service
        .register_owner_fence(Request::new(RegisterOwnerFenceRequest {
            fence: Some(fence.clone()),
        }))
        .await
        .expect("register owner fence");
    let operation_id = ocservia_command_authorization::state_update_operation_id(
        &node_id,
        &term.endpoint_id,
        NodeTrustState::Revoked as u32,
        3,
        "revoke",
    );
    let binding = signed_fence_binding(
        &signing_key,
        &fence,
        FenceOperationKind::StateUpdate,
        operation_id,
        "ocserv.fencing.v2",
    );
    let request = UpdateNodeTrustRequest {
        node_id: node_id.to_vec(),
        endpoint_id: term.endpoint_id.to_vec(),
        state: NodeTrustState::Revoked.into(),
        reason: "revoke".into(),
        revision: 3,
        fence_binding: Some(binding),
        operation_id: operation_id.to_vec(),
    };
    // Deliberately discard the first success: policy and connection effects
    // happened, but this acknowledgement is unavailable to the caller.
    drop(
        service
            .update_node_trust(Request::new(request.clone()))
            .await
            .expect("apply revoke"),
    );
    tokio::time::timeout(Duration::from_secs(2), connection.closed())
        .await
        .expect("update closes connection");
    let retry = service
        .update_node_trust(Request::new(request.clone()))
        .await
        .expect("same live fence retry")
        .into_inner();
    assert_eq!(
        retry.disposition,
        i32::from(TrustUpdateDisposition::Applied)
    );
    assert_eq!(retry.retained_revision, 3);
    assert_eq!(retry.retained_state, i32::from(NodeTrustState::Revoked));
    let mut changed = request.clone();
    changed.reason = "another operation".into();
    assert_eq!(
        service
            .update_node_trust(Request::new(changed))
            .await
            .expect_err("carrier mismatch")
            .code(),
        tonic::Code::InvalidArgument
    );
    // An absent connection also exists without any CloseNode invocation.
    // A verified close now returns NOT_FOUND, not a historical receipt.
    let close_binding = signed_fence_binding(
        &signing_key,
        &fence,
        FenceOperationKind::ConnectionClose,
        node_id,
        "ocserv.fencing.v2",
    );
    assert_eq!(
        service
            .close_node(Request::new(CloseNodeRequest {
                node_id: node_id.to_vec(),
                reason: "revoke".into(),
                fence_binding: Some(close_binding),
            }))
            .await
            .expect_err("already absent after update")
            .code(),
        tonic::Code::NotFound
    );
    // Simulate retained fence expiry without waiting for the wall clock.
    service
        .shared
        .inner
        .fences
        .lock()
        .await
        .get_mut(node_id.as_slice())
        .expect("retained fence")
        .verified
        .lease_until_seconds = 0;
    assert_eq!(
        service
            .update_node_trust(Request::new(request))
            .await
            .expect_err("expired fence retry")
            .code(),
        tonic::Code::FailedPrecondition
    );
    shutdown(&service, router).await.expect("shutdown router");
    client.close().await;
}

#[tokio::test]
async fn lost_close_ack_leaves_absence_without_a_historical_receipt() {
    let (signing_key, keyring) = controller_keyring();
    let node_id = *Uuid::now_v7().as_bytes();
    let service = IrohTransportService::new(8, IdentityPolicy::default(), keyring);
    let (_term, fence, router, client, connection) =
        fenced_agent_session(&service, &signing_key, node_id, 12).await;
    service
        .register_owner_fence(Request::new(RegisterOwnerFenceRequest {
            fence: Some(fence.clone()),
        }))
        .await
        .expect("register owner fence");
    let binding = signed_fence_binding(
        &signing_key,
        &fence,
        FenceOperationKind::ConnectionClose,
        node_id,
        "ocserv.fencing.v2",
    );
    let request = CloseNodeRequest {
        node_id: node_id.to_vec(),
        reason: "revoke".into(),
        fence_binding: Some(binding),
    };
    drop(
        service
            .close_node(Request::new(request.clone()))
            .await
            .expect("close executed; discard ack"),
    );
    tokio::time::timeout(Duration::from_secs(2), connection.closed())
        .await
        .expect("connection closed");
    assert_eq!(
        service
            .close_node(Request::new(request))
            .await
            .expect_err("retry has no history")
            .code(),
        tonic::Code::NotFound
    );
    assert_eq!(
        service
            .get_node_connection(Request::new(GetNodeConnectionRequest {
                node_id: node_id.to_vec(),
            }))
            .await
            .expect_err("current absence")
            .code(),
        tonic::Code::NotFound
    );
    shutdown(&service, router).await.expect("shutdown router");
    client.close().await;
}

#[tokio::test]
async fn restored_trust_snapshot_blocks_revoked_endpoint_without_operation_history() {
    let agent_key = SecretKey::generate();
    let endpoint = agent_key.public();
    let handshake = handshake(&agent_key);
    let node = handshake.node_id.clone();
    let controller_key = SecretKey::generate();
    for state in [NodeTrustState::Active, NodeTrustState::Revoked] {
        let restored = IdentityPolicy::default();
        restored
            .replace_snapshot([(endpoint, node.clone(), state, 3)])
            .expect("restore authoritative snapshot");
        assert_eq!(
            restored.permits(endpoint, AGENT_ALPN),
            state == NodeTrustState::Active
        );
        assert_eq!(
            restored.permits(endpoint, ENROLL_ALPN),
            state == NodeTrustState::Active
        );
        assert_eq!(restored.revision(endpoint), 3);
        assert_eq!(restored.retained_state(endpoint), state);
        // Recreate both transport service and router with restored trust;
        // the old service's in-memory operation history is unavailable.
        let service = test_service_with_policy(8, restored.clone());
        let (signing_key, _) = controller_keyring();
        let trust =
            test_trust_with_options(signing_key, vec![(endpoint, node.clone(), state, 3)], None)
                .await;
        let router = test_router(
            controller_key.clone(),
            RelayMode::Disabled,
            restored,
            trust,
            &service,
        )
        .await;
        let client = Endpoint::builder(presets::Minimal)
            .secret_key(agent_key.clone())
            .relay_mode(RelayMode::Disabled)
            .bind()
            .await
            .expect("build agent");
        let connected = tokio::time::timeout(
            Duration::from_secs(3),
            client.connect(router.endpoint().addr(), AGENT_ALPN),
        )
        .await
        .expect("bounded connection attempt");
        if state == NodeTrustState::Active {
            let connection = connected.expect("active snapshot admits agent");
            assert_eq!(
                send_handshake(&connection, &handshake).await.result,
                i32::from(HandshakeResult::Accepted)
            );
            wait_until_registered(&service, &node).await;
        } else {
            if let Ok(connection) = connected {
                tokio::time::timeout(Duration::from_secs(3), connection.closed())
                    .await
                    .expect("revoked transport closes connection");
            }
            assert!(
                !service
                    .shared
                    .inner
                    .connections
                    .lock()
                    .await
                    .contains_key(&node)
            );
        }
        shutdown(&service, router)
            .await
            .expect("shutdown restarted router");
        client.close().await;
    }
}

#[tokio::test]
async fn foreign_fence_terms_cannot_drive_state_updates() {
    let (signing_key, keyring) = controller_keyring();
    let node_id = *Uuid::now_v7().as_bytes();
    let service = IrohTransportService::new(8, IdentityPolicy::default(), keyring);
    let (term, fence, _router, _client, _connection) =
        fenced_agent_session(&service, &signing_key, node_id, 12).await;
    service
        .register_owner_fence(Request::new(RegisterOwnerFenceRequest {
            fence: Some(fence.clone()),
        }))
        .await
        .expect("register owner fence");

    // A binding signed for a different fence term cannot drive state,
    // even when it names the exact operation identity of the request.
    let stranger_operation_id = ocservia_command_authorization::state_update_operation_id(
        &node_id,
        &term.endpoint_id,
        NodeTrustState::Revoked as u32,
        3,
        "stale owner",
    );
    let stranger_term = FenceTerm::new(node_id, term.endpoint_id, 99);
    let stranger_fence = signed_controller_fence(&signing_key, &stranger_term, 30);
    let stranger_binding = signed_fence_binding(
        &signing_key,
        &stranger_fence,
        FenceOperationKind::StateUpdate,
        stranger_operation_id,
        "ocserv.fencing.v2",
    );
    let error = service
        .update_node_trust(Request::new(UpdateNodeTrustRequest {
            node_id: node_id.to_vec(),
            endpoint_id: term.endpoint_id.to_vec(),
            state: NodeTrustState::Revoked.into(),
            reason: "stale owner".to_owned(),
            revision: 3,
            fence_binding: Some(stranger_binding),
            operation_id: stranger_operation_id.to_vec(),
        }))
        .await
        .expect_err("foreign-term binding rejected");
    assert_eq!(error.code(), tonic::Code::PermissionDenied);
}

#[tokio::test]
async fn require_fencing_rejects_fenceless_mutation_flows() {
    let (_signing_key, keyring) = controller_keyring();
    let node_id = *Uuid::now_v7().as_bytes();
    let service = IrohTransportService::new(8, IdentityPolicy::default(), keyring);
    let error = service
        .send_command(Request::new(SendCommandRequest {
            node_id: node_id.to_vec(),
            command_envelope: command_envelope(&node_id, &new_traceparent(), String::new()),
        }))
        .await;
    // No session exists yet, so dispatch reports unavailable before the
    // fencing predicate; the predicate itself is covered by the fenced
    // matrix above. What must hold here is that a fenceless close and a
    // fenceless trust update are rejected outright.
    assert_eq!(
        error.expect_err("no session").code(),
        tonic::Code::Unavailable
    );
    let error = service
        .close_node(Request::new(CloseNodeRequest {
            node_id: node_id.to_vec(),
            reason: "operator close".to_owned(),
            fence_binding: None,
        }))
        .await
        .expect_err("fenceless close rejected under require-fencing");
    assert_eq!(error.code(), tonic::Code::PermissionDenied);
    let trust_endpoint = SecretKey::generate().public();
    let error = service
        .update_node_trust(Request::new(UpdateNodeTrustRequest {
            node_id: node_id.to_vec(),
            endpoint_id: trust_endpoint.as_bytes().to_vec(),
            state: NodeTrustState::Active.into(),
            reason: "fenceless trust update".to_owned(),
            revision: 2,
            fence_binding: None,
            operation_id: Vec::new(),
        }))
        .await
        .expect_err("fenceless trust update rejected under require-fencing");
    assert_eq!(error.code(), tonic::Code::PermissionDenied);
}

#[tokio::test]
async fn registered_fence_rejects_bindingless_operations() {
    let (signing_key, keyring) = controller_keyring();
    let node_id = *Uuid::now_v7().as_bytes();
    let service = IrohTransportService::new(8, IdentityPolicy::default(), keyring);
    let (term, fence, _router, _client, _connection) =
        fenced_agent_session(&service, &signing_key, node_id, 15).await;
    service
        .register_owner_fence(Request::new(RegisterOwnerFenceRequest {
            fence: Some(fence.clone()),
        }))
        .await
        .expect("register owner fence");

    // A registered fence binds the node's operations regardless of the
    // global require-fencing flag: fetch, consume, confirm, close, and
    // state updates without a binding are all rejected.
    let artifact_id = *Uuid::now_v7().as_bytes();
    let grant = shape_valid_grant(node_id, artifact_id);
    let Err(fetch) = service
        .fetch_artifact(Request::new(FetchArtifactRequest {
            node_id: node_id.to_vec(),
            artifact_id: artifact_id.to_vec(),
            purpose: "certificate_p12".to_owned(),
            max_bytes: 32,
            grant: Some(grant.clone()),
            fence_binding: None,
        }))
        .await
    else {
        panic!("bindingless fetch must fail with a registered fence");
    };
    assert_eq!(fetch.code(), tonic::Code::PermissionDenied);
    let error = service
        .consume_artifact(Request::new(ConsumeArtifactRequest {
            node_id: node_id.to_vec(),
            grant: Some(grant.clone()),
            sha256: vec![0x11; 32],
            size: 32,
            confirm_only: false,
            fence_binding: None,
        }))
        .await
        .expect_err("bindingless consume rejected with a registered fence");
    assert_eq!(error.code(), tonic::Code::PermissionDenied);
    let error = service
        .consume_artifact(Request::new(ConsumeArtifactRequest {
            node_id: node_id.to_vec(),
            grant: Some(grant),
            sha256: vec![0x11; 32],
            size: 32,
            confirm_only: true,
            fence_binding: None,
        }))
        .await
        .expect_err("bindingless confirm rejected with a registered fence");
    assert_eq!(error.code(), tonic::Code::PermissionDenied);
    let error = service
        .close_node(Request::new(CloseNodeRequest {
            node_id: node_id.to_vec(),
            reason: "operator close".to_owned(),
            fence_binding: None,
        }))
        .await
        .expect_err("bindingless close rejected with a registered fence");
    assert_eq!(error.code(), tonic::Code::PermissionDenied);
    let operation_id = ocservia_command_authorization::state_update_operation_id(
        &node_id,
        &term.endpoint_id,
        NodeTrustState::Active as u32,
        2,
        "bindingless state update",
    );
    let error = service
        .update_node_trust(Request::new(UpdateNodeTrustRequest {
            node_id: node_id.to_vec(),
            endpoint_id: term.endpoint_id.to_vec(),
            state: NodeTrustState::Active.into(),
            reason: "bindingless state update".to_owned(),
            revision: 2,
            fence_binding: None,
            operation_id: operation_id.to_vec(),
        }))
        .await
        .expect_err("bindingless state update rejected with a registered fence");
    assert_eq!(error.code(), tonic::Code::PermissionDenied);

    service.begin_shutdown().await;
}

#[tokio::test]
async fn state_update_binding_binds_the_exact_update_carrier() {
    let (signing_key, keyring) = controller_keyring();
    let node_id = *Uuid::now_v7().as_bytes();
    let service = IrohTransportService::new(8, IdentityPolicy::default(), keyring);
    let (term, fence, _router, _client, _connection) =
        fenced_agent_session(&service, &signing_key, node_id, 21).await;
    service
        .register_owner_fence(Request::new(RegisterOwnerFenceRequest {
            fence: Some(fence.clone()),
        }))
        .await
        .expect("register owner fence");

    let reason = "carrier binding fixture";
    let operation_id = ocservia_command_authorization::state_update_operation_id(
        &node_id,
        &term.endpoint_id,
        NodeTrustState::Active as u32,
        4,
        reason,
    );
    let binding = signed_fence_binding(
        &signing_key,
        &fence,
        FenceOperationKind::StateUpdate,
        operation_id,
        "ocserv.fencing.v2",
    );
    let tamper = |endpoint: [u8; 32], state: NodeTrustState, revision: u64, reason: &str| {
        UpdateNodeTrustRequest {
            node_id: node_id.to_vec(),
            endpoint_id: endpoint.to_vec(),
            state: state.into(),
            reason: reason.to_owned(),
            revision,
            fence_binding: Some(binding.clone()),
            operation_id: ocservia_command_authorization::state_update_operation_id(
                &node_id,
                &endpoint,
                state as u32,
                revision,
                reason,
            )
            .to_vec(),
        }
    };
    let mut other_endpoint = term.endpoint_id;
    other_endpoint[0] ^= 1;
    for request in [
        tamper(other_endpoint, NodeTrustState::Active, 4, reason),
        tamper(term.endpoint_id, NodeTrustState::Revoked, 4, reason),
        tamper(term.endpoint_id, NodeTrustState::Active, 5, reason),
        tamper(
            term.endpoint_id,
            NodeTrustState::Active,
            4,
            "different reason",
        ),
    ] {
        let error = service
            .update_node_trust(Request::new(request))
            .await
            .expect_err("tampered state update carrier rejected");
        assert!(
            matches!(
                error.code(),
                tonic::Code::PermissionDenied | tonic::Code::InvalidArgument
            ),
            "unexpected rejection code {}",
            error.code()
        );
    }

    service.begin_shutdown().await;
}

#[tokio::test]
async fn lease_deadlines_keep_nanosecond_precision() {
    let (signing_key, keyring) = controller_keyring();
    let node_id = *Uuid::now_v7().as_bytes();
    let endpoint_id = [6_u8; 32];
    let service = IrohTransportService::new(8, IdentityPolicy::default(), keyring);

    // A lease valid for another fraction of a second inside the current
    // second must be accepted; whole-second truncation would reject it.
    let fractional = signed_controller_fence_until(
        &signing_key,
        &FenceTerm::new(node_id, endpoint_id, 9),
        SystemTime::now() + Duration::from_millis(400),
    );
    let response = service
        .register_owner_fence(Request::new(RegisterOwnerFenceRequest {
            fence: Some(fractional),
        }))
        .await
        .expect("sub-second remaining lease accepted")
        .into_inner();
    assert_eq!(
        response.disposition,
        i32::from(OwnerFenceDisposition::Applied)
    );

    // A lease that expired a fraction of a second ago inside the current
    // second must be rejected.
    let expired = signed_controller_fence_until(
        &signing_key,
        &FenceTerm::new(node_id, endpoint_id, 10),
        SystemTime::now() - Duration::from_millis(150),
    );
    let error = service
        .register_owner_fence(Request::new(RegisterOwnerFenceRequest {
            fence: Some(expired),
        }))
        .await
        .expect_err("sub-second expired lease rejected");
    assert_eq!(error.code(), tonic::Code::FailedPrecondition);
}

#[tokio::test]
async fn transport_events_carry_the_owner_term() {
    let (signing_key, keyring) = controller_keyring();
    let node_id = *Uuid::now_v7().as_bytes();
    let service = IrohTransportService::new(8, IdentityPolicy::default(), keyring);
    let (term, fence, _router, _client, _connection) =
        fenced_agent_session(&service, &signing_key, node_id, 33).await;
    service
        .register_owner_fence(Request::new(RegisterOwnerFenceRequest {
            fence: Some(fence.clone()),
        }))
        .await
        .expect("register owner fence");
    let mut events = service
        .watch_events(Request::new(WatchEventsRequest {
            after_event_id: Vec::new(),
        }))
        .await
        .expect("watch events")
        .into_inner();

    let close_binding = signed_fence_binding(
        &signing_key,
        &fence,
        FenceOperationKind::ConnectionClose,
        node_id,
        "ocserv.fencing.v2",
    );
    service
        .close_node(Request::new(CloseNodeRequest {
            node_id: node_id.to_vec(),
            reason: "operator close".to_owned(),
            fence_binding: Some(close_binding),
        }))
        .await
        .expect("owner-bound close accepted");

    let event = next_event(&mut events, TransportEventType::Disconnected).await;
    assert_eq!(event.node_id, node_id.to_vec());
    assert_eq!(event.connection_id, term.connection_id.to_vec());
    assert_eq!(event.owner_epoch, term.owner_epoch);

    service.begin_shutdown().await;
}

type AgentFetch = (
    EventStreamOf<ArtifactChunk>,
    iroh::endpoint::SendStream,
    iroh::endpoint::RecvStream,
    [u8; 16],
    (iroh::protocol::Router, Endpoint, Connection),
);
type EventStreamOf<T> = Pin<Box<dyn Stream<Item = Result<T, Status>> + Send>>;

/// Starts an artifact fetch whose Agent has read the request and then
/// stays silent, so the forwarding worker is blocked on the stream.
async fn stalled_artifact_fetch(service: &IrohTransportService, ttl: Duration) -> AgentFetch {
    let (signing_key, _) = controller_keyring();
    let node_id = *Uuid::now_v7().as_bytes();
    let (_term, fence, router, client, connection) =
        fenced_agent_session(service, &signing_key, node_id, 61).await;
    service
        .register_owner_fence(Request::new(RegisterOwnerFenceRequest {
            fence: Some(fence.clone()),
        }))
        .await
        .expect("register owner fence");
    let artifact_id = *Uuid::now_v7().as_bytes();
    let binding = signed_fence_binding(
        &signing_key,
        &fence,
        FenceOperationKind::Artifact,
        artifact_id,
        "ocserv.fencing.v2",
    );
    let mut grant = shape_valid_grant(node_id, artifact_id);
    grant.expires_at = Some((SystemTime::now() + ttl).into());
    let fetched = service
        .fetch_artifact(Request::new(FetchArtifactRequest {
            node_id: node_id.to_vec(),
            artifact_id: artifact_id.to_vec(),
            purpose: "certificate_p12".to_owned(),
            max_bytes: 32,
            grant: Some(grant),
            fence_binding: Some(binding),
        }))
        .await
        .expect("fenced fetch accepted")
        .into_inner();
    let (fetch_send, mut fetch_recv) =
        tokio::time::timeout(Duration::from_secs(5), connection.accept_bi())
            .await
            .expect("artifact fetch stream opened")
            .expect("accept artifact fetch stream");
    let mut framed = [0_u8; 4];
    fetch_recv
        .read_exact(&mut framed)
        .await
        .expect("read artifact fetch length");
    let mut body = vec![0_u8; (u32::from_be_bytes(framed) & !ARTIFACT_FETCH_FRAME) as usize];
    fetch_recv
        .read_exact(&mut body)
        .await
        .expect("read artifact fetch request");
    (
        fetched,
        fetch_send,
        fetch_recv,
        artifact_id,
        (router, client, connection),
    )
}

async fn assert_artifact_stream_aborted(send: &mut iroh::endpoint::SendStream) {
    let stopped = tokio::time::timeout(Duration::from_secs(5), send.stopped())
        .await
        .expect("worker stopped the artifact stream")
        .expect("stop code is observable");
    assert_eq!(stopped, Some(ARTIFACT_STREAM_ABORT));
}

#[tokio::test]
async fn artifact_cancel_before_first_chunk_stops_the_stalled_stream() {
    let (_, keyring) = controller_keyring();
    let service = IrohTransportService::new(8, IdentityPolicy::default(), keyring);
    let (fetched, mut send, _recv, _artifact, (router, client, connection)) =
        stalled_artifact_fetch(&service, Duration::from_mins(1)).await;
    drop(fetched);
    assert_artifact_stream_aborted(&mut send).await;
    assert!(
        connection.close_reason().is_none(),
        "cancelling one artifact must not close the Agent connection"
    );
    service.begin_shutdown().await;
    drop((router, client));
}

#[tokio::test]
async fn artifact_cancel_after_first_chunk_stops_the_stalled_stream() {
    let (_, keyring) = controller_keyring();
    let service = IrohTransportService::new(8, IdentityPolicy::default(), keyring);
    let (mut fetched, mut send, _recv, artifact_id, (router, client, connection)) =
        stalled_artifact_fetch(&service, Duration::from_mins(1)).await;
    let chunk = ArtifactChunk {
        artifact_id: artifact_id.to_vec(),
        offset: 0,
        data: vec![1],
        eof: false,
        sha256: Vec::new(),
    }
    .encode_to_vec();
    send.write_all(
        &u32::try_from(chunk.len())
            .expect("chunk length")
            .to_be_bytes(),
    )
    .await
    .expect("write chunk length");
    send.write_all(&chunk).await.expect("write chunk");
    fetched
        .next()
        .await
        .expect("first chunk")
        .expect("first chunk is valid");
    drop(fetched);
    assert_artifact_stream_aborted(&mut send).await;
    assert!(connection.close_reason().is_none());
    service.begin_shutdown().await;
    drop((router, client));
}

#[tokio::test]
async fn artifact_grant_expiry_stops_the_stalled_stream() {
    let (_, keyring) = controller_keyring();
    let service = IrohTransportService::new(8, IdentityPolicy::default(), keyring);
    let (mut fetched, mut send, _recv, _artifact, (router, client, connection)) =
        stalled_artifact_fetch(&service, Duration::from_secs(1)).await;
    // The consumer stays connected but sees the bounded failure.
    let error = tokio::time::timeout(Duration::from_secs(5), fetched.next())
        .await
        .expect("grant deadline ends the stream")
        .expect("terminal error is delivered")
        .expect_err("expired grant fails the export");
    assert_eq!(error.code(), tonic::Code::DeadlineExceeded);
    assert_artifact_stream_aborted(&mut send).await;
    assert!(connection.close_reason().is_none());
    service.begin_shutdown().await;
    drop((router, client));
}

#[test]
fn expired_artifact_grant_fails_before_any_stream_is_opened() {
    // fetch_artifact computes this deadline before spawning the worker,
    // so an expired or missing expiry never opens an Agent stream.
    let mut grant = shape_valid_grant(*Uuid::now_v7().as_bytes(), *Uuid::now_v7().as_bytes());
    assert!(artifact_stream_deadline(&grant).is_ok());
    grant.expires_at = Some((SystemTime::now() - Duration::from_secs(1)).into());
    let expired = artifact_stream_deadline(&grant).expect_err("expired grant must fail");
    assert_eq!(expired.code(), tonic::Code::DeadlineExceeded);
    grant.expires_at = None;
    let missing = artifact_stream_deadline(&grant).expect_err("missing expiry must fail");
    assert_eq!(missing.code(), tonic::Code::InvalidArgument);
}

fn shape_valid_grant(node_id: [u8; 16], artifact_id: [u8; 16]) -> ArtifactGrantV1 {
    let now = now_timestamp();
    ArtifactGrantV1 {
        version: ArtifactGrantVersion::V1.into(),
        key_id: "ed25519-sha256:test-relay-only".to_owned(),
        node_id: node_id.to_vec(),
        artifact_id: artifact_id.to_vec(),
        certificate_id: vec![0_u8; 16],
        certificate_version: 1,
        operation_id: vec![0_u8; 16],
        authorized_subject: "review".to_owned(),
        purpose: "certificate_p12".to_owned(),
        max_bytes: 32,
        issued_at: Some(now),
        expires_at: Some(prost_types::Timestamp {
            seconds: now.seconds + 60,
            nanos: now.nanos,
        }),
        grant_id: Uuid::now_v7().as_bytes().to_vec(),
        signature: vec![0_u8; 64],
    }
}

/// Signs a fence whose lease deadline carries sub-second precision, so
/// nanosecond boundary behavior is observable.
fn signed_controller_fence_until(
    signing_key: &SigningKey,
    term: &FenceTerm,
    lease_until: SystemTime,
) -> ConnectionFenceV2 {
    let lease_until = prost_types::Timestamp::from(lease_until);
    // The canonical contract requires issued_at < lease_until, even for a
    // lease that already expired a fraction of a second ago.
    let issued = prost_types::Timestamp::from(SystemTime::now() - Duration::from_mins(1));
    let claims = ConnectionFenceClaimsV2 {
        signature_version: ocservia_command_authorization::FENCE_SIGNATURE_VERSION_ED25519_V1,
        key_id: verification_key_id(&signing_key.verifying_key()),
        fence_id: term.fence_id,
        node_id: term.node_id,
        endpoint_id: term.endpoint_id,
        owner_instance_id: term.owner_instance_id,
        owner_incarnation: term.owner_incarnation,
        owner_epoch: term.owner_epoch,
        connection_id: term.connection_id,
        authorization_revision: term.authorization_revision,
        capabilities: term.capabilities.clone(),
        lease_until_seconds: lease_until.seconds,
        lease_until_nanos: u32::try_from(lease_until.nanos.max(0)).expect("lease nanos"),
        issued_at_seconds: issued.seconds,
        issued_at_nanos: u32::try_from(issued.nanos.max(0)).expect("issued nanos"),
        expires_at_seconds: lease_until.seconds.max(issued.seconds) + 300,
        expires_at_nanos: 0,
    };
    let canonical = canonical_connection_fence_v2(&claims).expect("canonical fence");
    ConnectionFenceV2 {
        signature_version: FenceSignatureVersion::Ed25519V1.into(),
        key_id: claims.key_id,
        fence_id: term.fence_id.to_vec(),
        node_id: term.node_id.to_vec(),
        endpoint_id: term.endpoint_id.to_vec(),
        owner_instance_id: term.owner_instance_id.to_vec(),
        owner_incarnation: term.owner_incarnation,
        owner_epoch: term.owner_epoch,
        connection_id: term.connection_id.to_vec(),
        authorization_revision: term.authorization_revision,
        capabilities: term.capabilities.clone(),
        lease_until: Some(lease_until),
        issued_at: Some(issued),
        expires_at: Some(prost_types::Timestamp {
            seconds: claims.expires_at_seconds,
            nanos: 0,
        }),
        signature: signing_key.sign(&canonical).to_bytes().to_vec(),
    }
}

#[test]
fn fence_registration_decision_matrix() {
    let node = *Uuid::now_v7().as_bytes();
    let endpoint = [1_u8; 32];
    let mut term = FenceTerm::new(node, endpoint, 5);
    let base = RegisteredFence {
        verified: ocservia_command_authorization::VerifiedConnectionFenceV2 {
            fence_id: term.fence_id,
            owner_instance_id: term.owner_instance_id,
            owner_incarnation: term.owner_incarnation,
            owner_epoch: term.owner_epoch,
            connection_id: term.connection_id,
            authorization_revision: term.authorization_revision,
            capabilities: term.capabilities.clone(),
            lease_until_seconds: unix_now() + 30,
            lease_until_nanos: 0,
            expires_at_seconds: unix_now() + 300,
            expires_at_nanos: 0,
        },
        fence: ConnectionFenceV2::default(),
        endpoint_id: endpoint,
    };
    assert_eq!(
        fence_registration_decision(None, &base),
        FenceRegistration::Apply
    );
    assert_eq!(
        fence_registration_decision(Some(&base), &base),
        FenceRegistration::Refresh
    );
    term.supersede();
    let higher = RegisteredFence {
        verified: ocservia_command_authorization::VerifiedConnectionFenceV2 {
            fence_id: term.fence_id,
            owner_instance_id: term.owner_instance_id,
            owner_incarnation: term.owner_incarnation,
            owner_epoch: term.owner_epoch,
            connection_id: term.connection_id,
            authorization_revision: term.authorization_revision,
            capabilities: term.capabilities.clone(),
            lease_until_seconds: unix_now() + 30,
            lease_until_nanos: 0,
            expires_at_seconds: unix_now() + 300,
            expires_at_nanos: 0,
        },
        fence: ConnectionFenceV2::default(),
        endpoint_id: endpoint,
    };
    assert_eq!(
        fence_registration_decision(Some(&base), &higher),
        FenceRegistration::Apply
    );
    assert_eq!(
        fence_registration_decision(Some(&higher), &base),
        FenceRegistration::Stale
    );
    let mut collision = higher.clone();
    collision.verified.fence_id = *Uuid::now_v7().as_bytes();
    assert_eq!(
        fence_registration_decision(Some(&higher), &collision),
        FenceRegistration::EpochCollision
    );
}
