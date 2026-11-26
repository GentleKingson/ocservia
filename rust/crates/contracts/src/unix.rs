//! Shared Unix filesystem flags and safe resource-name validation.

/// Portable `O_NOFOLLOW` open flag for `OpenOptionsExt::custom_flags`.
///
/// Uses a `usize`/`u32`-width cast rather than `i32::from` because
/// `rustix::fs::OFlags::NOFOLLOW` bits vary across `x86_64` and ARM64 targets.
#[allow(clippy::cast_possible_wrap)]
pub const O_NOFOLLOW: i32 = rustix::fs::OFlags::NOFOLLOW.bits() as i32;

/// Reports whether `value` is a bounded ASCII identifier suitable for managed
/// Ocserv usernames, group names, and resource keys (`[A-Za-z0-9][A-Za-z0-9_.-]{0,63}`).
#[must_use]
pub fn is_valid_resource_name(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 64
        && value.bytes().enumerate().all(|(index, byte)| {
            byte.is_ascii_alphanumeric() || (index > 0 && matches!(byte, b'_' | b'.' | b'-'))
        })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn nofollow_flag_fits_platform_i32() {
        assert_eq!(
            O_NOFOLLOW,
            i32::try_from(rustix::fs::OFlags::NOFOLLOW.bits()).expect("NOFOLLOW fits in i32")
        );
    }

    #[test]
    fn resource_name_validation_enforces_character_and_length_bounds() {
        assert!(is_valid_resource_name("alice"));
        assert!(is_valid_resource_name("alice_01.vpn-east"));
        assert!(is_valid_resource_name(&"a".repeat(64)));

        assert!(!is_valid_resource_name(""));
        assert!(!is_valid_resource_name("-leading"));
        assert!(!is_valid_resource_name(".leading"));
        assert!(!is_valid_resource_name("_leading"));
        assert!(!is_valid_resource_name("alice@example"));
        assert!(!is_valid_resource_name(&"a".repeat(65)));
    }
}
