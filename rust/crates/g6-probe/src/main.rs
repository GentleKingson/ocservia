//! G6 readiness probes for connection-owner fencing staleness.
//!
//! The G6 harness uses this binary to demonstrate that both enforcement
//! points of the V2 connection-owner fence reject a superseded owner term:
//!
//! - `uds-stale-fence` replays a Controller-signed fence carrying a stale
//!   owner epoch through the transportd Unix socket and expects the `Stale`
//!   disposition with the retained (higher) epoch.
//! - `agent-stale-command` impersonates the Controller endpoint for a
//!   bounded probe window while transportd is stopped, completes a
//!   protocol-1.1 agent session handshake with a harness-signed session
//!   grant, delivers one stale-fenced `synthetic.noop` command, and expects
//!   the Agent to reject it with `stale_owner_epoch`.
//! - `node-connection` reads one node's live transport connection record
//!   (path, session timestamps, owner epoch) over the transportd Unix
//!   socket, so the harness can archive session and relay-path evidence
//!   from the same authoritative source the control plane uses.
//!
//! All modes exit non-zero unless the expected result was observed and
//! print exactly one JSON object describing the observation so the harness
//! can archive it as raw evidence.

use std::collections::HashMap;
use std::env;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::{Duration, SystemTime};

use ed25519_dalek::pkcs8::DecodePrivateKey;
use ed25519_dalek::{Signature, Signer, SigningKey};
use iroh::endpoint::{RelayMode, presets};
use iroh::protocol::{AcceptError, ProtocolHandler, Router};
use iroh::{Endpoint, RelayMap, RelayUrl, SecretKey};
use ocservia_command_authorization::{
    ConnectionFenceClaimsV2, ControllerCommandKeyring, SessionGrantClaimsV1,
    canonical_connection_fence_v2, canonical_session_grant_v1, verification_key_id,
};
use ocservia_contracts::decode_strict_command_envelope;
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    AgentEvent, AgentEventType, CommandDeliveryMode, CommandEnvelope, CommandResult,
    CommandResultState, ConnectionFenceV2, FenceSignatureVersion, HandshakeResult, SessionGrantV1,
    SessionGrantVersion, SessionHandshake, SessionHandshakeResponse, SyntheticNoop,
    command_envelope,
};
use ocservia_contracts::generated::ocserv::platform::transport::v1::transport_service_client::TransportServiceClient;
use ocservia_contracts::generated::ocserv::platform::transport::v1::{
    GetNodeConnectionRequest, GetOwnerFenceRequest, NodeConnection, OwnerFenceDisposition,
    RegisterOwnerFenceRequest,
};
use prost::Message;
use rustls_pki_types::pem::PemObject as _;
use uuid::Uuid;

const AGENT_ALPN: &[u8] = b"ocserv-platform/agent/1";
const HANDSHAKE_TIMEOUT: Duration = Duration::from_secs(10);
const MAX_FRAME_BYTES: usize = 1024 * 1024;
/// The stale envelope keeps its synthetic mutation capability, but the
/// impersonated handshake is intentionally read-only and unfenced. A correct
/// Agent rejects the old epoch before capability authorization; a broken
/// high-water mark falls through to a non-stale rejection instead.
const PROBE_CAPABILITY: &str = "synthetic.noop";
const PROBE_SESSION_CAPABILITY: &str = "ocserv.status.read";

const FENCE_OPTION_NAMES: &[&str] = &[
    "--signing-key-file",
    "--node-id",
    "--endpoint-id",
    "--owner-instance-id",
    "--owner-incarnation",
    "--stale-epoch",
    "--authorization-revision",
    "--lease-seconds",
    "--validity-seconds",
];

const UDS_OPTION_NAMES: &[&str] = &["--socket", "--expect-retained-epoch"];

const AGENT_OPTION_NAMES: &[&str] = &[
    "--controller-key-file",
    "--relay-url",
    "--relay-token-file",
    "--relay-ca-file",
    "--wait-seconds",
];

const NODE_OPTION_NAMES: &[&str] = &[
    "--socket",
    "--node-id",
    "--expect-path",
    "--signing-key-file",
];

fn main() {
    let args: Vec<String> = env::args().skip(1).collect();
    let usage =
        "usage: ocservia-g6-probe <uds-stale-fence|agent-stale-command|node-connection> [options]";
    let Some(mode) = args.first() else {
        eprintln!("{usage}");
        std::process::exit(2);
    };
    let parsed = parse_mode_arguments(mode, &args[1..]);
    let result = match (mode.as_str(), parsed) {
        ("uds-stale-fence", Ok(options)) => {
            init_tracing();
            tokio::runtime::Builder::new_current_thread()
                .enable_all()
                .build()
                .expect("tokio runtime")
                .block_on(run_uds_stale_fence(options))
        }
        ("agent-stale-command", Ok(options)) => {
            init_tracing();
            tokio::runtime::Builder::new_multi_thread()
                .enable_all()
                .build()
                .expect("tokio runtime")
                .block_on(run_agent_stale_command(options))
        }
        ("node-connection", Ok(options)) => {
            init_tracing();
            tokio::runtime::Builder::new_current_thread()
                .enable_all()
                .build()
                .expect("tokio runtime")
                .block_on(run_node_connection(options))
        }
        _ => {
            eprintln!("{usage}");
            std::process::exit(2);
        }
    };
    if let Err(error) = result {
        eprintln!("ocservia-g6-probe: {error}");
        std::process::exit(1);
    }
}

fn init_tracing() {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info")),
        )
        .with_writer(std::io::stderr)
        .init();
}

/// The full option set of one probe invocation: the shared fence options
/// plus the options of the selected mode.
struct ProbeArguments {
    fence: HashMap<String, String>,
    mode: HashMap<String, Vec<String>>,
}

fn parse_mode_arguments(mode: &str, args: &[String]) -> Result<ProbeArguments, String> {
    let (fence_names, mode_names): (&[&str], &[&str]) = match mode {
        "uds-stale-fence" => (FENCE_OPTION_NAMES, UDS_OPTION_NAMES),
        "agent-stale-command" => (FENCE_OPTION_NAMES, AGENT_OPTION_NAMES),
        "node-connection" => (&[], NODE_OPTION_NAMES),
        _ => return Err(format!("unknown probe mode: {mode}")),
    };
    parse_probe_arguments(args, fence_names, mode_names)
}

fn parse_probe_arguments(
    args: &[String],
    fence_names: &[&str],
    mode_names: &[&str],
) -> Result<ProbeArguments, String> {
    let mut probe = ProbeArguments {
        fence: HashMap::new(),
        mode: HashMap::new(),
    };
    let mut index = 0;
    while index < args.len() {
        let argument = args[index].as_str();
        let value = args
            .get(index + 1)
            .ok_or_else(|| format!("{argument} requires a value"))?;
        // Mode-specific options take precedence when a spelling is shared.
        // In particular, node-connection accepts repeated --node-id values,
        // while the two stale-fence modes keep their one fence node id.
        if mode_names.contains(&argument) {
            probe
                .mode
                .entry(argument.to_owned())
                .or_default()
                .push(value.clone());
        } else if fence_names.contains(&argument) {
            probe.fence.insert(argument.to_owned(), value.clone());
        } else {
            return Err(format!("unknown option: {argument}"));
        }
        index += 2;
    }
    Ok(probe)
}

/// A fencing term the probe signs. Fresh fence, connection, and operation
/// identifiers ensure the probe never replays another component's recorded
/// proof objects: only the owner epoch is stale by construction.
struct FenceTerm {
    fence: ConnectionFenceV2,
    key_id: String,
}

fn unix_now_parts() -> (i64, u32) {
    match SystemTime::now().duration_since(SystemTime::UNIX_EPOCH) {
        Ok(elapsed) => (
            i64::try_from(elapsed.as_secs()).unwrap_or(i64::MAX),
            elapsed.subsec_nanos(),
        ),
        Err(_) => (i64::MAX, 0),
    }
}

fn timestamp(seconds: i64, nanos: u32) -> prost_types::Timestamp {
    prost_types::Timestamp {
        seconds,
        nanos: i32::try_from(nanos).unwrap_or(0),
    }
}

fn node_id_bytes(value: &Uuid) -> [u8; 16] {
    *value.as_bytes()
}

#[allow(clippy::too_many_arguments)]
fn build_stale_fence(
    signing: &SigningKey,
    node_id: Uuid,
    endpoint_id: [u8; 32],
    owner_instance_id: Uuid,
    owner_incarnation: u64,
    stale_epoch: u64,
    authorization_revision: u64,
    lease_seconds: i64,
    validity_seconds: i64,
) -> Result<FenceTerm, String> {
    let (now_seconds, now_nanos) = unix_now_parts();
    let expires_seconds = now_seconds.saturating_add(validity_seconds);
    let lease_until_seconds = now_seconds.saturating_add(lease_seconds);
    let claims = ConnectionFenceClaimsV2 {
        signature_version: 1,
        key_id: verification_key_id(&signing.verifying_key()),
        fence_id: node_id_bytes(&Uuid::now_v7()),
        node_id: node_id_bytes(&node_id),
        endpoint_id,
        owner_instance_id: node_id_bytes(&owner_instance_id),
        owner_incarnation,
        owner_epoch: stale_epoch,
        connection_id: node_id_bytes(&Uuid::now_v7()),
        authorization_revision,
        capabilities: vec![PROBE_CAPABILITY.to_owned()],
        lease_until_seconds,
        lease_until_nanos: now_nanos,
        issued_at_seconds: now_seconds,
        issued_at_nanos: now_nanos,
        expires_at_seconds: expires_seconds,
        expires_at_nanos: now_nanos,
    };
    let canonical = canonical_connection_fence_v2(&claims)
        .map_err(|error| format!("fence claims are invalid: {error}"))?;
    let signature: Signature = signing.sign(&canonical);
    Ok(FenceTerm {
        fence: ConnectionFenceV2 {
            signature_version: FenceSignatureVersion::Ed25519V1.into(),
            key_id: claims.key_id.clone(),
            fence_id: claims.fence_id.to_vec(),
            node_id: claims.node_id.to_vec(),
            endpoint_id: claims.endpoint_id.to_vec(),
            owner_instance_id: claims.owner_instance_id.to_vec(),
            owner_incarnation,
            owner_epoch: stale_epoch,
            connection_id: claims.connection_id.to_vec(),
            authorization_revision,
            capabilities: vec![PROBE_CAPABILITY.to_owned()],
            lease_until: Some(timestamp(lease_until_seconds, now_nanos)),
            issued_at: Some(timestamp(now_seconds, now_nanos)),
            expires_at: Some(timestamp(expires_seconds, now_nanos)),
            signature: signature.to_bytes().to_vec(),
        },
        key_id: claims.key_id,
    })
}

fn parse_uuid(value: &str, name: &str) -> Result<Uuid, String> {
    Uuid::parse_str(value).map_err(|_| format!("{name} must be a UUID"))
}

fn parse_endpoint_id(value: &str) -> Result<[u8; 32], String> {
    hex::decode(value)
        .map_err(|_| "--endpoint-id must be 64 hex characters".to_owned())?
        .try_into()
        .map_err(|_| "--endpoint-id must be 64 hex characters".to_owned())
}

fn parse_u64(value: &str, name: &str) -> Result<u64, String> {
    value
        .parse()
        .map_err(|_| format!("{name} must be a non-negative integer"))
}

fn parse_i64(value: &str, name: &str) -> Result<i64, String> {
    value
        .parse()
        .map_err(|_| format!("{name} must be an integer"))
}

/// Loads the Controller command signing key from the exact PKCS#8 PEM form
/// the Go control plane loads with `commandauth.LoadSigner`.
fn load_signing_key(path: &Path) -> Result<SigningKey, String> {
    let raw = std::fs::read_to_string(path)
        .map_err(|error| format!("read signing key {}: {error}", path.display()))?;
    SigningKey::from_pkcs8_pem(&raw).map_err(|_| {
        "signing key must contain exactly one PKCS#8 Ed25519 PRIVATE KEY block".to_owned()
    })
}

#[derive(Debug)]
struct FenceOptions {
    signing: SigningKey,
    node_id: Uuid,
    endpoint_id: [u8; 32],
    owner_instance_id: Uuid,
    owner_incarnation: u64,
    stale_epoch: u64,
    authorization_revision: u64,
    lease_seconds: i64,
    validity_seconds: i64,
}

fn parse_fence_options(options: &HashMap<String, String>) -> Result<FenceOptions, String> {
    let required = |name: &str| {
        options
            .get(name)
            .cloned()
            .ok_or_else(|| format!("{name} is required"))
    };
    let lease_seconds = match options.get("--lease-seconds") {
        Some(value) => parse_i64(value, "--lease-seconds")?,
        None => 120,
    };
    let validity_seconds = match options.get("--validity-seconds") {
        Some(value) => parse_i64(value, "--validity-seconds")?,
        None => 300,
    };
    let owner_incarnation = match options.get("--owner-incarnation") {
        Some(value) => parse_u64(value, "--owner-incarnation")?,
        None => 1,
    };
    let authorization_revision = match options.get("--authorization-revision") {
        Some(value) => parse_u64(value, "--authorization-revision")?,
        None => 1,
    };
    let stale_epoch = parse_u64(&required("--stale-epoch")?, "--stale-epoch")?;
    if stale_epoch == 0 {
        return Err("--stale-epoch must be at least 1".to_owned());
    }
    if lease_seconds <= 0 || validity_seconds <= 0 {
        return Err("--lease-seconds and --validity-seconds must be positive".to_owned());
    }
    Ok(FenceOptions {
        signing: load_signing_key(&PathBuf::from(required("--signing-key-file")?))?,
        node_id: parse_uuid(&required("--node-id")?, "--node-id")?,
        endpoint_id: parse_endpoint_id(&required("--endpoint-id")?)?,
        owner_instance_id: parse_uuid(&required("--owner-instance-id")?, "--owner-instance-id")?,
        owner_incarnation,
        stale_epoch,
        authorization_revision,
        lease_seconds,
        validity_seconds,
    })
}

fn emit_json(value: &serde_json::Value) {
    println!(
        "{}",
        serde_json::to_string(value).expect("probe JSON serializes")
    );
}

fn timestamp_to_rfc3339(value: Option<&prost_types::Timestamp>) -> Option<String> {
    let stamp = value?;
    let seconds = stamp.seconds;
    let nanos = u32::try_from(stamp.nanos).ok()?;
    let millis = nanos / 1_000_000;
    let days = seconds.div_euclid(86_400);
    let seconds_of_day = seconds.rem_euclid(86_400);
    let (year, month, day) = civil_from_days(days);
    Some(format!(
        "{year:04}-{month:02}-{day:02}T{:02}:{:02}:{:02}.{millis:03}Z",
        seconds_of_day / 3_600,
        (seconds_of_day % 3_600) / 60,
        seconds_of_day % 60
    ))
}

/// Converts days since 1970-01-01 to a proleptic Gregorian date (Howard
/// Hinnant's `civil_from_days`); the harness archives these stamps as raw
/// evidence, so the conversion must not pull a datetime dependency.
fn civil_from_days(days: i64) -> (i64, u32, u32) {
    let z = days + 719_468;
    let era = z.div_euclid(146_097);
    let day_of_era = z.rem_euclid(146_097);
    let year_of_era =
        (day_of_era - day_of_era / 1_460 + day_of_era / 36_524 - day_of_era / 146_096) / 365;
    let year = year_of_era + era * 400;
    let day_of_year = day_of_era - (365 * year_of_era + year_of_era / 4 - year_of_era / 100);
    let shifted_month = (5 * day_of_year + 2) / 153;
    let day = day_of_year - (153 * shifted_month + 2) / 5 + 1;
    let month = if shifted_month < 10 {
        shifted_month + 3
    } else {
        shifted_month - 9
    };
    (
        if month <= 2 { year + 1 } else { year },
        u32::try_from(month).expect("month is 1..=12 by construction"),
        u32::try_from(day).expect("day is 1..=31 by construction"),
    )
}

async fn run_node_connection(probe: ProbeArguments) -> Result<(), String> {
    let required_mode = |name: &str| -> Result<String, String> {
        probe
            .mode
            .get(name)
            .and_then(|values| values.first())
            .cloned()
            .filter(|value| !value.is_empty())
            .ok_or_else(|| format!("{name} is required"))
    };
    let socket = PathBuf::from(required_mode("--socket")?);
    let signing = load_signing_key(&PathBuf::from(required_mode("--signing-key-file")?))?;
    let keyring = ControllerCommandKeyring::new([signing.verifying_key()])
        .map_err(|error| format!("construct Controller verification keyring: {error}"))?;
    let node_ids = probe
        .mode
        .get("--node-id")
        .cloned()
        .unwrap_or_default()
        .iter()
        .map(|value| parse_uuid(value, "--node-id"))
        .collect::<Result<Vec<Uuid>, String>>()?;
    if node_ids.is_empty() {
        return Err("--node-id is required at least once".to_owned());
    }
    let expect_path = probe
        .mode
        .get("--expect-path")
        .and_then(|values| values.first())
        .cloned()
        .unwrap_or_else(|| "any".to_owned());
    let channel = tonic::transport::Endpoint::try_from("http://[::]:50051")
        .map_err(|error| format!("endpoint construction failed: {error}"))?
        .connect_with_connector(tower::service_fn(move |_| {
            let path = socket.clone();
            async move {
                tokio::net::UnixStream::connect(&path)
                    .await
                    .map(hyper_util::rt::TokioIo::new)
            }
        }))
        .await
        .map_err(|error| format!("transport socket connect failed: {error}"))?;
    let mut client = TransportServiceClient::new(channel);
    let mut all_matched = true;
    let mut observations = Vec::new();
    for node_id in node_ids {
        let response = match client
            .get_node_connection(GetNodeConnectionRequest {
                node_id: node_id.as_bytes().to_vec(),
            })
            .await
        {
            Ok(response) => response.into_inner(),
            Err(status) => return Err(format!("GetNodeConnection failed for {node_id}: {status}")),
        };
        let fence = client
            .get_owner_fence(GetOwnerFenceRequest {
                node_id: node_id.as_bytes().to_vec(),
            })
            .await
            .map_err(|status| format!("GetOwnerFence failed for {node_id}: {status}"))?
            .into_inner()
            .fence
            .ok_or_else(|| format!("GetOwnerFence returned no fence for {node_id}"))?;
        let (now_seconds, now_nanos) = unix_now_parts();
        let (observation, matched) = node_connection_observation(
            node_id,
            &response,
            &fence,
            &keyring,
            now_seconds,
            now_nanos,
            &expect_path,
        )?;
        all_matched &= matched;
        observations.push(observation);
    }
    emit_json(&serde_json::json!({
        "mode": "node_connection",
        "expected_path": expect_path,
        "all_matched": all_matched,
        "observations": observations,
    }));
    if all_matched {
        Ok(())
    } else {
        Err(format!(
            "at least one node does not use the expected path {expect_path}"
        ))
    }
}

fn node_connection_observation(
    node_id: Uuid,
    response: &NodeConnection,
    fence: &ConnectionFenceV2,
    keyring: &ControllerCommandKeyring,
    now_seconds: i64,
    now_nanos: u32,
    expect_path: &str,
) -> Result<(serde_json::Value, bool), String> {
    if response.node_id != node_id.as_bytes() {
        return Err(format!(
            "GetNodeConnection returned a different node identity for {node_id}"
        ));
    }
    let endpoint_id: [u8; 32] = response.endpoint_id.as_slice().try_into().map_err(|_| {
        format!("GetNodeConnection returned malformed endpoint identity for {node_id}")
    })?;
    if response.agent_instance_id.len() != 16 {
        return Err(format!(
            "GetNodeConnection returned malformed connection identity for {node_id}"
        ));
    }
    let verified = keyring
        .verify_connection_fence_v2_at(
            fence,
            node_id.as_bytes(),
            &endpoint_id,
            now_seconds,
            now_nanos,
        )
        .map_err(|error| format!("verify owner fence for {node_id}: {error}"))?;
    if (verified.lease_until_seconds, verified.lease_until_nanos) <= (now_seconds, now_nanos) {
        return Err(format!("verified owner lease has expired for {node_id}"));
    }
    let mut session_capabilities = response.negotiated_capabilities.clone();
    let mut fence_capabilities = verified.capabilities.clone();
    session_capabilities.sort();
    fence_capabilities.sort();
    if verified.owner_incarnation == 0
        || verified.owner_epoch == 0
        || verified.owner_epoch != response.owner_epoch
        || verified.authorization_revision != response.authorization_revision
        || fence_capabilities != session_capabilities
    {
        return Err(format!(
            "verified owner fence does not match the live connection for {node_id}"
        ));
    }
    let path_name = match response.path {
        1 => "direct",
        2 => "relay",
        _ => "unspecified",
    };
    let matched = expect_path == "any" || expect_path == path_name;
    Ok((
        serde_json::json!({
            "node_id": node_id.to_string(),
            "found": true,
            "endpoint_id": hex::encode(&response.endpoint_id),
            "agent_instance_id": hex::encode(&response.agent_instance_id),
            "path": path_name,
            "matched": matched,
            "path_detail": response.path_detail,
            "round_trip_time_millis": response.round_trip_time_millis,
            "connected_at": timestamp_to_rfc3339(response.connected_at.as_ref()),
            "last_seen": timestamp_to_rfc3339(response.last_seen.as_ref()),
            "session_expires_at": timestamp_to_rfc3339(response.session_expires_at.as_ref()),
            "owner_fence_id": hex::encode(verified.fence_id),
            "owner_instance_id": Uuid::from_slice(&verified.owner_instance_id)
                .map_err(|_| format!("owner instance id is malformed for {node_id}"))?
                .to_string(),
            "owner_incarnation": verified.owner_incarnation.to_string(),
            "connection_id": hex::encode(verified.connection_id),
            "owner_lease_until": timestamp_to_rfc3339(Some(&timestamp(
                verified.lease_until_seconds,
                verified.lease_until_nanos,
            ))),
            "owner_epoch": response.owner_epoch,
            "authorization_revision": response.authorization_revision,
            "negotiated_capabilities": session_capabilities,
        }),
        matched,
    ))
}

async fn run_uds_stale_fence(probe: ProbeArguments) -> Result<(), String> {
    let socket = probe
        .mode
        .get("--socket")
        .and_then(|values| values.first())
        .map(PathBuf::from)
        .ok_or_else(|| "--socket is required".to_owned())?;
    let expect_retained_epoch = parse_u64(
        probe
            .mode
            .get("--expect-retained-epoch")
            .and_then(|values| values.first())
            .ok_or_else(|| "--expect-retained-epoch is required".to_owned())?,
        "--expect-retained-epoch",
    )?;
    let options = parse_fence_options(&probe.fence)?;
    let term = build_stale_fence(
        &options.signing,
        options.node_id,
        options.endpoint_id,
        options.owner_instance_id,
        options.owner_incarnation,
        options.stale_epoch,
        options.authorization_revision,
        options.lease_seconds,
        options.validity_seconds,
    )?;
    let channel = tonic::transport::Endpoint::try_from("http://[::]:50051")
        .map_err(|error| format!("endpoint construction failed: {error}"))?
        .connect_with_connector(tower::service_fn(move |_| {
            let path = socket.clone();
            async move {
                tokio::net::UnixStream::connect(&path)
                    .await
                    .map(hyper_util::rt::TokioIo::new)
            }
        }))
        .await
        .map_err(|error| format!("transport socket connect failed: {error}"))?;
    let mut client = TransportServiceClient::new(channel);
    let rpc = client
        .register_owner_fence(RegisterOwnerFenceRequest {
            fence: Some(term.fence),
        })
        .await;
    let response = match rpc {
        Ok(response) => response.into_inner(),
        Err(status) => {
            emit_json(&serde_json::json!({
                "mode": "uds_stale_fence",
                "status": "rpc_error",
                "probe_epoch": options.stale_epoch,
                "error": status.message(),
                "node_id": options.node_id.to_string(),
            }));
            return Err(format!("RegisterOwnerFence failed: {status}"));
        }
    };
    let disposition = OwnerFenceDisposition::try_from(response.disposition)
        .unwrap_or(OwnerFenceDisposition::Unspecified);
    let rejected = disposition == OwnerFenceDisposition::Stale
        && response.retained_epoch == expect_retained_epoch
        && response.retained_epoch > options.stale_epoch;
    emit_json(&serde_json::json!({
        "mode": "uds_stale_fence",
        "status": if rejected { "rejected" } else { "unexpected" },
        "probe_epoch": options.stale_epoch,
        "disposition": format!("{disposition:?}").to_lowercase(),
        "retained_epoch": response.retained_epoch,
        "expected_retained_epoch": expect_retained_epoch,
        "fence_key_id": term.key_id,
        "node_id": options.node_id.to_string(),
    }));
    if rejected {
        Ok(())
    } else {
        Err("transportd did not reject the stale owner fence as expected".to_owned())
    }
}

#[derive(Clone, Debug)]
struct AgentObservation {
    node_id: Uuid,
    endpoint_id: [u8; 32],
    command_id: [u8; 16],
    state: &'static str,
    error_code: String,
    rejected: bool,
}

#[derive(Debug)]
struct StaleCommandHandler {
    options: Arc<FenceOptions>,
    observation: Arc<tokio::sync::Notify>,
    result: Arc<std::sync::OnceLock<AgentObservation>>,
}

impl ProtocolHandler for StaleCommandHandler {
    async fn accept(&self, connection: iroh::endpoint::Connection) -> Result<(), AcceptError> {
        let (mut send, mut recv) = tokio::time::timeout(HANDSHAKE_TIMEOUT, connection.accept_bi())
            .await
            .map_err(|_| frame_error("handshake stream timed out"))?
            .map_err(|_| frame_error("handshake stream failed"))?;
        let handshake_bytes = read_framed(&mut recv, MAX_FRAME_BYTES, "handshake").await?;
        let handshake = SessionHandshake::decode(handshake_bytes.as_slice())
            .map_err(|_| frame_error("handshake protobuf is invalid"))?;
        let node_bytes: [u8; 16] = handshake
            .node_id
            .clone()
            .try_into()
            .map_err(|_| frame_error("handshake node id is invalid"))?;
        let endpoint_bytes: [u8; 32] = handshake
            .endpoint_id
            .clone()
            .try_into()
            .map_err(|_| frame_error("handshake endpoint id is invalid"))?;
        if node_bytes != *self.options.node_id.as_bytes()
            || endpoint_bytes != self.options.endpoint_id
        {
            connection.close(
                iroh::endpoint::VarInt::from_u32(0x101),
                b"probe target mismatch",
            );
            return Ok(());
        }
        let response = build_handshake_response(&self.options, &node_bytes, &endpoint_bytes)
            .map_err(|error| frame_error(&error))?;
        write_framed(&mut send, &response).await?;
        drop(send);
        tracing::info!(
            node_id = %self.options.node_id,
            stale_epoch = self.options.stale_epoch,
            "probe session accepted; delivering stale-fenced command"
        );
        let term = build_stale_fence(
            &self.options.signing,
            self.options.node_id,
            self.options.endpoint_id,
            self.options.owner_instance_id,
            self.options.owner_incarnation,
            self.options.stale_epoch,
            self.options.authorization_revision,
            self.options.lease_seconds,
            self.options.validity_seconds,
        )
        .map_err(|error| frame_error(&error))?;
        let command_id = node_id_bytes(&Uuid::now_v7());
        let envelope = build_stale_envelope(&self.options, &term, command_id)
            .map_err(|error| frame_error(&error))?;
        let (mut command_send, mut command_recv) =
            tokio::time::timeout(HANDSHAKE_TIMEOUT, connection.open_bi())
                .await
                .map_err(|_| frame_error("command stream open timed out"))?
                .map_err(|_| frame_error("command stream open failed"))?;
        write_framed(&mut command_send, &envelope).await?;
        command_send
            .finish()
            .map_err(|_| frame_error("command stream finish failed"))?;
        let event_bytes = read_framed(&mut command_recv, MAX_FRAME_BYTES, "agent event").await?;
        let event = AgentEvent::decode(event_bytes.as_slice())
            .map_err(|_| frame_error("agent event protobuf is invalid"))?;
        if event.r#type != i32::from(AgentEventType::CommandResult) {
            return Err(frame_error("probe expected a command result event"));
        }
        let result = CommandResult::decode(event.payload.as_slice())
            .map_err(|_| frame_error("command result protobuf is invalid"))?;
        let state =
            CommandResultState::try_from(result.state).unwrap_or(CommandResultState::Unspecified);
        let rejected =
            state == CommandResultState::Rejected && result.error_code == "stale_owner_epoch";
        let _ = self.result.set(AgentObservation {
            node_id: self.options.node_id,
            endpoint_id: self.options.endpoint_id,
            command_id,
            state: match state {
                CommandResultState::Succeeded => "succeeded",
                CommandResultState::Failed => "failed",
                CommandResultState::Unknown => "unknown",
                CommandResultState::Rejected => "rejected",
                CommandResultState::Unspecified => "unspecified",
            },
            error_code: result.error_code.clone(),
            rejected,
        });
        self.observation.notify_one();
        Ok(())
    }
}

fn build_handshake_response(
    options: &FenceOptions,
    node_id: &[u8; 16],
    endpoint_id: &[u8; 32],
) -> Result<SessionHandshakeResponse, String> {
    let (now_seconds, now_nanos) = unix_now_parts();
    let expires_seconds = now_seconds.saturating_add(options.validity_seconds);
    let capabilities = vec![PROBE_SESSION_CAPABILITY.to_owned()];
    let claims = SessionGrantClaimsV1 {
        version: 1,
        key_id: verification_key_id(&options.signing.verifying_key()),
        protocol_major: 1,
        protocol_minor: 1,
        node_id: *node_id,
        endpoint_id: *endpoint_id,
        authorization_revision: options.authorization_revision,
        negotiated_capabilities: capabilities.clone(),
        issued_at_seconds: now_seconds,
        issued_at_nanos: now_nanos,
        expires_at_seconds: expires_seconds,
        expires_at_nanos: now_nanos,
    };
    let canonical = canonical_session_grant_v1(&claims)
        .map_err(|error| format!("session grant claims are invalid: {error}"))?;
    let signature: Signature = options.signing.sign(&canonical);
    Ok(SessionHandshakeResponse {
        result: HandshakeResult::Accepted.into(),
        protocol_major: 1,
        protocol_minor: 1,
        max_message_size: 64 * 1024,
        controller_version: env!("CARGO_PKG_VERSION").to_owned(),
        negotiated_capabilities: capabilities,
        session_grant: Some(SessionGrantV1 {
            version: SessionGrantVersion::V1.into(),
            key_id: claims.key_id,
            protocol_major: 1,
            protocol_minor: 1,
            node_id: node_id.to_vec(),
            endpoint_id: endpoint_id.to_vec(),
            authorization_revision: options.authorization_revision,
            negotiated_capabilities: claims.negotiated_capabilities,
            issued_at: Some(timestamp(now_seconds, now_nanos)),
            expires_at: Some(timestamp(expires_seconds, now_nanos)),
            signature: signature.to_bytes().to_vec(),
        }),
        connection_fence: None,
    })
}

fn build_stale_envelope(
    options: &FenceOptions,
    term: &FenceTerm,
    command_id: [u8; 16],
) -> Result<CommandEnvelope, String> {
    let (now_seconds, now_nanos) = unix_now_parts();
    let expires_seconds = now_seconds.saturating_add(options.validity_seconds);
    let operation_id = node_id_bytes(&Uuid::now_v7());
    let traceparent = format!(
        "00-{}-{}-01",
        hex::encode([7_u8; 16]),
        hex::encode(&command_id[..8])
    );
    let envelope = CommandEnvelope {
        protocol_version: "1.1".to_owned(),
        message_id: node_id_bytes(&Uuid::now_v7()).to_vec(),
        command_id: command_id.to_vec(),
        idempotency_key: operation_id.to_vec(),
        node_id: options.node_id.as_bytes().to_vec(),
        sequence: 1,
        issued_at: Some(timestamp(now_seconds, now_nanos)),
        expires_at: Some(timestamp(expires_seconds, now_nanos)),
        expected_revision: options.authorization_revision,
        traceparent,
        actor_id: "g6-probe".to_owned(),
        reason: "g6 stale-owner probe".to_owned(),
        delivery_mode: CommandDeliveryMode::ExecuteOrReplay.into(),
        semantic_payload_hash_version: 0,
        semantic_payload_sha256: Vec::new(),
        operation_id: operation_id.to_vec(),
        action: "operation.create".to_owned(),
        required_capability: PROBE_CAPABILITY.to_owned(),
        approval_id: Vec::new(),
        approval_request_sha256: Vec::new(),
        authorization: None,
        connection_fence: Some(term.fence.clone()),
        fence_binding: None,
        payload: Some(command_envelope::Payload::SyntheticNoop(SyntheticNoop {})),
    };
    decode_strict_command_envelope(&envelope.encode_to_vec())
        .map_err(|error| format!("probe envelope fails strict wire validation: {error}"))?;
    Ok(envelope)
}

async fn write_framed(
    send: &mut iroh::endpoint::SendStream,
    message: &impl Message,
) -> Result<(), AcceptError> {
    let bytes = message.encode_to_vec();
    let length =
        u32::try_from(bytes.len()).map_err(|_| frame_error("message length exceeds u32"))?;
    send.write_all(&length.to_be_bytes())
        .await
        .map_err(|_| frame_error("message length write failed"))?;
    send.write_all(&bytes)
        .await
        .map_err(|_| frame_error("message write failed"))?;
    Ok(())
}

async fn read_framed(
    recv: &mut iroh::endpoint::RecvStream,
    max_bytes: usize,
    kind: &str,
) -> Result<Vec<u8>, AcceptError> {
    let mut length = [0_u8; 4];
    tokio::time::timeout(HANDSHAKE_TIMEOUT, recv.read_exact(&mut length))
        .await
        .map_err(|_| frame_error(&format!("{kind} length timed out")))?
        .map_err(|_| frame_error(&format!("{kind} length read failed")))?;
    let length = u32::from_be_bytes(length) as usize;
    if length == 0 || length > max_bytes {
        return Err(frame_error(&format!("{kind} size invalid")));
    }
    let mut bytes = vec![0_u8; length];
    tokio::time::timeout(HANDSHAKE_TIMEOUT, recv.read_exact(&mut bytes))
        .await
        .map_err(|_| frame_error(&format!("{kind} body timed out")))?
        .map_err(|_| frame_error(&format!("{kind} body read failed")))?;
    Ok(bytes)
}

fn frame_error(message: &str) -> AcceptError {
    AcceptError::from_err(std::io::Error::new(
        std::io::ErrorKind::InvalidData,
        message.to_owned(),
    ))
}

async fn run_agent_stale_command(probe: ProbeArguments) -> Result<(), String> {
    let options = Arc::new(parse_fence_options(&probe.fence)?);
    let required_mode = |name: &str| -> Result<String, String> {
        probe
            .mode
            .get(name)
            .and_then(|values| values.first())
            .cloned()
            .ok_or_else(|| format!("{name} is required"))
    };
    let relay_urls: Vec<String> = probe.mode.get("--relay-url").cloned().unwrap_or_default();
    let wait_seconds = match probe.mode.get("--wait-seconds") {
        Some(values) => parse_i64(
            values
                .first()
                .ok_or_else(|| "--wait-seconds requires a value".to_owned())?,
            "--wait-seconds",
        )?,
        None => 180,
    };
    if wait_seconds <= 0 {
        return Err("--wait-seconds must be positive".to_owned());
    }
    if relay_urls.is_empty() {
        return Err("at least one --relay-url is required".to_owned());
    }
    let secret =
        load_controller_secret_key(&PathBuf::from(required_mode("--controller-key-file")?))?;
    let token = std::fs::read_to_string(PathBuf::from(required_mode("--relay-token-file")?))
        .map_err(|error| format!("read relay token: {error}"))?
        .trim()
        .to_owned();
    let relay_ca_file = match probe.mode.get("--relay-ca-file") {
        Some(values) => PathBuf::from(
            values
                .first()
                .ok_or_else(|| "--relay-ca-file requires a value".to_owned())?
                .clone(),
        ),
        None => PathBuf::from("/nonexistent-g6-probe-relay-ca"),
    };
    let endpoint = bind_probe_endpoint(secret, &relay_urls, token, &relay_ca_file).await?;
    tracing::info!(
        controller_endpoint_id = %endpoint.id(),
        wait_seconds,
        "probe endpoint bound; waiting for the target agent"
    );
    let handler = StaleCommandHandler {
        observation: Arc::new(tokio::sync::Notify::new()),
        result: Arc::new(std::sync::OnceLock::new()),
        options: options.clone(),
    };
    let notify = handler.observation.clone();
    let result_slot = handler.result.clone();
    let router = Router::builder(endpoint.clone())
        .accept(AGENT_ALPN, handler)
        .spawn();
    let observed = tokio::time::timeout(
        Duration::from_secs(u64::try_from(wait_seconds).unwrap_or(u64::from(u32::MAX))),
        async {
            loop {
                if result_slot.get().is_some() {
                    return;
                }
                notify.notified().await;
            }
        },
    )
    .await;
    let _ = router.shutdown().await;
    endpoint.close().await;
    let Some(observation) = result_slot.get() else {
        emit_json(&serde_json::json!({
            "mode": "agent_stale_command",
            "status": "timeout",
            "waited_seconds": wait_seconds,
            "node_id": options.node_id.to_string(),
        }));
        return Err("target agent did not complete the probe within the wait window".to_owned());
    };
    let _ = observed;
    emit_json(&serde_json::json!({
        "mode": "agent_stale_command",
        "status": if observation.rejected { "rejected" } else { "unexpected" },
        "node_id": observation.node_id.to_string(),
        "endpoint_id": hex::encode(observation.endpoint_id),
        "command_id": hex::encode(observation.command_id),
        "state": observation.state,
        "error_code": observation.error_code,
        "probe_epoch": options.stale_epoch,
    }));
    if observation.rejected {
        Ok(())
    } else {
        Err("agent did not reject the stale-fenced command with stale_owner_epoch".to_owned())
    }
}

/// Binds the impersonated Controller endpoint on the same relays the agents
/// use for discovery, so their redials reach the probe while transportd is
/// stopped. The relay CA file adds the harness relay authority to the relay
/// client root store.
async fn bind_probe_endpoint(
    secret: SecretKey,
    relay_urls: &[String],
    token: String,
    relay_ca_file: &Path,
) -> Result<Endpoint, String> {
    let urls = relay_urls
        .iter()
        .map(|raw| {
            raw.parse::<RelayUrl>()
                .map_err(|_| format!("relay URL is invalid: {raw}"))
        })
        .collect::<Result<Vec<_>, _>>()?;
    let relay_map = RelayMap::from_iter(urls).with_auth_token(token);
    let roots: Vec<_> = rustls_pki_types::CertificateDer::pem_file_iter(relay_ca_file)
        .map_err(|error| format!("relay CA file is unreadable: {error}"))?
        .collect::<Result<Vec<_>, _>>()
        .map_err(|error| format!("relay CA PEM is invalid: {error}"))?;
    if roots.is_empty() {
        return Err("relay CA file contains no certificates".to_owned());
    }
    let mut builder = Endpoint::builder(presets::N0)
        .secret_key(secret)
        .relay_mode(RelayMode::Custom(relay_map));
    builder = builder.ca_tls_config(iroh::tls::CaTlsConfig::default().with_extra_roots(roots));
    builder
        .bind()
        .await
        .map_err(|error| format!("probe endpoint bind failed: {error}"))
}

/// Loads the Controller iroh endpoint key in the exact 32-raw-byte or
/// 64-lowercase-hex form transportd loads with `--key-file`.
fn load_controller_secret_key(path: &Path) -> Result<SecretKey, String> {
    let raw = std::fs::read(path)
        .map_err(|error| format!("read controller key {}: {error}", path.display()))?;
    let secret: [u8; 32] = if raw.len() == 32 {
        raw.try_into().expect("length checked")
    } else {
        let text = std::str::from_utf8(&raw)
            .map_err(|_| "controller key must contain 32 raw bytes or 64 lowercase hex".to_owned())?
            .trim_end_matches(['\n', '\r']);
        if text.len() != 64 || text.bytes().any(|byte| byte.is_ascii_uppercase()) {
            return Err("controller key must contain 32 raw bytes or 64 lowercase hex".to_owned());
        }
        hex::decode(text)
            .map_err(|_| "controller key contains invalid hex".to_owned())?
            .try_into()
            .map_err(|_| "controller key must contain exactly 32 bytes".to_owned())?
    };
    Ok(SecretKey::from_bytes(&secret))
}

#[cfg(test)]
#[allow(clippy::unwrap_used, clippy::expect_used)]
mod tests {
    use super::*;

    use ed25519_dalek::pkcs8::EncodePrivateKey;
    use ocservia_command_authorization::ControllerCommandKeyring;

    fn probe_options() -> (FenceOptions, SigningKey) {
        let signing = SigningKey::from_bytes(&[7; 32]);
        (
            FenceOptions {
                signing: signing.clone(),
                node_id: Uuid::now_v7(),
                endpoint_id: [9; 32],
                owner_instance_id: Uuid::now_v7(),
                owner_incarnation: 3,
                stale_epoch: 1,
                authorization_revision: 12,
                lease_seconds: 60,
                validity_seconds: 120,
            },
            signing,
        )
    }

    fn fixture_term() -> (FenceTerm, FenceOptions, ControllerCommandKeyring) {
        let (options, signing) = probe_options();
        let term = build_stale_fence(
            &signing,
            options.node_id,
            options.endpoint_id,
            options.owner_instance_id,
            options.owner_incarnation,
            options.stale_epoch,
            options.authorization_revision,
            options.lease_seconds,
            options.validity_seconds,
        )
        .expect("fence builds");
        let keyring =
            ControllerCommandKeyring::new([signing.verifying_key()]).expect("fixture keyring");
        (term, options, keyring)
    }

    fn write_temporary(bytes: &[u8]) -> std::path::PathBuf {
        let directory = std::env::temp_dir()
            .canonicalize()
            .expect("temp directory resolves");
        let path = directory.join(format!("g6-probe-{}", Uuid::now_v7()));
        std::fs::write(&path, bytes).expect("write temporary file");
        path
    }

    #[test]
    fn stale_fence_is_verified_by_the_agent_keyring() {
        let (term, options, keyring) = fixture_term();
        let (now_seconds, _) = unix_now_parts();
        let verified = keyring
            .verify_connection_fence_v2(
                &term.fence,
                options.node_id.as_bytes(),
                &options.endpoint_id,
                now_seconds,
            )
            .expect("keyring verifies the probe fence");
        assert_eq!(verified.owner_epoch, 1);
        assert_eq!(verified.owner_incarnation, 3);
        assert_eq!(verified.authorization_revision, 12);
        assert_eq!(verified.capabilities, vec!["synthetic.noop".to_owned()]);
    }

    #[test]
    fn tampered_fence_epoch_fails_keyring_verification() {
        let (mut term, options, keyring) = fixture_term();
        term.fence.owner_epoch = 99;
        let (now_seconds, _) = unix_now_parts();
        assert!(
            keyring
                .verify_connection_fence_v2(
                    &term.fence,
                    options.node_id.as_bytes(),
                    &options.endpoint_id,
                    now_seconds,
                )
                .is_err()
        );
    }

    #[test]
    fn session_grant_is_verified_by_the_agent_keyring() {
        let (options, signing) = probe_options();
        let node_id = node_id_bytes(&options.node_id);
        let response = build_handshake_response(&options, &node_id, &options.endpoint_id)
            .expect("response builds");
        let keyring =
            ControllerCommandKeyring::new([signing.verifying_key()]).expect("fixture keyring");
        let grant = response.session_grant.as_ref().expect("grant attached");
        let (now_seconds, _) = unix_now_parts();
        let verified = keyring
            .verify_session_grant(grant, &node_id, &options.endpoint_id, now_seconds)
            .expect("keyring verifies the probe grant");
        assert_eq!(verified.authorization_revision, 12);
        assert_eq!(
            verified.negotiated_capabilities,
            vec![PROBE_SESSION_CAPABILITY.to_owned()]
        );
        assert_eq!(
            response.negotiated_capabilities,
            verified.negotiated_capabilities
        );
        assert!(response.connection_fence.is_none());
    }

    #[test]
    fn stale_envelope_passes_strict_wire_validation_and_carries_the_fence() {
        let (term, options, _) = fixture_term();
        let envelope = build_stale_envelope(&options, &term, node_id_bytes(&Uuid::now_v7()))
            .expect("envelope builds");
        assert_eq!(
            envelope
                .connection_fence
                .as_ref()
                .expect("fence attached")
                .owner_epoch,
            1
        );
        assert_eq!(envelope.node_id, options.node_id.as_bytes().to_vec());
        assert_eq!(envelope.required_capability, PROBE_CAPABILITY);
        assert_ne!(envelope.required_capability, PROBE_SESSION_CAPABILITY);
        assert!(envelope.authorization.is_none());
    }

    #[test]
    fn pkcs8_signing_key_round_trips_through_the_loader() {
        let signing = SigningKey::from_bytes(&[7; 32]);
        let pem = signing
            .to_pkcs8_pem(ed25519_dalek::pkcs8::spki::der::pem::LineEnding::LF)
            .expect("encode PKCS#8 PEM");
        let path = write_temporary(pem.as_bytes());
        let loaded = load_signing_key(&path).expect("loader parses the PKCS#8 form");
        std::fs::remove_file(&path).ok();
        assert_eq!(loaded.verifying_key(), signing.verifying_key());
        assert_eq!(
            verification_key_id(&loaded.verifying_key()),
            verification_key_id(&signing.verifying_key())
        );
    }

    #[test]
    fn controller_secret_key_loader_accepts_both_wire_forms() {
        let key = SecretKey::from_bytes(&[3; 32]);
        let raw_path = write_temporary(&[3; 32]);
        assert_eq!(
            load_controller_secret_key(&raw_path)
                .expect("raw form loads")
                .public(),
            key.public()
        );
        std::fs::remove_file(&raw_path).ok();
        let hex_path = write_temporary(hex::encode([3; 32]).as_bytes());
        assert_eq!(
            load_controller_secret_key(&hex_path)
                .expect("hex form loads")
                .public(),
            key.public()
        );
        std::fs::remove_file(&hex_path).ok();
    }

    fn argument_list(values: &[&str]) -> Vec<String> {
        values.iter().map(ToString::to_string).collect()
    }

    #[test]
    fn probe_arguments_split_mode_and_fence_options() {
        let args = argument_list(&[
            "--socket",
            "/run/transportd.sock",
            "--expect-retained-epoch",
            "2",
            "--signing-key-file",
            "/keys/signing.pem",
            "--node-id",
            "01894a5c-6c1e-7b8f-9a2c-3d4e5f607182",
            "--endpoint-id",
            &"a".repeat(64),
            "--owner-instance-id",
            "01894a5c-6c1e-7b8f-9a2c-3d4e5f607183",
            "--stale-epoch",
            "1",
        ]);
        let probe = parse_probe_arguments(&args, FENCE_OPTION_NAMES, UDS_OPTION_NAMES)
            .expect("arguments parse");
        assert_eq!(
            probe.mode.get("--socket").and_then(|v| v.first()),
            Some(&"/run/transportd.sock".to_owned())
        );
        assert_eq!(probe.fence.get("--stale-epoch"), Some(&"1".to_owned()));
        assert_eq!(
            probe.fence.get("--node-id"),
            Some(&"01894a5c-6c1e-7b8f-9a2c-3d4e5f607182".to_owned())
        );
        assert!(!probe.mode.contains_key("--relay-url"));
    }

    #[test]
    fn probe_arguments_reject_unknown_options_and_missing_values() {
        let args = argument_list(&["--socket", "/run/x", "--nonsense", "1"]);
        assert!(parse_probe_arguments(&args, FENCE_OPTION_NAMES, UDS_OPTION_NAMES).is_err());
        let truncated = argument_list(&["--socket"]);
        assert!(parse_probe_arguments(&truncated, FENCE_OPTION_NAMES, UDS_OPTION_NAMES).is_err());
    }

    #[test]
    fn fence_options_require_the_stale_epoch_and_keys() {
        let args = argument_list(&[
            "--stale-epoch",
            "0",
            "--node-id",
            "01894a5c-6c1e-7b8f-9a2c-3d4e5f607182",
        ]);
        let probe =
            parse_probe_arguments(&args, FENCE_OPTION_NAMES, UDS_OPTION_NAMES).expect("parse");
        let error = parse_fence_options(&probe.fence).expect_err("zero epoch must be rejected");
        assert!(error.contains("stale-epoch"));
    }

    #[test]
    fn node_connection_preserves_every_repeated_node_id() {
        let first = "01894a5c-6c1e-7b8f-9a2c-3d4e5f607182";
        let second = "01894a5c-6c1e-7b8f-9a2c-3d4e5f607183";
        let args = argument_list(&[
            "--socket",
            "/run/transportd.sock",
            "--expect-path",
            "any",
            "--node-id",
            first,
            "--node-id",
            second,
        ]);
        let probe = parse_mode_arguments("node-connection", &args).expect("arguments parse");

        assert_eq!(
            probe.mode.get("--node-id"),
            Some(&vec![first.to_owned(), second.to_owned()])
        );
        assert!(!probe.fence.contains_key("--node-id"));
    }

    #[test]
    fn node_connection_observation_binds_the_verified_owner_term() {
        let node_id = Uuid::parse_str("01894a5c-6c1e-7b8f-9a2c-3d4e5f607182").expect("node UUID");
        let owner_id = Uuid::parse_str("01894a5c-6c1e-7b8f-9a2c-3d4e5f607183").expect("owner UUID");
        let endpoint_id = [4_u8; 32];
        let signing = SigningKey::from_bytes(&[9_u8; 32]);
        let keyring =
            ControllerCommandKeyring::new([signing.verifying_key()]).expect("fixture keyring");
        let fence = build_stale_fence(&signing, node_id, endpoint_id, owner_id, 3, 9, 11, 120, 300)
            .expect("signed fence")
            .fence;
        let response = NodeConnection {
            node_id: node_id.as_bytes().to_vec(),
            endpoint_id: endpoint_id.to_vec(),
            path: 1,
            round_trip_time_millis: 7,
            connected_at: Some(timestamp(unix_now_parts().0, 0)),
            agent_instance_id: vec![5_u8; 16],
            path_detail: "direct:test".to_owned(),
            last_seen: Some(timestamp(unix_now_parts().0, 0)),
            negotiated_capabilities: vec![PROBE_CAPABILITY.to_owned()],
            authorization_revision: 11,
            session_expires_at: Some(timestamp(unix_now_parts().0 + 300, 0)),
            owner_epoch: 9,
        };
        let expected_connection_id = hex::encode(&fence.connection_id);

        let (now_seconds, now_nanos) = unix_now_parts();
        let (observation, matched) = node_connection_observation(
            node_id,
            &response,
            &fence,
            &keyring,
            now_seconds,
            now_nanos,
            "direct",
        )
        .expect("matching observation");
        assert!(matched);
        assert_eq!(observation["owner_instance_id"], owner_id.to_string());
        assert_eq!(observation["owner_incarnation"], "3");
        assert_eq!(observation["connection_id"], expected_connection_id);
        assert_eq!(observation["owner_epoch"], 9);
        assert_eq!(observation["endpoint_id"], hex::encode([4_u8; 32]));
        assert_eq!(observation["agent_instance_id"], hex::encode([5_u8; 16]));
        assert!(observation["owner_lease_until"].is_string());

        let mut wrong_capabilities = response.clone();
        wrong_capabilities.negotiated_capabilities = vec!["ocserv.status.read".to_owned()];
        let error = node_connection_observation(
            node_id,
            &wrong_capabilities,
            &fence,
            &keyring,
            now_seconds,
            now_nanos,
            "any",
        )
        .expect_err("connection and fence capabilities mismatch must fail closed");
        assert!(error.contains("verified owner fence"));

        let mut tampered_fence = fence;
        tampered_fence.owner_epoch = 10;
        let error = node_connection_observation(
            node_id,
            &response,
            &tampered_fence,
            &keyring,
            now_seconds,
            now_nanos,
            "any",
        )
        .expect_err("tampered owner fence must fail signature verification");
        assert!(error.contains("verify owner fence"));
    }
}
