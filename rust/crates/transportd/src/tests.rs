#![allow(
    clippy::many_single_char_names,
    clippy::similar_names,
    clippy::too_many_lines,
    clippy::type_complexity
)]

use std::net::SocketAddr;

use ed25519_dalek::{Signer as _, SigningKey};
use iroh::{RelayMap, Watcher as _, tls::CaTlsConfig};
use ocservia_command_authorization::{
    SessionGrantClaimsV1, canonical_session_grant_v1, state_update_operation_id,
    verification_key_id,
};
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    ArtifactGrantV1, ArtifactGrantVersion, CommandAuthorizationProof, CommandAuthorizationVersion,
    CommandDeliveryMode, GroupObservation, PrivdReceiptVersion, PrivdResultReceiptV1,
    PrivilegedCommandKind, PrivilegedResultKind, SealedSecretPurpose, SealedSecretVersion,
    SealingKeyDescriptorV1, SemanticPayloadHashVersion, ServiceReload, SessionGrantV1,
    SessionGrantVersion, SyntheticNoop, UserObservation,
};
use ocservia_contracts::generated::ocserv::platform::transport::v1::{
    AuthorizeSessionRequest, CheckEndpointRequest, CheckEndpointResponse, ListNodeTrustRequest,
    ListNodeTrustResponse, NodeTrustBinding, ValidateEnrollmentRequest, ValidateEnrollmentResponse,
    trust_service_server::{TrustService, TrustServiceServer},
};
use ocservia_privd_attestation::{key_id, sign_receipt, verify_receipt};
use ocservia_test_support::{FenceSpec, keyring, sign_fence_binding, ts, unix_seconds};
use sha2::{Digest as _, Sha256};
use tokio::{
    net::{TcpListener, TcpStream},
    sync::oneshot,
    task::JoinHandle,
};
use tokio_stream::wrappers::TcpListenerStream;

use super::*;

const RELAYED_AUTHORIZATION_SIGNATURE: &[u8] = &[0xa5; 64];

fn controller_keyring() -> (SigningKey, Arc<ControllerCommandKeyring>) {
    let key = SigningKey::from_bytes(&[7; 32]);
    let kr = Arc::new(keyring(&key));
    (key, kr)
}

fn test_service(event_capacity: usize) -> IrohTransportService {
    test_service_with_policy(event_capacity, IdentityPolicy::default())
}

fn test_service_with_policy(event_capacity: usize, policy: IdentityPolicy) -> IrohTransportService {
    IrohTransportService::new(event_capacity, policy, controller_keyring().1)
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
        let ep = request.into_inner().endpoint_id;
        let permitted = self
            .bindings
            .iter()
            .any(|(b, _, s, _)| b.as_bytes() == ep.as_slice() && *s == NodeTrustState::Active);
        Ok(Response::new(CheckEndpointResponse { permitted }))
    }

    async fn authorize_session(
        &self,
        request: Request<AuthorizeSessionRequest>,
    ) -> Result<Response<SessionHandshakeResponse>, Status> {
        let req = request.into_inner();
        let hs = req
            .handshake
            .ok_or_else(|| Status::invalid_argument("handshake missing"))?;
        let node_id: [u8; 16] = hs
            .node_id
            .as_slice()
            .try_into()
            .map_err(|_| Status::invalid_argument("node_id invalid"))?;
        let ep_id: [u8; 32] = req
            .remote_endpoint_id
            .as_slice()
            .try_into()
            .map_err(|_| Status::invalid_argument("endpoint_id invalid"))?;
        let mut caps = hs.capabilities;
        caps.sort();
        caps.dedup();
        let now = unix_seconds();
        let kid = verification_key_id(&self.signing_key.verifying_key());
        let claims = SessionGrantClaimsV1 {
            version: u32::try_from(i32::from(SessionGrantVersion::V1))
                .map_err(|_| Status::internal("v"))?,
            key_id: kid.clone(),
            protocol_major: PROTOCOL_MAJOR,
            protocol_minor: PROTOCOL_MINOR,
            node_id,
            endpoint_id: ep_id,
            authorization_revision: 1,
            negotiated_capabilities: caps.clone(),
            issued_at_seconds: now - 1,
            issued_at_nanos: 0,
            expires_at_seconds: now + 300,
            expires_at_nanos: 0,
        };
        let canonical =
            canonical_session_grant_v1(&claims).map_err(|_| Status::internal("canonical grant"))?;
        let session_grant = SessionGrantV1 {
            version: SessionGrantVersion::V1.into(),
            key_id: kid,
            protocol_major: PROTOCOL_MAJOR,
            protocol_minor: PROTOCOL_MINOR,
            node_id: node_id.to_vec(),
            endpoint_id: ep_id.to_vec(),
            authorization_revision: 1,
            negotiated_capabilities: caps.clone(),
            issued_at: Some(ts(claims.issued_at_seconds, 0)),
            expires_at: Some(ts(claims.expires_at_seconds, 0)),
            signature: self.signing_key.sign(&canonical).to_bytes().to_vec(),
        };
        let fence = self.override_fence.clone().unwrap_or_else(|| {
            let mut term = FenceTerm::new(
                node_id,
                ep_id,
                self.next_epoch.fetch_add(1, Ordering::Relaxed),
            );
            term.capabilities.clone_from(&caps);
            signed_fence(&self.signing_key, &term, 300)
        });
        Ok(Response::new(SessionHandshakeResponse {
            result: HandshakeResult::Accepted.into(),
            protocol_major: PROTOCOL_MAJOR,
            protocol_minor: PROTOCOL_MINOR,
            max_message_size: hs.max_message_size,
            controller_version: "test".into(),
            negotiated_capabilities: caps,
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
            .map(|(ep, node_id, state, revision)| NodeTrustBinding {
                node_id: node_id.clone(),
                endpoint_id: ep.as_bytes().to_vec(),
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
    let listener = TcpListener::bind("127.0.0.1:0").await.expect("bind trust");
    let addr = listener.local_addr().expect("addr");
    let svc = Arc::new(TestTrustService {
        signing_key,
        bindings,
        override_fence,
        next_epoch: AtomicU64::new(1),
    });
    tokio::spawn(
        tonic::transport::Server::builder()
            .add_service(TrustServiceServer::from_arc(svc))
            .serve_with_incoming(TcpListenerStream::new(listener)),
    );
    let ch = tonic::transport::Endpoint::from_shared(format!("http://{addr}"))
        .expect("endpoint")
        .connect()
        .await
        .expect("connect trust");
    TrustAuthority::new(ch)
}

async fn test_trust_for_key(
    key: &SecretKey,
    hs: &SessionHandshake,
    override_fence: Option<ConnectionFenceV2>,
) -> TrustAuthority {
    test_trust_with_options(
        controller_keyring().0,
        vec![(key.public(), hs.node_id.clone(), NodeTrustState::Active, 1)],
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
    build_router_with_trust_tls_roots(secret_key, relay_mode, policy, trust, Vec::new(), service)
        .await
        .expect("build router")
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
    let mut cfg = ServerConfig::default();
    cfg.relay = Some(relay);
    let server = Server::spawn(cfg).await.expect("start relay");
    let url = format!("https://{}", server.https_addr().expect("TLS addr"));
    let map = RelayMap::try_from_iter([url.as_str()]).expect("relay map");
    (map, certs, server)
}

struct RestartableTcpProxy {
    addr: SocketAddr,
    stop: oneshot::Sender<()>,
    task: JoinHandle<()>,
}

impl RestartableTcpProxy {
    async fn start(addr: SocketAddr, target: SocketAddr) -> Self {
        let listener = TcpListener::bind(addr).await.expect("bind proxy");
        let addr = listener.local_addr().expect("proxy addr");
        let (stop, mut stop_rx) = oneshot::channel();
        let task = tokio::spawn(async move {
            let mut children = tokio::task::JoinSet::new();
            loop {
                tokio::select! {
                    _ = &mut stop_rx => break,
                    accepted = listener.accept() => {
                        let Ok((mut client, _)) = accepted else { break };
                        children.spawn(async move {
                            if let Ok(mut up) = TcpStream::connect(target).await {
                                let _ = tokio::io::copy_bidirectional(&mut client, &mut up).await;
                            }
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
        let _ = self.stop.send(());
        self.task.await.expect("join proxy");
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
        agent_version: "test".into(),
        controller_version: String::new(),
        node_id: Uuid::now_v7().as_bytes().to_vec(),
        endpoint_id: key.public().as_bytes().to_vec(),
        capabilities: vec!["ocserv.fencing.v2".into(), "ocserv.status.read".into()],
        ocserv_version: "1.3".into(),
        os_release: "test".into(),
        boot_id: "boot".into(),
        agent_instance_id: Uuid::now_v7().as_bytes().to_vec(),
        supported_compressions: Vec::new(),
        max_message_size: MAX_FRAME_BYTES_U32,
        time: Some(now_timestamp()),
        nonce: vec![7; 32],
        sealing_keys: sealing_keys(),
    }
}

fn identity_policy(key: &SecretKey, hs: &SessionHandshake) -> IdentityPolicy {
    IdentityPolicy::new(
        HashMap::from([(key.public(), hs.node_id.clone())]),
        HashSet::new(),
    )
}

fn command_envelope(node_id: &[u8], traceparent: &str, reason: String) -> Vec<u8> {
    CommandEnvelope {
        protocol_version: "1.1".into(),
        message_id: Uuid::now_v7().as_bytes().to_vec(),
        command_id: Uuid::now_v7().as_bytes().to_vec(),
        idempotency_key: Uuid::now_v7().as_bytes().to_vec(),
        node_id: node_id.to_vec(),
        sequence: 1,
        issued_at: Some(now_timestamp()),
        expires_at: Some((SystemTime::now() + Duration::from_secs(30)).into()),
        expected_revision: 1,
        traceparent: traceparent.into(),
        actor_id: "test".into(),
        required_capability: "ocserv.status.read".into(),
        reason,
        authorization: Some(CommandAuthorizationProof {
            version: CommandAuthorizationVersion::V1.into(),
            key_id: "ed25519-sha256:test-transport-relay-only".into(),
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

async fn write_frame_bytes(send: &mut iroh::endpoint::SendStream, bytes: &[u8]) {
    let len = u32::try_from(bytes.len()).expect("u32");
    send.write_all(&len.to_be_bytes()).await.expect("write len");
    send.write_all(bytes).await.expect("write body");
}

async fn read_frame_bytes(recv: &mut iroh::endpoint::RecvStream) -> Vec<u8> {
    let mut len = [0_u8; 4];
    recv.read_exact(&mut len).await.expect("read len");
    let mut body = vec![0_u8; u32::from_be_bytes(len) as usize];
    recv.read_exact(&mut body).await.expect("read body");
    body
}

async fn send_handshake(conn: &Connection, hs: &SessionHandshake) -> SessionHandshakeResponse {
    let (mut send, mut recv) = conn.open_bi().await.expect("open hs");
    write_frame_bytes(&mut send, &hs.encode_to_vec()).await;
    send.finish().expect("finish hs");
    SessionHandshakeResponse::decode(read_frame_bytes(&mut recv).await.as_slice())
        .expect("decode hs resp")
}

fn privileged_command_result(cmd: &CommandEnvelope, include_receipt: bool) -> CommandResult {
    let now = now_timestamp();
    let result_bytes = b"completed".to_vec();
    let key = SigningKey::from_bytes(&[7; 32]);
    let proof = include_receipt.then(|| {
        sign_receipt(
            PrivdResultReceiptV1 {
                receipt_version: PrivdReceiptVersion::V1.into(),
                node_id: cmd.node_id.clone(),
                privd_attestation_key_id: key_id(&key.verifying_key()),
                command_id: cmd.command_id.clone(),
                operation_id: cmd.operation_id.clone(),
                idempotency_key: cmd.idempotency_key.clone(),
                semantic_payload_hash_version: cmd.semantic_payload_hash_version,
                semantic_payload_sha256: cmd.semantic_payload_sha256.clone(),
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
            &key,
        )
        .expect("sign receipt")
    });
    CommandResult {
        command_id: cmd.command_id.clone(),
        idempotency_key: cmd.idempotency_key.clone(),
        payload_sha256: cmd.semantic_payload_sha256.clone(),
        state: CommandResultState::Succeeded.into(),
        result: result_bytes,
        error_code: String::new(),
        accepted_at: Some(now),
        completed_at: Some(now),
        replayed: false,
        semantic_payload_hash_version: cmd.semantic_payload_hash_version,
        privileged_result_proof: proof,
    }
}

enum CommandReply {
    Result { receipt: bool, foreign_id: bool },
    Eof,
    HeartbeatFirst,
}

async fn respond_command(conn: Connection, mode: CommandReply) {
    let (mut send, mut recv) = conn.accept_bi().await.expect("accept cmd");
    let decoded =
        CommandEnvelope::decode(read_frame_bytes(&mut recv).await.as_slice()).expect("decode cmd");
    assert_eq!(
        decoded.authorization.as_ref().expect("auth").signature,
        RELAYED_AUTHORIZATION_SIGNATURE
    );
    match mode {
        CommandReply::Result {
            receipt,
            foreign_id,
        } => {
            let mut res = privileged_command_result(&decoded, receipt);
            if foreign_id {
                res.command_id = Uuid::now_v7().as_bytes().to_vec();
            }
            let frame = AgentEvent {
                r#type: AgentEventType::CommandResult.into(),
                payload: res.encode_to_vec(),
            }
            .encode_to_vec();
            write_frame_bytes(&mut send, &frame).await;
            send.finish().expect("finish");
        }
        CommandReply::Eof => {
            send.finish().expect("finish eof");
        }
        CommandReply::HeartbeatFirst => {
            for ev in [
                AgentEvent {
                    r#type: AgentEventType::Heartbeat.into(),
                    payload: Vec::new(),
                },
                AgentEvent {
                    r#type: AgentEventType::CommandResult.into(),
                    payload: privileged_command_result(&decoded, true).encode_to_vec(),
                },
            ] {
                let f = ev.encode_to_vec();
                let len = u32::try_from(f.len()).expect("u32").to_be_bytes();
                if send.write_all(&len).await.is_err() || send.write_all(&f).await.is_err() {
                    return;
                }
            }
            let _ = send.finish();
        }
    }
}

async fn next_event(events: &mut EventStream, event_type: TransportEventType) -> TransportEvent {
    tokio::time::timeout(Duration::from_secs(2), async {
        loop {
            let ev = events.next().await.expect("open").expect("valid");
            if ev.r#type == i32::from(event_type) {
                break ev;
            }
        }
    })
    .await
    .expect("event arrives")
}

async fn wait_until_registered(service: &IrohTransportService, node_id: &[u8]) {
    tokio::time::timeout(Duration::from_secs(2), async {
        while !service
            .shared
            .inner
            .connections
            .lock()
            .await
            .contains_key(node_id)
        {
            tokio::task::yield_now().await;
        }
    })
    .await
    .expect("registered");
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
        .expect("query")
        .into_inner()
        .last_seen
}

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
            capabilities: vec!["ocserv.fencing.v2".into(), "ocserv.service.reload".into()],
        }
    }

    fn supersede(&mut self) {
        self.owner_epoch += 1;
        self.fence_id = *Uuid::now_v7().as_bytes();
        self.connection_id = *Uuid::now_v7().as_bytes();
    }

    fn spec(&self) -> FenceSpec {
        FenceSpec::new(self.node_id, self.endpoint_id, self.owner_epoch)
            .authority(self.authorization_revision)
            .capabilities(self.capabilities.clone())
            .ids(
                self.fence_id,
                self.owner_instance_id,
                self.owner_incarnation,
                self.connection_id,
            )
    }
}

fn signed_fence(key: &SigningKey, term: &FenceTerm, lease_seconds: i64) -> ConnectionFenceV2 {
    signed_fence_at(key, term, 0, lease_seconds)
}

fn signed_fence_at(
    key: &SigningKey,
    term: &FenceTerm,
    issued_offset: i64,
    lease_offset: i64,
) -> ConnectionFenceV2 {
    let issued = unix_seconds() + issued_offset;
    let lease = unix_seconds() + lease_offset;
    term.spec()
        .issued(issued, 0)
        .lease(lease, 0)
        .expires(lease.max(issued) + 300, 0)
        .sign(key)
}

fn signed_fence_until(
    key: &SigningKey,
    term: &FenceTerm,
    lease_until: SystemTime,
) -> ConnectionFenceV2 {
    let lease = prost_types::Timestamp::from(lease_until);
    let issued = prost_types::Timestamp::from(SystemTime::now() - Duration::from_mins(1));
    term.spec()
        .issued(
            issued.seconds,
            u32::try_from(issued.nanos.max(0)).expect("n"),
        )
        .lease(lease.seconds, u32::try_from(lease.nanos.max(0)).expect("n"))
        .expires(lease.seconds.max(issued.seconds) + 300, 0)
        .sign(key)
}

fn bind_op(
    key: &SigningKey,
    fence: &ConnectionFenceV2,
    kind: FenceOperationKind,
    op_id: [u8; 16],
    cap: &str,
) -> FenceBindingV2 {
    let now = unix_seconds();
    sign_fence_binding(key, fence, kind, op_id, cap, (now, 0), (now + 300, 0))
}

fn state_update_req(
    key: &SigningKey,
    fence: &ConnectionFenceV2,
    node_id: [u8; 16],
    endpoint_id: [u8; 32],
    state: NodeTrustState,
    revision: u64,
    reason: &str,
) -> UpdateNodeTrustRequest {
    let op_id = state_update_operation_id(&node_id, &endpoint_id, state as u32, revision, reason);
    UpdateNodeTrustRequest {
        node_id: node_id.to_vec(),
        endpoint_id: endpoint_id.to_vec(),
        state: state.into(),
        reason: reason.into(),
        revision,
        fence_binding: Some(bind_op(
            key,
            fence,
            FenceOperationKind::StateUpdate,
            op_id,
            "ocserv.fencing.v2",
        )),
        operation_id: op_id.to_vec(),
    }
}

fn close_node_req(
    key: &SigningKey,
    fence: &ConnectionFenceV2,
    node_id: [u8; 16],
    reason: &str,
) -> CloseNodeRequest {
    CloseNodeRequest {
        node_id: node_id.to_vec(),
        reason: reason.into(),
        fence_binding: Some(bind_op(
            key,
            fence,
            FenceOperationKind::ConnectionClose,
            node_id,
            "ocserv.fencing.v2",
        )),
    }
}

fn shape_valid_grant(node_id: [u8; 16], artifact_id: [u8; 16]) -> ArtifactGrantV1 {
    let now = now_timestamp();
    ArtifactGrantV1 {
        version: ArtifactGrantVersion::V1.into(),
        key_id: "ed25519-sha256:test-relay-only".into(),
        node_id: node_id.to_vec(),
        artifact_id: artifact_id.to_vec(),
        certificate_id: vec![0; 16],
        certificate_version: 1,
        operation_id: vec![0; 16],
        authorized_subject: "review".into(),
        purpose: "certificate_p12".into(),
        max_bytes: 32,
        issued_at: Some(now),
        expires_at: Some(ts(now.seconds + 60, now.nanos)),
        grant_id: Uuid::now_v7().as_bytes().to_vec(),
        signature: vec![0; 64],
    }
}

async fn register_fence(
    service: &IrohTransportService,
    fence: &ConnectionFenceV2,
) -> RegisterOwnerFenceResponse {
    service
        .register_owner_fence(Request::new(RegisterOwnerFenceRequest {
            fence: Some(fence.clone()),
        }))
        .await
        .expect("register fence")
        .into_inner()
}

async fn fenced_agent_session(
    service: &IrohTransportService,
    signing_key: &SigningKey,
    node_id: [u8; 16],
    owner_epoch: u64,
) -> (FenceTerm, ConnectionFenceV2, Router, Endpoint, Connection) {
    let agent_key = SecretKey::generate();
    let mut hs = handshake(&agent_key);
    hs.node_id = node_id.to_vec();
    hs.capabilities = vec![
        "ocserv.fencing.v2".into(),
        "ocserv.service.reload".into(),
        "ocserv.status.read".into(),
    ];
    let mut term = FenceTerm::new(node_id, *agent_key.public().as_bytes(), owner_epoch);
    term.capabilities.clone_from(&hs.capabilities);
    let fence = signed_fence(signing_key, &term, 120);
    let trust = test_trust_with_options(
        signing_key.clone(),
        vec![(
            agent_key.public(),
            hs.node_id.clone(),
            NodeTrustState::Active,
            1,
        )],
        Some(fence.clone()),
    )
    .await;
    let router = test_router(
        SecretKey::generate(),
        RelayMode::Disabled,
        identity_policy(&agent_key, &hs),
        trust,
        service,
    )
    .await;
    let client = Endpoint::builder(presets::Minimal)
        .secret_key(agent_key)
        .relay_mode(RelayMode::Disabled)
        .bind()
        .await
        .expect("client");
    let conn = client
        .connect(router.endpoint().addr(), AGENT_ALPN)
        .await
        .expect("connect");
    assert_eq!(
        send_handshake(&conn, &hs).await.result,
        i32::from(HandshakeResult::Accepted)
    );
    wait_until_registered(service, &hs.node_id).await;
    (term, fence, router, client, conn)
}

fn fenced_command_envelope(
    node_id: &[u8],
    traceparent: &str,
    fence: &ConnectionFenceV2,
    binding: &FenceBindingV2,
) -> Vec<u8> {
    let mut cmd =
        CommandEnvelope::decode(command_envelope(node_id, traceparent, String::new()).as_slice())
            .expect("decode");
    cmd.connection_fence = Some(fence.clone());
    cmd.fence_binding = Some(binding.clone());
    cmd.encode_to_vec()
}

fn binding_op_id(bytes: &[u8]) -> [u8; 16] {
    CommandEnvelope::decode(bytes)
        .expect("decode")
        .command_id
        .try_into()
        .expect("cmd id")
}

fn telemetry_event(node_id: &[u8], bytes: usize) -> TransportEvent {
    event(
        node_id,
        &[9; 32],
        TransportEventType::Telemetry,
        vec![0; bytes],
    )
}

// ---------------------------------------------------------------------------
// Pure unit invariants
// ---------------------------------------------------------------------------

#[test]
fn policy_enrollment_and_rate_unit_invariants() {
    // Identity policy transitions & tombstone rejection.
    let approved = SecretKey::generate().public();
    let revoked = SecretKey::generate().public();
    let policy = IdentityPolicy::new(
        HashMap::from([(approved, Uuid::now_v7().as_bytes().to_vec())]),
        HashSet::from([revoked]),
    );
    assert!(policy.permits(approved, AGENT_ALPN));
    assert!(!policy.permits(revoked, ENROLL_ALPN));
    assert!(!policy.permits(SecretKey::generate().public(), AGENT_ALPN));
    let dyn_ep = SecretKey::generate().public();
    let dyn_node = Uuid::now_v7().as_bytes().to_vec();
    assert_eq!(
        policy.update(dyn_ep, dyn_node.clone(), NodeTrustState::Active, 1),
        PolicyUpdateDisposition::Applied
    );
    assert!(policy.permits(dyn_ep, AGENT_ALPN));
    assert_eq!(
        policy.update(dyn_ep, dyn_node.clone(), NodeTrustState::Revoked, 2),
        PolicyUpdateDisposition::Applied
    );
    assert_eq!(
        policy.update(dyn_ep, dyn_node.clone(), NodeTrustState::Active, 1),
        PolicyUpdateDisposition::Stale
    );
    assert_eq!(
        policy.update(dyn_ep, dyn_node, NodeTrustState::Active, 3),
        PolicyUpdateDisposition::Rejected
    );
    assert!(!policy.permits(dyn_ep, AGENT_ALPN) && policy.revoked(dyn_ep));
    let restored_rev = SecretKey::generate().public();
    let restored = IdentityPolicy::new(HashMap::new(), HashSet::from([restored_rev]));
    assert_eq!(
        restored.update(
            restored_rev,
            Uuid::now_v7().as_bytes().to_vec(),
            NodeTrustState::Active,
            u64::MAX
        ),
        PolicyUpdateDisposition::Rejected
    );

    // Enrollment request binds authenticated endpoint, token format, and Unicode capability length.
    let key = SecretKey::generate();
    let body = "a".repeat(43);
    let mut req = EnrollRequest {
        token: format!("obt2_{body}"),
        endpoint_id: key.public().as_bytes().to_vec(),
        agent_version: "test".into(),
        os_release: "test".into(),
        ocserv_version: "test".into(),
        boot_id: "boot".into(),
        agent_instance_id: Uuid::now_v7().as_bytes().to_vec(),
        capabilities: vec!["ocserv.fencing.v2".into(), "ocserv.status.read".into()],
        environment: "test".into(),
        nonce: vec![0; 16],
        time: Some(now_timestamp()),
        enrollment_protocol_major: 0,
        enrollment_protocol_minor: 0,
        proof: None,
        sealing_keys: sealing_keys(),
    };
    ocservia_agent_protocol::identity::authorize_enrollment(&mut req, &key).expect("sign");
    assert!(validate_enrollment(&req, key.public()).is_ok());
    for (tok, ok) in [
        (body.clone(), false),
        (format!("obt1_{body}"), false),
        (format!("obt2_{body}"), true),
        (format!("oit1_{body}"), false),
        (format!("obt2_{body}a"), false),
    ] {
        let mut r = req.clone();
        r.token = tok;
        if ok || r.token.starts_with("obt1_") || r.token == body {
            ocservia_agent_protocol::identity::authorize_enrollment(&mut r, &key).expect("sign");
        }
        assert_eq!(validate_enrollment(&r, key.public()).is_ok(), ok);
    }
    assert!(validate_enrollment(&req, SecretKey::generate().public()).is_err());
    req.capabilities = vec!["界".repeat(128)];
    assert!(validate_enrollment(&req, key.public()).is_ok());
    req.capabilities = vec!["界".repeat(129)];
    assert!(validate_enrollment(&req, key.public()).is_err());

    // Global attempt rate is rolling and non-evictable.
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

#[test]
fn handshake_command_receipt_queue_and_artifact_deadline_invariants() {
    // Privileged terminal result receipt rules.
    let node_id = Uuid::now_v7();
    let cmd = CommandEnvelope::decode(
        command_envelope(node_id.as_bytes(), &new_traceparent(), String::new()).as_slice(),
    )
    .expect("dec");
    assert!(valid_privileged_result_proof(
        &cmd,
        &privileged_command_result(&cmd, true)
    ));
    assert!(!valid_privileged_result_proof(
        &cmd,
        &privileged_command_result(&cmd, false)
    ));
    let mut bad_ver = privileged_command_result(&cmd, true);
    bad_ver.privileged_result_proof.as_mut().expect("p").version =
        PrivdReceiptVersion::Unspecified.into();
    assert!(!valid_privileged_result_proof(&cmd, &bad_ver));
    let mut foreign = privileged_command_result(&cmd, true);
    foreign
        .privileged_result_proof
        .as_mut()
        .and_then(|p| p.receipt_v1.as_mut())
        .expect("r")
        .command_id = Uuid::now_v7().as_bytes().to_vec();
    assert!(!valid_privileged_result_proof(&cmd, &foreign));
    let mut rej_unsigned = privileged_command_result(&cmd, false);
    rej_unsigned.state = CommandResultState::Rejected.into();
    assert!(!valid_privileged_result_proof(&cmd, &rej_unsigned));
    let mut rej_signed = privileged_command_result(&cmd, true);
    rej_signed.state = CommandResultState::Rejected.into();
    assert!(valid_privileged_result_proof(&cmd, &rej_signed));
    let synthetic = CommandEnvelope {
        payload: Some(command_envelope::Payload::SyntheticNoop(
            SyntheticNoop::default(),
        )),
        ..cmd
    };
    assert!(valid_privileged_result_proof(&synthetic, &rej_unsigned));

    // Command response deadline honors expiry & ReconcileOnly.
    let valid_dl = command_response_deadline(&CommandEnvelope {
        expires_at: Some((SystemTime::now() + Duration::from_secs(30)).into()),
        ..CommandEnvelope::default()
    })
    .expect("valid");
    assert!(valid_dl > tokio::time::Instant::now() + STREAM_TIMEOUT);
    let expired = CommandEnvelope {
        expires_at: Some((SystemTime::now() - Duration::from_secs(1)).into()),
        ..CommandEnvelope::default()
    };
    assert_eq!(
        command_response_deadline(&expired).expect_err("exp").code(),
        tonic::Code::DeadlineExceeded
    );
    let before = tokio::time::Instant::now();
    let rec_dl = command_response_deadline(&CommandEnvelope {
        delivery_mode: CommandDeliveryMode::ReconcileOnly.into(),
        ..expired
    })
    .expect("reconcile");
    assert!(
        rec_dl >= before + STREAM_TIMEOUT && rec_dl <= tokio::time::Instant::now() + STREAM_TIMEOUT
    );

    // Handshake limits & grantless protocol v1.0/v1.1 rejection.
    let key = SecretKey::generate();
    let mut hs = handshake(&key);
    assert!(
        validate_handshake(&hs, key.public()).is_ok() && validate_protocol_version(&hs).is_ok()
    );
    hs.protocol_minor = 0;
    assert!(validate_protocol_version(&hs).is_err());
    hs.protocol_minor = PROTOCOL_MINOR;
    hs.protocol_major = 2;
    assert!(
        validate_handshake(&hs, key.public()).is_ok() && validate_protocol_version(&hs).is_err()
    );
    hs.protocol_major = PROTOCOL_MAJOR;
    assert!(validate_handshake(&hs, SecretKey::generate().public()).is_err());
    for minor in [0, 1] {
        let resp = SessionHandshakeResponse {
            result: HandshakeResult::Accepted.into(),
            protocol_major: 1,
            protocol_minor: minor,
            max_message_size: hs.max_message_size,
            controller_version: "test".into(),
            negotiated_capabilities: vec!["ocserv.status.read".into()],
            session_grant: None,
            connection_fence: None,
        };
        assert!(validate_negotiated_session(&hs, key.public(), &resp).is_err());
    }

    // Queue bounds, permit isolation, traceparent, and artifact grant deadlines.
    let bounded = test_service(usize::MAX);
    assert_eq!(bounded.shared.inner.event_capacity, MAX_CONNECTIONS);
    let enroll_permits = bounded
        .shared
        .inner
        .enrollment_connection_permits
        .clone()
        .try_acquire_many_owned(u32::try_from(MAX_ENROLLMENT_CONNECTIONS).expect("u32"))
        .expect("reserve");
    assert_eq!(
        bounded
            .shared
            .inner
            .enrollment_connection_permits
            .available_permits(),
        0
    );
    assert_eq!(
        bounded
            .shared
            .inner
            .agent_connection_permits
            .available_permits(),
        MAX_AGENT_CONNECTIONS
    );
    drop(enroll_permits);

    let tp = event(
        Uuid::now_v7().as_bytes(),
        key.public().as_bytes(),
        TransportEventType::Connected,
        Vec::new(),
    )
    .traceparent;
    let parts: Vec<_> = tp.split('-').collect();
    assert_eq!(
        (
            parts.len(),
            parts[0],
            parts[1].len(),
            parts[2].len(),
            parts[3]
        ),
        (4, "00", 32, 16, "01")
    );

    let mut grant = shape_valid_grant(*Uuid::now_v7().as_bytes(), *Uuid::now_v7().as_bytes());
    assert!(artifact_stream_deadline(&grant).is_ok());
    grant.expires_at = Some((SystemTime::now() - Duration::from_secs(1)).into());
    assert_eq!(
        artifact_stream_deadline(&grant).expect_err("exp").code(),
        tonic::Code::DeadlineExceeded
    );
    grant.expires_at = None;
    assert_eq!(
        artifact_stream_deadline(&grant).expect_err("none").code(),
        tonic::Code::InvalidArgument
    );
}

#[test]
fn enrollment_identity_churn_refuses_new_identity_without_evicting_oldest() {
    let now = Instant::now();
    let mut attempts = HashMap::new();
    let oldest = SecretKey::generate().public();
    record_attempt(&mut attempts, oldest, now, MAX_ENROLLMENT_CONNECTIONS).expect("oldest");
    for _ in 1..MAX_ENROLLMENT_CONNECTIONS {
        record_attempt(
            &mut attempts,
            SecretKey::generate().public(),
            now + Duration::from_millis(1),
            MAX_ENROLLMENT_CONNECTIONS,
        )
        .expect("fill");
    }
    let newcomer = SecretKey::generate().public();
    assert_eq!(
        record_attempt(
            &mut attempts,
            newcomer,
            now + Duration::from_millis(2),
            MAX_ENROLLMENT_CONNECTIONS
        ),
        Err(AttemptRejection::Capacity)
    );
    assert_eq!(attempts.len(), MAX_ENROLLMENT_CONNECTIONS);
    assert!(attempts.contains_key(&oldest) && !attempts.contains_key(&newcomer));
    assert_eq!(
        record_attempt(
            &mut HashMap::new(),
            SecretKey::generate().public(),
            now,
            MAX_AGENT_CONNECTIONS
        ),
        Ok(())
    );
}

#[test]
fn fence_registration_decision_matrix() {
    let node = *Uuid::now_v7().as_bytes();
    let ep = [1_u8; 32];
    let mk = |t: &FenceTerm| RegisteredFence {
        verified: ocservia_command_authorization::VerifiedConnectionFenceV2 {
            fence_id: t.fence_id,
            owner_instance_id: t.owner_instance_id,
            owner_incarnation: t.owner_incarnation,
            owner_epoch: t.owner_epoch,
            connection_id: t.connection_id,
            authorization_revision: t.authorization_revision,
            capabilities: t.capabilities.clone(),
            lease_until_seconds: unix_now() + 30,
            lease_until_nanos: 0,
            expires_at_seconds: unix_now() + 300,
            expires_at_nanos: 0,
        },
        fence: ConnectionFenceV2::default(),
        endpoint_id: ep,
    };
    let mut term = FenceTerm::new(node, ep, 5);
    let base = mk(&term);
    assert_eq!(
        fence_registration_decision(None, &base),
        FenceRegistration::Apply
    );
    assert_eq!(
        fence_registration_decision(Some(&base), &base),
        FenceRegistration::Refresh
    );
    term.supersede();
    let higher = mk(&term);
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

// ---------------------------------------------------------------------------
// Pinned security-acceptance & budget tests
// ---------------------------------------------------------------------------

#[tokio::test]
async fn trust_update_reports_stale_and_rejects_tombstone_reactivation() {
    let (key, kr) = controller_keyring();
    let service = IrohTransportService::new(8, IdentityPolicy::default(), kr);
    let ep = SecretKey::generate().public();
    let node_id = *Uuid::now_v7().as_bytes();
    let fence = signed_fence(&key, &FenceTerm::new(node_id, *ep.as_bytes(), 1), 300);
    register_fence(&service, &fence).await;

    let apply = |state: NodeTrustState, rev: u64| {
        let req = state_update_req(
            &key,
            &fence,
            node_id,
            *ep.as_bytes(),
            state,
            rev,
            "trust transition fixture",
        );
        let svc = service.clone();
        async move {
            svc.update_node_trust(Request::new(req))
                .await
                .expect("update")
                .into_inner()
        }
    };
    assert_eq!(
        apply(NodeTrustState::Active, 2).await.disposition,
        i32::from(TrustUpdateDisposition::Applied)
    );
    let stale = apply(NodeTrustState::Active, 1).await;
    assert_eq!(
        (
            stale.disposition,
            stale.retained_revision,
            stale.retained_state
        ),
        (
            i32::from(TrustUpdateDisposition::Stale),
            2,
            i32::from(NodeTrustState::Active)
        )
    );
    assert_eq!(
        apply(NodeTrustState::Revoked, 3).await.disposition,
        i32::from(TrustUpdateDisposition::Applied)
    );
    let react = apply(NodeTrustState::Active, 4).await;
    assert_eq!(
        (
            react.disposition,
            react.retained_revision,
            react.retained_state
        ),
        (
            i32::from(TrustUpdateDisposition::Rejected),
            3,
            i32::from(NodeTrustState::Revoked)
        )
    );
}

#[tokio::test]
async fn trust_budgets_shared_limits_and_lifecycle_invariants() {
    // Clones share budgets & metrics labels omit endpoint IDs.
    let trust = TrustAuthority::new(
        tonic::transport::Endpoint::from_static("http://[::1]:50051").connect_lazy(),
    );
    assert!(Arc::ptr_eq(&trust.budgets, &trust.clone().budgets));
    for c in [
        TrustClass::KnownAgentHandshake,
        TrustClass::AgentAuthorization,
        TrustClass::AgentRegistrationRecheck,
        TrustClass::UnknownEnrollmentPreAuth,
        TrustClass::EnrollmentCompletion,
    ] {
        assert_eq!(trust.metrics(c), TrustMetricsSnapshot::default());
        assert!(!c.label().contains("endpoint"));
    }

    // Unknown enrollment flood cannot starve approved classes and recovers after 1m.
    let mut cfg = TrustBudgetConfig::default()
        .with_limit(
            TrustClass::UnknownEnrollmentPreAuth,
            TrustClassLimit {
                attempts_per_minute: 20,
                concurrency: 20,
            },
        )
        .expect("l");
    for class in [
        TrustClass::KnownAgentHandshake,
        TrustClass::AgentAuthorization,
        TrustClass::AgentRegistrationRecheck,
        TrustClass::EnrollmentCompletion,
    ] {
        cfg = cfg
            .with_limit(
                class,
                TrustClassLimit {
                    attempts_per_minute: 3,
                    concurrency: 3,
                },
            )
            .expect("l");
    }
    let budgets = Arc::new(TrustBudgetSet::new(&cfg));
    let now = Instant::now();
    let mut held = Vec::new();
    for _ in 0..20 {
        held.push(
            budgets
                .acquire_at(TrustClass::UnknownEnrollmentPreAuth, now)
                .await
                .expect("p"),
        );
    }
    assert_eq!(
        budgets
            .acquire_at(TrustClass::UnknownEnrollmentPreAuth, now)
            .await
            .err(),
        Some(AttemptRejection::Capacity)
    );
    for c in [
        TrustClass::KnownAgentHandshake,
        TrustClass::AgentAuthorization,
        TrustClass::AgentRegistrationRecheck,
        TrustClass::EnrollmentCompletion,
    ] {
        drop(budgets.acquire_at(c, now).await.expect("approved"));
        assert_eq!(budgets.snapshot(c).accepted, 1);
    }
    drop(held);
    assert_eq!(
        budgets
            .acquire_at(TrustClass::UnknownEnrollmentPreAuth, now)
            .await
            .err(),
        Some(AttemptRejection::Rate)
    );
    drop(
        budgets
            .acquire_at(
                TrustClass::UnknownEnrollmentPreAuth,
                now + Duration::from_mins(1),
            )
            .await
            .expect("recovered"),
    );

    // Close invalidates only target registration token; shutdown ends event streams.
    let service = test_service(8);
    let target = Uuid::now_v7().as_bytes().to_vec();
    let other = Uuid::now_v7().as_bytes().to_vec();
    let tok_t = service.shared.registration_token(&target).await;
    let tok_o = service.shared.registration_token(&other).await;
    assert!(service.shared.remove(&target, b"revoked").await.is_none());
    assert!(!Arc::ptr_eq(
        &service.shared.registration_token(&target).await,
        &tok_t
    ));
    assert!(Arc::ptr_eq(
        &service.shared.registration_token(&other).await,
        &tok_o
    ));

    let mut stream = service
        .watch_events(Request::new(WatchEventsRequest {
            after_event_id: Vec::new(),
        }))
        .await
        .expect("watch")
        .into_inner();
    service.begin_shutdown().await;
    assert!(
        tokio::time::timeout(Duration::from_secs(1), stream.next())
            .await
            .expect("ends")
            .is_none()
    );
}

#[tokio::test]
async fn router_restart_retains_identity_and_direct_connectivity() {
    let controller_key = SecretKey::generate();
    let controller_id = controller_key.public();
    let agent_key = SecretKey::generate();
    let hs = handshake(&agent_key);
    let node_id = hs.node_id.clone();

    for _ in 0..2 {
        let service = test_service(8);
        let trust = test_trust_for_key(&agent_key, &hs, None).await;
        let router = test_router(
            controller_key.clone(),
            RelayMode::Disabled,
            identity_policy(&agent_key, &hs),
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
            .expect("client");
        let conn = client
            .connect(router.endpoint().addr(), AGENT_ALPN)
            .await
            .expect("connect");
        assert_eq!(
            send_handshake(&conn, &hs).await.result,
            i32::from(HandshakeResult::Accepted)
        );
        wait_until_registered(&service, &node_id).await;
        conn.close(VarInt::from_u32(0), b"restart fixture");
        client.close().await;
        shutdown(&service, router).await.expect("shutdown");
    }
}

#[tokio::test]
async fn newer_authority_revision_closes_the_retained_session() {
    let agent_key = SecretKey::generate();
    let hs = handshake(&agent_key);
    let policy = identity_policy(&agent_key, &hs);
    let service = test_service_with_policy(8, policy.clone());
    let trust = test_trust_for_key(&agent_key, &hs, None).await;
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
        .expect("client");
    let conn = client
        .connect(router.endpoint().addr(), AGENT_ALPN)
        .await
        .expect("connect");
    let resp = send_handshake(&conn, &hs).await;
    assert_eq!(resp.result, i32::from(HandshakeResult::Accepted));
    wait_until_registered(&service, &hs.node_id).await;
    let meta = service
        .get_node_connection(Request::new(GetNodeConnectionRequest {
            node_id: hs.node_id.clone(),
        }))
        .await
        .expect("query")
        .into_inner();
    assert_eq!(
        (meta.endpoint_id.as_slice(), meta.path),
        (
            agent_key.public().as_bytes().as_slice(),
            i32::from(ConnectionPath::Direct)
        )
    );

    let (signing_key, _) = controller_keyring();
    let fence = resp.connection_fence.expect("fence");
    let req = state_update_req(
        &signing_key,
        &fence,
        meta.node_id.as_slice().try_into().expect("node"),
        *agent_key.public().as_bytes(),
        NodeTrustState::Active,
        2,
        "capability authority changed",
    );
    service
        .update_node_trust(Request::new(req))
        .await
        .expect("advance authority");
    tokio::time::timeout(Duration::from_secs(2), conn.closed())
        .await
        .expect("closed");
    shutdown(&service, router).await.expect("shutdown");
    client.close().await;
}

// ---------------------------------------------------------------------------
// Telemetry, ALPN, stalled handshake, replacement & command streams
// ---------------------------------------------------------------------------

#[tokio::test]
async fn telemetry_budgets_retention_and_security_priority() {
    // Flood does not evict consumers or starve control events.
    let s1 = test_service(8);
    let mut ev1 = s1
        .watch_events(Request::new(WatchEventsRequest {
            after_event_id: Vec::new(),
        }))
        .await
        .expect("w")
        .into_inner();
    let (a, b) = (Uuid::now_v7(), Uuid::now_v7());
    for _ in 0..64 {
        s1.shared
            .publish_telemetry(telemetry_event(a.as_bytes(), 16), false)
            .await;
    }
    s1.shared
        .publish(event(
            b.as_bytes(),
            &[8; 32],
            TransportEventType::Connected,
            Vec::new(),
        ))
        .await;
    {
        let st = s1.shared.inner.events.lock().await;
        assert_eq!(st.subscribers.len(), 1);
        assert!(st.telemetry_retained <= 4 && st.retained.len() <= 8);
    }
    assert_eq!(
        next_event(&mut ev1, TransportEventType::Connected)
            .await
            .node_id,
        b.as_bytes()
    );

    // Forged security priority is bounded per node without evicting consumer or starving others.
    let s2 = test_service(8);
    let _ev2 = s2
        .watch_events(Request::new(WatchEventsRequest {
            after_event_id: Vec::new(),
        }))
        .await
        .expect("w")
        .into_inner();
    let mut granted = 0;
    for _ in 0..32 {
        assert!(s2.shared.admit_telemetry(a.as_bytes(), 16).await);
        let sec = s2.shared.claim_security_priority(a.as_bytes()).await;
        granted += usize::from(sec);
        s2.shared
            .publish_telemetry(telemetry_event(a.as_bytes(), 16), sec)
            .await;
    }
    assert_eq!(granted, 4);
    s2.shared
        .publish(event(
            b.as_bytes(),
            &[8; 32],
            TransportEventType::Connected,
            Vec::new(),
        ))
        .await;
    assert_eq!(s2.shared.inner.events.lock().await.subscribers.len(), 1);
    assert!(
        s2.shared.admit_telemetry(b.as_bytes(), 16).await
            && s2.shared.claim_security_priority(b.as_bytes()).await
    );

    // Retained telemetry bytes stay bounded and keep control events.
    let s3 = test_service(4096);
    s3.shared
        .publish(event(
            Uuid::now_v7().as_bytes(),
            &[8; 32],
            TransportEventType::Connected,
            Vec::new(),
        ))
        .await;
    for _ in 0..200 {
        s3.shared
            .publish_telemetry(
                telemetry_event(Uuid::now_v7().as_bytes(), 512 * 1024),
                false,
            )
            .await;
    }
    {
        let st = s3.shared.inner.events.lock().await;
        assert!(st.telemetry_retained_bytes <= MAX_RETAINED_TELEMETRY_BYTES);
        assert_eq!(
            st.retained.iter().map(|e| e.payload.len()).sum::<usize>(),
            st.telemetry_retained_bytes
        );
        assert!(
            st.retained
                .iter()
                .any(|e| e.r#type == i32::from(TransportEventType::Connected))
        );
    }

    // Offline burst budget isolates nodes.
    let s4 = test_service(8);
    for _ in 0..32 {
        assert!(s4.shared.admit_telemetry(a.as_bytes(), 512 * 1024).await);
    }
    assert!(!s4.shared.admit_telemetry(a.as_bytes(), 512 * 1024).await);
    assert!(s4.shared.admit_telemetry(b.as_bytes(), 512 * 1024).await);
    for _ in 0..40 {
        let _ = s4.shared.admit_telemetry(b.as_bytes(), 1).await;
    }
    assert!(!s4.shared.admit_telemetry(b.as_bytes(), 1).await);
}

#[tokio::test]
async fn router_security_telemetry_stream_stalled_handshake_and_replacement() {
    // Wrong ALPN and unknown agent rejected.
    let s0 = test_service(8);
    let r0 = test_router(
        SecretKey::generate(),
        RelayMode::Disabled,
        IdentityPolicy::default(),
        test_trust_with_options(controller_keyring().0, Vec::new(), None).await,
        &s0,
    )
    .await;
    let c0 = Endpoint::builder(presets::Minimal)
        .relay_mode(RelayMode::Disabled)
        .bind()
        .await
        .expect("c0");
    assert!(
        c0.connect(r0.endpoint().addr(), b"wrong/alpn/1")
            .await
            .is_err()
    );
    let unknown = c0
        .connect(r0.endpoint().addr(), AGENT_ALPN)
        .await
        .expect("tls");
    tokio::time::timeout(Duration::from_secs(2), unknown.closed())
        .await
        .expect("closed");
    c0.close().await;
    shutdown(&s0, r0).await.expect("shut");

    // Telemetry uni-stream & replacement disconnect-before-connect ordering.
    let agent_key = SecretKey::generate();
    let hs = handshake(&agent_key);
    let service = test_service(8);
    let router = test_router(
        SecretKey::generate(),
        RelayMode::Disabled,
        identity_policy(&agent_key, &hs),
        test_trust_for_key(&agent_key, &hs, None).await,
        &service,
    )
    .await;
    let client = Endpoint::builder(presets::Minimal)
        .secret_key(agent_key.clone())
        .relay_mode(RelayMode::Disabled)
        .bind()
        .await
        .expect("c");
    let mut events = service
        .watch_events(Request::new(WatchEventsRequest {
            after_event_id: Vec::new(),
        }))
        .await
        .expect("w")
        .into_inner();
    let first = client
        .connect(router.endpoint().addr(), AGENT_ALPN)
        .await
        .expect("c1");
    send_handshake(&first, &hs).await;
    wait_until_registered(&service, &hs.node_id).await;

    let max_name = |p: &str, i: usize| format!("{p}{i:06}{}", "x".repeat(64 - p.len() - 6));
    let batch = TelemetryBatch {
        batch_id: Uuid::now_v7().as_bytes().to_vec(),
        node_id: hs.node_id.clone(),
        sequence: 1,
        priority: 2,
        users: (0..384)
            .map(|i| UserObservation {
                username: max_name("u", i),
                enabled: true,
                revision: 1,
                fingerprint_sha256: vec![1; 32],
            })
            .collect(),
        groups: (0..768)
            .map(|i| GroupObservation {
                group_name: max_name("g", i),
                members: if i < 384 {
                    vec![max_name("u", i)]
                } else {
                    Vec::new()
                },
                revision: 1,
                fingerprint_sha256: vec![2; 32],
            })
            .collect(),
        ..TelemetryBatch::default()
    };
    let payload = batch.encode_to_vec();
    assert!(payload.len() <= 512 * 1024);
    let mut uni = first.open_uni().await.expect("uni");
    write_frame_bytes(&mut uni, &payload).await;
    uni.finish().expect("fin");
    let ev = next_event(&mut events, TransportEventType::Telemetry).await;
    assert_eq!((ev.node_id, ev.payload), (hs.node_id.clone(), payload));

    let second = client
        .connect(router.endpoint().addr(), AGENT_ALPN)
        .await
        .expect("c2");
    send_handshake(&second, &hs).await;
    let mut kinds = Vec::new();
    tokio::time::timeout(Duration::from_secs(2), async {
        while kinds.len() < 2 {
            let e = events.next().await.expect("open").expect("ok");
            if e.r#type == i32::from(TransportEventType::Connected)
                || e.r#type == i32::from(TransportEventType::Disconnected)
            {
                kinds.push(e.r#type);
            }
        }
    })
    .await
    .expect("events");
    assert_eq!(
        kinds,
        vec![
            i32::from(TransportEventType::Disconnected),
            i32::from(TransportEventType::Connected)
        ]
    );
    tokio::time::timeout(Duration::from_secs(2), first.closed())
        .await
        .expect("first closed");
    shutdown(&service, router).await.expect("shut");
    client.close().await;

    // Stalled handshake response releases connection permit at deadline.
    let s2 = test_service(8);
    let hs2 = handshake(&agent_key);
    let r2 = Router::builder(
        Endpoint::builder(presets::Minimal)
            .secret_key(SecretKey::generate())
            .relay_mode(RelayMode::Disabled)
            .bind()
            .await
            .expect("ep"),
    )
    .accept(
        AGENT_ALPN,
        SessionHandler {
            shared: s2.shared.clone(),
            policy: identity_policy(&agent_key, &hs2),
            kind: ProtocolKind::Agent,
            trust: test_trust_for_key(&agent_key, &hs2, None).await,
            handshake_timeout: Duration::from_millis(500),
        },
    )
    .spawn();
    let stalled_client = Endpoint::builder(presets::Minimal)
        .secret_key(agent_key)
        .relay_mode(RelayMode::Disabled)
        .transport_config(
            QuicTransportConfig::builder()
                .stream_receive_window(VarInt::from_u32(0))
                .build(),
        )
        .bind()
        .await
        .expect("stalled");
    let conn = stalled_client
        .connect(r2.endpoint().addr(), AGENT_ALPN)
        .await
        .expect("conn");
    let (mut send, _recv) = conn.open_bi().await.expect("bi");
    write_frame_bytes(&mut send, &hs2.encode_to_vec()).await;
    send.finish().expect("fin");
    let permits = &s2.shared.inner.agent_connection_permits;
    tokio::time::timeout(Duration::from_secs(2), async {
        while permits.available_permits() == MAX_AGENT_CONNECTIONS {
            tokio::task::yield_now().await;
        }
    })
    .await
    .expect("held");
    tokio::time::timeout(Duration::from_secs(5), conn.closed())
        .await
        .expect("closed");
    tokio::time::timeout(Duration::from_secs(2), async {
        while permits.available_permits() != MAX_AGENT_CONNECTIONS {
            tokio::task::yield_now().await;
        }
    })
    .await
    .expect("released");
    assert_eq!(s2.shared.inner.connections.lock().await.len(), 0);
    shutdown(&s2, r2).await.expect("shut");
    stalled_client.close().await;
}

#[tokio::test]
async fn command_responses_are_published_with_negotiated_limits() {
    let agent_key = SecretKey::generate();
    let mut hs = handshake(&agent_key);
    hs.max_message_size = 2048;
    let service = test_service(16);
    let router = test_router(
        SecretKey::generate(),
        RelayMode::Disabled,
        identity_policy(&agent_key, &hs),
        test_trust_for_key(&agent_key, &hs, None).await,
        &service,
    )
    .await;
    let client = Endpoint::builder(presets::Minimal)
        .secret_key(agent_key)
        .relay_mode(RelayMode::Disabled)
        .bind()
        .await
        .expect("c");
    let conn = client
        .connect(router.endpoint().addr(), AGENT_ALPN)
        .await
        .expect("conn");
    let resp = send_handshake(&conn, &hs).await;
    assert_eq!(resp.result, i32::from(HandshakeResult::Accepted));
    wait_until_registered(&service, &hs.node_id).await;
    let meta = service
        .get_node_connection(Request::new(GetNodeConnectionRequest {
            node_id: hs.node_id.clone(),
        }))
        .await
        .expect("q")
        .into_inner();
    assert_eq!(meta.negotiated_capabilities, hs.capabilities);

    let (signing_key, _) = controller_keyring();
    let session_fence = resp.connection_fence.expect("fence");
    let fenced_cmd = |tp: &str| {
        let bare = command_envelope(&hs.node_id, tp, String::new());
        let b = bind_op(
            &signing_key,
            &session_fence,
            FenceOperationKind::Command,
            binding_op_id(&bare),
            "ocserv.status.read",
        );
        let mut d = CommandEnvelope::decode(bare.as_slice()).expect("dec");
        d.connection_fence = Some(session_fence.clone());
        d.fence_binding = Some(b);
        d.encode_to_vec()
    };

    // Unnegotiated capability, stale revision, expired session grant, oversized frame all rejected before transport.
    let mut unapproved = CommandEnvelope::decode(
        command_envelope(&hs.node_id, &new_traceparent(), String::new()).as_slice(),
    )
    .expect("d");
    unapproved.required_capability = "ocserv.service.reload".into();
    assert_eq!(
        service
            .send_command(Request::new(SendCommandRequest {
                node_id: hs.node_id.clone(),
                command_envelope: unapproved.encode_to_vec()
            }))
            .await
            .expect_err("cap")
            .code(),
        tonic::Code::PermissionDenied
    );
    service
        .shared
        .inner
        .connections
        .lock()
        .await
        .get_mut(&hs.node_id)
        .expect("s")
        .authorization_revision = 7;
    assert_eq!(
        service
            .send_command(Request::new(SendCommandRequest {
                node_id: hs.node_id.clone(),
                command_envelope: command_envelope(&hs.node_id, &new_traceparent(), String::new())
            }))
            .await
            .expect_err("rev")
            .code(),
        tonic::Code::FailedPrecondition
    );
    {
        let mut conns = service.shared.inner.connections.lock().await;
        let reg = conns.get_mut(&hs.node_id).expect("s");
        reg.authorization_revision = 0;
        reg.session_expires_at = Some(SystemTime::UNIX_EPOCH);
    }
    assert_eq!(
        service
            .send_command(Request::new(SendCommandRequest {
                node_id: hs.node_id.clone(),
                command_envelope: command_envelope(&hs.node_id, &new_traceparent(), String::new())
            }))
            .await
            .expect_err("exp")
            .code(),
        tonic::Code::FailedPrecondition
    );
    service
        .shared
        .inner
        .connections
        .lock()
        .await
        .get_mut(&hs.node_id)
        .expect("s")
        .session_expires_at = None;

    let tp = new_traceparent();
    let baseline = last_seen(&service, &hs.node_id).await;
    assert_eq!(
        service
            .send_command(Request::new(SendCommandRequest {
                node_id: hs.node_id.clone(),
                command_envelope: command_envelope(&hs.node_id, &tp, "x".repeat(2048))
            }))
            .await
            .expect_err("size")
            .code(),
        tonic::Code::ResourceExhausted
    );
    assert_eq!(last_seen(&service, &hs.node_id).await, baseline);

    // Valid attested command result succeeds and updates last_seen.
    tokio::time::sleep(Duration::from_millis(2)).await;
    let agent = tokio::spawn(respond_command(
        conn.clone(),
        CommandReply::Result {
            receipt: true,
            foreign_id: false,
        },
    ));
    let mut events = service
        .watch_events(Request::new(WatchEventsRequest {
            after_event_id: Vec::new(),
        }))
        .await
        .expect("w")
        .into_inner();
    let cmd_bytes = fenced_cmd(&tp);
    let sent = CommandEnvelope::decode(cmd_bytes.as_slice()).expect("dec");
    service
        .send_command(Request::new(SendCommandRequest {
            node_id: hs.node_id.clone(),
            command_envelope: cmd_bytes,
        }))
        .await
        .expect("send");
    agent.await.expect("agent");
    let ev = next_event(&mut events, TransportEventType::CommandResult).await;
    assert_eq!((ev.node_id, ev.traceparent), (hs.node_id.clone(), tp));
    let res = CommandResult::decode(ev.payload.as_slice()).expect("res");
    assert_eq!(
        (
            CommandResultState::try_from(res.state).expect("s"),
            res.result.as_slice()
        ),
        (CommandResultState::Succeeded, b"completed".as_slice())
    );
    let receipt = verify_receipt(
        res.privileged_result_proof.as_ref().expect("proof"),
        &signing_key.verifying_key(),
    )
    .expect("verify");
    assert_eq!(
        (
            receipt.node_id,
            receipt.command_id,
            receipt.operation_id,
            receipt.idempotency_key
        ),
        (
            sent.node_id,
            sent.command_id,
            sent.operation_id,
            sent.idempotency_key
        )
    );
    assert_eq!(
        receipt.result_bytes_sha256,
        Sha256::digest(b"completed").to_vec()
    );
    assert_ne!(last_seen(&service, &hs.node_id).await, baseline);

    // Missing receipt, foreign command ID, empty EOF, and heartbeat-first all publish Unknown without forwarding heartbeat.
    for reply in [
        CommandReply::Result {
            receipt: false,
            foreign_id: false,
        },
        CommandReply::Result {
            receipt: true,
            foreign_id: true,
        },
        CommandReply::Eof,
        CommandReply::HeartbeatFirst,
    ] {
        let trace = new_traceparent();
        let task = tokio::spawn(respond_command(conn.clone(), reply));
        let bytes = fenced_cmd(&trace);
        let expected = CommandEnvelope::decode(bytes.as_slice()).expect("dec");
        service
            .send_command(Request::new(SendCommandRequest {
                node_id: hs.node_id.clone(),
                command_envelope: bytes,
            }))
            .await
            .expect("send");
        task.await.expect("task");
        let ev = tokio::time::timeout(Duration::from_secs(2), async {
            loop {
                let e = events.next().await.expect("open").expect("ok");
                assert_ne!(e.r#type, i32::from(TransportEventType::Heartbeat));
                if e.r#type == i32::from(TransportEventType::CommandResult) {
                    break e;
                }
            }
        })
        .await
        .expect("ev");
        assert_eq!(ev.traceparent, trace);
        let r = CommandResult::decode(ev.payload.as_slice()).expect("res");
        assert_eq!(
            CommandResultState::try_from(r.state).expect("s"),
            CommandResultState::Unknown
        );
        assert_eq!(
            (r.command_id, r.idempotency_key),
            (expected.command_id, expected.idempotency_key)
        );
    }

    shutdown(&service, router).await.expect("shut");
    client.close().await;
}

// ---------------------------------------------------------------------------
// Relay tests (pinned by `cargo test -p ocservia-transportd relay`)
// ---------------------------------------------------------------------------

#[tokio::test]
async fn relay_only_connection_and_disabled_relay_failure() {
    let (relay_map, roots, relay) = private_tls_relay().await;
    let agent_key = SecretKey::generate();
    let hs = handshake(&agent_key);
    let service = test_service(8);
    let router = build_router_with_direct(
        SecretKey::generate(),
        RelayMode::Custom(relay_map.clone()),
        identity_policy(&agent_key, &hs),
        test_trust_for_key(&agent_key, &hs, None).await,
        &service,
        false,
        roots.clone(),
    )
    .await
    .expect("router");
    tokio::time::timeout(Duration::from_secs(30), router.endpoint().online())
        .await
        .expect("online");
    let full = router.endpoint().addr();
    let relay_only = iroh::EndpointAddr::new(full.id)
        .with_relay_url(full.relay_urls().next().expect("url").clone());

    let disabled = Endpoint::builder(presets::Minimal)
        .relay_mode(RelayMode::Disabled)
        .bind()
        .await
        .expect("d");
    assert!(!matches!(
        tokio::time::timeout(
            Duration::from_secs(5),
            disabled.connect(relay_only.clone(), AGENT_ALPN)
        )
        .await,
        Ok(Ok(_))
    ));
    disabled.close().await;

    let client = Endpoint::builder(presets::N0)
        .secret_key(agent_key)
        .relay_mode(RelayMode::Custom(relay_map))
        .ca_tls_config(CaTlsConfig::default().with_extra_roots(roots))
        .clear_address_lookup()
        .clear_ip_transports()
        .bind()
        .await
        .expect("client");
    let conn = tokio::time::timeout(
        Duration::from_secs(30),
        client.connect(relay_only, AGENT_ALPN),
    )
    .await
    .expect("t")
    .expect("c");
    assert_eq!(
        send_handshake(&conn, &hs).await.result,
        i32::from(HandshakeResult::Accepted)
    );
    tokio::time::timeout(Duration::from_secs(2), async {
        while service
            .shared
            .inner
            .connections
            .lock()
            .await
            .get(&hs.node_id)
            .map(|e| e.metadata.path)
            != Some(i32::from(ConnectionPath::Relay))
        {
            tokio::task::yield_now().await;
        }
    })
    .await
    .expect("relay path");
    shutdown(&service, router).await.expect("shut");
    client.close().await;
    relay.shutdown().await.expect("stop");
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
    let relay_addr = relay.https_addr().expect("addr");
    let proxy = RestartableTcpProxy::start("127.0.0.1:0".parse().expect("a"), relay_addr).await;
    let url: iroh::RelayUrl = format!("https://{}", proxy.addr).parse().expect("u");
    let map = RelayMap::from_iter([iroh_relay::RelayConfig::from(url.clone())]);
    let agent_key = SecretKey::generate();
    let hs = handshake(&agent_key);
    let service = test_service(16);
    let router = build_router_with_direct(
        SecretKey::generate(),
        RelayMode::Custom(map.clone()),
        identity_policy(&agent_key, &hs),
        test_trust_for_key(&agent_key, &hs, None).await,
        &service,
        false,
        roots.clone(),
    )
    .await
    .expect("router");
    let identity = router.endpoint().id();
    let mut proxy = Some(proxy);
    for recovering in [false, true] {
        if recovering {
            let addr = proxy.take().expect("p").stop().await;
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
            .expect("disconnected");
            proxy = Some(RestartableTcpProxy::start(addr, relay_addr).await);
        }
        tokio::time::timeout(Duration::from_secs(90), router.endpoint().online())
            .await
            .expect("online");
        let client = Endpoint::builder(presets::Minimal)
            .secret_key(agent_key.clone())
            .relay_mode(RelayMode::Custom(map.clone()))
            .ca_tls_config(CaTlsConfig::default().with_extra_roots(roots.clone()))
            .clear_ip_transports()
            .bind()
            .await
            .expect("c");
        let target = iroh::EndpointAddr::new(identity).with_relay_url(url.clone());
        let conn =
            tokio::time::timeout(Duration::from_secs(15), client.connect(target, AGENT_ALPN))
                .await
                .expect("t")
                .expect("c");
        assert_eq!(
            send_handshake(&conn, &hs).await.result,
            i32::from(HandshakeResult::Accepted)
        );
        assert_eq!(router.endpoint().id(), identity);
        client.close().await;
    }
    shutdown(&service, router).await.expect("shut");
    proxy.expect("p").stop().await;
    relay.shutdown().await.expect("stop");
}

#[tokio::test]
async fn relay_and_direct_paths_converge_to_direct() {
    let (relay_map, roots, relay) = private_tls_relay().await;
    let agent_key = SecretKey::generate();
    let hs = handshake(&agent_key);
    let service = test_service(16);
    let router = build_router_with_trust_tls_roots(
        SecretKey::generate(),
        RelayMode::Custom(relay_map.clone()),
        identity_policy(&agent_key, &hs),
        test_trust_for_key(&agent_key, &hs, None).await,
        roots.clone(),
        &service,
    )
    .await
    .expect("router");
    tokio::time::timeout(Duration::from_secs(30), router.endpoint().online())
        .await
        .expect("online");
    let client = Endpoint::builder(presets::N0)
        .secret_key(agent_key)
        .relay_mode(RelayMode::Custom(relay_map))
        .ca_tls_config(CaTlsConfig::default().with_extra_roots(roots))
        .clear_address_lookup()
        .bind()
        .await
        .expect("c");
    let conn = tokio::time::timeout(
        Duration::from_secs(30),
        client.connect(router.endpoint().addr(), AGENT_ALPN),
    )
    .await
    .expect("t")
    .expect("c");
    assert_eq!(
        send_handshake(&conn, &hs).await.result,
        i32::from(HandshakeResult::Accepted)
    );
    tokio::time::timeout(Duration::from_secs(30), async {
        loop {
            let paths = conn.paths();
            if paths.iter().any(|p| p.is_relay())
                && paths.iter().any(|p| p.is_ip() && p.is_selected())
            {
                break;
            }
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
    })
    .await
    .expect("direct");
    let meta = service
        .get_node_connection(Request::new(GetNodeConnectionRequest {
            node_id: hs.node_id,
        }))
        .await
        .expect("q")
        .into_inner();
    assert_eq!(meta.path, i32::from(ConnectionPath::Direct));
    shutdown(&service, router).await.expect("shut");
    client.close().await;
    relay.shutdown().await.expect("stop");
}

// ---------------------------------------------------------------------------
// Artifact streaming, fence carriers, cancel & expiry
// ---------------------------------------------------------------------------

type EventStreamOf<T> = Pin<Box<dyn Stream<Item = Result<T, Status>> + Send>>;
type AgentFetch = (
    EventStreamOf<ArtifactChunk>,
    iroh::endpoint::SendStream,
    iroh::endpoint::RecvStream,
    [u8; 16],
    (Router, Endpoint, Connection),
);

async fn stalled_artifact_fetch(service: &IrohTransportService, ttl: Duration) -> AgentFetch {
    let (key, _) = controller_keyring();
    let node_id = *Uuid::now_v7().as_bytes();
    let (_term, fence, router, client, conn) =
        fenced_agent_session(service, &key, node_id, 61).await;
    register_fence(service, &fence).await;
    let art_id = *Uuid::now_v7().as_bytes();
    let binding = bind_op(
        &key,
        &fence,
        FenceOperationKind::Artifact,
        art_id,
        "ocserv.fencing.v2",
    );
    let mut grant = shape_valid_grant(node_id, art_id);
    grant.expires_at = Some((SystemTime::now() + ttl).into());
    let fetched = service
        .fetch_artifact(Request::new(FetchArtifactRequest {
            node_id: node_id.to_vec(),
            artifact_id: art_id.to_vec(),
            purpose: "certificate_p12".into(),
            max_bytes: 32,
            grant: Some(grant),
            fence_binding: Some(binding),
        }))
        .await
        .expect("fetch")
        .into_inner();
    let (send, mut recv) = tokio::time::timeout(Duration::from_secs(5), conn.accept_bi())
        .await
        .expect("t")
        .expect("bi");
    let mut hdr = [0_u8; 4];
    recv.read_exact(&mut hdr).await.expect("hdr");
    let mut body = vec![0_u8; (u32::from_be_bytes(hdr) & !ARTIFACT_FETCH_FRAME) as usize];
    recv.read_exact(&mut body).await.expect("body");
    (fetched, send, recv, art_id, (router, client, conn))
}

async fn assert_artifact_stream_aborted(send: &mut iroh::endpoint::SendStream) {
    let stopped = tokio::time::timeout(Duration::from_secs(5), send.stopped())
        .await
        .expect("t")
        .expect("ok");
    assert_eq!(stopped, Some(ARTIFACT_STREAM_ABORT));
}

#[tokio::test]
async fn artifact_fence_verification_and_carrier_forwarding() {
    // Unsigned artifact requests fail before connection lookup.
    let s0 = test_service(8);
    let n0 = Uuid::now_v7();
    let a0 = Uuid::now_v7();
    let Err(fetch_err) = s0
        .fetch_artifact(Request::new(FetchArtifactRequest {
            node_id: n0.as_bytes().to_vec(),
            artifact_id: a0.as_bytes().to_vec(),
            purpose: "certificate_p12".into(),
            max_bytes: 32,
            grant: None,
            fence_binding: None,
        }))
        .await
    else {
        panic!("unsigned fetch must fail");
    };
    assert_eq!(fetch_err.code(), tonic::Code::PermissionDenied);
    assert_eq!(
        s0.consume_artifact(Request::new(ConsumeArtifactRequest {
            node_id: n0.as_bytes().to_vec(),
            grant: None,
            sha256: vec![0; 32],
            size: 32,
            confirm_only: false,
            fence_binding: None,
        }))
        .await
        .expect_err("unsigned consume")
        .code(),
        tonic::Code::PermissionDenied
    );

    // Acceptance waits for fence verification; refreshed wire proof crosses agent boundary on fetch & consume.
    let (key, kr) = controller_keyring();
    let node_id = *Uuid::now_v7().as_bytes();
    let service = IrohTransportService::new(8, IdentityPolicy::default(), kr);
    let (term, _hs_fence, _router, _client, conn) =
        fenced_agent_session(&service, &key, node_id, 58).await;
    let expired_hs = signed_fence_at(&key, &term, -10, -1);
    let exp_verified = service
        .shared
        .keyring()
        .verify_connection_fence_v2_at(&expired_hs, &term.node_id, &term.endpoint_id, unix_now(), 0)
        .expect("v");
    service
        .shared
        .inner
        .connections
        .lock()
        .await
        .get_mut(node_id.as_slice())
        .expect("s")
        .fence = Arc::new(RegisteredFence {
        verified: exp_verified,
        fence: expired_hs.clone(),
        endpoint_id: term.endpoint_id,
    });
    let fence = signed_fence(&key, &term, 60);
    register_fence(&service, &fence).await;

    let art_id = *Uuid::now_v7().as_bytes();
    let grant = shape_valid_grant(node_id, art_id);
    let fetch_binding = bind_op(
        &key,
        &fence,
        FenceOperationKind::Artifact,
        art_id,
        "ocserv.fencing.v2",
    );
    let mut pending = service.fetch_artifact(Request::new(FetchArtifactRequest {
        node_id: node_id.to_vec(),
        artifact_id: art_id.to_vec(),
        purpose: "certificate_p12".into(),
        max_bytes: 32,
        grant: Some(grant.clone()),
        fence_binding: Some(fetch_binding.clone()),
    }));
    let lock = service.shared.inner.fences.lock().await;
    assert!(
        tokio::time::timeout(Duration::from_millis(200), &mut pending)
            .await
            .is_err()
    );
    drop(lock);
    let mut fetched = tokio::time::timeout(Duration::from_secs(5), pending)
        .await
        .expect("t")
        .expect("ok")
        .into_inner();

    let (mut fsend, mut frecv) = tokio::time::timeout(Duration::from_secs(5), conn.accept_bi())
        .await
        .expect("t")
        .expect("bi");
    let mut hdr = [0_u8; 4];
    frecv.read_exact(&mut hdr).await.expect("h");
    assert_eq!(hdr[0] & 0xc0, 0x80);
    let mut body = vec![0_u8; (u32::from_be_bytes(hdr) & !ARTIFACT_FETCH_FRAME) as usize];
    frecv.read_exact(&mut body).await.expect("b");
    let fwd = ArtifactFetchRequest::decode(body.as_slice()).expect("dec");
    assert_eq!(fwd.connection_fence.as_ref(), Some(&fence));
    assert_ne!(fwd.connection_fence.as_ref(), Some(&expired_hs));
    assert_eq!(fwd.fence_binding.as_ref(), Some(&fetch_binding));
    let chunk = ArtifactChunk {
        artifact_id: art_id.to_vec(),
        offset: 0,
        data: vec![1],
        eof: true,
        sha256: vec![0; 32],
    }
    .encode_to_vec();
    write_frame_bytes(&mut fsend, &chunk).await;
    fsend.finish().expect("fin");
    assert!(fetched.next().await.expect("item").is_ok());
    assert!(
        tokio::time::timeout(Duration::from_secs(5), fsend.stopped())
            .await
            .expect("t")
            .expect("ok")
            .is_none()
    );

    let grant_id: [u8; 16] = grant.grant_id.as_slice().try_into().expect("gid");
    let consume_binding = bind_op(
        &key,
        &fence,
        FenceOperationKind::Artifact,
        grant_id,
        "ocserv.fencing.v2",
    );
    let consume_task = tokio::spawn({
        let svc = service.clone();
        let g = grant.clone();
        let cb = consume_binding.clone();
        async move {
            svc.consume_artifact(Request::new(ConsumeArtifactRequest {
                node_id: node_id.to_vec(),
                grant: Some(g),
                sha256: vec![0x11; 32],
                size: 32,
                confirm_only: false,
                fence_binding: Some(cb),
            }))
            .await
        }
    });
    let (mut csend, mut crecv) = tokio::time::timeout(Duration::from_secs(5), conn.accept_bi())
        .await
        .expect("t")
        .expect("bi");
    crecv.read_exact(&mut hdr).await.expect("h");
    assert_eq!(hdr[0] & 0xc0, 0xc0);
    let mut cbody = vec![0_u8; (u32::from_be_bytes(hdr) & !ARTIFACT_CONSUME_FRAME) as usize];
    crecv.read_exact(&mut cbody).await.expect("b");
    let cfwd = AgentArtifactConsumeRequest::decode(cbody.as_slice()).expect("dec");
    assert_eq!(
        (cfwd.connection_fence.as_ref(), cfwd.fence_binding.as_ref()),
        (Some(&fence), Some(&consume_binding))
    );
    let cresp = AgentArtifactConsumeResponse {
        artifact_id: art_id.to_vec(),
        grant_id: grant_id.to_vec(),
        consumed: true,
    }
    .encode_to_vec();
    write_frame_bytes(&mut csend, &cresp).await;
    csend.finish().expect("fin");
    consume_task.await.expect("join").expect("ok");
    service.begin_shutdown().await;
}

#[tokio::test]
async fn artifact_cancel_and_grant_expiry_abort_stalled_streams() {
    let (_, kr) = controller_keyring();
    let service = IrohTransportService::new(8, IdentityPolicy::default(), kr);

    // Cancel before first chunk.
    let (fetched, mut send, _recv, _, (_r1, _c1, conn1)) =
        stalled_artifact_fetch(&service, Duration::from_mins(1)).await;
    drop(fetched);
    assert_artifact_stream_aborted(&mut send).await;
    assert!(conn1.close_reason().is_none());

    // Cancel after first chunk.
    let (mut fetched, mut send, _recv, art_id, (_r2, _c2, conn2)) =
        stalled_artifact_fetch(&service, Duration::from_mins(1)).await;
    let chunk = ArtifactChunk {
        artifact_id: art_id.to_vec(),
        offset: 0,
        data: vec![1],
        eof: false,
        sha256: Vec::new(),
    }
    .encode_to_vec();
    write_frame_bytes(&mut send, &chunk).await;
    fetched.next().await.expect("first").expect("ok");
    drop(fetched);
    assert_artifact_stream_aborted(&mut send).await;
    assert!(conn2.close_reason().is_none());

    // Grant expiry aborts stalled stream with DeadlineExceeded.
    let (mut fetched, mut send, _recv, _, (_r3, _c3, conn3)) =
        stalled_artifact_fetch(&service, Duration::from_secs(1)).await;
    let err = tokio::time::timeout(Duration::from_secs(5), fetched.next())
        .await
        .expect("t")
        .expect("item")
        .expect_err("exp");
    assert_eq!(err.code(), tonic::Code::DeadlineExceeded);
    assert_artifact_stream_aborted(&mut send).await;
    assert!(conn3.close_reason().is_none());

    service.begin_shutdown().await;
}

// ---------------------------------------------------------------------------
// Connection-owner fence enforcement & recovery
// ---------------------------------------------------------------------------

#[tokio::test]
async fn fenced_command_dispatch_takeover_and_registration_boundaries() {
    let (key, kr) = controller_keyring();
    let node_id = *Uuid::now_v7().as_bytes();
    let service = IrohTransportService::new(8, IdentityPolicy::default(), kr);
    let (mut term, fence, _router, _client, _conn) =
        fenced_agent_session(&service, &key, node_id, 41).await;
    let first = register_fence(&service, &fence).await;
    assert_eq!(
        (first.disposition, first.retained_epoch),
        (i32::from(OwnerFenceDisposition::Refreshed), 41)
    );

    // Bare command, misbound command ID, and unheld capability all rejected.
    let bare = command_envelope(&node_id, &new_traceparent(), String::new());
    let other_id = binding_op_id(&command_envelope(
        &node_id,
        &new_traceparent(),
        String::new(),
    ));
    let this_id = binding_op_id(&bare);
    let misbound = fenced_command_envelope(
        &node_id,
        &new_traceparent(),
        &fence,
        &bind_op(
            &key,
            &fence,
            FenceOperationKind::Command,
            other_id,
            "ocserv.service.reload",
        ),
    );
    let unheld = fenced_command_envelope(
        &node_id,
        &new_traceparent(),
        &fence,
        &bind_op(
            &key,
            &fence,
            FenceOperationKind::Command,
            this_id,
            "ocserv.tenant.admin",
        ),
    );
    for env in [bare, misbound, unheld] {
        assert_eq!(
            service
                .send_command(Request::new(SendCommandRequest {
                    node_id: node_id.to_vec(),
                    command_envelope: env
                }))
                .await
                .expect_err("rej")
                .code(),
            tonic::Code::PermissionDenied
        );
    }

    // Same term refresh succeeds; higher epoch takeover retires live session, rejects stale & collision.
    let refreshed = register_fence(&service, &signed_fence(&key, &term, 60)).await;
    assert_eq!(
        (refreshed.disposition, refreshed.retained_epoch),
        (i32::from(OwnerFenceDisposition::Refreshed), 41)
    );
    term.supersede();
    let takeover = register_fence(&service, &signed_fence(&key, &term, 30)).await;
    assert_eq!(
        (takeover.disposition, takeover.retained_epoch),
        (i32::from(OwnerFenceDisposition::Applied), 42)
    );
    assert!(
        service
            .shared
            .inner
            .connections
            .lock()
            .await
            .get(node_id.as_slice())
            .is_none()
    );

    let stale_cmd = fenced_command_envelope(
        &node_id,
        &new_traceparent(),
        &fence,
        &bind_op(
            &key,
            &fence,
            FenceOperationKind::Command,
            this_id,
            "ocserv.service.reload",
        ),
    );
    let err = service
        .send_command(Request::new(SendCommandRequest {
            node_id: node_id.to_vec(),
            command_envelope: stale_cmd,
        }))
        .await
        .expect_err("stale");
    assert!(matches!(
        err.code(),
        tonic::Code::PermissionDenied | tonic::Code::Unavailable
    ));

    let stale_reg = register_fence(
        &service,
        &signed_fence(&key, &FenceTerm::new(node_id, term.endpoint_id, 41), 30),
    )
    .await;
    assert_eq!(
        (stale_reg.disposition, stale_reg.retained_epoch),
        (i32::from(OwnerFenceDisposition::Stale), 42)
    );
    assert_eq!(
        service
            .get_owner_fence(Request::new(GetOwnerFenceRequest {
                node_id: node_id.to_vec()
            }))
            .await
            .expect("get")
            .into_inner()
            .fence
            .expect("f")
            .owner_epoch,
        42
    );
    assert_eq!(
        service
            .register_owner_fence(Request::new(RegisterOwnerFenceRequest {
                fence: Some(signed_fence(
                    &key,
                    &FenceTerm::new(node_id, term.endpoint_id, 42),
                    30
                ))
            }))
            .await
            .expect_err("collision")
            .code(),
        tonic::Code::InvalidArgument
    );

    // Expired lease, unknown key, and nanosecond lease boundaries.
    let n2 = *Uuid::now_v7().as_bytes();
    let ep2 = [9_u8; 32];
    assert_eq!(
        service
            .register_owner_fence(Request::new(RegisterOwnerFenceRequest {
                fence: Some(signed_fence_at(
                    &key,
                    &FenceTerm::new(n2, ep2, 3),
                    -120,
                    -30
                ))
            }))
            .await
            .expect_err("exp")
            .code(),
        tonic::Code::FailedPrecondition
    );
    assert_eq!(
        service
            .register_owner_fence(Request::new(RegisterOwnerFenceRequest {
                fence: Some(signed_fence(
                    &SigningKey::from_bytes(&[11; 32]),
                    &FenceTerm::new(n2, ep2, 4),
                    30
                ))
            }))
            .await
            .expect_err("key")
            .code(),
        tonic::Code::PermissionDenied
    );
    assert_eq!(
        service
            .get_owner_fence(Request::new(GetOwnerFenceRequest {
                node_id: n2.to_vec()
            }))
            .await
            .expect_err("404")
            .code(),
        tonic::Code::NotFound
    );

    let frac_ok = register_fence(
        &service,
        &signed_fence_until(
            &key,
            &FenceTerm::new(n2, ep2, 9),
            SystemTime::now() + Duration::from_millis(400),
        ),
    )
    .await;
    assert_eq!(
        frac_ok.disposition,
        i32::from(OwnerFenceDisposition::Applied)
    );
    assert_eq!(
        service
            .register_owner_fence(Request::new(RegisterOwnerFenceRequest {
                fence: Some(signed_fence_until(
                    &key,
                    &FenceTerm::new(n2, ep2, 10),
                    SystemTime::now() - Duration::from_millis(150)
                ))
            }))
            .await
            .expect_err("sub-s exp")
            .code(),
        tonic::Code::FailedPrecondition
    );
    service.begin_shutdown().await;
}

#[tokio::test]
async fn owner_lease_expiry_closes_and_refresh_preserves_agent_session() {
    let (key, kr) = controller_keyring();
    let service = IrohTransportService::new(8, IdentityPolicy::default(), kr);

    // Short lease expires and closes session without new command.
    let n1 = *Uuid::now_v7().as_bytes();
    let (t1, _, _r1, _c1, conn1) = fenced_agent_session(&service, &key, n1, 7).await;
    register_fence(&service, &signed_fence(&key, &t1, 2)).await;
    tokio::time::timeout(Duration::from_secs(5), conn1.closed())
        .await
        .expect("expired closes");

    // Refreshing the same term outlives the initial short handshake snapshot.
    let n2 = *Uuid::now_v7().as_bytes();
    let (t2, _, _r2, _c2, conn2) = fenced_agent_session(&service, &key, n2, 7).await;
    let short = signed_fence(&key, &t2, 2);
    service
        .shared
        .inner
        .connections
        .lock()
        .await
        .get_mut(n2.as_slice())
        .expect("s")
        .fence = Arc::new(RegisteredFence {
        verified: service
            .shared
            .keyring()
            .verify_connection_fence_v2_at(&short, &t2.node_id, &t2.endpoint_id, unix_now(), 0)
            .expect("v"),
        fence: short.clone(),
        endpoint_id: t2.endpoint_id,
    });
    register_fence(&service, &short).await;
    register_fence(&service, &signed_fence(&key, &t2, 120)).await;
    assert!(
        tokio::time::timeout(Duration::from_secs(3), conn2.closed())
            .await
            .is_err()
    );
    service.begin_shutdown().await;
}

#[tokio::test]
async fn fenced_admin_operations_binding_and_lost_ack_recovery() {
    let (key, kr) = controller_keyring();
    let service = IrohTransportService::new(8, IdentityPolicy::default(), kr);

    // Fenceless close/state-update/send_command fail closed.
    let unreg = *Uuid::now_v7().as_bytes();
    assert_eq!(
        service
            .send_command(Request::new(SendCommandRequest {
                node_id: unreg.to_vec(),
                command_envelope: command_envelope(&unreg, &new_traceparent(), String::new())
            }))
            .await
            .expect_err("unavail")
            .code(),
        tonic::Code::Unavailable
    );
    assert_eq!(
        service
            .close_node(Request::new(CloseNodeRequest {
                node_id: unreg.to_vec(),
                reason: "close".into(),
                fence_binding: None
            }))
            .await
            .expect_err("close")
            .code(),
        tonic::Code::PermissionDenied
    );
    assert_eq!(
        service
            .update_node_trust(Request::new(UpdateNodeTrustRequest {
                node_id: unreg.to_vec(),
                endpoint_id: SecretKey::generate().public().as_bytes().to_vec(),
                state: NodeTrustState::Active.into(),
                reason: "fenceless".into(),
                revision: 2,
                fence_binding: None,
                operation_id: Vec::new(),
            }))
            .await
            .expect_err("trust")
            .code(),
        tonic::Code::PermissionDenied
    );

    // Registered fence rejects bindingless fetch/consume/confirm/close/state-update, wrong kind, foreign term, and carrier tampering.
    let n1 = *Uuid::now_v7().as_bytes();
    let (t1, f1, _r1, _c1, _conn1) = fenced_agent_session(&service, &key, n1, 15).await;
    register_fence(&service, &f1).await;
    let art = *Uuid::now_v7().as_bytes();
    let grant = shape_valid_grant(n1, art);
    assert!(matches!(
        service.fetch_artifact(Request::new(FetchArtifactRequest { node_id: n1.to_vec(), artifact_id: art.to_vec(), purpose: "certificate_p12".into(), max_bytes: 32, grant: Some(grant.clone()), fence_binding: None })).await,
        Err(s) if s.code() == tonic::Code::PermissionDenied
    ));
    for confirm_only in [false, true] {
        assert_eq!(
            service
                .consume_artifact(Request::new(ConsumeArtifactRequest {
                    node_id: n1.to_vec(),
                    grant: Some(grant.clone()),
                    sha256: vec![0x11; 32],
                    size: 32,
                    confirm_only,
                    fence_binding: None
                }))
                .await
                .expect_err("c")
                .code(),
            tonic::Code::PermissionDenied
        );
    }
    assert_eq!(
        service
            .close_node(Request::new(CloseNodeRequest {
                node_id: n1.to_vec(),
                reason: "c".into(),
                fence_binding: None
            }))
            .await
            .expect_err("cl")
            .code(),
        tonic::Code::PermissionDenied
    );
    assert_eq!(
        service
            .close_node(Request::new(CloseNodeRequest {
                node_id: n1.to_vec(),
                reason: "c".into(),
                fence_binding: Some(bind_op(
                    &key,
                    &f1,
                    FenceOperationKind::StateUpdate,
                    n1,
                    "ocserv.fencing.v2"
                )),
            }))
            .await
            .expect_err("wrong kind")
            .code(),
        tonic::Code::PermissionDenied
    );
    let op_id = state_update_operation_id(
        &n1,
        &t1.endpoint_id,
        NodeTrustState::Active as u32,
        4,
        "carrier",
    );
    assert_eq!(
        service
            .update_node_trust(Request::new(UpdateNodeTrustRequest {
                node_id: n1.to_vec(),
                endpoint_id: t1.endpoint_id.to_vec(),
                state: NodeTrustState::Active.into(),
                reason: "carrier".into(),
                revision: 4,
                fence_binding: None,
                operation_id: op_id.to_vec(),
            }))
            .await
            .expect_err("bl")
            .code(),
        tonic::Code::PermissionDenied
    );
    let foreign_f = signed_fence(&key, &FenceTerm::new(n1, t1.endpoint_id, 99), 30);
    assert_eq!(
        service
            .update_node_trust(Request::new(state_update_req(
                &key,
                &foreign_f,
                n1,
                t1.endpoint_id,
                NodeTrustState::Revoked,
                3,
                "stale"
            )))
            .await
            .expect_err("foreign")
            .code(),
        tonic::Code::PermissionDenied
    );
    let carrier_b = bind_op(
        &key,
        &f1,
        FenceOperationKind::StateUpdate,
        op_id,
        "ocserv.fencing.v2",
    );
    let mut flipped_ep = t1.endpoint_id;
    flipped_ep[0] ^= 1;
    for (ep, st, rev, rsn) in [
        (flipped_ep, NodeTrustState::Active, 4, "carrier"),
        (t1.endpoint_id, NodeTrustState::Revoked, 4, "carrier"),
        (t1.endpoint_id, NodeTrustState::Active, 5, "carrier"),
        (t1.endpoint_id, NodeTrustState::Active, 4, "different"),
    ] {
        let tid = state_update_operation_id(&n1, &ep, st as u32, rev, rsn);
        let code = service
            .update_node_trust(Request::new(UpdateNodeTrustRequest {
                node_id: n1.to_vec(),
                endpoint_id: ep.to_vec(),
                state: st.into(),
                reason: rsn.into(),
                revision: rev,
                fence_binding: Some(carrier_b.clone()),
                operation_id: tid.to_vec(),
            }))
            .await
            .expect_err("tamper")
            .code();
        assert!(matches!(
            code,
            tonic::Code::PermissionDenied | tonic::Code::InvalidArgument
        ));
    }

    // Owner-bound close emits Disconnected with owner term; retry after lost ack returns NOT_FOUND.
    let mut events = service
        .watch_events(Request::new(WatchEventsRequest {
            after_event_id: Vec::new(),
        }))
        .await
        .expect("w")
        .into_inner();
    let close_req = close_node_req(&key, &f1, n1, "operator close");
    service
        .close_node(Request::new(close_req.clone()))
        .await
        .expect("close");
    let disc = next_event(&mut events, TransportEventType::Disconnected).await;
    assert_eq!(
        (disc.node_id, disc.connection_id, disc.owner_epoch),
        (n1.to_vec(), t1.connection_id.to_vec(), t1.owner_epoch)
    );
    assert_eq!(
        service
            .close_node(Request::new(close_req))
            .await
            .expect_err("retry close")
            .code(),
        tonic::Code::NotFound
    );
    assert_eq!(
        service
            .get_node_connection(Request::new(GetNodeConnectionRequest {
                node_id: n1.to_vec()
            }))
            .await
            .expect_err("absent")
            .code(),
        tonic::Code::NotFound
    );

    // Revoke retry after lost ack succeeds under same live fence, rejects changed carrier, fails after fence expiry.
    let n2 = *Uuid::now_v7().as_bytes();
    let (t2, f2, r2, c2, conn2) = fenced_agent_session(&service, &key, n2, 12).await;
    register_fence(&service, &f2).await;
    let revoke = state_update_req(
        &key,
        &f2,
        n2,
        t2.endpoint_id,
        NodeTrustState::Revoked,
        3,
        "revoke",
    );
    drop(
        service
            .update_node_trust(Request::new(revoke.clone()))
            .await
            .expect("apply revoke"),
    );
    tokio::time::timeout(Duration::from_secs(2), conn2.closed())
        .await
        .expect("closed");
    let retry = service
        .update_node_trust(Request::new(revoke.clone()))
        .await
        .expect("retry")
        .into_inner();
    assert_eq!(
        (
            retry.disposition,
            retry.retained_revision,
            retry.retained_state
        ),
        (
            i32::from(TrustUpdateDisposition::Applied),
            3,
            i32::from(NodeTrustState::Revoked)
        )
    );
    let mut changed = revoke.clone();
    changed.reason = "another".into();
    assert_eq!(
        service
            .update_node_trust(Request::new(changed))
            .await
            .expect_err("mismatch")
            .code(),
        tonic::Code::InvalidArgument
    );
    assert_eq!(
        service
            .close_node(Request::new(close_node_req(&key, &f2, n2, "revoke")))
            .await
            .expect_err("absent")
            .code(),
        tonic::Code::NotFound
    );
    Arc::make_mut(
        service
            .shared
            .inner
            .fences
            .lock()
            .await
            .get_mut(n2.as_slice())
            .expect("f"),
    )
    .verified
    .lease_until_seconds = 0;
    assert_eq!(
        service
            .update_node_trust(Request::new(revoke))
            .await
            .expect_err("exp")
            .code(),
        tonic::Code::FailedPrecondition
    );

    shutdown(&service, r2).await.expect("shut");
    c2.close().await;
}

#[tokio::test]
async fn restored_trust_snapshot_blocks_revoked_endpoint_without_operation_history() {
    let agent_key = SecretKey::generate();
    let ep = agent_key.public();
    let hs = handshake(&agent_key);
    let node = hs.node_id.clone();
    let ctrl_key = SecretKey::generate();
    for state in [NodeTrustState::Active, NodeTrustState::Revoked] {
        let restored = IdentityPolicy::default();
        restored
            .replace_snapshot([(ep, node.clone(), state, 3)])
            .expect("restore");
        assert_eq!(
            (
                restored.permits(ep, AGENT_ALPN),
                restored.permits(ep, ENROLL_ALPN)
            ),
            (
                state == NodeTrustState::Active,
                state == NodeTrustState::Active
            )
        );
        assert_eq!(
            (restored.revision(ep), restored.retained_state(ep)),
            (3, state)
        );
        let service = test_service_with_policy(8, restored.clone());
        let trust = test_trust_with_options(
            controller_keyring().0,
            vec![(ep, node.clone(), state, 3)],
            None,
        )
        .await;
        let router = test_router(
            ctrl_key.clone(),
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
            .expect("c");
        let res = tokio::time::timeout(
            Duration::from_secs(3),
            client.connect(router.endpoint().addr(), AGENT_ALPN),
        )
        .await
        .expect("t");
        if state == NodeTrustState::Active {
            let conn = res.expect("admit");
            assert_eq!(
                send_handshake(&conn, &hs).await.result,
                i32::from(HandshakeResult::Accepted)
            );
            wait_until_registered(&service, &node).await;
            drop(conn);
        } else {
            if let Ok(conn) = res {
                tokio::time::timeout(Duration::from_secs(3), conn.closed())
                    .await
                    .expect("closed");
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
        shutdown(&service, router).await.expect("shut");
        client.close().await;
    }
}

#[test]
fn identity_policy_reverse_index_rejects_duplicate_nodes_and_updates_atomically() {
    let ep_a = SecretKey::generate().public();
    let ep_b = SecretKey::generate().public();
    let ep_c = SecretKey::generate().public();
    let node_1 = vec![0x11; 16];
    let node_2 = vec![0x22; 16];

    // Snapshot with two active endpoints bound to the same node_id must be rejected atomically.
    let policy = IdentityPolicy::default();
    assert!(
        policy
            .replace_snapshot([
                (ep_a, node_1.clone(), NodeTrustState::Active, 1),
                (ep_b, node_1.clone(), NodeTrustState::Active, 1),
            ])
            .is_err()
    );
    assert!(!policy.permits(ep_a, AGENT_ALPN));
    assert!(!policy.permits(ep_b, AGENT_ALPN));

    // Clean snapshot installs both forward and reverse bindings.
    policy
        .replace_snapshot([(ep_a, node_1.clone(), NodeTrustState::Active, 2)])
        .expect("valid snapshot");
    assert!(policy.permits(ep_a, AGENT_ALPN));

    // A second endpoint trying to claim node_1 while ep_a is still Active is rejected via O(1) index.
    let (disp, prev_rev, retained_rev, retained_state) =
        policy.update_with_status(ep_b, node_1.clone(), NodeTrustState::Active, 3);
    assert_eq!(
        (disp, prev_rev, retained_rev, retained_state),
        (
            PolicyUpdateDisposition::Rejected,
            0,
            0,
            NodeTrustState::Unspecified
        )
    );
    assert!(!policy.permits(ep_b, AGENT_ALPN));

    // Same-revision idempotent update returns Applied; stale revision returns Stale.
    assert_eq!(
        policy.update_with_status(ep_a, node_1.clone(), NodeTrustState::Active, 2),
        (
            PolicyUpdateDisposition::Applied,
            2,
            2,
            NodeTrustState::Active
        )
    );
    assert_eq!(
        policy.update_with_status(ep_a, node_1.clone(), NodeTrustState::Active, 1),
        (PolicyUpdateDisposition::Stale, 2, 2, NodeTrustState::Active)
    );

    // Revoking ep_a atomically evicts node_1 from the reverse index so a replacement endpoint can bind.
    assert_eq!(
        policy.update_with_status(ep_a, node_1.clone(), NodeTrustState::Revoked, 4),
        (
            PolicyUpdateDisposition::Applied,
            2,
            4,
            NodeTrustState::Revoked
        )
    );
    assert!(policy.revoked(ep_a));
    assert!(!policy.permits(ep_a, AGENT_ALPN));

    // Replacement endpoint ep_b can now bind to node_1, while ep_c binds independently to node_2.
    assert_eq!(
        policy.update_with_status(ep_b, node_1.clone(), NodeTrustState::Active, 5),
        (
            PolicyUpdateDisposition::Applied,
            0,
            5,
            NodeTrustState::Active
        )
    );
    assert_eq!(
        policy.update_with_status(ep_c, node_2, NodeTrustState::Active, 1),
        (
            PolicyUpdateDisposition::Applied,
            0,
            1,
            NodeTrustState::Active
        )
    );
    assert!(policy.permits(ep_b, AGENT_ALPN));
    assert!(policy.permits(ep_c, AGENT_ALPN));

    // Once ep_b holds node_1, another endpoint claiming node_1 is rejected again.
    let ep_d = SecretKey::generate().public();
    assert_eq!(
        policy.update_with_status(ep_d, node_1, NodeTrustState::Active, 6),
        (
            PolicyUpdateDisposition::Rejected,
            0,
            0,
            NodeTrustState::Unspecified
        )
    );
}
