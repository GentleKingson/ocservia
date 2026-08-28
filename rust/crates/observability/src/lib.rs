//! Shared structured logging and bounded in-process Agent metrics.

#![forbid(unsafe_code)]

use std::sync::atomic::{AtomicU64, Ordering};

/// Initializes JSON structured logging using `RUST_LOG` when present.
///
/// # Errors
///
/// Returns an error when a global subscriber was already installed.
pub fn init(service_name: &'static str) -> Result<(), tracing::subscriber::SetGlobalDefaultError> {
    let filter = tracing_subscriber::EnvFilter::try_from_default_env()
        .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info"));
    let subscriber = tracing_subscriber::fmt()
        .json()
        .with_env_filter(filter)
        .with_current_span(true)
        .with_span_list(true)
        .finish();
    tracing::subscriber::set_global_default(subscriber)?;
    tracing::info!(
        service.name = service_name,
        service.version = env!("CARGO_PKG_VERSION"),
        "service starting"
    );
    Ok(())
}

/// Minimal counters exported by the Agent/privd boundary in I06.
#[derive(Debug, Default)]
pub struct AgentMetrics {
    reconnects: AtomicU64,
    privd_failures: AtomicU64,
    readonly_requests: AtomicU64,
}

impl AgentMetrics {
    /// Records an Iroh reconnect attempt.
    pub fn record_reconnect(&self) {
        self.reconnects.fetch_add(1, Ordering::Relaxed);
    }

    /// Records a privd failure.
    pub fn record_privd_failure(&self) {
        self.privd_failures.fetch_add(1, Ordering::Relaxed);
    }

    /// Records a read-only collection request.
    pub fn record_readonly_request(&self) {
        self.readonly_requests.fetch_add(1, Ordering::Relaxed);
    }

    /// Returns `(reconnects, privd_failures, readonly_requests)`.
    #[must_use]
    pub fn snapshot(&self) -> (u64, u64, u64) {
        (
            self.reconnects.load(Ordering::Relaxed),
            self.privd_failures.load(Ordering::Relaxed),
            self.readonly_requests.load(Ordering::Relaxed),
        )
    }
}

/// Spawns a background task that periodically publishes the process's live
/// tokio task count to `path` as a single-line JSON object
/// `{"at":<unix_millis>,"tasks_alive":<count>}`. Each publication replaces
/// the file atomically so a concurrent reader never observes a partial
/// write. The writer keeps running until the runtime shuts down; write
/// failures are logged once and terminate the writer so a broken stats sink
/// cannot spin.
///
/// # Errors
///
/// Returns an error when the initial write to `path` fails, so callers fail
/// fast instead of silently producing no statistics.
pub fn spawn_runtime_stats_writer(path: std::path::PathBuf) -> Result<(), StatsWriteError> {
    write_runtime_stats(&path)?;
    tokio::spawn(async move {
        let mut ticker = tokio::time::interval(std::time::Duration::from_secs(2));
        ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
        loop {
            ticker.tick().await;
            if let Err(error) = write_runtime_stats(&path) {
                tracing::warn!(error = %error, path = %path.display(), "runtime stats writer stopping");
                return;
            }
        }
    });
    Ok(())
}

fn write_runtime_stats(path: &std::path::Path) -> Result<(), StatsWriteError> {
    let alive = tokio::runtime::Handle::try_current()
        .map(|handle| handle.metrics().num_alive_tasks())
        .map_err(StatsWriteError::NoRuntime)?;
    let now = std::time::SystemTime::now()
        .duration_since(std::time::SystemTime::UNIX_EPOCH)
        .map_err(StatsWriteError::Clock)?;
    let line = serde_json::json!({
        "at": u64::try_from(now.as_millis()).unwrap_or(u64::MAX),
        "tasks_alive": alive,
    });
    let mut temporary = path.to_path_buf().into_os_string();
    temporary.push(".tmp");
    let temporary = std::path::PathBuf::from(temporary);
    std::fs::write(&temporary, format!("{line}\n")).map_err(|error| StatsWriteError::Write {
        path: path.to_path_buf(),
        source: error,
    })?;
    std::fs::rename(&temporary, path).map_err(|error| StatsWriteError::Write {
        path: path.to_path_buf(),
        source: error,
    })?;
    Ok(())
}

/// Failures of the runtime statistics writer.
#[derive(Debug)]
pub enum StatsWriteError {
    /// No tokio runtime is available on this thread.
    NoRuntime(tokio::runtime::TryCurrentError),
    /// The system clock is before the UNIX epoch.
    Clock(std::time::SystemTimeError),
    /// The statistics file could not be written or renamed.
    Write {
        /// The intended statistics file path.
        path: std::path::PathBuf,
        /// The underlying filesystem error.
        source: std::io::Error,
    },
}

impl std::fmt::Display for StatsWriteError {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::NoRuntime(_) => write!(formatter, "no tokio runtime is running"),
            Self::Clock(_) => write!(formatter, "system clock is before the UNIX epoch"),
            Self::Write { path, .. } => {
                write!(
                    formatter,
                    "runtime stats file {} is not writable",
                    path.display()
                )
            }
        }
    }
}

impl std::error::Error for StatsWriteError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Self::NoRuntime(error) => Some(error),
            Self::Clock(error) => Some(error),
            Self::Write { source, .. } => Some(source),
        }
    }
}

#[cfg(test)]
mod runtime_stats_tests {
    use super::spawn_runtime_stats_writer;
    use std::time::Duration;

    #[tokio::test]
    async fn runtime_stats_writer_publishes_live_task_counts() {
        let path = std::env::temp_dir()
            .canonicalize()
            .expect("tmp")
            .join(format!("ocservia-runtime-stats-{}.json", unique_suffix()));
        spawn_runtime_stats_writer(path.clone()).expect("writer starts");
        tokio::time::sleep(Duration::from_millis(2500)).await;
        let body = std::fs::read_to_string(&path).expect("stats written");
        let value: serde_json::Value = serde_json::from_str(body.trim()).expect("stats json");
        assert!(value["at"].as_u64().is_some_and(|at| at > 0));
        assert!(
            value["tasks_alive"]
                .as_u64()
                .is_some_and(|alive| alive >= 1)
        );
        std::fs::remove_file(&path).ok();
    }

    #[tokio::test]
    async fn runtime_stats_writer_fails_fast_on_unwritable_path() {
        let path = std::env::temp_dir()
            .canonicalize()
            .expect("tmp")
            .join(format!(
                "ocservia-runtime-stats-missing-{0}-{1}/stats.json",
                unique_suffix(),
                unique_suffix()
            ));
        assert!(spawn_runtime_stats_writer(path).is_err());
    }

    fn unique_suffix() -> u128 {
        std::time::SystemTime::now()
            .duration_since(std::time::SystemTime::UNIX_EPOCH)
            .expect("clock")
            .as_nanos()
            + u128::from(std::process::id())
    }
}

// G6 dependency-cache experiment run 1: first-party source touch.
