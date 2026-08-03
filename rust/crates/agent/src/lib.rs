//! Unprivileged Agent runtime primitives and fixed privd client.

#![forbid(unsafe_code)]

use std::io;
use std::path::Path;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use ocservia_agent_protocol::{
    PrivdRequest, PrivdResponse, ReadRequest, privd_request, read_frame, write_frame,
};
use rand::Rng;
use tokio::net::UnixStream;
use uuid::Uuid;

/// Maximum concurrent read-only collection tasks per Agent.
pub const MAX_READ_CONCURRENCY: usize = 4;

/// Full Jitter reconnect policy.
#[derive(Clone, Copy, Debug)]
pub struct Backoff {
    base: Duration,
    cap: Duration,
}

impl Default for Backoff {
    fn default() -> Self {
        Self {
            base: Duration::from_millis(250),
            cap: Duration::from_secs(30),
        }
    }
}

impl Backoff {
    /// Creates a bounded Full Jitter policy.
    ///
    /// # Errors
    ///
    /// Rejects zero or inverted bounds.
    pub fn new(base: Duration, cap: Duration) -> Result<Self, io::Error> {
        if base.is_zero() || cap < base {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "backoff bounds invalid",
            ));
        }
        Ok(Self { base, cap })
    }

    /// Returns a uniformly jittered delay between zero and the exponential cap.
    #[must_use]
    pub fn delay(&self, attempt: u32, rng: &mut impl Rng) -> Duration {
        let exponent = attempt.min(31);
        let multiplier = 1_u32.checked_shl(exponent).unwrap_or(u32::MAX);
        let ceiling = self.base.saturating_mul(multiplier).min(self.cap);
        let max_millis = u64::try_from(ceiling.as_millis()).unwrap_or(u64::MAX);
        Duration::from_millis(rng.random_range(0..=max_millis))
    }
}

/// Fixed privd client over `AF_UNIX`.
#[derive(Clone, Debug)]
pub struct PrivdClient {
    socket: std::path::PathBuf,
    timeout: Duration,
}

impl PrivdClient {
    /// Creates a client with a bounded request deadline.
    ///
    /// # Errors
    ///
    /// Rejects zero or greater-than-30-second timeouts.
    pub fn new(socket: std::path::PathBuf, timeout: Duration) -> Result<Self, io::Error> {
        if timeout.is_zero() || timeout > Duration::from_secs(30) {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "privd timeout invalid",
            ));
        }
        Ok(Self { socket, timeout })
    }

    /// Calls exactly one fixed read-only operation.
    ///
    /// # Errors
    ///
    /// Returns an I/O or deadline error; structured privd failures remain in the response.
    pub async fn call(
        &self,
        operation: privd_request::Operation,
    ) -> Result<PrivdResponse, io::Error> {
        let deadline = SystemTime::now()
            .checked_add(self.timeout)
            .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidData, "privd deadline overflow"))?
            .duration_since(UNIX_EPOCH)
            .map_err(|_| io::Error::new(io::ErrorKind::InvalidData, "system clock unavailable"))?;
        let deadline_unix_ms = u64::try_from(deadline.as_millis())
            .map_err(|_| io::Error::new(io::ErrorKind::InvalidData, "privd deadline overflow"))?;
        let request = PrivdRequest {
            request_id: Uuid::now_v7().as_bytes().to_vec(),
            deadline_unix_ms,
            operation: Some(operation),
        };
        let mut stream = tokio::time::timeout(self.timeout, UnixStream::connect(&self.socket))
            .await
            .map_err(|_| io::Error::new(io::ErrorKind::TimedOut, "privd connect timed out"))??;
        tokio::time::timeout(self.timeout, write_frame(&mut stream, &request))
            .await
            .map_err(|_| io::Error::new(io::ErrorKind::TimedOut, "privd request timed out"))??;
        let response: PrivdResponse = tokio::time::timeout(self.timeout, read_frame(&mut stream))
            .await
            .map_err(|_| io::Error::new(io::ErrorKind::TimedOut, "privd response timed out"))??;
        if response.request_id != request.request_id {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "privd response correlation invalid",
            ));
        }
        Ok(response)
    }

    /// Reads all five I06 observations with at most four active tasks.
    ///
    /// # Errors
    ///
    /// Returns the first transport error.
    pub async fn snapshot(&self) -> Result<Vec<PrivdResponse>, io::Error> {
        let operations = [
            privd_request::Operation::ServiceStatus(ReadRequest {}),
            privd_request::Operation::OcservVersion(ReadRequest {}),
            privd_request::Operation::SessionList(ReadRequest {}),
            privd_request::Operation::IpBanList(ReadRequest {}),
            privd_request::Operation::ConfigFingerprint(ReadRequest {}),
        ];
        let permits = std::sync::Arc::new(tokio::sync::Semaphore::new(MAX_READ_CONCURRENCY));
        let mut tasks = tokio::task::JoinSet::new();
        for operation in operations {
            let permit = std::sync::Arc::clone(&permits)
                .acquire_owned()
                .await
                .map_err(|_| io::Error::other("read supervisor closed"))?;
            let client = self.clone();
            tasks.spawn(async move {
                let _permit = permit;
                client.call(operation).await
            });
        }
        let mut responses = Vec::with_capacity(5);
        while let Some(result) = tasks.join_next().await {
            responses.push(result.map_err(|_| io::Error::other("read task failed"))??);
        }
        Ok(responses)
    }
}

/// Rejects root execution before any network or local RPC is opened.
///
/// # Errors
///
/// Returns permission denied for effective UID zero.
pub fn ensure_unprivileged(effective_uid: u32) -> Result<(), io::Error> {
    if effective_uid == 0 {
        return Err(io::Error::new(
            io::ErrorKind::PermissionDenied,
            "ocservia-agent must not run as root",
        ));
    }
    Ok(())
}

/// Reads a bounded, fixed host identity file.
///
/// # Errors
///
/// Rejects missing, oversized, empty, or control-character-containing content.
async fn read_host_value(path: &Path, max_bytes: usize) -> Result<String, io::Error> {
    let bytes = tokio::fs::read(path).await?;
    if bytes.is_empty() || bytes.len() > max_bytes {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "host identity value size invalid",
        ));
    }
    let value = String::from_utf8(bytes)
        .map_err(|_| io::Error::new(io::ErrorKind::InvalidData, "host identity value invalid"))?;
    let value = value.trim();
    if value.is_empty() || value.chars().any(char::is_control) {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "host identity value invalid",
        ));
    }
    Ok(value.to_owned())
}

/// Reads the bounded `ID` field from the fixed os-release file.
///
/// # Errors
///
/// Rejects missing, oversized, malformed, or unsupported content.
pub async fn read_os_release() -> Result<String, io::Error> {
    let bytes = tokio::fs::read("/etc/os-release").await?;
    if bytes.is_empty() || bytes.len() > 8192 {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "os-release size invalid",
        ));
    }
    let text = std::str::from_utf8(&bytes)
        .map_err(|_| io::Error::new(io::ErrorKind::InvalidData, "os-release invalid"))?;
    let value = text
        .lines()
        .find_map(|line| line.strip_prefix("ID="))
        .map(|value| value.trim_matches('"'))
        .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidData, "os-release ID missing"))?;
    if value.is_empty()
        || value.len() > 64
        || !value
            .bytes()
            .all(|byte| byte.is_ascii_lowercase() || byte.is_ascii_digit() || byte == b'-')
    {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "os-release ID invalid",
        ));
    }
    Ok(value.to_owned())
}

/// Reads the fixed Linux boot identifier with a strict bound.
///
/// # Errors
///
/// Rejects missing, oversized, malformed, or empty content.
pub async fn read_boot_id() -> Result<String, io::Error> {
    read_host_value(Path::new("/proc/sys/kernel/random/boot_id"), 64).await
}

#[cfg(test)]
mod tests {
    use rand::{SeedableRng, rngs::StdRng};

    use super::*;

    #[test]
    fn root_is_refused() {
        assert_eq!(
            ensure_unprivileged(0).expect_err("root refused").kind(),
            io::ErrorKind::PermissionDenied
        );
        ensure_unprivileged(65532).expect("service UID allowed");
    }

    #[test]
    fn full_jitter_is_bounded_and_not_constant() {
        let policy = Backoff::default();
        let mut rng = StdRng::seed_from_u64(7);
        let values = (0..64)
            .map(|_| policy.delay(20, &mut rng))
            .collect::<Vec<_>>();
        assert!(values.iter().all(|delay| *delay <= Duration::from_secs(30)));
        assert!(values.windows(2).any(|pair| pair[0] != pair[1]));
    }
}
