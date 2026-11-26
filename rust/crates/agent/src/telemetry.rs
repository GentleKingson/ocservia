use crate::HANDSHAKE_TIMEOUT;
use crate::session::SessionContext;
use crate::startup::invalid;
use ocservia_agent_protocol::{
    MAX_MANAGED_RESOURCES, PrivdResponse, UpgradeOperationResult, privd_response,
};
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    AgentUpgradeOutcomeState, AgentUpgradeResultReport, GroupObservation, IpBanObservation,
    MetricSample, ObservedSnapshot, SessionObservation, TelemetryBatch, TelemetryDropCounters,
    TelemetryPriority, UserObservation,
};
use prost::Message;
use std::collections::HashMap;
use std::io;
use std::time::SystemTime;
use uuid::Uuid;

#[derive(Default)]
pub(super) struct SessionCache {
    previous: HashMap<(String, String, String), SessionObservation>,
}

impl SessionCache {
    fn observe(
        &mut self,
        list: &ocservia_agent_protocol::SessionList,
        now: SystemTime,
    ) -> Vec<SessionObservation> {
        let mut next = HashMap::with_capacity(list.sessions.len());
        let mut sessions = Vec::with_capacity(list.sessions.len());
        for session in &list.sessions {
            let key = (
                session.id.clone(),
                session.username.clone(),
                session.vpn_ip.clone(),
            );
            let previous = self.previous.get(&key).filter(|prior| {
                session.connected_at.is_none() || session.connected_at == prior.connected_at
            });
            let observation = SessionObservation {
                session_id: session.id.clone(),
                username: session.username.clone(),
                client_ip: session.remote_ip.clone(),
                vpn_ip: session.vpn_ip.clone(),
                connected_at: session
                    .connected_at
                    .or_else(|| previous.and_then(|p| p.connected_at))
                    .or_else(|| Some(now.into())),
                bytes_in: session
                    .bytes_in
                    .or_else(|| previous.map(|p| p.bytes_in))
                    .unwrap_or_default(),
                bytes_out: session
                    .bytes_out
                    .or_else(|| previous.map(|p| p.bytes_out))
                    .unwrap_or_default(),
            };
            next.insert(key, observation.clone());
            sessions.push(observation);
        }
        self.previous = next;
        sessions
    }
}

pub(super) async fn send_telemetry(
    connection: &iroh::endpoint::Connection,
    payload: &[u8],
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    if payload.is_empty() || payload.len() > 512 * 1024 {
        return Err(invalid("telemetry payload size invalid").into());
    }
    let mut send = tokio::time::timeout(HANDSHAKE_TIMEOUT, connection.open_uni()).await??;
    send.write_all(&u32::try_from(payload.len())?.to_be_bytes())
        .await?;
    send.write_all(payload).await?;
    send.finish()?;
    Ok(())
}

#[allow(clippy::too_many_lines)]
pub(super) fn build_telemetry(
    session: &mut SessionContext<'_>,
    sequence: u64,
    observations: &[PrivdResponse],
    upgrade_outcomes: Vec<UpgradeOperationResult>,
    connection: &iroh::endpoint::Connection,
    drops: [u64; 4],
) -> Result<TelemetryBatch, io::Error> {
    let now = SystemTime::now();
    let mut service = serde_json::json!({"active_state":"unknown","sub_state":"unknown"});
    let mut version = "unknown".to_owned();
    let mut sessions = None;
    let mut ip_bans = Vec::new();
    let mut users = Vec::new();
    let mut groups = Vec::new();
    let mut user_snapshot_complete = false;
    let mut group_snapshot_complete = false;
    let mut fingerprint = serde_json::json!({});
    for observation in observations {
        match &observation.result {
            Some(privd_response::Result::ServiceStatus(value)) => {
                service = serde_json::json!({"load_state":value.load_state,"active_state":value.active_state,"sub_state":value.sub_state});
            }
            Some(privd_response::Result::OcservVersion(value)) => {
                version.clone_from(&value.version);
            }
            Some(privd_response::Result::ConfigFingerprint(value)) => {
                fingerprint = serde_json::json!({"config_sha256":value.sha256,"config_size_bytes":value.size_bytes});
            }
            Some(privd_response::Result::SessionList(value)) => {
                sessions = Some(session.session_cache.observe(value, now));
            }
            Some(privd_response::Result::IpBanList(value)) => {
                ip_bans = value
                    .bans
                    .iter()
                    .map(|ban| IpBanObservation {
                        ip: ban.ip.clone(),
                        seconds_remaining: ban.seconds_remaining,
                    })
                    .collect();
            }
            Some(privd_response::Result::UserList(value)) => {
                user_snapshot_complete = true;
                let applied_users: std::collections::HashMap<String, u64> = session
                    .command_executor
                    .journal()
                    .applied_revisions("user")
                    .map_err(io::Error::other)?
                    .into_iter()
                    .collect();
                users = value
                    .users
                    .iter()
                    .map(|user| {
                        let revision = applied_users
                            .get(user.username.as_str())
                            .copied()
                            .unwrap_or(0);
                        UserObservation {
                            username: user.username.clone(),
                            enabled: user.enabled,
                            revision,
                        }
                    })
                    .collect();
            }
            Some(privd_response::Result::GroupList(value)) => {
                group_snapshot_complete = true;
                let mut applied_groups: std::collections::HashMap<String, u64> = session
                    .command_executor
                    .journal()
                    .applied_revisions("group")
                    .map_err(io::Error::other)?
                    .into_iter()
                    .collect();
                groups = value
                    .groups
                    .iter()
                    .map(|group| {
                        let revision = applied_groups
                            .remove(group.group_name.as_str())
                            .unwrap_or(0);
                        group_observation(group.group_name.clone(), group.members.clone(), revision)
                    })
                    .collect();
                if !applied_groups.is_empty() {
                    groups.extend(applied_groups.into_iter().map(|(group_name, revision)| {
                        group_observation(group_name, Vec::new(), revision)
                    }));
                }
                groups.sort_by(|left, right| left.group_name.cmp(&right.group_name));
            }
            _ => {}
        }
    }
    let sessions = sessions.ok_or_else(|| invalid("privd session snapshot incomplete: check use-occtl = true and run sudo occtl --json show status"))?;
    if !user_snapshot_complete || !group_snapshot_complete {
        return Err(invalid("privd user/group snapshot incomplete"));
    }
    if users.len() > MAX_MANAGED_RESOURCES || groups.len() > MAX_MANAGED_RESOURCES.saturating_mul(2)
    {
        return Err(invalid("user/group telemetry capacity exceeded"));
    }
    let paths = connection.paths();
    let selected = paths.iter().find(iroh::endpoint::Path::is_selected);
    let ocserv = serde_json::json!({"service":service,"configuration":fingerprint});
    let system = session.host_metrics.sample();
    let session_count = f64::from(u32::try_from(sessions.len()).unwrap_or(u32::MAX));
    let mut samples = Vec::new();
    let mut sample = |metric: &str, value| {
        samples.push(MetricSample {
            sampled_at: Some(now.into()),
            metric: metric.to_owned(),
            value,
        });
    };
    sample("session_count", session_count);
    for (name, value) in &system {
        if let Some(number) = value.as_f64() {
            sample(name, number);
        }
    }
    let path = if let Some(path_info) = selected {
        let mode = if path_info.is_ip() {
            "direct"
        } else if path_info.is_relay() {
            "relay"
        } else {
            "unknown"
        };
        let rtt = path_info.rtt();
        sample("connection_rtt_ms", rtt.as_secs_f64() * 1000.0);
        serde_json::json!({"mode": mode, "rtt_ms": u64::try_from(rtt.as_millis()).unwrap_or(u64::MAX)})
    } else {
        serde_json::json!({"mode":"unknown","rtt_ms":0})
    };
    let batch = TelemetryBatch {
        batch_id: Uuid::now_v7().as_bytes().to_vec(),
        node_id: session.node_id.as_bytes().to_vec(),
        sequence,
        priority: i32::from(TelemetryPriority::CurrentHealth),
        snapshot: Some(ObservedSnapshot {
            observed_at: Some(now.into()),
            boot_id: session.boot_id.to_owned(),
            agent_instance_id: session.agent_instance_id.as_bytes().to_vec(),
            agent_version: ocservia_contracts::agent_upgrade::release_version().to_owned(),
            ocserv_version: version,
            os_release: session.os_release.to_owned(),
            architecture: ocservia_contracts::agent_upgrade::runtime_architecture()
                .unwrap_or_default()
                .to_owned(),
            ocserv_json: serde_json::to_vec(&ocserv).unwrap_or_else(|_| b"{}".to_vec()),
            system_json: serde_json::to_vec(&system)?,
            path_json: serde_json::to_vec(&path).unwrap_or_else(|_| b"{}".to_vec()),
            upgrade_results: upgrade_outcomes.into_iter().map(upgrade_report).collect(),
            dropped: Some(TelemetryDropCounters {
                security: drops[0],
                health: drops[1],
                aggregate: drops[2],
                raw: drops[3],
            }),
        }),
        sessions,
        samples,
        security_events: Vec::new(),
        ip_bans,
        users,
        groups,
    };
    if batch.encoded_len() > 512 * 1024 {
        return Err(invalid("telemetry payload size invalid"));
    }
    Ok(batch)
}

fn upgrade_report(outcome: UpgradeOperationResult) -> AgentUpgradeResultReport {
    AgentUpgradeResultReport {
        operation_id: outcome.operation_id,
        state: match outcome.state.as_str() {
            "succeeded" => AgentUpgradeOutcomeState::Succeeded,
            "failed" => AgentUpgradeOutcomeState::Failed,
            "rolled_back" => AgentUpgradeOutcomeState::RolledBack,
            _ => AgentUpgradeOutcomeState::Unspecified,
        } as i32,
        target_version: outcome.target_version,
        completed_unix_ms: outcome.completed_unix_ms,
        detail: outcome.detail,
        privileged_result_proof: outcome.privileged_result_proof,
    }
}

fn group_observation(group_name: String, members: Vec<String>, revision: u64) -> GroupObservation {
    GroupObservation {
        group_name,
        members,
        revision,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn session_snapshots_keep_first_seen_and_last_known_counters_until_absent() {
        let mut cache = SessionCache::default();
        let start = SystemTime::UNIX_EPOCH;
        let later = start + std::time::Duration::from_secs(30);
        let mut list = ocservia_agent_protocol::SessionList {
            sessions: vec![ocservia_agent_protocol::Session {
                id: "5".into(),
                username: "alice".into(),
                bytes_in: Some(10),
                ..Default::default()
            }],
        };
        cache.observe(&list, start);
        list.sessions[0].bytes_in = None;
        let observed = &cache.observe(&list, later)[0];
        assert_eq!(observed.connected_at, Some(start.into()));
        assert_eq!(observed.bytes_in, 10);
        cache.observe(&ocservia_agent_protocol::SessionList::default(), later);
        let reused = &cache.observe(&list, later)[0];
        assert_eq!(reused.connected_at, Some(later.into()));
        assert_eq!(reused.bytes_in, 0);
    }

    #[test]
    fn group_observation_preserves_member_order_and_empty_membership() {
        for members in [vec!["bob".to_owned(), "ann".to_owned()], Vec::new()] {
            let observed = group_observation("vpn".to_owned(), members.clone(), 7);
            assert_eq!(observed.members, members);
            assert_eq!(observed.revision, 7);
        }
    }
}
