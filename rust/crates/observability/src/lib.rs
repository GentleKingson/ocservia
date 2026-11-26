//! Shared structured logging and runtime task statistics.

#![forbid(unsafe_code)]

/// Initializes JSON structured logging on stderr using `RUST_LOG` when present,
/// falling back to `"info"`.
///
/// # Errors
///
/// Returns an error when a global subscriber was already installed.
pub fn init(
    service_name: &'static str,
    release_version: &'static str,
) -> Result<(), tracing::subscriber::SetGlobalDefaultError> {
    init_with_default_filter(service_name, release_version, "info")
}

/// Initializes JSON structured logging on stderr using `RUST_LOG` when present,
/// falling back to `default_filter`.
///
/// # Errors
///
/// Returns an error when a global subscriber was already installed.
pub fn init_with_default_filter(
    service_name: &'static str,
    release_version: &'static str,
    default_filter: &str,
) -> Result<(), tracing::subscriber::SetGlobalDefaultError> {
    let filter = tracing_subscriber::EnvFilter::try_from_default_env()
        .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new(default_filter));
    let subscriber = tracing_subscriber::fmt()
        .json()
        .with_writer(std::io::stderr)
        .with_env_filter(filter)
        .with_current_span(true)
        .with_span_list(true)
        .finish();
    tracing::subscriber::set_global_default(subscriber)?;
    tracing::info!(
        service.name = service_name,
        service.version = release_version,
        "service starting"
    );
    Ok(())
}

/// Waits for either `SIGTERM` or `Ctrl-C` (`SIGINT`) so daemons shut down cleanly.
pub async fn shutdown_signal() {
    let terminate = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate());
    let Ok(mut terminate) = terminate else {
        tracing::error!("failed to install SIGTERM handler");
        let _ = tokio::signal::ctrl_c().await;
        return;
    };
    tokio::select! {
        result = tokio::signal::ctrl_c() => {
            if result.is_err() {
                tracing::error!("failed to install Ctrl-C handler");
            }
        }
        _ = terminate.recv() => {}
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
    let handle = tokio::runtime::Handle::try_current().map_err(StatsWriteError::NoRuntime)?;
    write_runtime_stats_with_alive(&path, handle.metrics().num_alive_tasks())?;
    tokio::spawn(async move {
        let mut ticker = tokio::time::interval(std::time::Duration::from_secs(2));
        ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
        // First tick fires immediately; the synchronous initial write above
        // already published the startup sample.
        ticker.tick().await;
        loop {
            ticker.tick().await;
            let alive = handle.metrics().num_alive_tasks();
            let path_for_io = path.clone();
            let write_result = tokio::task::spawn_blocking(move || {
                write_runtime_stats_with_alive(&path_for_io, alive)
            })
            .await;
            match write_result {
                Ok(Ok(())) => {}
                Ok(Err(error)) => {
                    tracing::warn!(error = %error, path = %path.display(), "runtime stats writer stopping");
                    return;
                }
                Err(join_error) => {
                    tracing::warn!(error = %join_error, path = %path.display(), "runtime stats writer task failed");
                    return;
                }
            }
        }
    });
    Ok(())
}

fn write_runtime_stats_with_alive(
    path: &std::path::Path,
    alive: usize,
) -> Result<(), StatsWriteError> {
    let now = std::time::SystemTime::now()
        .duration_since(std::time::SystemTime::UNIX_EPOCH)
        .map_err(StatsWriteError::Clock)?;
    let at = u64::try_from(now.as_millis()).unwrap_or(u64::MAX);
    let payload = format!("{{\"at\":{at},\"tasks_alive\":{alive}}}\n");
    let mut temporary = path.to_path_buf().into_os_string();
    temporary.push(".tmp");
    let temporary = std::path::PathBuf::from(temporary);
    std::fs::write(&temporary, payload).map_err(|error| StatsWriteError::Write {
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
mod output_tests {
    #[test]
    fn structured_logs_do_not_pollute_command_output() {
        const CHILD: &str = "OCSERV_LOG_OUTPUT_TEST_CHILD";
        if std::env::var_os(CHILD).is_some() {
            super::init("output-test", "9.8.7").expect("logging starts");
            println!("01900000-0000-7000-8000-000000000001");
            tracing::info!("connection closed after command result");
            return;
        }
        let output = std::process::Command::new(std::env::current_exe().expect("test executable"))
            .args([
                "--exact",
                "output_tests::structured_logs_do_not_pollute_command_output",
                "--nocapture",
            ])
            .env(CHILD, "1")
            .env("RUST_LOG", "info")
            .output()
            .expect("child test runs");
        assert!(output.status.success());
        let stdout = String::from_utf8(output.stdout).expect("stdout UTF-8");
        let stderr = String::from_utf8(output.stderr).expect("stderr UTF-8");
        assert!(stdout.contains("01900000-0000-7000-8000-000000000001"));
        assert!(!stdout.contains("connection closed after command result"));
        assert!(stderr.contains("connection closed after command result"));
        let starting = stderr
            .lines()
            .filter_map(|line| serde_json::from_str::<serde_json::Value>(line).ok())
            .find(|line| line["fields"]["message"] == "service starting")
            .expect("startup log present");
        assert_eq!(starting["fields"]["service.version"], "9.8.7");
    }
}

#[cfg(test)]
mod runtime_stats_tests {
    use super::spawn_runtime_stats_writer;
    use ocservia_test_support::PrivateTempDir;
    use std::time::Duration;

    #[tokio::test(start_paused = true)]
    async fn runtime_stats_writer_publishes_live_task_counts() {
        let dir = PrivateTempDir::new("obs");
        let path = dir.join("runtime-stats.json");
        spawn_runtime_stats_writer(path.clone()).expect("writer starts");
        let initial = std::fs::read_to_string(&path).expect("initial stats written");
        tokio::task::yield_now().await;
        tokio::time::advance(Duration::from_secs(2)).await;
        // The writer publishes from a blocking worker. Use a wall-clock deadline
        // while waiting for the atomic replacement.
        tokio::time::resume();
        let value = tokio::time::timeout(Duration::from_secs(1), async {
            loop {
                let body = std::fs::read_to_string(&path).expect("stats written");
                if body != initial {
                    break serde_json::from_str::<serde_json::Value>(&body).expect("stats JSON");
                }
                tokio::time::sleep(Duration::from_millis(5)).await;
            }
        })
        .await
        .expect("updated stats written");
        assert!(value["at"].as_u64().is_some_and(|at| at > 0));
        assert!(
            value["tasks_alive"]
                .as_u64()
                .is_some_and(|alive| alive >= 1)
        );
    }

    #[tokio::test]
    async fn runtime_stats_writer_fails_fast_on_unwritable_path() {
        let dir = PrivateTempDir::new("obs-missing");
        let path = dir.join("missing/stats.json");
        assert!(spawn_runtime_stats_writer(path).is_err());
    }
}
