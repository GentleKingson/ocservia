#![allow(
    clippy::many_single_char_names,
    clippy::similar_names,
    clippy::too_many_lines
)]

use ed25519_dalek::SigningKey;
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    CommandAuthorizationProof, CommandDeliveryMode, PrivdResultReceiptV1, PrivilegedCommandKind,
    PrivilegedResultKind, SealingKeyDescriptorV1, SemanticPayloadHashVersion, ServiceReload,
};
use ocservia_contracts::generated::ocserv::platform::transport::v1::{
    AuthorizeSessionRequest, CheckEndpointRequest, CheckEndpointResponse, ListNodeTrustRequest,
    ListNodeTrustResponse, StoreVpnAuditRequest, ValidateEnrollmentRequest,
    ValidateEnrollmentResponse,
    trust_service_server::{TrustService, TrustServiceServer},
};
use ocservia_privd_attestation::{key_id, result_payload_cbor_hash, sign_receipt, verify_receipt};
use ocservia_test_support::{keyring, sign_session_grant, unix_seconds};
use tokio::net::TcpListener;
use tokio_stream::{StreamExt as _, wrappers::TcpListenerStream};

use super::*;

struct TestTrust {
    signing_key: SigningKey,
    epoch: u64,
}

#[tonic::async_trait]
impl TrustService for TestTrust {
    async fn store_vpn_audit(
        &self,
        _: Request<StoreVpnAuditRequest>,
    ) -> Result<
        Response<ocservia_contracts::generated::ocserv::platform::agent::v1::VpnAuditReceipt>,
        Status,
    > {
        Err(Status::unimplemented(""))
    }
    async fn validate_enrollment(
        &self,
        _: Request<ValidateEnrollmentRequest>,
    ) -> Result<Response<ValidateEnrollmentResponse>, Status> {
        Ok(Response::new(ValidateEnrollmentResponse {
            permitted: true,
        }))
    }
    async fn check_endpoint(
        &self,
        _: Request<CheckEndpointRequest>,
    ) -> Result<Response<CheckEndpointResponse>, Status> {
        Ok(Response::new(CheckEndpointResponse { permitted: true }))
    }
    async fn authorize_session(
        &self,
        req: Request<AuthorizeSessionRequest>,
    ) -> Result<Response<SessionHandshakeResponse>, Status> {
        let r = req.into_inner();
        let hs = r.handshake.ok_or_else(|| Status::invalid_argument("hs"))?;
        let now = unix_seconds();
        let grant = sign_session_grant(
            &self.signing_key,
            hs.node_id.as_slice().try_into().unwrap(),
            r.remote_endpoint_id.as_slice().try_into().unwrap(),
            1,
            self.epoch,
            hs.capabilities.clone(),
            now - 1,
            now + 301,
        );
        Ok(Response::new(SessionHandshakeResponse {
            result: HandshakeResult::Accepted.into(),
            protocol_major: PROTOCOL_MAJOR,
            protocol_minor: PROTOCOL_MINOR,
            max_message_size: hs.max_message_size,
            controller_version: "test".into(),
            negotiated_capabilities: hs.capabilities,
            session_grant: Some(grant),
        }))
    }
    async fn enroll(&self, _: Request<EnrollRequest>) -> Result<Response<EnrollResponse>, Status> {
        Ok(Response::new(EnrollResponse::default()))
    }
    async fn list_node_trust(
        &self,
        _: Request<ListNodeTrustRequest>,
    ) -> Result<Response<ListNodeTrustResponse>, Status> {
        Ok(Response::new(ListNodeTrustResponse::default()))
    }
}

async fn setup_router(
    agent_key: &SecretKey,
    signing_key: &SigningKey,
    node_id: &[u8],
    epoch: u64,
    svc: &IrohTransportService,
) -> Router {
    let listener = TcpListener::bind("127.0.0.1:0").await.expect("bind");
    let addr = listener.local_addr().expect("addr");
    tokio::spawn(
        tonic::transport::Server::builder()
            .add_service(TrustServiceServer::from_arc(Arc::new(TestTrust {
                signing_key: signing_key.clone(),
                epoch,
            })))
            .serve_with_incoming(TcpListenerStream::new(listener)),
    );
    let trust = TrustAuthority::new(
        tonic::transport::Endpoint::from_shared(format!("http://{addr}"))
            .expect("ep")
            .connect()
            .await
            .expect("conn"),
    );
    let policy = IdentityPolicy::new(
        HashMap::from([(agent_key.public(), node_id.to_vec())]),
        HashSet::new(),
    );
    build_router_with_trust_tls_roots(
        SecretKey::generate(),
        RelayMode::Disabled,
        policy,
        trust,
        Vec::new(),
        svc,
    )
    .await
    .expect("router")
}

async fn write_frame(send: &mut iroh::endpoint::SendStream, bytes: &[u8]) {
    send.write_all(&u32::try_from(bytes.len()).expect("u32").to_be_bytes())
        .await
        .expect("len");
    send.write_all(bytes).await.expect("body");
}
async fn read_frame(recv: &mut iroh::endpoint::RecvStream) -> Vec<u8> {
    let mut len = [0_u8; 4];
    recv.read_exact(&mut len).await.expect("len");
    let mut body = vec![0_u8; u32::from_be_bytes(len) as usize];
    recv.read_exact(&mut body).await.expect("body");
    body
}
async fn send_hs(conn: &Connection, hs: &SessionHandshake) -> SessionHandshakeResponse {
    let (mut send, mut recv) = conn.open_bi().await.expect("hs");
    write_frame(&mut send, &hs.encode_to_vec()).await;
    send.finish().expect("finish");
    SessionHandshakeResponse::decode(read_frame(&mut recv).await.as_slice()).expect("decode")
}
async fn next_ev(events: &mut EventStream, kind: TransportEventType) -> TransportEvent {
    tokio::time::timeout(Duration::from_secs(2), async {
        loop {
            let ev = events.next().await.expect("open").expect("valid");
            if ev.r#type == i32::from(kind) {
                break ev;
            }
        }
    })
    .await
    .expect("event")
}

#[tokio::test]
async fn trust_update_reports_stale_and_rejects_tombstone_reactivation() {
    let key = SigningKey::from_bytes(&[7; 32]);
    let svc = IrohTransportService::new(8, IdentityPolicy::default(), Arc::new(keyring(&key)));
    let (ep, node_id) = (SecretKey::generate().public(), *Uuid::now_v7().as_bytes());
    let apply = |state: NodeTrustState, revision: u64| {
        svc.update_node_trust(Request::new(UpdateNodeTrustRequest {
            node_id: node_id.to_vec(),
            endpoint_id: ep.as_bytes().to_vec(),
            state: state.into(),
            reason: "trust".into(),
            revision,
        }))
    };
    assert_eq!(
        apply(NodeTrustState::Active, 2)
            .await
            .expect("u")
            .into_inner()
            .disposition,
        i32::from(TrustUpdateDisposition::Applied)
    );
    assert_eq!(
        apply(NodeTrustState::Active, 1)
            .await
            .expect("u")
            .into_inner()
            .disposition,
        i32::from(TrustUpdateDisposition::Stale)
    );
    assert_eq!(
        apply(NodeTrustState::Revoked, 3)
            .await
            .expect("u")
            .into_inner()
            .disposition,
        i32::from(TrustUpdateDisposition::Applied)
    );
    let react = apply(NodeTrustState::Active, 4)
        .await
        .expect("u")
        .into_inner();
    assert_eq!(
        (react.disposition, react.retained_state),
        (
            i32::from(TrustUpdateDisposition::Rejected),
            i32::from(NodeTrustState::Revoked)
        )
    );
}

#[tokio::test]
async fn session_epoch_fencing_and_attested_command_responses() {
    let key = SigningKey::from_bytes(&[7; 32]);
    let (agent_key, node_id) = (SecretKey::generate(), Uuid::now_v7().as_bytes().to_vec());
    let hs = SessionHandshake {
        protocol_major: PROTOCOL_MAJOR,
        protocol_minor: PROTOCOL_MINOR,
        agent_version: "test".into(),
        ocserv_version: "1.3".into(),
        os_release: "test".into(),
        boot_id: "boot".into(),
        agent_instance_id: Uuid::now_v7().as_bytes().to_vec(),
        node_id: node_id.clone(),
        endpoint_id: agent_key.public().as_bytes().to_vec(),
        capabilities: vec!["ocserv.status.read".into()],
        max_message_size: 2048,
        time: Some(now_timestamp()),
        nonce: vec![7; 32],
        sealing_keys: [1, 2]
            .map(|p| SealingKeyDescriptorV1 {
                version: 1,
                purpose: p,
                key_id: format!("k{p}"),
                public_key_sha256: vec![0x11; 32],
            })
            .into(),
        ..SessionHandshake::default()
    };
    let svc = IrohTransportService::new(16, IdentityPolicy::default(), Arc::new(keyring(&key)));
    let mut events = svc
        .watch_events(Request::new(WatchEventsRequest {
            after_event_id: Vec::new(),
        }))
        .await
        .expect("watch")
        .into_inner();

    let r1 = setup_router(&agent_key, &key, &node_id, 5, &svc).await;
    let c1 = Endpoint::builder(presets::Minimal)
        .secret_key(agent_key.clone())
        .relay_mode(RelayMode::Disabled)
        .bind()
        .await
        .expect("bind");
    let conn1 = c1
        .connect(r1.endpoint().addr(), AGENT_ALPN)
        .await
        .expect("conn1");
    assert_eq!(
        send_hs(&conn1, &hs).await.result,
        i32::from(HandshakeResult::Accepted)
    );
    let _ = next_ev(&mut events, TransportEventType::Connected).await;

    let r_stale = setup_router(&agent_key, &key, &node_id, 4, &svc).await;
    let stale_conn = c1
        .connect(r_stale.endpoint().addr(), AGENT_ALPN)
        .await
        .expect("conn");
    let (mut send, _) = stale_conn.open_bi().await.expect("open");
    write_frame(&mut send, &hs.encode_to_vec()).await;
    send.finish().expect("finish");
    tokio::time::timeout(Duration::from_secs(3), stale_conn.closed())
        .await
        .expect("stale closes");
    let _ = r_stale.shutdown().await;

    let r2 = setup_router(&agent_key, &key, &node_id, 6, &svc).await;
    let conn2 = c1
        .connect(r2.endpoint().addr(), AGENT_ALPN)
        .await
        .expect("conn2");
    assert_eq!(
        send_hs(&conn2, &hs).await.result,
        i32::from(HandshakeResult::Accepted)
    );
    let _ = next_ev(&mut events, TransportEventType::Connected).await;

    let valid_cmd = CommandEnvelope {
        protocol_version: "1.2".into(),
        message_id: Uuid::now_v7().as_bytes().to_vec(),
        command_id: Uuid::now_v7().as_bytes().to_vec(),
        idempotency_key: Uuid::now_v7().as_bytes().to_vec(),
        node_id: node_id.clone(),
        sequence: 1,
        issued_at: Some(now_timestamp()),
        expires_at: Some((SystemTime::now() + Duration::from_secs(30)).into()),
        expected_revision: 1,
        traceparent: new_traceparent(),
        actor_id: "test".into(),
        required_capability: "ocserv.status.read".into(),
        authorization: Some(CommandAuthorizationProof {
            version: 1,
            key_id: "k".into(),
            signature: vec![0xa5; 64],
        }),
        delivery_mode: CommandDeliveryMode::ExecuteOrReplay.into(),
        payload: Some(command_envelope::Payload::ServiceReload(ServiceReload {})),
        semantic_payload_hash_version: SemanticPayloadHashVersion::V2 as i32,
        semantic_payload_sha256: vec![0x11; 32],
        operation_id: Uuid::now_v7().as_bytes().to_vec(),
        ..CommandEnvelope::default()
    };
    let agent = tokio::spawn({
        let conn = conn2.clone();
        async move {
            let (mut send, mut recv) = conn.accept_bi().await.expect("cmd");
            let dec = CommandEnvelope::decode(read_frame(&mut recv).await.as_slice()).expect("dec");
            let (now, ak) = (now_timestamp(), SigningKey::from_bytes(&[7; 32]));
            let proof = sign_receipt(
                PrivdResultReceiptV1 {
                    receipt_version: 1,
                    node_id: dec.node_id.clone(),
                    privd_attestation_key_id: key_id(&ak.verifying_key()),
                    command_id: dec.command_id.clone(),
                    operation_id: dec.operation_id.clone(),
                    idempotency_key: dec.idempotency_key.clone(),
                    semantic_payload_hash_version: dec.semantic_payload_hash_version,
                    semantic_payload_sha256: dec.semantic_payload_sha256.clone(),
                    command_kind: PrivilegedCommandKind::ServiceReload.into(),
                    result_kind: PrivilegedResultKind::Mutation.into(),
                    terminal_state: CommandResultState::Succeeded.into(),
                    result_cbor_sha256: result_payload_cbor_hash(
                        PrivilegedResultKind::Mutation,
                        &[],
                    )
                    .expect("cbor")
                    .to_vec(),
                    effect_sequence: 1,
                    accepted_at: Some(now),
                    completed_at: Some(now),
                    ..PrivdResultReceiptV1::default()
                },
                &ak,
            )
            .expect("sign");
            let res = CommandResult {
                command_id: dec.command_id,
                idempotency_key: dec.idempotency_key,
                payload_sha256: dec.semantic_payload_sha256,
                state: CommandResultState::Succeeded.into(),
                accepted_at: Some(now),
                completed_at: Some(now),
                semantic_payload_hash_version: dec.semantic_payload_hash_version,
                privileged_result_proof: Some(proof),
                ..CommandResult::default()
            };
            write_frame(
                &mut send,
                &AgentEvent {
                    r#type: AgentEventType::CommandResult.into(),
                    payload: res.encode_to_vec(),
                }
                .encode_to_vec(),
            )
            .await;
            send.finish().expect("finish");
        }
    });
    svc.send_command(Request::new(SendCommandRequest {
        node_id,
        command_envelope: valid_cmd.encode_to_vec(),
    }))
    .await
    .expect("send");
    agent.await.expect("agent");
    let ev = next_ev(&mut events, TransportEventType::CommandResult).await;
    verify_receipt(
        CommandResult::decode(ev.payload.as_slice())
            .expect("res")
            .privileged_result_proof
            .as_ref()
            .expect("proof"),
        &key.verifying_key(),
    )
    .expect("verify");
    let _ = (r1.shutdown().await, shutdown(&svc, r2).await);
    c1.close().await;
}
