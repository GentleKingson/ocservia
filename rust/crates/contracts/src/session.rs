//! Stable session framing and capability policy shared by transportd and the Agent.

/// Frame kind for an artifact fetch request on an Agent session stream.
pub const ARTIFACT_FETCH_FRAME: u32 = 1 << 31;
/// Frame kind for an artifact consumption request on an Agent session stream.
pub const ARTIFACT_CONSUME_FRAME: u32 = 3 << 30;

/// Read-only capabilities permitted on authenticated sessions without sealing keys.
///
/// This is deliberately an explicit allowlist. A newly introduced capability
/// ending in `.read` is not included automatically until it is reviewed and
/// added here.
pub const READ_ONLY_SESSION_CAPABILITIES: &[&str] = &[
    "ocserv.config_fingerprint.read",
    "ocserv.ip_bans.read",
    "ocserv.sessions.read",
    "ocserv.status.read",
    "ocserv.version.read",
];

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
        assert!(
            READ_ONLY_SESSION_CAPABILITIES
                .binary_search(&"ocserv.status.read")
                .is_ok()
        );
        assert!(
            READ_ONLY_SESSION_CAPABILITIES
                .binary_search(&"synthetic.noop")
                .is_err()
        );
        assert!(
            READ_ONLY_SESSION_CAPABILITIES
                .binary_search(&"future.feature.read")
                .is_err()
        );
    }
}
