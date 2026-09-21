//! Finite complete-config/v1 identity. No caller-selected filesystem paths.

use crate::generated::ocserv::platform::agent::v1::{
    CompleteConfigCandidate, NodeLocalTlsReference, complete_config_directive::Value,
};
use std::net::Ipv4Addr;

pub const PLAN_CAPABILITY: &str = "ocserv.config.complete.plan";
pub const APPLY_CAPABILITY: &str = "ocserv.config.complete.apply";
pub const PROVIDER: &str = "node-local-tls-v1";
const REQUIRED: [&str; 12] = [
    "auth",
    "cookie-timeout",
    "device",
    "dns",
    "ipv4-network",
    "max-clients",
    "max-same-clients",
    "server-cert",
    "server-key",
    "socket-file",
    "tcp-port",
    "udp-port",
];

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct InvalidProfile;

fn label(value: &str, max: usize) -> bool {
    !value.is_empty()
        && value.len() <= max
        && !value.contains("..")
        && value
            .bytes()
            .enumerate()
            .all(|(i, b)| b.is_ascii_alphanumeric() || i > 0 && matches!(b, b'.' | b'_' | b'-'))
}

fn network(value: &str) -> bool {
    let Some((address, bits)) = value.split_once('/') else {
        return false;
    };
    let Ok(address) = address.parse::<Ipv4Addr>() else {
        return false;
    };
    let Ok(bits) = bits.parse::<u32>() else {
        return false;
    };
    (1..=30).contains(&bits)
        && format!("{address}/{bits}") == value
        && u32::from(address) & ((1_u32 << (32 - bits)) - 1) == 0
}

fn literal(name: &str, value: &str) -> bool {
    match name {
        "auth" => value == "plain[passwd=/etc/ocserv/ocpasswd]",
        "socket-file" => value == "/run/ocserv.socket",
        "device" => label(value, 15) && !value.contains('.'),
        "ipv4-network" => network(value),
        "route" => value == "default" || network(value),
        "dns" => value
            .parse::<Ipv4Addr>()
            .is_ok_and(|ip| ip.to_string() == value && !ip.is_unspecified() && !ip.is_multicast()),
        "tcp-port" | "udp-port" | "max-clients" | "max-same-clients" | "cookie-timeout" => {
            let Ok(number) = value.parse::<u32>() else {
                return false;
            };
            if number.to_string() != value {
                return false;
            }
            if name == "cookie-timeout" {
                (60..=86400).contains(&number)
            } else {
                number <= 65535 && (number > 0 || name == "udp-port")
            }
        }
        _ => false,
    }
}

fn append_length(out: &mut Vec<u8>, value: &[u8]) -> Result<(), InvalidProfile> {
    out.extend_from_slice(
        &u32::try_from(value.len())
            .map_err(|_| InvalidProfile)?
            .to_be_bytes(),
    );
    out.extend_from_slice(value);
    Ok(())
}

/// Returns the exact Go/Rust canonical transcript, with all finite-profile checks.
///
/// # Errors
/// Rejects missing, duplicate, unsorted or unsafe directives and unpinned TLS.
pub fn canonical(candidate: &CompleteConfigCandidate) -> Result<Vec<u8>, InvalidProfile> {
    if candidate.node_id.len() != 16
        || candidate.node_id.iter().all(|byte| *byte == 0)
        || candidate.expected_revision > i64::MAX as u64
        || !(REQUIRED.len()..=REQUIRED.len() + 2).contains(&candidate.directives.len())
    {
        return Err(InvalidProfile);
    }
    let mut out = b"ocservia.complete-config.v1\0".to_vec();
    out.extend_from_slice(&candidate.node_id);
    out.extend_from_slice(&candidate.expected_revision.to_be_bytes());
    out.extend_from_slice(
        &u32::try_from(candidate.directives.len())
            .map_err(|_| InvalidProfile)?
            .to_be_bytes(),
    );
    let mut previous = "";
    let mut server_ref: Option<&NodeLocalTlsReference> = None;
    let mut max_clients = 0;
    let mut max_same = 0;
    for directive in &candidate.directives {
        if directive.name.as_str() <= previous {
            return Err(InvalidProfile);
        }
        previous = &directive.name;
        append_length(&mut out, directive.name.as_bytes())?;
        match directive.value.as_ref().ok_or(InvalidProfile)? {
            Value::Literal(value) => {
                if !literal(&directive.name, value) {
                    return Err(InvalidProfile);
                }
                out.push(0);
                append_length(&mut out, value.as_bytes())?;
                if directive.name == "max-clients" {
                    max_clients = value.parse::<u32>().map_err(|_| InvalidProfile)?;
                }
                if directive.name == "max-same-clients" {
                    max_same = value.parse::<u32>().map_err(|_| InvalidProfile)?;
                }
            }
            Value::Tls(reference) => {
                if !matches!(
                    directive.name.as_str(),
                    "server-cert" | "server-key" | "ca-cert"
                ) || reference.secret_ref_id.len() != 16
                    || reference.secret_ref_id.iter().all(|b| *b == 0)
                    || !label(&reference.version, 64)
                    || reference.certificate_sha256.len() != 32
                    || reference.spki_sha256.len() != 32
                    || !matches!(reference.ca_sha256.len(), 0 | 32)
                    || directive.name == "ca-cert" && reference.ca_sha256.len() != 32
                    || server_ref.is_some_and(|current| current != reference)
                {
                    return Err(InvalidProfile);
                }
                server_ref = Some(reference);
                out.push(1);
                out.extend_from_slice(&reference.secret_ref_id);
                append_length(&mut out, reference.version.as_bytes())?;
                out.extend_from_slice(&reference.certificate_sha256);
                out.extend_from_slice(&reference.spki_sha256);
                append_length(&mut out, &reference.ca_sha256)?;
            }
        }
    }
    if max_same > max_clients
        || REQUIRED
            .iter()
            .any(|name| !candidate.directives.iter().any(|d| d.name == *name))
    {
        return Err(InvalidProfile);
    }
    Ok(out)
}
