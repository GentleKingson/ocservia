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
use sha2::Digest;
use std::io;
use std::time::SystemTime;
use uuid::Uuid;

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
    session: &SessionContext<'_>,
    sequence: u64,
    observations: &[PrivdResponse],
    upgrade_outcomes: Vec<UpgradeOperationResult>,
    connection: &iroh::endpoint::Connection,
    drops: [u64; 4],
) -> Result<TelemetryBatch, io::Error> {
    let now = SystemTime::now();
    let mut service = serde_json::json!({"active_state":"unknown","sub_state":"unknown"});
    let mut version = "unknown".to_owned();
    let mut sessions = Vec::new();
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
                sessions = value
                    .sessions
                    .iter()
                    .map(|session| SessionObservation {
                        session_id: session.id.clone(),
                        username: session.username.clone(),
                        client_ip: session.remote_ip.clone(),
                        connected_at: Some(now.into()),
                        bytes_in: 0,
                        bytes_out: 0,
                    })
                    .collect();
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
                users = value
                    .users
                    .iter()
                    .map(|user| {
                        let canonical = format!(
                            "{{\"name\":\"{}\",\"enabled\":{}}}",
                            user.username, user.enabled
                        );
                        UserObservation {
                            username: user.username.clone(),
                            enabled: user.enabled,
                            revision: session
                                .journal
                                .applied_revision("user", &user.username)
                                .ok()
                                .flatten()
                                .unwrap_or(0),
                            fingerprint_sha256: sha2::Sha256::digest(canonical.as_bytes()).to_vec(),
                        }
                    })
                    .collect();
            }
            Some(privd_response::Result::GroupList(value)) => {
                group_snapshot_complete = true;
                groups = value
                    .groups
                    .iter()
                    .map(|group| {
                        let revision = session
                            .journal
                            .applied_revision("group", &group.group_name)
                            .ok()
                            .flatten()
                            .unwrap_or(0);
                        group_observation(group.group_name.clone(), group.members.clone(), revision)
                    })
                    .collect();
            }
            _ => {}
        }
    }
    if !user_snapshot_complete || !group_snapshot_complete {
        return Err(invalid("privd user/group snapshot incomplete"));
    }
    if let Ok(applied_groups) = session.journal.applied_revisions("group") {
        for (group_name, revision) in applied_groups {
            if groups.iter().any(|group| group.group_name == group_name) {
                continue;
            }
            groups.push(group_observation(group_name, Vec::new(), revision));
        }
        groups.sort_by(|left, right| left.group_name.cmp(&right.group_name));
    }
    if users.len() > MAX_MANAGED_RESOURCES || groups.len() > MAX_MANAGED_RESOURCES.saturating_mul(2)
    {
        return Err(invalid("user/group telemetry capacity exceeded"));
    }
    let paths = connection.paths();
    let selected = paths.iter().find(iroh::endpoint::Path::is_selected);
    let (mode, rtt) = selected.map_or(("unknown", 0), |path| {
        (
            if path.is_ip() {
                "direct"
            } else if path.is_relay() {
                "relay"
            } else {
                "unknown"
            },
            u64::try_from(path.rtt().as_millis()).unwrap_or(u64::MAX),
        )
    });
    let ocserv = serde_json::json!({"service":service,"configuration":fingerprint});
    let path = serde_json::json!({"mode":mode,"rtt_ms":rtt});
    let session_count = f64::from(u32::try_from(sessions.len()).unwrap_or(u32::MAX));
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
            system_json: b"{}".to_vec(),
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
        samples: vec![MetricSample {
            sampled_at: Some(now.into()),
            metric: "session_count".to_owned(),
            value: session_count,
        }],
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
    let encoded_members = members
        .iter()
        .map(|member| format!("\"{member}\""))
        .collect::<Vec<_>>()
        .join(",");
    let canonical = format!("{{\"name\":\"{group_name}\",\"members\":[{encoded_members}]}}");
    GroupObservation {
        group_name,
        members,
        revision,
        fingerprint_sha256: sha2::Sha256::digest(canonical.as_bytes()).to_vec(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn group_fingerprint_preserves_member_order_and_empty_membership() {
        for (members, canonical) in [
            (
                vec!["bob".to_owned(), "ann".to_owned()],
                r#"{"name":"vpn","members":["bob","ann"]}"#,
            ),
            (Vec::new(), r#"{"name":"vpn","members":[]}"#),
        ] {
            let observed = group_observation("vpn".to_owned(), members.clone(), 7);
            assert_eq!(observed.members, members);
            assert_eq!(observed.revision, 7);
            assert_eq!(
                observed.fingerprint_sha256,
                sha2::Sha256::digest(canonical.as_bytes()).to_vec()
            );
        }
    }
}
