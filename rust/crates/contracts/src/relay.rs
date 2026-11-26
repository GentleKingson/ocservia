//! Shared Iroh relay mode, TLS root loading, DNS resolver, and protected token helpers.

use std::io;
use std::os::unix::fs::{MetadataExt as _, OpenOptionsExt as _};
use std::path::Path;

use iroh::endpoint::RelayMode;
use iroh::{RelayMap, RelayUrl};
use rustls_pki_types::pem::PemObject as _;
use zeroize::Zeroizing;

use crate::unix::O_NOFOLLOW;

fn invalid(detail: &str) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidInput, detail)
}

/// Opens `path` with `O_NOFOLLOW`, runs caller-supplied file metadata validation,
/// reads up to `max_bytes + 1` bytes, and returns the trimmed UTF-8 token.
///
/// # Errors
///
/// Returns validation errors from `validate_file`, I/O errors, or `InvalidInput`
/// on non-UTF-8 content.
pub fn read_protected_token_file(
    path: &Path,
    max_bytes: u64,
    label: &str,
    validate_file: impl FnOnce(&std::fs::File) -> io::Result<()>,
) -> io::Result<String> {
    let mut file = std::fs::OpenOptions::new()
        .read(true)
        .custom_flags(O_NOFOLLOW)
        .open(path)?;
    validate_file(&file)?;
    let capacity = usize::try_from(max_bytes).unwrap_or(1024);
    let mut raw = Vec::with_capacity(capacity);
    std::io::Read::read_to_end(
        &mut std::io::Read::take(&mut file, max_bytes.saturating_add(1)),
        &mut raw,
    )?;
    let raw = Zeroizing::new(raw);
    let token = std::str::from_utf8(&raw)
        .map_err(|_| invalid(&format!("{label} must be UTF-8")))?
        .trim_end_matches(['\n', '\r']);
    Ok(token.to_owned())
}

/// Validates a process-owned (`0600`) or `root:<expected_group>` (`0640`) regular token file.
///
/// # Errors
///
/// Returns `InvalidInput` if non-regular or `PermissionDenied` if mode/ownership fails.
pub fn validate_owner_or_root_group_token_file(
    file: &std::fs::File,
    expected_owner: u32,
    expected_group: u32,
    label: &str,
) -> io::Result<()> {
    let metadata = file.metadata()?;
    if !metadata.file_type().is_file() {
        return Err(invalid(&format!("{label} path must be a regular file")));
    }
    let mode = metadata.mode();
    let owner_only = metadata.uid() == expected_owner && mode.trailing_zeros() >= 6;
    let protected_group = metadata.uid() == 0
        && metadata.gid() == expected_group
        && mode & 0o027 == 0
        && mode & 0o040 != 0;
    if !owner_only && !protected_group {
        return Err(io::Error::new(
            io::ErrorKind::PermissionDenied,
            format!("{label} must be process-owned mode 0600 or root:agent mode 0640"),
        ));
    }
    Ok(())
}

/// Validates a single-link process-owned relay token file (owner-only or `0440` in `expected_group`).
///
/// # Errors
///
/// Returns `PermissionDenied` when file type, hard-link count, ownership, or mode is unsafe.
pub fn validate_owner_or_group_readonly_token_file(
    file: &std::fs::File,
    expected_owner: u32,
    expected_group: u32,
) -> io::Result<()> {
    let metadata = file.metadata()?;
    let owner_only = metadata.mode().trailing_zeros() >= 6;
    let controller_readable = metadata.gid() == expected_group && metadata.mode() & 0o7777 == 0o440;
    if !metadata.file_type().is_file()
        || metadata.nlink() != 1
        || metadata.uid() != expected_owner
        || !(owner_only || controller_readable)
    {
        return Err(io::Error::new(
            io::ErrorKind::PermissionDenied,
            "relay token must be a single-link process-owned file with owner-only permissions or mode 0440 in the process group",
        ));
    }
    Ok(())
}

/// Validates and builds the `RelayMode` (`disabled` or `custom` single credential-free HTTPS URL + token).
///
/// # Errors
///
/// Returns `InvalidInput` / `PermissionDenied` on invalid mode, URL, or token file.
pub fn build_relay_mode(
    mode: &str,
    raw_url: Option<&str>,
    token_file: Option<&Path>,
    validate_token_file: impl FnOnce(&std::fs::File) -> io::Result<()>,
) -> io::Result<RelayMode> {
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
            let token = Zeroizing::new(read_protected_token_file(
                token_file,
                512,
                "relay token",
                validate_token_file,
            )?);
            if !(32..=512).contains(&token.len()) || token.chars().any(char::is_whitespace) {
                return Err(invalid(
                    "relay token must be 32..512 non-whitespace UTF-8 bytes",
                ));
            }
            Ok(RelayMode::Custom(
                RelayMap::from_iter([url]).with_auth_token(token.to_string()),
            ))
        }
        _ => Err(invalid("relay mode must be disabled or custom")),
    }
}

/// Loads optional PEM relay CA certificates, rejecting unreadable, malformed, or empty files.
///
/// # Errors
///
/// Returns `InvalidInput` if the CA file cannot be read, parsed, or contains no certificates.
pub fn load_relay_tls_roots(
    relay_ca_file: Option<&Path>,
) -> io::Result<Vec<rustls_pki_types::CertificateDer<'static>>> {
    let Some(path) = relay_ca_file else {
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

/// Resolves Relay names through system DNS and hosts only, disabling public fallbacks.
#[must_use]
pub fn system_dns_resolver() -> iroh::dns::DnsResolver {
    iroh::dns::DnsResolver::builder()
        .with_system_defaults()
        .disable_fallback()
        .build()
}

/// Applies optional extra relay CA roots to an Iroh endpoint builder.
#[must_use]
pub fn with_relay_tls_roots(
    builder: iroh::endpoint::Builder,
    roots: Vec<rustls_pki_types::CertificateDer<'static>>,
) -> iroh::endpoint::Builder {
    if roots.is_empty() {
        builder
    } else {
        builder.ca_tls_config(iroh::tls::CaTlsConfig::default().with_extra_roots(roots))
    }
}
