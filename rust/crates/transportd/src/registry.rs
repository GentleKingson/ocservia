use std::collections::HashMap;
use std::sync::{Arc, Weak};

use iroh::endpoint::{Connection, VarInt};
use iroh::protocol::AcceptError;
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    ConnectionFenceV2, FenceBindingV2, FenceOperationKind,
};
use ocservia_contracts::generated::ocserv::platform::transport::v1::{
    NodeConnection, NodeTrustState, OwnerFenceDisposition, RegisterOwnerFenceResponse,
    TransportEventType,
};
use tokio::sync::{Notify, OwnedSemaphorePermit};
use tonic::Status;

use crate::command_stream::event_with_term;
use crate::fence::{
    FenceRegistration, RegisteredFence, fence_registration_decision, fixed16, fixed32,
    lease_expired_at, unix_now_parts, validate_uuid,
};
use crate::monitors::{NegotiatedSession, protocol_error};
use crate::trust::IdentityPolicy;
use crate::{RegisteredConnection, Shared};

impl Shared {
    pub(super) async fn retire_prior_authorization(
        &self,
        node_id: &[u8],
        revision: u64,
        state: NodeTrustState,
    ) {
        let mut registry = self.inner.connections.lock().await;
        if registry
            .get(node_id)
            .is_some_and(|entry| entry.authorization_revision < revision)
        {
            let reason = if state == NodeTrustState::Revoked {
                b"node revoked".as_slice()
            } else {
                b"node authorization changed".as_slice()
            };
            self.remove_locked(&mut registry, node_id, reason).await;
        }
    }

    pub(super) async fn registered_fence(&self, node_id: &[u8]) -> Option<Arc<RegisteredFence>> {
        self.inner.fences.lock().await.get(node_id).map(Arc::clone)
    }

    pub(super) async fn remove(
        &self,
        node_id: &[u8],
        reason: &[u8],
    ) -> Option<RegisteredConnection> {
        let mut registry = self.inner.connections.lock().await;
        self.remove_locked(&mut registry, node_id, reason).await
    }

    async fn remove_locked(
        &self,
        registry: &mut HashMap<Vec<u8>, RegisteredConnection>,
        node_id: &[u8],
        reason: &[u8],
    ) -> Option<RegisteredConnection> {
        // Invalidate any live trust decision made before this close attempt,
        // including when the requested node has not registered yet.
        self.inner.registration_tokens.lock().await.remove(node_id);
        let registered = registry.remove(node_id);
        if let Some(registered) = &registered {
            registered.lease_notify.notify_one();
            registered.connection.close(VarInt::from_u32(0x104), reason);
            self.publish(event_with_term(
                node_id,
                &registered.metadata.endpoint_id,
                TransportEventType::Disconnected,
                reason.to_vec(),
                &registered.fence,
            ))
            .await;
        }
        registered
    }

    pub(super) async fn registration_token(&self, node_id: &[u8]) -> Arc<()> {
        let mut tokens = self.inner.registration_tokens.lock().await;
        tokens.retain(|_, token| token.strong_count() > 0);
        if let Some(token) = tokens.get(node_id).and_then(Weak::upgrade) {
            return token;
        }
        let token = Arc::new(());
        tokens.insert(node_id.to_vec(), Arc::downgrade(&token));
        token
    }

    pub(super) async fn register_session(
        &self,
        policy: &IdentityPolicy,
        connection: &Connection,
        metadata: NodeConnection,
        session: NegotiatedSession,
        registration_token: &Arc<()>,
    ) -> Result<(), AcceptError> {
        let node_id = metadata.node_id.clone();
        let mut registry = self.inner.connections.lock().await;
        // Trust updates change policy before taking the registry lock. Checking
        // both local policy and this node's close token makes a concurrent revoke
        // either reject this session here or close it after registration.
        if policy.revoked(connection.remote_id())
            || !Arc::ptr_eq(&self.registration_token(&node_id).await, registration_token)
        {
            connection.close(VarInt::from_u32(0x101), b"session revoked");
            return Err(protocol_error("endpoint was revoked during handshake"));
        }
        {
            let mut fences = self.inner.fences.lock().await;
            match fence_registration_decision(fences.get(&node_id).map(Arc::as_ref), &session.fence)
            {
                FenceRegistration::Stale => {
                    drop(fences);
                    drop(registry);
                    connection.close(VarInt::from_u32(0x109), b"stale owner epoch");
                    return Err(protocol_error(
                        "session fence epoch is lower than the registered owner epoch",
                    ));
                }
                FenceRegistration::EpochCollision => {
                    drop(fences);
                    drop(registry);
                    connection.close(VarInt::from_u32(0x10a), b"owner epoch reused");
                    return Err(protocol_error(
                        "two distinct fences claim the same owner epoch",
                    ));
                }
                FenceRegistration::Apply | FenceRegistration::Refresh => {
                    fences.insert(node_id.clone(), Arc::clone(&session.fence));
                }
            }
        }
        let replaced = registry.insert(
            node_id.clone(),
            RegisteredConnection {
                metadata: metadata.clone(),
                connection: connection.clone(),
                max_message_size: session.handshake.max_message_size as usize,
                negotiated_capabilities: session.negotiated_capabilities,
                authorization_revision: session.authorization_revision,
                session_expires_at: session.session_expires_at,
                fence: Arc::clone(&session.fence),
                lease_notify: Arc::new(Notify::new()),
            },
        );
        if let Some(previous) = replaced {
            previous.lease_notify.notify_one();
            previous
                .connection
                .close(VarInt::from_u32(0x106), b"superseded connection");
            self.publish(event_with_term(
                &node_id,
                &previous.metadata.endpoint_id,
                TransportEventType::Disconnected,
                b"connection superseded".to_vec(),
                &previous.fence,
            ))
            .await;
        }
        self.publish(event_with_term(
            &node_id,
            &metadata.endpoint_id,
            TransportEventType::Connected,
            metadata.path_detail.as_bytes().to_vec(),
            &session.fence,
        ))
        .await;
        drop(registry);

        Ok(())
    }

    pub(super) async fn artifact_connection(
        &self,
        node_id: &[u8],
        binding: Option<&FenceBindingV2>,
        operation_id: &[u8],
    ) -> Result<(Connection, Option<ConnectionFenceV2>), Status> {
        let (connection, endpoint_bytes, session_fence) = self
            .inner
            .connections
            .lock()
            .await
            .get(node_id)
            .map(|entry| {
                (
                    entry.connection.clone(),
                    entry.metadata.endpoint_id.clone(),
                    Arc::clone(&entry.fence),
                )
            })
            .ok_or_else(|| Status::unavailable("node is not connected"))?;
        let dispatch_fence = self
            .verify_operation_binding(
                node_id,
                Some(&fixed32(&endpoint_bytes)?),
                Some(&session_fence),
                binding,
                FenceOperationKind::Artifact,
                operation_id,
            )
            .await?;
        Ok((
            connection,
            dispatch_fence.map(|registered| registered.fence.clone()),
        ))
    }

    pub(super) async fn register_fence(
        &self,
        fence: ConnectionFenceV2,
    ) -> Result<RegisterOwnerFenceResponse, Status> {
        let node_id = validate_uuid(&fence.node_id, "node_id")?;
        let endpoint_id = fixed32(&fence.endpoint_id)?;
        let keyring = self.keyring();
        let (now_seconds, now_nanos) = unix_now_parts();
        let verified = keyring
            .verify_connection_fence_v2_at(
                &fence,
                &fixed16(&node_id)?,
                &endpoint_id,
                now_seconds,
                now_nanos,
            )
            .map_err(|_| Status::permission_denied("owner fence is invalid"))?;
        if lease_expired_at(verified.lease_until_seconds, verified.lease_until_nanos) {
            return Err(Status::failed_precondition("owner fence lease has expired"));
        }
        // A live registered session pins the endpoint this fence must claim,
        // so a fence cannot be planted for a node owned by another endpoint.
        let mut connections = self.inner.connections.lock().await;
        if connections
            .get(&node_id)
            .is_some_and(|entry| entry.metadata.endpoint_id.as_slice() != endpoint_id.as_slice())
        {
            return Err(Status::permission_denied(
                "owner fence endpoint does not match the registered session",
            ));
        }
        let retained_epoch = verified.owner_epoch;
        let registered = Arc::new(RegisteredFence {
            verified,
            fence,
            endpoint_id,
        });
        let disposition = {
            let mut fences = self.inner.fences.lock().await;
            if let Some(existing) = fences.get(&node_id)
                && existing.verified.owner_epoch > registered.verified.owner_epoch
            {
                let retained_epoch = existing.verified.owner_epoch;
                return Ok(RegisterOwnerFenceResponse {
                    disposition: OwnerFenceDisposition::Stale.into(),
                    retained_epoch,
                });
            }
            match fence_registration_decision(fences.get(&node_id).map(Arc::as_ref), &registered) {
                FenceRegistration::Stale => OwnerFenceDisposition::Stale,
                FenceRegistration::EpochCollision => {
                    return Err(Status::invalid_argument(
                        "owner epoch is already claimed by a different fence",
                    ));
                }
                FenceRegistration::Refresh => {
                    fences.insert(node_id.clone(), Arc::clone(&registered));
                    OwnerFenceDisposition::Refreshed
                }
                FenceRegistration::Apply => {
                    fences.insert(node_id.clone(), Arc::clone(&registered));
                    OwnerFenceDisposition::Applied
                }
            }
        };
        if disposition == OwnerFenceDisposition::Refreshed
            && let Some(entry) = connections.get(&node_id)
        {
            entry.lease_notify.notify_one();
        }
        if disposition == OwnerFenceDisposition::Applied {
            if connections
                .get(&node_id)
                .is_some_and(|entry| entry.fence.verified.fence_id != registered.verified.fence_id)
            {
                self.remove_locked(&mut connections, &node_id, b"owner fence superseded")
                    .await;
            } else if let Some(entry) = connections.get(&node_id) {
                entry.lease_notify.notify_one();
            }
        }
        Ok(RegisterOwnerFenceResponse {
            disposition: disposition.into(),
            retained_epoch,
        })
    }
}

pub(super) async fn finish_session(
    shared: &Shared,
    node_id: &[u8],
    connection: &Connection,
    monitors: (
        tokio::task::JoinHandle<()>,
        tokio::task::JoinHandle<()>,
        tokio::task::JoinHandle<()>,
    ),
    _permit: OwnedSemaphorePermit,
) {
    monitors.0.abort();
    monitors.1.abort();
    monitors.2.abort();
    let mut registry = shared.inner.connections.lock().await;
    let finishing = registry
        .get(node_id)
        .filter(|entry| entry.connection.stable_id() == connection.stable_id())
        .map(|entry| Arc::clone(&entry.fence));
    if let Some(fence) = finishing {
        registry.remove(node_id);
        shared
            .publish(event_with_term(
                node_id,
                connection.remote_id().as_bytes(),
                TransportEventType::Disconnected,
                b"connection closed".to_vec(),
                &fence,
            ))
            .await;
    }
}
