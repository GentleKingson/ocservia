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
