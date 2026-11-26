use std::collections::HashMap;
use std::io;
use std::net::IpAddr;

use ocservia_agent_protocol::{
    ErrorKind, IpBan, IpBanList, OcservVersion, PrivdError, ServiceStatus, Session, SessionList,
};

/// Stable adapter failures.
#[derive(Debug)]
pub enum AdapterError {
    /// Trusted resources must be absolute paths.
    InvalidResource,
    /// A fixed resource failed a metadata check; values contain no file contents.
    ResourceMetadata {
        resource: &'static str,
        reason: &'static str,
        actual: u64,
        expected: u64,
    },
    /// A fixed resource could not be opened without following links.
    ResourceIo {
        resource: &'static str,
        reason: &'static str,
        source: io::Error,
    },
    /// A password record failed parsing; only physical positions and counts are retained.
    UserFileFormat {
        reason: &'static str,
        line: usize,
        fields: usize,
        first_line: Option<usize>,
    },
    /// An operation argument was not in its canonical typed form.
    InvalidRequest,
    /// A signed connection-owner term is older than the durable root floor.
    StaleOwnerEpoch,
    /// A session identifier belongs to a prior host boot.
    StaleBoot,
    /// Fixed local resource was unavailable.
    Unavailable,
    /// Child exceeded its deadline.
    DeadlineExceeded,
    /// stdout or stderr exceeded the configured bound.
    OutputLimit,
    /// A desired mutation would exceed a bounded authoritative-state capacity.
    CapacityExceeded,
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
            Self::ResourceMetadata {
                resource,
                reason,
                actual,
                expected,
            } => {
                if *reason == "mode" || *reason == "writable_mode_bits" {
                    write!(
                        formatter,
                        "resource={resource} reason={reason} actual={actual:04o} expected={expected:04o}"
                    )
                } else {
                    write!(
                        formatter,
                        "resource={resource} reason={reason} actual={actual} expected={expected}"
                    )
                }
            }
            Self::ResourceIo {
                resource,
                reason,
                source,
            } => write!(formatter, "resource={resource} reason={reason}: {source}"),
            Self::UserFileFormat {
                reason,
                line,
                fields,
                first_line,
            } => {
                write!(
                    formatter,
                    "resource=ocpasswd reason={reason} line={line} fields={fields} expected_fields=3"
                )?;
                if let Some(first) = first_line {
                    write!(formatter, " first_line={first}")?;
                }
                Ok(())
            }
            Self::InvalidRequest => write!(formatter, "fixed operation argument invalid"),
            Self::StaleOwnerEpoch => write!(formatter, "connection-owner term is stale"),
            Self::StaleBoot => write!(formatter, "session boot identity is stale"),
            Self::Unavailable => write!(formatter, "fixed local resource unavailable"),
            Self::DeadlineExceeded => write!(formatter, "fixed read operation timed out"),
            Self::OutputLimit => write!(formatter, "fixed read operation output limit exceeded"),
            Self::CapacityExceeded => write!(formatter, "authoritative state capacity exceeded"),
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
            Self::Io(error) | Self::ResourceIo { source: error, .. } => Some(error),
            _ => None,
        }
    }
}

impl From<AdapterError> for PrivdError {
    fn from(error: AdapterError) -> Self {
        let kind = match &error {
            AdapterError::InvalidResource
            | AdapterError::MalformedOutput
            | AdapterError::ResourceMetadata { .. }
            | AdapterError::UserFileFormat { .. } => ErrorKind::MalformedOutput,
            AdapterError::StaleOwnerEpoch => ErrorKind::PermissionDenied,
            AdapterError::InvalidRequest | AdapterError::StaleBoot => ErrorKind::InvalidRequest,
            AdapterError::Unavailable | AdapterError::Io(_) | AdapterError::ResourceIo { .. } => {
                ErrorKind::Unavailable
            }
            AdapterError::DeadlineExceeded => ErrorKind::DeadlineExceeded,
            AdapterError::OutputLimit => ErrorKind::OutputLimit,
            AdapterError::CapacityExceeded => ErrorKind::CapacityExceeded,
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

/// Parses supported Ocserv version output.
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
    if !(version.starts_with("1.2.")
        || version.starts_with("1.3.")
        || version.starts_with("1.4.")
        || version == "1.5.0"
        || version == "1.6.0")
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
        let connected_at = object
            .get("Connected at")
            .or_else(|| object.get("Session started at"))
            .map(|value| {
                let (date, minute) = value
                    .as_str()
                    .and_then(|s| s.split_once(' '))
                    .ok_or(AdapterError::MalformedOutput)?;
                format!("{date}T{minute}:00Z")
                    .parse()
                    .map_err(|_| AdapterError::MalformedOutput)
            })
            .transpose()?;
        let counter = |key: &str| {
            object
                .get(key)
                .map(|v| {
                    v.as_u64()
                        .or_else(|| v.as_str()?.parse::<u64>().ok())
                        .filter(|n| i64::try_from(*n).is_ok())
                        .ok_or(AdapterError::MalformedOutput)
                })
                .transpose()
        };
        sessions.push(Session {
            connected_at,
            bytes_in: counter("RX")?,
            bytes_out: counter("TX")?,
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

pub(crate) fn canonical_ip(value: &str) -> Result<String, AdapterError> {
    value
        .parse::<IpAddr>()
        .map(|ip| ip.to_string())
        .map_err(|_| AdapterError::MalformedOutput)
}
