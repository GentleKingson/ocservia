//! Version-aware, read-only Ocserv adapter with fixed executables and arguments.

#![forbid(unsafe_code)]

use std::collections::HashMap;
use std::io;
use std::net::IpAddr;
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::time::Duration;

use ocservia_agent_protocol::{
    ConfigFingerprint, ErrorKind, IpBan, IpBanList, OcservVersion, PrivdError, ServiceStatus,
    Session, SessionList,
};
use sha2::{Digest, Sha256};
use tokio::io::{AsyncRead, AsyncReadExt};
use tokio::process::Command;

const MAX_CONFIG_BYTES: usize = 1024 * 1024;
const DEFAULT_OUTPUT_BYTES: usize = 256 * 1024;

/// Root-controlled fixed local resources used by the adapter.
#[derive(Clone, Debug)]
pub struct FixedResources {
    systemctl: PathBuf,
    ocserv: PathBuf,
    occtl: PathBuf,
    config: PathBuf,
}

impl Default for FixedResources {
    fn default() -> Self {
        Self {
            systemctl: PathBuf::from("/usr/bin/systemctl"),
            ocserv: PathBuf::from("/usr/sbin/ocserv"),
            occtl: PathBuf::from("/usr/bin/occtl"),
            config: PathBuf::from("/etc/ocserv/ocserv.conf"),
        }
    }
}

impl FixedResources {
    /// Constructs resources from trusted process-startup configuration.
    ///
    /// These values are never populated from an Agent RPC.
    ///
    /// # Errors
    ///
    /// Rejects non-absolute paths.
    pub fn new(
        systemctl: PathBuf,
        ocserv: PathBuf,
        occtl: PathBuf,
        config: PathBuf,
    ) -> Result<Self, AdapterError> {
        for path in [&systemctl, &ocserv, &occtl, &config] {
            if !path.is_absolute() {
                return Err(AdapterError::InvalidResource);
            }
        }
        Ok(Self {
            systemctl,
            ocserv,
            occtl,
            config,
        })
    }
}

/// Adapter execution limits.
#[derive(Clone, Copy, Debug)]
pub struct Limits {
    /// Maximum child execution time.
    pub timeout: Duration,
    /// Maximum retained bytes per stdout/stderr stream.
    pub output_bytes: usize,
}

impl Default for Limits {
    fn default() -> Self {
        Self {
            timeout: Duration::from_secs(5),
            output_bytes: DEFAULT_OUTPUT_BYTES,
        }
    }
}

/// Read-only Ocserv adapter.
#[derive(Clone, Debug)]
pub struct Adapter {
    resources: FixedResources,
    limits: Limits,
}

impl Adapter {
    /// Creates an adapter with fixed trusted resources.
    #[must_use]
    pub const fn new(resources: FixedResources, limits: Limits) -> Self {
        Self { resources, limits }
    }

    /// Returns the fixed `ocserv.service` state.
    ///
    /// # Errors
    ///
    /// Returns a stable adapter error for child or parser failures.
    pub async fn service_status(&self) -> Result<ServiceStatus, AdapterError> {
        let output = self
            .execute(
                &self.resources.systemctl,
                &[
                    "show",
                    "ocserv.service",
                    "--property=LoadState",
                    "--property=ActiveState",
                    "--property=SubState",
                    "--no-pager",
                ],
            )
            .await?;
        parse_service_status(&output.stdout)
    }

    /// Returns the installed Ocserv version.
    ///
    /// # Errors
    ///
    /// Returns a stable adapter error for child or parser failures.
    pub async fn ocserv_version(&self) -> Result<OcservVersion, AdapterError> {
        let output = self.execute(&self.resources.ocserv, &["--version"]).await?;
        // Packaged Ocserv writes its banner to stderr; retain stdout compatibility.
        let banner = if output.stderr.is_empty() {
            &output.stdout
        } else {
            &output.stderr
        };
        parse_version(banner)
    }

    /// Returns current sessions parsed into a stable DTO.
    ///
    /// # Errors
    ///
    /// Returns a stable adapter error for child or parser failures.
    pub async fn session_list(&self) -> Result<SessionList, AdapterError> {
        let output = self
            .execute(&self.resources.occtl, &["--json", "show", "users"])
            .await?;
        parse_sessions(&output.stdout)
    }

    /// Returns current IP bans parsed into a stable DTO.
    ///
    /// # Errors
    ///
    /// Returns a stable adapter error for child or parser failures.
    pub async fn ip_ban_list(&self) -> Result<IpBanList, AdapterError> {
        let output = self
            .execute(&self.resources.occtl, &["--json", "show", "ip", "bans"])
            .await?;
        parse_ip_bans(&output.stdout)
    }

    /// Fingerprints the fixed Ocserv config with a strict size bound.
    ///
    /// # Errors
    ///
    /// Returns a stable adapter error for missing, unreadable, or oversized data.
    pub async fn config_fingerprint(&self) -> Result<ConfigFingerprint, AdapterError> {
        let file = tokio::fs::File::open(&self.resources.config)
            .await
            .map_err(AdapterError::Io)?;
        let mut bytes = Vec::with_capacity(4096);
        file.take(u64::try_from(MAX_CONFIG_BYTES + 1).map_err(|_| AdapterError::OutputLimit)?)
            .read_to_end(&mut bytes)
            .await
            .map_err(AdapterError::Io)?;
        if bytes.len() > MAX_CONFIG_BYTES {
            return Err(AdapterError::OutputLimit);
        }
        Ok(ConfigFingerprint {
            sha256: hex::encode(Sha256::digest(&bytes)),
            size_bytes: u64::try_from(bytes.len()).map_err(|_| AdapterError::OutputLimit)?,
        })
    }

    async fn execute(&self, program: &Path, args: &[&str]) -> Result<ChildOutput, AdapterError> {
        let mut command = Command::new(program);
        command
            .args(args)
            .process_group(0)
            .stdin(Stdio::null())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .kill_on_drop(true);
        let mut child = command.spawn().map_err(AdapterError::Io)?;
        let stdout = child.stdout.take().ok_or(AdapterError::Unavailable)?;
        let stderr = child.stderr.take().ok_or(AdapterError::Unavailable)?;
        let stdout_task = tokio::spawn(read_bounded(stdout, self.limits.output_bytes));
        let stderr_task = tokio::spawn(read_bounded(stderr, self.limits.output_bytes));
        let status =
            if let Ok(result) = tokio::time::timeout(self.limits.timeout, child.wait()).await {
                result.map_err(AdapterError::Io)?
            } else {
                kill_process_group(&child);
                let _ = child.wait().await;
                return Err(AdapterError::DeadlineExceeded);
            };
        let stdout = stdout_task.await.map_err(|_| AdapterError::Unavailable)??;
        let stderr = stderr_task.await.map_err(|_| AdapterError::Unavailable)??;
        if stdout.exceeded || stderr.exceeded {
            return Err(AdapterError::OutputLimit);
        }
        if !status.success() {
            return Err(AdapterError::CommandFailed {
                code: status.code(),
            });
        }
        Ok(ChildOutput {
            stdout: stdout.bytes,
            stderr: stderr.bytes,
        })
    }
}

fn kill_process_group(child: &tokio::process::Child) {
    let Some(raw_pid) = child.id().and_then(|pid| i32::try_from(pid).ok()) else {
        return;
    };
    let Some(pid) = rustix::process::Pid::from_raw(raw_pid) else {
        return;
    };
    let _ = rustix::process::kill_process_group(pid, rustix::process::Signal::KILL);
}

#[derive(Debug)]
struct ChildOutput {
    stdout: Vec<u8>,
    stderr: Vec<u8>,
}

#[derive(Debug)]
struct BoundedOutput {
    bytes: Vec<u8>,
    exceeded: bool,
}

async fn read_bounded(
    mut reader: impl AsyncRead + Unpin,
    limit: usize,
) -> Result<BoundedOutput, AdapterError> {
    let mut bytes = Vec::with_capacity(limit.min(4096));
    let mut buffer = [0_u8; 4096];
    let mut exceeded = false;
    loop {
        let count = reader.read(&mut buffer).await.map_err(AdapterError::Io)?;
        if count == 0 {
            break;
        }
        let remaining = limit.saturating_sub(bytes.len());
        if count > remaining {
            bytes.extend_from_slice(&buffer[..remaining]);
            exceeded = true;
        } else {
            bytes.extend_from_slice(&buffer[..count]);
        }
    }
    Ok(BoundedOutput { bytes, exceeded })
}

/// Stable adapter failures.
#[derive(Debug)]
pub enum AdapterError {
    /// Trusted resources must be absolute paths.
    InvalidResource,
    /// Fixed local resource was unavailable.
    Unavailable,
    /// Child exceeded its deadline.
    DeadlineExceeded,
    /// stdout or stderr exceeded the configured bound.
    OutputLimit,
    /// Fixed child returned nonzero.
    CommandFailed { code: Option<i32> },
    /// Output did not match a supported version fixture.
    MalformedOutput,
    /// Local I/O failure.
    Io(io::Error),
}

impl std::fmt::Display for AdapterError {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::InvalidResource => write!(formatter, "fixed resource path invalid"),
            Self::Unavailable => write!(formatter, "fixed local resource unavailable"),
            Self::DeadlineExceeded => write!(formatter, "fixed read operation timed out"),
            Self::OutputLimit => write!(formatter, "fixed read operation output limit exceeded"),
            Self::CommandFailed { code } => {
                write!(formatter, "fixed read operation failed (status {code:?})")
            }
            Self::MalformedOutput => write!(formatter, "ocserv output format unsupported"),
            Self::Io(error) => write!(formatter, "fixed local resource failed: {error}"),
        }
    }
}

impl std::error::Error for AdapterError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Self::Io(error) => Some(error),
            _ => None,
        }
    }
}

impl From<AdapterError> for PrivdError {
    fn from(error: AdapterError) -> Self {
        let kind = match &error {
            AdapterError::InvalidResource | AdapterError::MalformedOutput => {
                ErrorKind::MalformedOutput
            }
            AdapterError::Unavailable | AdapterError::Io(_) => ErrorKind::Unavailable,
            AdapterError::DeadlineExceeded => ErrorKind::DeadlineExceeded,
            AdapterError::OutputLimit => ErrorKind::OutputLimit,
            AdapterError::CommandFailed { .. } => ErrorKind::CommandFailed,
        };
        Self {
            kind: kind.into(),
            detail: error.to_string().chars().take(256).collect(),
        }
    }
}

fn utf8(bytes: &[u8]) -> Result<&str, AdapterError> {
    std::str::from_utf8(bytes).map_err(|_| AdapterError::MalformedOutput)
}

/// Parses the fixed `systemctl show` output.
///
/// # Errors
///
/// Returns `MalformedOutput` unless all fixed fields are present and valid.
pub fn parse_service_status(bytes: &[u8]) -> Result<ServiceStatus, AdapterError> {
    let mut values = HashMap::new();
    for line in utf8(bytes)?.lines().filter(|line| !line.is_empty()) {
        let (key, value) = line.split_once('=').ok_or(AdapterError::MalformedOutput)?;
        if values.insert(key, value).is_some() || value.len() > 64 {
            return Err(AdapterError::MalformedOutput);
        }
    }
    Ok(ServiceStatus {
        load_state: required_value(&values, "LoadState")?,
        active_state: required_value(&values, "ActiveState")?,
        sub_state: required_value(&values, "SubState")?,
    })
}

fn required_value(values: &HashMap<&str, &str>, key: &str) -> Result<String, AdapterError> {
    let value = values.get(key).ok_or(AdapterError::MalformedOutput)?;
    if value.is_empty()
        || !value
            .bytes()
            .all(|byte| byte.is_ascii_lowercase() || byte == b'-')
    {
        return Err(AdapterError::MalformedOutput);
    }
    Ok((*value).to_owned())
}

/// Parses supported Ocserv 1.2 and 1.3 version output.
///
/// # Errors
///
/// Returns `MalformedOutput` for unsupported or invalid version text.
pub fn parse_version(bytes: &[u8]) -> Result<OcservVersion, AdapterError> {
    let text = utf8(bytes)?.trim();
    if text.len() > 4096 {
        return Err(AdapterError::MalformedOutput);
    }
    let first_line = text.lines().next().ok_or(AdapterError::MalformedOutput)?;
    let version = first_line
        .strip_prefix("ocserv ")
        .or_else(|| first_line.strip_prefix("OpenConnect VPN Server "))
        .ok_or(AdapterError::MalformedOutput)?;
    if !(version.starts_with("1.2.") || version.starts_with("1.3.") || version.starts_with("1.4."))
        || version.len() > 32
        || !version
            .bytes()
            .all(|byte| byte.is_ascii_digit() || byte == b'.')
    {
        return Err(AdapterError::MalformedOutput);
    }
    Ok(OcservVersion {
        version: version.to_owned(),
    })
}

/// Parses the versioned `occtl --json show users` format.
///
/// # Errors
///
/// Returns `MalformedOutput` for invalid headers, fields, addresses, or bounds.
pub fn parse_sessions(bytes: &[u8]) -> Result<SessionList, AdapterError> {
    let value: serde_json::Value =
        serde_json::from_slice(bytes).map_err(|_| AdapterError::MalformedOutput)?;
    let items = value.as_array().ok_or(AdapterError::MalformedOutput)?;
    if items.len() > 4096 {
        return Err(AdapterError::MalformedOutput);
    }
    let mut sessions = Vec::new();
    for item in items {
        let object = item.as_object().ok_or(AdapterError::MalformedOutput)?;
        let id = object
            .get("ID")
            .and_then(serde_json::Value::as_u64)
            .filter(|id| *id > 0)
            .ok_or(AdapterError::MalformedOutput)?
            .to_string();
        let username = validated_token(json_string(object, "Username")?, 128)?;
        let remote_ip = canonical_ip(json_string(object, "Remote IP")?)?;
        let vpn_ip = object
            .get("IPv4")
            .or_else(|| object.get("IPv6"))
            .and_then(serde_json::Value::as_str)
            .ok_or(AdapterError::MalformedOutput)
            .and_then(canonical_ip)?;
        sessions.push(Session {
            id,
            username,
            remote_ip,
            vpn_ip,
        });
    }
    Ok(SessionList { sessions })
}

/// Parses the versioned `occtl --json show ip bans` format.
///
/// # Errors
///
/// Returns `MalformedOutput` for invalid headers, fields, addresses, or bounds.
pub fn parse_ip_bans(bytes: &[u8]) -> Result<IpBanList, AdapterError> {
    let value: serde_json::Value =
        serde_json::from_slice(bytes).map_err(|_| AdapterError::MalformedOutput)?;
    let items = value.as_array().ok_or(AdapterError::MalformedOutput)?;
    if items.len() > 4096 {
        return Err(AdapterError::MalformedOutput);
    }
    let mut bans = Vec::new();
    for item in items {
        let object = item.as_object().ok_or(AdapterError::MalformedOutput)?;
        bans.push(IpBan {
            ip: canonical_ip(json_string(object, "IP")?)?,
            seconds_remaining: None,
        });
    }
    Ok(IpBanList { bans })
}

fn json_string<'a>(
    object: &'a serde_json::Map<String, serde_json::Value>,
    name: &str,
) -> Result<&'a str, AdapterError> {
    object
        .get(name)
        .and_then(serde_json::Value::as_str)
        .ok_or(AdapterError::MalformedOutput)
}

fn validated_token(value: &str, limit: usize) -> Result<String, AdapterError> {
    if value.is_empty() || value.len() > limit || value.chars().any(char::is_control) {
        return Err(AdapterError::MalformedOutput);
    }
    Ok(value.to_owned())
}

fn canonical_ip(value: &str) -> Result<String, AdapterError> {
    value
        .parse::<IpAddr>()
        .map(|ip| ip.to_string())
        .map_err(|_| AdapterError::MalformedOutput)
}

#[cfg(test)]
mod tests {
    use std::os::unix::fs::PermissionsExt;

    use super::*;

    fn executable(name: &str, body: &str) -> PathBuf {
        let directory =
            std::env::temp_dir().join(format!("ocservia-adapter-{}", uuid::Uuid::now_v7()));
        std::fs::create_dir_all(&directory).expect("create fixture directory");
        let path = directory.join(name);
        std::fs::write(&path, format!("#!/bin/sh\n{body}\n")).expect("write fixture program");
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o700))
            .expect("make fixture executable");
        path
    }

    #[test]
    fn version_fixtures_and_malformed_output() {
        assert_eq!(
            parse_version(b"ocserv 1.2.4\n")
                .expect("1.2 fixture")
                .version,
            "1.2.4"
        );
        assert_eq!(
            parse_version(
                b"OpenConnect VPN Server 1.3.0\n\nCompiled with: seccomp, PAM\nGnuTLS version: 3.8.9\n"
            )
                .expect("1.3 fixture")
                .version,
            "1.3.0"
        );
        assert_eq!(
            parse_version(b"ocserv 1.4.1\n")
                .expect("1.4 fixture")
                .version,
            "1.4.1"
        );
        assert!(parse_version(b"ocserv development\n").is_err());
        assert!(parse_version(&[0xff]).is_err());
    }

    #[tokio::test]
    async fn reads_packaged_version_banner_from_stderr() {
        let version_program = executable(
            "ocserv-version",
            "printf 'OpenConnect VPN Server 1.3.0\\n' >&2",
        );
        let fixture_directory = version_program.parent().expect("fixture parent").to_owned();
        let resources = FixedResources::new(
            PathBuf::from("/bin/false"),
            version_program,
            PathBuf::from("/bin/false"),
            PathBuf::from("/etc/hosts"),
        )
        .expect("fixed resources");
        let version = Adapter::new(resources, Limits::default())
            .ocserv_version()
            .await
            .expect("stderr version banner");
        assert_eq!(version.version, "1.3.0");
        std::fs::remove_dir_all(fixture_directory).expect("remove version fixture");
    }

    #[test]
    fn parses_session_and_ban_fixtures() {
        for (users, bans) in [
            (
                include_bytes!("../fixtures/ocserv-1.2/users.json").as_slice(),
                include_bytes!("../fixtures/ocserv-1.2/bans.json").as_slice(),
            ),
            (
                include_bytes!("../fixtures/ocserv-1.3/users.json").as_slice(),
                include_bytes!("../fixtures/ocserv-1.3/bans.json").as_slice(),
            ),
            (
                include_bytes!("../fixtures/ocserv-1.4/users.json").as_slice(),
                include_bytes!("../fixtures/ocserv-1.4/bans.json").as_slice(),
            ),
        ] {
            let sessions = parse_sessions(users).expect("sessions fixture");
            assert_eq!(sessions.sessions.len(), 2);
            assert_eq!(sessions.sessions[1].remote_ip, "2001:db8::1");
            let bans = parse_ip_bans(bans).expect("bans fixture");
            assert_eq!(bans.bans.len(), 2);
            assert_eq!(bans.bans[1].seconds_remaining, None);
        }
        assert!(parse_ip_bans(b"[{\"IP\":\"192.0.2.1\"},]").is_err());
    }

    #[test]
    fn status_requires_exact_keys() {
        let status =
            parse_service_status(b"LoadState=loaded\nActiveState=active\nSubState=running\n")
                .expect("valid status");
        assert_eq!(status.active_state, "active");
        assert!(parse_service_status(b"ActiveState=active\n").is_err());
    }

    #[tokio::test]
    async fn child_timeout_and_output_limit_are_enforced() {
        let timeout_program = executable("slow-ocserv", "while :; do :; done");
        let timeout_directory = timeout_program.parent().expect("fixture parent").to_owned();
        let resources = FixedResources::new(
            PathBuf::from("/bin/false"),
            timeout_program,
            PathBuf::from("/bin/false"),
            PathBuf::from("/etc/hosts"),
        )
        .expect("fixed resources");
        let adapter = Adapter::new(
            resources,
            Limits {
                timeout: Duration::from_millis(50),
                output_bytes: 1024,
            },
        );
        assert!(matches!(
            adapter.ocserv_version().await,
            Err(AdapterError::DeadlineExceeded)
        ));
        std::fs::remove_dir_all(timeout_directory).expect("remove timeout fixture");

        let noisy_program = executable("noisy-ocserv", "yes x | head -c 4096");
        let noisy_directory = noisy_program.parent().expect("fixture parent").to_owned();
        let resources = FixedResources::new(
            PathBuf::from("/bin/false"),
            noisy_program,
            PathBuf::from("/bin/false"),
            PathBuf::from("/etc/hosts"),
        )
        .expect("fixed resources");
        let adapter = Adapter::new(
            resources,
            Limits {
                timeout: Duration::from_secs(1),
                output_bytes: 1024,
            },
        );
        assert!(matches!(
            adapter.ocserv_version().await,
            Err(AdapterError::OutputLimit)
        ));
        std::fs::remove_dir_all(noisy_directory).expect("remove noisy fixture");
    }
}
