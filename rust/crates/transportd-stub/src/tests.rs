use ocservia_contracts::traceparent::generate as new_traceparent;

use super::*;
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    CommandEnvelope, SemanticPayloadHashVersion, SyntheticEcho, SyntheticNoop,
};
use tokio_stream::StreamExt;

fn uid() -> Vec<u8> {
    Uuid::now_v7().as_bytes().to_vec()
}
fn probe(heartbeats: u32, delay_ms: u32) -> SimulationProbe {
    SimulationProbe {
        heartbeat_count: heartbeats,
        delay_millis: delay_ms,
        duplicate_event: false,
        return_error: false,
        disconnect_after: false,
    }
}
fn probe_req(node_id: Vec<u8>, key: Vec<u8>, p: SimulationProbe) -> SendCommandRequest {
    let now = SystemTime::now();
    let env = CommandEnvelope {
        protocol_version: "1.0".into(),
        message_id: uid(),
        command_id: uid(),
        idempotency_key: key,
        node_id: node_id.clone(),
        sequence: 1,
        issued_at: Some(now.into()),
        expires_at: Some((now + Duration::from_mins(1)).into()),
        traceparent: new_traceparent(),
        actor_id: "test".into(),
        reason: "test".into(),
        delivery_mode: CommandDeliveryMode::ExecuteOrReplay.into(),
        payload: Some(command_envelope::Payload::SimulationProbe(p)),
        semantic_payload_hash_version: SemanticPayloadHashVersion::Unspecified as i32,
        ..CommandEnvelope::default()
    };
    SendCommandRequest {
        node_id,
        command_envelope: env.encode_to_vec(),
    }
}
async fn wait_node(svc: &StubService, node: &[u8]) {
    tokio::time::timeout(Duration::from_secs(1), async {
        while !svc.state.nodes.lock().await.contains_key(node) {
            tokio::task::yield_now().await;
        }
    })
    .await
    .expect("connected");
}
fn close_req(node_id: Vec<u8>, reason: String) -> Request<CloseNodeRequest> {
    Request::new(CloseNodeRequest { node_id, reason })
}

#[test]
fn traceparent_queue_cap_and_payload_envelope_bounds() {
    assert!(validate_traceparent(&new_traceparent()).is_ok());
    assert!(validate_traceparent("not-a-trace").is_err());
    assert!(
        validate_traceparent("00-0123456789ABCDEF0123456789abcdef-0123456789abcdef-01").is_err()
    );
    assert_eq!(StubService::new(usize::MAX).state.retention, 4096);
    let ev = new_event(
        &uid(),
        TransportEventType::Disconnected,
        &new_traceparent(),
        vec![0; MAX_COMMAND_BYTES],
    );
    assert!(ev.encoded_len() <= MAX_COMMAND_BYTES + 4 * 1024);
}

#[tokio::test]
async fn heartbeat_expiry_capacity_and_idempotency_invariants() {
    let svc = StubService::new(8);
    svc.send_command(Request::new(probe_req(uid(), uid(), probe(1, 30_000))))
        .await
        .expect("30s");

    // Expired command rejected before acceptance.
    let mut req = probe_req(uid(), uid(), probe(1, 0));
    let mut env = CommandEnvelope::decode(req.command_envelope.as_slice()).expect("env");
    let now = SystemTime::now();
    env.issued_at = Some((now - Duration::from_mins(2)).into());
    env.expires_at = Some((now - Duration::from_mins(1)).into());
    req.command_envelope = env.encode_to_vec();
    let svc_exp = StubService::new(8);
    assert_eq!(
        svc_exp
            .send_command(Request::new(req))
            .await
            .expect_err("exp")
            .code(),
        tonic::Code::DeadlineExceeded
    );
    assert_eq!(svc_exp.state.accepted_commands.lock().await.keys.len(), 0);

    // Capacity mode reports bounds and emits telemetry.
    let cap = StubService::new_capacity(8);
    cap.send_command(Request::new(probe_req(uid(), uid(), probe(1, 0))))
        .await
        .expect("cap");
    tokio::time::timeout(Duration::from_secs(1), async {
        while cap.stats().await.active_tasks != 0 || cap.stats().await.accepted_commands != 1 {
            tokio::task::yield_now().await;
        }
    })
    .await
    .expect("done");
    let retained = cap.state.delivery.lock().await.retained.clone();
    assert!(
        retained
            .iter()
            .any(|e| e.r#type == i32::from(TransportEventType::Telemetry)
                && TelemetryBatch::decode(e.payload.as_slice()).is_ok())
    );
    assert_eq!(cap.stats().await.task_capacity, 8);

    // Repeated idempotency key executes once, and keys are not evicted at capacity.
    let idem = StubService::new(1);
    let (n1, k1) = (uid(), uid());
    for _ in 0..2 {
        assert!(
            idem.send_command(Request::new(probe_req(n1.clone(), k1.clone(), probe(1, 0))))
                .await
                .expect("ok")
                .into_inner()
                .accepted
        );
    }
    tokio::time::timeout(Duration::from_secs(1), async {
        while idem.state.tasks.available_permits() == 0 {
            tokio::task::yield_now().await;
        }
    })
    .await
    .expect("done");
    assert_eq!(
        idem.send_command(Request::new(probe_req(uid(), uid(), probe(1, 0))))
            .await
            .expect_err("cap")
            .code(),
        tonic::Code::ResourceExhausted
    );
    idem.send_command(Request::new(probe_req(n1, k1.clone(), probe(1, 0))))
        .await
        .expect("still idempotent");
    assert!(idem.state.accepted_commands.lock().await.keys.contains(&k1));
}

#[tokio::test]
async fn reconcile_only_observes_without_accepting_or_running_commands() {
    for expired in [true, false] {
        for payload in [
            command_envelope::Payload::SyntheticNoop(SyntheticNoop {}),
            command_envelope::Payload::SyntheticEcho(SyntheticEcho {
                message: "test".into(),
            }),
            command_envelope::Payload::SimulationProbe(probe(1, 0)),
        ] {
            let svc = StubService::new(8);
            let mut exec = probe_req(uid(), uid(), probe(1, 0));
            let mut env = CommandEnvelope::decode(exec.command_envelope.as_slice()).expect("env");
            env.payload = Some(payload);
            exec.command_envelope = env.encode_to_vec();
            if expired {
                let now = SystemTime::now();
                env.issued_at = Some((now - Duration::from_mins(2)).into());
                env.expires_at = Some((now - Duration::from_mins(1)).into());
            }
            env.delivery_mode = CommandDeliveryMode::ReconcileOnly.into();
            let mut rec = exec.clone();
            rec.command_envelope = env.encode_to_vec();
            assert!(
                svc.send_command(Request::new(rec.clone()))
                    .await
                    .expect("obs")
                    .into_inner()
                    .accepted
            );
            tokio::task::yield_now().await;
            let st = svc.stats().await;
            assert_eq!(
                (
                    st.accepted_commands,
                    st.active_tasks,
                    st.retained_events,
                    st.connected_nodes
                ),
                (0, 0, 0, 0)
            );

            assert!(
                svc.send_command(Request::new(exec))
                    .await
                    .expect("exec")
                    .into_inner()
                    .accepted
            );
            tokio::time::timeout(Duration::from_secs(1), async {
                while svc.stats().await.active_tasks != 0 {
                    tokio::task::yield_now().await;
                }
            })
            .await
            .expect("done");
            let before = svc.stats().await;
            assert_eq!(before.accepted_commands, 1);
            assert!(
                svc.send_command(Request::new(rec))
                    .await
                    .expect("obs2")
                    .into_inner()
                    .accepted
            );
            tokio::task::yield_now().await;
            assert_eq!(svc.stats().await, before);
        }
    }
}

#[tokio::test]
async fn close_node_cancellation_validation_and_subscriber_backpressure() {
    // Unknown node close fails NotFound without publishing.
    let svc = StubService::new(8);
    assert_eq!(
        svc.close_node(close_req(uid(), "test".into()))
            .await
            .expect_err("nf")
            .code(),
        tonic::Code::NotFound
    );
    assert_eq!(svc.state.delivery.lock().await.retained.len(), 0);

    // Oversized close reason preserves connection; valid close cancels in-flight probe with traceparent.
    let node = uid();
    let req = probe_req(node.clone(), uid(), probe(2, 250));
    let tp = CommandEnvelope::decode(req.command_envelope.as_slice())
        .expect("env")
        .traceparent;
    svc.send_command(Request::new(req)).await.expect("ok");
    wait_node(&svc, &node).await;
    assert_eq!(
        svc.close_node(close_req(node.clone(), "x".repeat(MAX_COMMAND_BYTES + 1)))
            .await
            .expect_err("big")
            .code(),
        tonic::Code::ResourceExhausted
    );
    assert!(svc.state.nodes.lock().await.contains_key(&node));
    svc.close_node(close_req(node, "operator close".into()))
        .await
        .expect("closed");
    tokio::time::sleep(Duration::from_millis(300)).await;
    let evs = svc.state.delivery.lock().await.retained.clone();
    assert_eq!(
        evs.iter()
            .filter(|e| e.r#type == i32::from(TransportEventType::Disconnected))
            .count(),
        1
    );
    assert!(
        evs.iter()
            .all(|e| e.r#type == i32::from(TransportEventType::Connected)
                || e.r#type == i32::from(TransportEventType::Disconnected))
    );
    assert_eq!(
        evs.iter()
            .find(|e| e.r#type == i32::from(TransportEventType::Disconnected))
            .unwrap()
            .traceparent,
        tp
    );

    // Slow subscriber does not block node close, and closed subscribers are reaped.
    let s1 = StubService::new(1);
    let first = s1
        .watch_events(Request::new(WatchEventsRequest::default()))
        .await
        .expect("w");
    drop(first);
    let _slow = s1
        .watch_events(Request::new(WatchEventsRequest::default()))
        .await
        .expect("w2")
        .into_inner();
    let n2 = uid();
    s1.send_command(Request::new(probe_req(n2.clone(), uid(), probe(2, 0))))
        .await
        .expect("ok");
    wait_node(&s1, &n2).await;
    tokio::time::timeout(
        Duration::from_secs(1),
        s1.close_node(close_req(n2, "close".into())),
    )
    .await
    .expect("fast")
    .expect("ok");

    // Slow subscriber applies publication backpressure.
    let bp = StubService::new(1);
    let nb = uid();
    let mut stream = bp
        .watch_events(Request::new(WatchEventsRequest::default()))
        .await
        .expect("w")
        .into_inner();
    bp.publish(new_event(
        &nb,
        TransportEventType::Connected,
        &new_traceparent(),
        Vec::new(),
    ))
    .await;
    let pub_task = tokio::spawn({
        let (bp, nb) = (bp.clone(), nb.clone());
        async move {
            bp.publish(new_event(
                &nb,
                TransportEventType::Heartbeat,
                &new_traceparent(),
                Vec::new(),
            ))
            .await;
        }
    });
    tokio::task::yield_now().await;
    assert!(!pub_task.is_finished());
    assert!(stream.next().await.is_some());
    tokio::time::timeout(Duration::from_secs(1), pub_task)
        .await
        .expect("unblocked")
        .expect("join");

    // Cancellation disconnects skipped subscribers during fanout.
    let fan = StubService::new(2);
    let nf = uid();
    let generation = Uuid::now_v7();
    let (cancellation, _) = watch::channel(false);
    fan.state.nodes.lock().await.insert(
        nf.clone(),
        ActiveNode {
            connection: NodeConnection {
                node_id: nf.clone(),
                endpoint_id: uid(),
                path: ConnectionPath::Direct.into(),
                connected_at: Some(now_timestamp()),
                agent_instance_id: uid(),
                path_detail: "stub/direct".into(),
                last_seen: Some(now_timestamp()),
                ..NodeConnection::default()
            },
            generation,
            cancellation: cancellation.clone(),
            traceparent: new_traceparent(),
        },
    );
    let _slow2 = fan
        .watch_events(Request::new(WatchEventsRequest::default()))
        .await
        .expect("s")
        .into_inner();
    let mut healthy = fan
        .watch_events(Request::new(WatchEventsRequest::default()))
        .await
        .expect("h")
        .into_inner();
    for t in [TransportEventType::Connected, TransportEventType::Heartbeat] {
        fan.publish(new_event(&nf, t, &new_traceparent(), Vec::new()))
            .await;
    }
    assert!(healthy.next().await.is_some() && healthy.next().await.is_some());
    let p2 = tokio::spawn({
        let (fan, nf) = (fan.clone(), nf.clone());
        async move {
            fan.publish_if_active(
                &nf,
                generation,
                new_event(
                    &nf,
                    TransportEventType::CommandResult,
                    &new_traceparent(),
                    Vec::new(),
                ),
            )
            .await
        }
    });
    tokio::task::yield_now().await;
    assert!(!p2.is_finished());
    cancellation.send_replace(true);
    assert!(
        !tokio::time::timeout(Duration::from_secs(1), p2)
            .await
            .expect("t")
            .expect("j")
    );
    assert_eq!(fan.state.delivery.lock().await.subscribers.len(), 0);
    assert!(healthy.next().await.is_none());
}
