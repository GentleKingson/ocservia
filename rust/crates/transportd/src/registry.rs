use std::collections::HashMap;
use std::sync::{Arc, Weak};
use std::time::SystemTime;

use iroh::endpoint::{Connection, VarInt};
use iroh::protocol::AcceptError;
use ocservia_contracts::generated::ocserv::platform::transport::v1::{
    NodeConnection, NodeTrustState, TransportEventType,
};
use tokio::sync::OwnedSemaphorePermit;
use tonic::Status;

use crate::command_stream::event_with_session;
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
        self.inner.registration_tokens.lock().await.remove(node_id);
        let registered = registry.remove(node_id);
        if let Some(registered) = &registered {
            registered.connection.close(VarInt::from_u32(0x104), reason);
            self.publish(event_with_session(
                node_id,
                &registered.metadata.endpoint_id,
                TransportEventType::Disconnected,
                reason.to_vec(),
                &registered.connection_id,
                registered.session_epoch,
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
        if policy.revoked(connection.remote_id())
            || !Arc::ptr_eq(&self.registration_token(&node_id).await, registration_token)
        {
            connection.close(VarInt::from_u32(0x101), b"session revoked");
            return Err(protocol_error("endpoint was revoked during handshake"));
        }
        {
            let mut epochs = self.inner.session_epochs.lock().await;
            if epochs
                .get(&node_id)
                .is_some_and(|&current| session.session_epoch <= current)
            {
                drop(epochs);
                drop(registry);
                connection.close(VarInt::from_u32(0x109), b"stale session epoch");
                return Err(protocol_error(
                    "session epoch is not newer than the registered epoch",
                ));
            }
            epochs.insert(node_id.clone(), session.session_epoch);
        }
        let connection_id = session.connection_id;
        let session_epoch = session.session_epoch;
        let replaced = registry.insert(
            node_id.clone(),
            RegisteredConnection {
                metadata: metadata.clone(),
                connection: connection.clone(),
                max_message_size: session.handshake.max_message_size as usize,
                negotiated_capabilities: session.negotiated_capabilities,
                authorization_revision: session.authorization_revision,
                session_epoch,
                connection_id,
                session_expires_at: session.session_expires_at,
            },
        );
        if let Some(previous) = replaced {
            previous
                .connection
                .close(VarInt::from_u32(0x106), b"superseded connection");
            self.publish(event_with_session(
                &node_id,
                &previous.metadata.endpoint_id,
                TransportEventType::Disconnected,
                b"connection superseded".to_vec(),
                &previous.connection_id,
                previous.session_epoch,
            ))
            .await;
        }
        self.publish(event_with_session(
            &node_id,
            &metadata.endpoint_id,
            TransportEventType::Connected,
            metadata.path_detail.as_bytes().to_vec(),
            &connection_id,
            session_epoch,
        ))
        .await;
        drop(registry);

        Ok(())
    }

    pub(super) async fn artifact_connection(&self, node_id: &[u8]) -> Result<Connection, Status> {
        let registry = self.inner.connections.lock().await;
        let entry = registry
            .get(node_id)
            .ok_or_else(|| Status::unavailable("node is not connected"))?;
        if entry.session_expires_at <= SystemTime::now() {
            return Err(Status::failed_precondition(
                "Controller session grant has expired",
            ));
        }
        Ok(entry.connection.clone())
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
        tokio::task::JoinHandle<()>,
    ),
    _permit: OwnedSemaphorePermit,
) {
    monitors.0.abort();
    monitors.1.abort();
    monitors.2.abort();
    monitors.3.abort();
    let mut registry = shared.inner.connections.lock().await;
    let finishing = registry
        .get(node_id)
        .filter(|entry| entry.connection.stable_id() == connection.stable_id())
        .map(|entry| (entry.connection_id, entry.session_epoch));
    if let Some((connection_id, session_epoch)) = finishing {
        registry.remove(node_id);
        tracing::info!(
            event_type = "agent_session_disconnected",
            node_id = %hex::encode(node_id),
            connection_id = %hex::encode(connection_id),
            session_epoch,
            "Agent session closed"
        );
        shared
            .publish(event_with_session(
                node_id,
                connection.remote_id().as_bytes(),
                TransportEventType::Disconnected,
                b"connection closed".to_vec(),
                &connection_id,
                session_epoch,
            ))
            .await;
    }
}
