//! Shared typed validation for the agent upgrade release identity.
//!
//! The Controller, Agent, and privd all bind the same immutable release
//! identity (`target_version`, `package_sha256`, `architecture`). The helpers
//! below are the single Rust definition of that grammar; the Go control plane
//! mirrors them exactly in `internal/semanticpayload`.

#![forbid(unsafe_code)]

/// Required session capability for the typed agent upgrade command.
pub const AGENT_UPGRADE_CAPABILITY: &str = "ocserv.agent.upgrade.v1";

/// Release package architectures of the published native agent packages.
pub const AGENT_UPGRADE_ARCHITECTURES: &[&str] = &["amd64", "arm64"];

/// Maximum accepted `target_version` length in bytes.
const MAX_TARGET_VERSION_BYTES: usize = 128;

/// Reports whether `version` is a strict `SemVer` 2.0.0 version without the
/// Go module `v` prefix.
#[must_use]
pub fn valid_target_version(version: &str) -> bool {
    if version.len() < 5 || version.len() > MAX_TARGET_VERSION_BYTES {
        return false;
    }
    let mut core = version;
    if let Some(index) = version.find('+') {
        if !valid_identifier_list(&version[index + 1..], false) {
            return false;
        }
        core = &core[..index];
    }
    if let Some(index) = core.find('-') {
        if !valid_identifier_list(&core[index + 1..], true) {
            return false;
        }
        core = &core[..index];
    }
    let mut parts = core.split('.');
    let (Some(major), Some(minor), Some(patch), None) =
        (parts.next(), parts.next(), parts.next(), parts.next())
    else {
        return false;
    };
    valid_number(major) && valid_number(minor) && valid_number(patch)
}

/// Reports whether `architecture` is one of the supported package
/// architectures.
#[must_use]
pub fn valid_architecture(architecture: &str) -> bool {
    AGENT_UPGRADE_ARCHITECTURES.contains(&architecture)
}

/// Returns the release package architecture of the running process target, or
/// `None` on targets outside the published package matrix.
#[must_use]
pub fn runtime_architecture() -> Option<&'static str> {
    match std::env::consts::ARCH {
        "x86_64" => Some("amd64"),
        "aarch64" => Some("arm64"),
        _ => None,
    }
}

fn valid_identifier_list(value: &str, prerelease: bool) -> bool {
    if value.is_empty() {
        return false;
    }
    value.split('.').all(|identifier| {
        if identifier.is_empty() {
            return false;
        }
        let numeric = identifier.bytes().all(|byte| byte.is_ascii_digit());
        if numeric {
            // Prerelease numeric identifiers must not carry leading zeros;
            // build identifiers are permissive per SemVer 2.0.0.
            return !prerelease || valid_number(identifier);
        }
        identifier
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || byte == b'-')
    })
}

fn valid_number(value: &str) -> bool {
    !(value.is_empty()
        || (value.len() > 1 && value.starts_with('0'))
        || !value.bytes().all(|byte| byte.is_ascii_digit()))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn accepts_strict_semver_target_versions() {
        for version in [
            "0.0.0",
            "1.2.3",
            "10.20.30",
            "1.0.0-alpha",
            "1.0.0-alpha.1",
            "1.0.0-0.3.7",
            "1.0.0-x.7.z.92",
            "1.0.0+build.01",
            "1.0.0-rc.1+build.5",
        ] {
            assert!(valid_target_version(version), "expected valid: {version}");
        }
    }

    #[test]
    fn rejects_non_semver_target_versions() {
        for version in [
            "",
            "1",
            "1.2",
            "1.2.3.4",
            "01.2.3",
            "1.02.3",
            "1.2.03",
            "v1.2.3",
            "1.2.3-01",
            "1.2.3-",
            "1.2.3+",
            "1.2.3-alpha..1",
            "1.2.3-alpha_1",
            "latest",
            &"1".repeat(200),
        ] {
            assert!(
                !valid_target_version(version),
                "expected invalid: {version}"
            );
        }
    }

    #[test]
    fn architecture_matrix_is_fixed() {
        assert!(valid_architecture("amd64"));
        assert!(valid_architecture("arm64"));
        for architecture in ["", "x86_64", "aarch64", "AMD64", "arm64 "] {
            assert!(!valid_architecture(architecture));
        }
        assert_eq!(AGENT_UPGRADE_CAPABILITY, "ocserv.agent.upgrade.v1");
    }
}
