use crate::ENROLL_ALPN;
use crate::session::{controller_dial_target, exchange_message, supported_capabilities};
use iroh::endpoint::{RelayMode, VarInt, presets};
use iroh::{Endpoint, EndpointId, RelayMap, RelayUrl};
use ocservia_agent_protocol::{PrivdResponse, privd_response};
use ocservia_command_authorization::{ControllerCommandKeyring, load_verification_key};
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    EnrollRequest, EnrollResponse, HandshakeResult, SealedSecretPurpose, SealedSecretVersion,
    SealingKeyDescriptorV1,
};
use rustls_pki_types::pem::PemObject as _;
use std::env;
use std::io;
use std::os::unix::fs::{MetadataExt as _, OpenOptionsExt};
use std::path::{Path, PathBuf};
use std::time::SystemTime;
use uuid::Uuid;
use zeroize::Zeroizing;

pub(super) fn print_version_query() -> bool {
    if std::env::args().nth(1).as_deref() == Some("--binding-version") {
        println!("1");
        return true;
    }
    // The packaging pipeline verifies the binary's embedded release
    // identity before it is shipped, so --version stays a read-only
    // query that works for any caller.
    if std::env::args().any(|argument| argument == "--version") {
        println!(
            "ocservia-agent {}",
            ocservia_contracts::agent_upgrade::release_version()
        );
        return true;
    }
    false
}

/// Fails closed unless every read-only privd snapshot probe succeeded, so an
/// Agent never reports a healthy session against a broken supervisor.
pub(super) fn require_healthy_snapshot(
    observations: Vec<PrivdResponse>,
) -> Result<Vec<PrivdResponse>, Box<dyn std::error::Error + Send + Sync>> {
    let failures = observations
        .iter()
        .filter_map(|response| match &response.result {
            Some(privd_response::Result::Error(error)) => Some(error.detail.as_str()),
            None => Some("missing privd response result"),
            _ => None,
        })
        .collect::<Vec<_>>();
    if !failures.is_empty() {
        return Err(invalid(&format!(
            "privd read-only snapshot failed: {}",
            failures.join("; ")
        ))
        .into());
    }
    tracing::info!(
        observations = observations.len(),
        "initial read-only snapshot collected"
    );
    Ok(observations)
}

pub(super) fn prepare_enrollment_if_requested(config: &Config) -> Result<bool, io::Error> {
    if !config.prepare_enrollment {
        return Ok(false);
    }
    println!("{}", prepare_enrollment(config)?);
    Ok(true)
}

pub(super) async fn run_one_shot_mode(
    config: &Config,
) -> Result<bool, Box<dyn std::error::Error + Send + Sync>> {
    if let Some(token_file) = config.enrollment_token_file.as_deref() {
        enroll_agent(config, token_file).await?;
        return Ok(true);
    }
    Ok(false)
}

pub(super) fn prepare_enrollment(config: &Config) -> Result<String, io::Error> {
    let controller = config
        .controller
        .ok_or_else(|| invalid("--controller is required for enrollment preparation"))?;
    let identity = ocservia_agent_identity::Identity::provision(&config.identity_dir, controller)?;
    Ok(hex::encode(identity.endpoint_id().as_bytes()))
}

async fn enroll_agent(
    config: &Config,
    token_file: &Path,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let controller = config
        .controller
        .ok_or_else(|| invalid("--controller is required for enrollment"))?;
    let environment = config
        .enrollment_environment
        .as_deref()
        .ok_or_else(|| invalid("--enrollment-environment is required"))?;
    let token = read_enrollment_token(token_file)?;
    let identity = ocservia_agent_identity::Identity::provision(&config.identity_dir, controller)?;
    let endpoint = with_relay_tls_roots(
        Endpoint::builder(presets::N0),
        load_relay_tls_roots(config)?,
    )
    .secret_key(identity.secret_key().clone())
    .relay_mode(config.relay_mode.clone())
    .clear_address_lookup()
    .dns_resolver(system_dns_resolver())
    .bind()
    .await?;
    let connection = endpoint
        .connect(
            controller_dial_target(controller, &config.relay_mode),
            ENROLL_ALPN,
        )
        .await?;
    let mut request = EnrollRequest {
        token,
        endpoint_id: identity.endpoint_id().as_bytes().to_vec(),
        agent_version: ocservia_contracts::agent_upgrade::release_version().to_owned(),
        os_release: ocservia_agent::read_os_release().await?,
        ocserv_version: "unknown".to_owned(),
        boot_id: ocservia_agent::read_boot_id().await?,
        agent_instance_id: Uuid::now_v7().as_bytes().to_vec(),
        capabilities: supported_capabilities(),
        environment: environment.to_owned(),
        nonce: Uuid::now_v7().as_bytes().to_vec(),
        time: Some(SystemTime::now().into()),
        enrollment_protocol_major: 0,
        enrollment_protocol_minor: 0,
        proof: None,
        sealing_keys: config.sealing_keys.clone(),
    };
    identity.authorize_enrollment(&mut request)?;
    let response: EnrollResponse = exchange_message(&connection, &request, "enrollment").await?;
    let node_id = Uuid::from_slice(&response.node_id)
        .map_err(|_| invalid("Controller returned an invalid enrollment node ID"))?;
    if node_id.get_version_num() != 7
        || !matches!(
            HandshakeResult::try_from(response.result),
            Ok(HandshakeResult::PendingApproval | HandshakeResult::Accepted)
        )
    {
        return Err(invalid("Controller rejected enrollment").into());
    }
    println!("{node_id}");
    connection.close(VarInt::from_u32(0), b"enrollment complete");
    endpoint.close().await;
    Ok(())
}

#[derive(Debug)]
pub(super) struct Config {
    pub(super) identity_dir: PathBuf,
    pub(super) journal: PathBuf,
    pub(super) privd_socket: PathBuf,
    pub(super) controller: Option<EndpointId>,
    pub(super) node_id: Option<Uuid>,
    pub(super) probe_only: bool,
    pub(super) relay_mode: RelayMode,
    pub(super) command_verification_key_files: Vec<PathBuf>,
    pub(super) prepare_enrollment: bool,
    pub(super) enrollment_token_file: Option<PathBuf>,
    pub(super) enrollment_environment: Option<String>,
    pub(super) sealing_keys: Vec<SealingKeyDescriptorV1>,
    pub(super) stats_file: Option<PathBuf>,
    pub(super) relay_ca_file: Option<PathBuf>,
    pub(super) synthetic_barrier_file: Option<PathBuf>,
}

/// Loads the PEM relay certificate authority as additional relay TLS roots.
/// Deployments whose relays chain to public roots leave this unset.
pub(super) fn load_relay_tls_roots(
    config: &Config,
) -> Result<Vec<rustls_pki_types::CertificateDer<'static>>, io::Error> {
    let Some(path) = config.relay_ca_file.as_ref() else {
        return Ok(Vec::new());
    };
    let roots: Vec<_> = rustls_pki_types::CertificateDer::pem_file_iter(path)
        .map_err(|error| invalid(&format!("relay CA file is unreadable: {error}")))?
        .collect::<Result<Vec<_>, _>>()
        .map_err(|error| invalid(&format!("relay CA PEM is invalid: {error}")))?;
    if roots.is_empty() {
        return Err(invalid("relay CA file contains no certificates"));
    }
    Ok(roots)
}

/// Resolves Relay names through system DNS and hosts only; a lookup failure is
/// reported instead of falling back to iroh's public resolvers.
pub(super) fn system_dns_resolver() -> iroh::dns::DnsResolver {
    iroh::dns::DnsResolver::builder()
        .with_system_defaults()
        .disable_fallback()
        .build()
}

/// Applies the optional private relay CA to one endpoint builder.
pub(super) fn with_relay_tls_roots(
    builder: iroh::endpoint::Builder,
    roots: Vec<rustls_pki_types::CertificateDer<'static>>,
) -> iroh::endpoint::Builder {
    if roots.is_empty() {
        builder
    } else {
        builder.ca_tls_config(iroh::tls::CaTlsConfig::default().with_extra_roots(roots))
    }
}

pub(super) fn load_controller_command_keys(
    config: &Config,
) -> Result<Option<ControllerCommandKeyring>, Box<dyn std::error::Error + Send + Sync>> {
    if config.probe_only || config.prepare_enrollment || config.enrollment_token_file.is_some() {
        return Ok(None);
    }
    if config.command_verification_key_files.is_empty() {
        return Err(invalid("--controller-command-key-file is required").into());
    }
    let expected_owner = rustix::process::geteuid().as_raw();
    let expected_group = rustix::process::getegid().as_raw();
    let keys = config
        .command_verification_key_files
        .iter()
        .map(|path| load_verification_key(path, expected_owner, expected_group))
        .collect::<Result<Vec<_>, _>>()?;
    Ok(Some(ControllerCommandKeyring::new(keys)?))
}

#[allow(clippy::too_many_lines)]
pub(super) fn parse_args() -> Result<Config, io::Error> {
    let mut config = Config {
        identity_dir: PathBuf::from("/var/lib/ocservia-agent/identity"),
        journal: PathBuf::from("/var/lib/ocservia-agent/agent.db"),
        privd_socket: PathBuf::from("/run/ocserv-platform/privd.sock"),
        controller: None,
        node_id: None,
        probe_only: false,
        relay_mode: RelayMode::Disabled,
        command_verification_key_files: Vec::new(),
        prepare_enrollment: false,
        enrollment_token_file: None,
        enrollment_environment: None,
        sealing_keys: Vec::new(),
        stats_file: None,
        relay_ca_file: None,
        synthetic_barrier_file: None,
    };
    let mut relay_mode = String::from("disabled");
    let mut relay_url = None;
    let mut relay_token_file = None;
    let mut user_seal_key_id = None;
    let mut user_seal_key_sha256 = None;
    let mut p12_seal_key_id = None;
    let mut p12_seal_key_sha256 = None;
    let mut args = env::args().skip(1);
    while let Some(argument) = args.next() {
        match argument.as_str() {
            "--identity-dir" => {
                config.identity_dir = PathBuf::from(required(&mut args, "--identity-dir")?);
            }
            "--journal" => config.journal = PathBuf::from(required(&mut args, "--journal")?),
            "--privd-socket" => {
                config.privd_socket = PathBuf::from(required(&mut args, "--privd-socket")?);
            }
            "--controller" => {
                config.controller = Some(
                    required(&mut args, "--controller")?
                        .parse()
                        .map_err(|_| invalid("controller EndpointID invalid"))?,
                );
            }
            "--node-id" => {
                config.node_id = Some(
                    required(&mut args, "--node-id")?
                        .parse()
                        .map_err(|_| invalid("node ID invalid"))?,
                );
            }
            "--probe-privd-only" => config.probe_only = true,
            "--controller-command-key-file" => {
                if config.command_verification_key_files.len() == 8 {
                    return Err(invalid(
                        "at most eight Controller command verification keys are allowed",
                    ));
                }
                config
                    .command_verification_key_files
                    .push(PathBuf::from(required(
                        &mut args,
                        "--controller-command-key-file",
                    )?));
            }
            "--prepare-enrollment" => config.prepare_enrollment = true,
            "--enrollment-token-file" => {
                config.enrollment_token_file = Some(PathBuf::from(required(
                    &mut args,
                    "--enrollment-token-file",
                )?));
            }
            "--enrollment-environment" => {
                config.enrollment_environment =
                    Some(required(&mut args, "--enrollment-environment")?);
            }
            "--user-password-seal-key-id" => {
                user_seal_key_id = Some(required(&mut args, "--user-password-seal-key-id")?);
            }
            "--user-password-seal-public-key-sha256" => {
                user_seal_key_sha256 = Some(required(
                    &mut args,
                    "--user-password-seal-public-key-sha256",
                )?);
            }
            "--p12-password-seal-key-id" => {
                p12_seal_key_id = Some(required(&mut args, "--p12-password-seal-key-id")?);
            }
            "--p12-password-seal-public-key-sha256" => {
                p12_seal_key_sha256 = Some(required(
                    &mut args,
                    "--p12-password-seal-public-key-sha256",
                )?);
            }
            "--relay-mode" => relay_mode = required(&mut args, "--relay-mode")?,
            "--relay-url" => {
                if relay_url.is_some() {
                    return Err(invalid("at most one --relay-url is allowed"));
                }
                relay_url = Some(required(&mut args, "--relay-url")?);
            }
            "--relay-token-file" => {
                relay_token_file = Some(PathBuf::from(required(&mut args, "--relay-token-file")?));
            }
            "--stats-file" => {
                config.stats_file = Some(PathBuf::from(required(&mut args, "--stats-file")?));
            }
            "--relay-ca-file" => {
                let path = PathBuf::from(required(&mut args, "--relay-ca-file")?);
                if !path.is_absolute() {
                    return Err(invalid("--relay-ca-file must be an absolute path"));
                }
                config.relay_ca_file = Some(path);
            }
            "--synthetic-barrier-file" => {
                config.synthetic_barrier_file = Some(PathBuf::from(required(
                    &mut args,
                    "--synthetic-barrier-file",
                )?));
            }
            _ => return Err(invalid("unknown agent argument")),
        }
    }
    validate_absolute_paths(&config, relay_token_file.as_deref())?;
    if config.prepare_enrollment
        && (relay_mode != "disabled" || relay_url.is_some() || relay_token_file.is_some())
    {
        return Err(invalid(
            "enrollment preparation does not accept relay configuration",
        ));
    }
    validate_enrollment_mode(&config)?;
    config.sealing_keys = build_sealing_keys(
        &config,
        user_seal_key_id,
        user_seal_key_sha256,
        p12_seal_key_id,
        p12_seal_key_sha256,
    )?;
    config.relay_mode = build_relay_mode(
        &relay_mode,
        relay_url.as_deref(),
        relay_token_file.as_deref(),
    )?;
    Ok(config)
}

pub(super) fn build_sealing_keys(
    config: &Config,
    user_key_id: Option<String>,
    user_sha256: Option<String>,
    p12_key_id: Option<String>,
    p12_sha256: Option<String>,
) -> io::Result<Vec<SealingKeyDescriptorV1>> {
    if config.probe_only || config.prepare_enrollment {
        if user_key_id.is_some()
            || user_sha256.is_some()
            || p12_key_id.is_some()
            || p12_sha256.is_some()
        {
            return Err(invalid(
                "probe and preparation modes do not accept sealing keys",
            ));
        }
        return Ok(Vec::new());
    }
    let keys = [
        (SealedSecretPurpose::UserPassword, user_key_id, user_sha256),
        (
            SealedSecretPurpose::CertificateP12Password,
            p12_key_id,
            p12_sha256,
        ),
    ]
    .into_iter()
    .map(|(purpose, key_id, digest)| {
        let key_id = key_id.ok_or_else(|| invalid("both password sealing keys are required"))?;
        let digest =
            digest.ok_or_else(|| invalid("both password sealing key fingerprints are required"))?;
        if key_id.is_empty()
            || key_id.len() > 128
            || !key_id
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'.' | b'_' | b'-'))
            || digest.len() != 64
            || !digest
                .bytes()
                .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
        {
            return Err(invalid("password sealing key descriptor invalid"));
        }
        Ok(SealingKeyDescriptorV1 {
            version: SealedSecretVersion::V1.into(),
            purpose: purpose.into(),
            key_id,
            public_key_sha256: hex::decode(digest)
                .map_err(|_| invalid("password sealing key fingerprint invalid"))?,
        })
    })
    .collect::<io::Result<Vec<_>>>()?;
    if keys[0].key_id == keys[1].key_id || keys[0].public_key_sha256 == keys[1].public_key_sha256 {
        return Err(invalid("password sealing keys must be distinct"));
    }
    Ok(keys)
}

fn validate_absolute_paths(config: &Config, relay_token_file: Option<&Path>) -> io::Result<()> {
    if !config.identity_dir.is_absolute()
        || config
            .command_verification_key_files
            .iter()
            .any(|path| !path.is_absolute())
        || relay_token_file.is_some_and(|path| !path.is_absolute())
        || config
            .enrollment_token_file
            .as_ref()
            .is_some_and(|path| !path.is_absolute())
        || config
            .synthetic_barrier_file
            .as_ref()
            .is_some_and(|path| !path.is_absolute())
    {
        return Err(invalid(
            "identity, Controller command key, token, and synthetic barrier paths must be absolute",
        ));
    }
    Ok(())
}

pub(super) fn validate_enrollment_mode(config: &Config) -> Result<(), io::Error> {
    if config.prepare_enrollment
        && (config.controller.is_none()
            || config.enrollment_token_file.is_some()
            || config.enrollment_environment.is_some()
            || config.node_id.is_some()
            || config.probe_only
            || !config.command_verification_key_files.is_empty())
        || config.enrollment_token_file.is_some() != config.enrollment_environment.is_some()
        || config.enrollment_token_file.is_some() && (config.node_id.is_some() || config.probe_only)
        || config.enrollment_environment.as_ref().is_some_and(|value| {
            value.is_empty() || value.len() > 64 || value.chars().any(char::is_whitespace)
        })
    {
        return Err(invalid("enrollment mode arguments are invalid"));
    }
    Ok(())
}

pub(super) fn read_enrollment_token(path: &Path) -> Result<String, io::Error> {
    let mut file = std::fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc_o_nofollow())
        .open(path)?;
    let metadata = file.metadata()?;
    let owner_only = metadata.file_type().is_file()
        && metadata.uid() == rustix::process::geteuid().as_raw()
        && metadata.mode().trailing_zeros() >= 6;
    let protected_group = metadata.file_type().is_file()
        && metadata.uid() == 0
        && metadata.gid() == rustix::process::getegid().as_raw()
        && metadata.mode() & 0o027 == 0
        && metadata.mode() & 0o040 != 0;
    if !owner_only && !protected_group {
        return Err(io::Error::new(
            io::ErrorKind::PermissionDenied,
            "enrollment token must be process-owned mode 0600 or root:agent mode 0640",
        ));
    }
    let mut raw = Vec::with_capacity(50);
    std::io::Read::read_to_end(&mut std::io::Read::take(&mut file, 51), &mut raw)?;
    let raw = Zeroizing::new(raw);
    let token = std::str::from_utf8(&raw)
        .map_err(|_| invalid("enrollment token must be UTF-8"))?
        .trim_end_matches(['\n', '\r']);
    // The root installation token is never accepted by the Agent. Only its
    // one-way derived obt2_ child may cross this boundary.
    let encoded = token.strip_prefix("obt2_").unwrap_or(token);
    if encoded.len() != 43
        || !encoded
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_'))
    {
        return Err(invalid("enrollment token is invalid"));
    }
    Ok(token.to_owned())
}

pub(super) fn build_relay_mode(
    mode: &str,
    raw_url: Option<&str>,
    token_file: Option<&Path>,
) -> Result<RelayMode, io::Error> {
    match mode {
        "disabled" if raw_url.is_none() && token_file.is_none() => Ok(RelayMode::Disabled),
        "disabled" => Err(invalid(
            "relay URL and token are accepted only with custom relay mode",
        )),
        "custom" => {
            let raw = raw_url.ok_or_else(|| invalid("custom relay mode requires --relay-url"))?;
            let token_file = token_file
                .ok_or_else(|| invalid("custom relay mode requires --relay-token-file"))?;
            let url: RelayUrl = raw.parse().map_err(|_| invalid("relay URL is invalid"))?;
            if url.scheme() != "https"
                || !url.username().is_empty()
                || url.password().is_some()
                || url.query().is_some()
                || url.fragment().is_some()
                || url.host_str().is_none()
            {
                return Err(invalid(
                    "relay URL must be credential-free HTTPS without query or fragment",
                ));
            }
            let token = read_relay_token(token_file)?;
            Ok(RelayMode::Custom(
                RelayMap::from_iter([url]).with_auth_token(token),
            ))
        }
        _ => Err(invalid("relay mode must be disabled or custom")),
    }
}

#[allow(clippy::verbose_bit_mask)]
fn read_relay_token(path: &Path) -> Result<String, io::Error> {
    let mut file = std::fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc_o_nofollow())
        .open(path)?;
    let metadata = file.metadata()?;
    if !metadata.file_type().is_file() {
        return Err(invalid("relay token path must be a regular file"));
    }
    let mode = metadata.mode();
    let owner_only = metadata.uid() == rustix::process::geteuid().as_raw() && mode & 0o077 == 0;
    let protected_group = metadata.uid() == 0
        && metadata.gid() == rustix::process::getegid().as_raw()
        && mode & 0o027 == 0
        && mode & 0o040 != 0;
    if !owner_only && !protected_group {
        return Err(io::Error::new(
            io::ErrorKind::PermissionDenied,
            "relay token must be process-owned mode 0600 or root:agent mode 0640",
        ));
    }
    let mut raw = Vec::with_capacity(129);
    std::io::Read::read_to_end(&mut std::io::Read::take(&mut file, 513), &mut raw)?;
    let raw = Zeroizing::new(raw);
    let token = std::str::from_utf8(&raw)
        .map_err(|_| invalid("relay token must be UTF-8"))?
        .trim_end_matches(['\n', '\r']);
    if !(32..=512).contains(&token.len()) || token.chars().any(char::is_whitespace) {
        return Err(invalid(
            "relay token must be 32..512 non-whitespace UTF-8 bytes",
        ));
    }
    Ok(token.to_owned())
}

#[cfg(target_os = "linux")]
const fn libc_o_nofollow() -> i32 {
    0x20_000
}

#[cfg(target_os = "macos")]
const fn libc_o_nofollow() -> i32 {
    0x100
}

fn required(args: &mut impl Iterator<Item = String>, name: &str) -> Result<String, io::Error> {
    args.next()
        .ok_or_else(|| invalid(&format!("{name} requires a value")))
}

pub(super) fn invalid(detail: &str) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidInput, detail)
}
