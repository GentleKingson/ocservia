//! Shared typed validation for the agent upgrade release identity.
//!
//! The Controller, Agent, and privd all bind the same immutable release
//! identity (`target_version`, `package_sha256`, `architecture`). The helpers
//! below are the single Rust definition of that grammar; the Go control plane
//! mirrors them exactly in `internal/semanticpayload`.

#![forbid(unsafe_code)]

use std::cmp::Ordering;

/// Required session capability for the typed agent upgrade command. Version
/// `v2` is fence-capable: only runners that advertise it execute the
/// operation with the execution-time downgrade fence and the installation
/// commit record. The Controller refuses to schedule upgrades from nodes
/// that still advertise `ocserv.agent.upgrade.v1`, because a pre-fence
/// source runner would execute the first N -> N+1 hop unprotected.
pub const AGENT_UPGRADE_CAPABILITY: &str = "ocserv.agent.upgrade.v2";

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

/// Compile-time release identity of this installed package. Release builds
/// inject the published package version through `OCSERV_AGENT_RELEASE_VERSION`
/// so the Agent heartbeat, the upgrade fence, and the package MANIFEST all
/// report the same version; local development builds fall back to the crate
/// version.
#[must_use]
pub const fn release_version() -> &'static str {
    match option_env!("OCSERV_AGENT_RELEASE_VERSION") {
        Some(version) => version,
        None => env!("CARGO_PKG_VERSION"),
    }
}

/// Reports whether `target` is strictly newer than `current` under `SemVer`
/// 2.0.0 precedence (build metadata ignored). Both versions must be valid;
/// anything unparsable fails closed to `false`.
#[must_use]
pub fn is_strict_upgrade(current: &str, target: &str) -> bool {
    match (semver_core(current), semver_core(target)) {
        (Some(current), Some(target)) => semver_precedence(&target, &current).is_gt(),
        _ => false,
    }
}

type SemVerCore = ([u64; 3], Vec<Identifier>);

#[derive(Clone, Debug, PartialEq, Eq)]
enum Identifier {
    Numeric(String),
    Alphanumeric(String),
}

fn semver_core(version: &str) -> Option<SemVerCore> {
    if !valid_target_version(version) {
        return None;
    }
    // Build metadata never affects precedence.
    let precedence = version.split('+').next().unwrap_or_default();
    let (core, prerelease) = match precedence.split_once('-') {
        Some((core, prerelease)) => (core, prerelease),
        None => (precedence, ""),
    };
    let mut parts = core.split('.');
    let numbers = [
        parts.next()?.parse::<u64>().ok()?,
        parts.next()?.parse::<u64>().ok()?,
        parts.next()?.parse::<u64>().ok()?,
    ];
    if parts.next().is_some() {
        return None;
    }
    let identifiers = if prerelease.is_empty() {
        Vec::new()
    } else {
        prerelease
            .split('.')
            .map(|identifier| {
                // Keep numeric identifiers as their canonical digit string:
                // SemVer puts no upper bound on their magnitude, so they
                // must compare numerically beyond any fixed-width integer.
                if identifier.bytes().all(|byte| byte.is_ascii_digit()) {
                    Identifier::Numeric(identifier.to_owned())
                } else {
                    Identifier::Alphanumeric(identifier.to_owned())
                }
            })
            .collect()
    };
    Some((numbers, identifiers))
}

/// Ordering by `SemVer` 2.0.0 precedence: numeric core fields, then the
/// prerelease rules where a release outranks any of its prereleases.
fn semver_precedence(left: &SemVerCore, right: &SemVerCore) -> Ordering {
    for (l, r) in left.0.iter().zip(right.0.iter()) {
        if l != r {
            return l.cmp(r);
        }
    }
    semver_prerelease_order(&left.1, &right.1)
}

fn semver_prerelease_order(left: &[Identifier], right: &[Identifier]) -> Ordering {
    // A version without prerelease identifiers outranks one with them.
    if left.is_empty() || right.is_empty() {
        return left.len().cmp(&right.len()).reverse();
    }
    for (l, r) in left.iter().zip(right.iter()) {
        let order = match (l, r) {
            (Identifier::Numeric(l), Identifier::Numeric(r)) => {
                // Canonical digit strings (no leading zeros) compare
                // numerically by length and then lexicographically, which
                // stays exact for identifiers beyond any fixed-width
                // integer the same way the Go ordering does.
                l.len().cmp(&r.len()).then_with(|| l.cmp(r))
            }
            (Identifier::Numeric(_), Identifier::Alphanumeric(_)) => Ordering::Less,
            (Identifier::Alphanumeric(_), Identifier::Numeric(_)) => Ordering::Greater,
            (Identifier::Alphanumeric(l), Identifier::Alphanumeric(r)) => l.cmp(r),
        };
        if order != Ordering::Equal {
            return order;
        }
    }
    left.len().cmp(&right.len())
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
        assert_eq!(AGENT_UPGRADE_CAPABILITY, "ocserv.agent.upgrade.v2");
    }

    #[test]
    fn strict_upgrade_follows_semver_precedence() {
        for (current, target) in [
            ("0.1.0", "0.2.0"),
            ("0.1.9", "0.2.0"),
            ("1.2.3", "1.2.4"),
            ("1.9.9", "2.0.0"),
            ("1.0.0-alpha", "1.0.0"),
            ("1.0.0-alpha", "1.0.0-alpha.1"),
            ("1.0.0-alpha.1", "1.0.0-beta"),
            ("1.0.0-2", "1.0.0-10"),
            ("1.0.0", "1.0.1+build.9"),
        ] {
            assert!(
                is_strict_upgrade(current, target),
                "expected upgrade: {current} -> {target}"
            );
        }
    }

    #[test]
    fn strict_upgrade_rejects_replays_downgrades_and_malformed_versions() {
        for (current, target) in [
            ("1.2.3", "1.2.3"),
            ("1.2.3", "1.2.2"),
            ("2.0.0", "1.9.9"),
            ("1.0.0", "1.0.0-rc.1"),
            ("1.0.0-alpha.1", "1.0.0-alpha"),
            ("1.0.0-10", "1.0.0-2"),
            ("", "1.2.3"),
            ("1.2.3", ""),
            ("v1.2.3", "1.2.4"),
            ("1.2.3", "latest"),
        ] {
            assert!(
                !is_strict_upgrade(current, target),
                "expected refusal: {current} -> {target}"
            );
        }
    }

    #[test]
    fn release_version_is_a_valid_target_version() {
        // The injected identity must satisfy the same grammar as upgrade
        // targets; local builds fall back to the crate version.
        assert!(valid_target_version(release_version()));
    }

    #[test]
    fn strict_upgrade_matches_the_shared_cross_language_corpus() {
        // The Go admission path and this Rust execution-time fence must
        // answer the same ordering question identically; the shared corpus
        // pins both, including numeric prerelease identifiers beyond any
        // fixed-width integer.
        let raw = std::fs::read_to_string(
            std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
                .join("../../../testdata/agent-upgrade-strict-upgrade-v1.json"),
        )
        .expect("shared corpus readable");
        let corpus: serde_json::Value = serde_json::from_str(&raw).expect("shared corpus json");
        let cases = corpus
            .get("cases")
            .and_then(serde_json::Value::as_array)
            .expect("corpus cases");
        assert!(!cases.is_empty(), "corpus must not be empty");
        for case in cases {
            let name = case.get("name").and_then(serde_json::Value::as_str);
            let current = case.get("current").and_then(serde_json::Value::as_str);
            let target = case.get("target").and_then(serde_json::Value::as_str);
            let expected = case
                .get("strict_upgrade")
                .and_then(serde_json::Value::as_bool);
            let (Some(name), Some(current), Some(target), Some(expected)) =
                (name, current, target, expected)
            else {
                panic!("corpus case is incomplete: {case}");
            };
            assert!(
                valid_target_version(current) && valid_target_version(target),
                "corpus case {name} must use valid target versions"
            );
            assert_eq!(
                is_strict_upgrade(current, target),
                expected,
                "corpus case {name}: {current} -> {target}"
            );
        }
    }
}
