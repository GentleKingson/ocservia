//! Stable session capability policy shared by transportd and the Agent.

/// Capabilities permitted in grantless protocol 1.0 sessions.
///
/// This is deliberately an explicit allowlist. A newly introduced capability
/// ending in `.read` does not become available in compatibility mode until it
/// is reviewed and added here.
pub const READ_ONLY_SESSION_CAPABILITIES: &[&str] = &[
    "ocserv.config_fingerprint.read",
    "ocserv.ip_bans.read",
    "ocserv.sessions.read",
    "ocserv.status.read",
    "ocserv.version.read",
];

/// Returns whether a capability is permitted in a grantless read-only session.
#[must_use]
pub fn is_read_only_session_capability(capability: &str) -> bool {
    READ_ONLY_SESSION_CAPABILITIES
        .binary_search(&capability)
        .is_ok()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn read_only_session_allowlist_is_sorted_unique_and_closed() {
        assert!(READ_ONLY_SESSION_CAPABILITIES.is_sorted());
        assert!(
            READ_ONLY_SESSION_CAPABILITIES
                .windows(2)
                .all(|pair| pair[0] != pair[1])
        );
        assert!(is_read_only_session_capability("ocserv.status.read"));
        assert!(!is_read_only_session_capability("synthetic.noop"));
        assert!(!is_read_only_session_capability("future.feature.read"));
    }
}
