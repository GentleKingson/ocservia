//! W3C `traceparent` header validation and generation.

/// Reports whether `value` is a valid version-00 W3C `traceparent` string
/// with non-zero trace and parent span identifiers.
#[must_use]
pub fn is_valid(value: &str) -> bool {
    let bytes = value.as_bytes();
    bytes.len() == 55
        && &bytes[0..3] == b"00-"
        && bytes[35] == b'-'
        && bytes[52] == b'-'
        && is_lower_hex(&bytes[3..35])
        && is_lower_hex(&bytes[36..52])
        && is_lower_hex(&bytes[53..55])
        && bytes[3..35].iter().any(|&byte| byte != b'0')
        && bytes[36..52].iter().any(|&byte| byte != b'0')
}

fn is_lower_hex(bytes: &[u8]) -> bool {
    bytes
        .iter()
        .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(byte))
}

/// Validates a `traceparent` string, returning `Status::invalid_argument` when malformed.
///
/// # Errors
///
/// Returns `tonic::Status::invalid_argument` if `value` is not a valid W3C `traceparent`.
#[cfg(feature = "transport")]
pub fn validate_status(value: &str) -> Result<(), tonic::Status> {
    if !is_valid(value) {
        return Err(tonic::Status::invalid_argument("invalid traceparent"));
    }
    Ok(())
}

/// Generates a sampled version-00 W3C `traceparent` string backed by `UUIDv7` entropy.
#[cfg(feature = "transport")]
#[must_use]
pub fn generate() -> String {
    let trace_id = uuid::Uuid::now_v7();
    let span_id = uuid::Uuid::now_v7();
    format!(
        "00-{}-{}-01",
        trace_id.simple(),
        &span_id.simple().to_string()[..16]
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn valid_and_invalid_traceparents() {
        assert!(is_valid(
            "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"
        ));
        assert!(!is_valid("not-a-trace"));
        assert!(!is_valid(
            "00-0123456789ABCDEF0123456789abcdef-0123456789abcdef-01"
        ));
        assert!(!is_valid(
            "00-00000000000000000000000000000000-0123456789abcdef-01"
        ));
        assert!(!is_valid(
            "00-0123456789abcdef0123456789abcdef-0000000000000000-01"
        ));
    }

    #[cfg(feature = "transport")]
    #[test]
    fn generated_traceparent_validates() {
        let generated = generate();
        assert!(validate_status(&generated).is_ok());
        assert!(validate_status("bad").is_err());
    }
}
