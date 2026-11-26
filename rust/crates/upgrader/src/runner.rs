use std::fmt::Write as _;
use std::fs::{self, File};
use std::io;
use std::os::unix::net::UnixStream;
use std::path::{Path, PathBuf};
use std::process::Command;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use crate::fs::{
    TempOwner, UpgradeLock, bounded_detail, cleanup_stale_temporaries, ensure_operations_root,
    open_operations_root, open_spool_file, parse_canonical_operation, sha256_reader,
    sync_directory, validate_directory_strict, write_atomic,
};
use crate::scheduler::{load_intent, load_state, optional_text_record};
use crate::{
    DEFAULT_PRIVD_SOCKET_PATH, DEFAULT_SPOOL_DIR, DEFAULT_UPGRADE_BASE_URL, MAX_DETAIL_BYTES,
    MAX_DOWNLOAD_ATTEMPTS, MAX_SPOOL_PACKAGE_BYTES, OperationState, RECORD_SCHEMA_VERSION,
    UpgradeIntent, UpgradeStoreError,
};

enum NativeApplyError {
    TransientDownload(UpgradeStoreError),
    Terminal(UpgradeStoreError),
}

impl From<UpgradeStoreError> for NativeApplyError {
    fn from(error: UpgradeStoreError) -> Self {
        Self::Terminal(error)
    }
}

impl From<io::Error> for NativeApplyError {
    fn from(error: io::Error) -> Self {
        Self::Terminal(UpgradeStoreError::Io(error))
    }
}

/// The standalone runner bound to one filesystem root. Production always uses
/// `/`; a different root exists only for staged lifecycle verification.
#[derive(Clone, Debug)]
pub struct UpgradeRunner {
    root: PathBuf,
    base_url_override: Option<String>,
    probe_checks: usize,
    probe_interval: Duration,
    privd_socket: Option<PathBuf>,
}

impl UpgradeRunner {
    #[must_use]
    pub fn new(root: PathBuf) -> Self {
        let is_production = root == Path::new("/");
        Self {
            base_url_override: std::env::var("OCSERV_UPGRADE_BASE_URL")
                .ok()
                .filter(|v| !v.is_empty()),
            probe_checks: if is_production { 4 } else { 2 },
            probe_interval: if is_production {
                Duration::from_secs(2)
            } else {
                Duration::from_millis(10)
            },
            privd_socket: is_production.then(|| PathBuf::from(DEFAULT_PRIVD_SOCKET_PATH)),
            root,
        }
    }

    #[cfg(test)]
    #[must_use]
    pub fn with_base_url_override(mut self, base_url: Option<String>) -> Self {
        self.base_url_override = base_url;
        self
    }

    #[cfg(test)]
    #[must_use]
    pub fn with_probe_schedule(mut self, checks: usize, interval: Duration) -> Self {
        self.probe_checks = checks.max(1);
        self.probe_interval = interval;
        self
    }

    #[cfg(test)]
    #[must_use]
    pub fn with_privd_socket(mut self, socket: Option<PathBuf>) -> Self {
        self.privd_socket = socket;
        self
    }

    #[must_use]
    pub fn operations_dir(&self) -> PathBuf {
        self.root.join("var/lib/ocservia-upgrade/operations")
    }

    #[must_use]
    pub fn spool_dir(&self) -> PathBuf {
        self.root.join(DEFAULT_SPOOL_DIR.trim_start_matches('/'))
    }

    /// Executes (or replays) exactly one durable upgrade operation.
    ///
    /// # Errors
    ///
    /// Refusals are persisted as a durable `failed` result before the error is
    /// returned, except refusals of the operation identity itself and bounded
    /// transient pre-install download retries.
    pub fn run(&self, operation: &str) -> Result<OperationState, UpgradeStoreError> {
        let operation_id = parse_canonical_operation(operation)?;
        let operations_root = self.operations_dir();
        open_operations_root(&operations_root, false)?;
        let operation_dir = operations_root.join(operation_id.to_string());
        validate_directory_strict(&operation_dir)?;
        let intent = load_intent(&operation_dir)?;
        if intent.operation_id != operation_id {
            return Err(UpgradeStoreError::Unsafe(
                "durable intent operation identity does not match its directory",
            ));
        }
        let state = load_state(&operation_dir)?;
        if state.is_terminal() {
            tracing::info!(operation = %operation_id, state = state.as_str(), "durable upgrade already terminal");
            return Ok(state);
        }
        let _execution = UpgradeLock::acquire_execution(&operations_root)?;
        cleanup_stale_temporaries(&operation_dir, TempOwner::Run)?;
        cleanup_stale_downloads(&self.spool_dir())?;
        let state = load_state(&operation_dir)?;
        if state.is_terminal() {
            return Ok(state);
        }
        self.execute(&operation_dir, &intent)
    }

    fn execute(
        &self,
        operation_dir: &Path,
        intent: &UpgradeIntent,
    ) -> Result<OperationState, UpgradeStoreError> {
        let (state, detail) = match self.apply_native(operation_dir, intent) {
            Ok(result) => result,
            Err(NativeApplyError::TransientDownload(failure)) => {
                let attempts_path = operation_dir.join("download-attempts");
                let prior = match optional_text_record(&attempts_path, 32) {
                    Ok(Some(raw)) => match raw.trim().parse::<u32>() {
                        Ok(value) => value,
                        Err(_) => {
                            return refuse(
                                operation_dir,
                                intent,
                                UpgradeStoreError::Unsafe(
                                    "durable download attempt count malformed",
                                ),
                            );
                        }
                    },
                    Ok(None) => 0,
                    Err(err) => return refuse(operation_dir, intent, err),
                };
                let next_attempt = prior.saturating_add(1);
                if next_attempt < MAX_DOWNLOAD_ATTEMPTS {
                    write_atomic(
                        &attempts_path,
                        format!("{next_attempt}\n").as_bytes(),
                        TempOwner::Run,
                    )?;
                    tracing::warn!(
                        operation = %intent.operation_id,
                        attempt = next_attempt,
                        max_attempts = MAX_DOWNLOAD_ATTEMPTS,
                        error = %failure,
                        "transient upgrade package download failed; leaving operation non-terminal for retry"
                    );
                    return Err(failure);
                }
                return refuse(operation_dir, intent, failure);
            }
            Err(NativeApplyError::Terminal(failure)) => {
                return refuse(operation_dir, intent, failure);
            }
        };
        write_terminal(operation_dir, intent, state, &detail)?;
        Ok(state)
    }

    fn apply_native(
        &self,
        operation_dir: &Path,
        intent: &UpgradeIntent,
    ) -> Result<(OperationState, String), NativeApplyError> {
        if ocservia_contracts::agent_upgrade::runtime_architecture()
            != Some(intent.architecture.as_str())
        {
            return Err(UpgradeStoreError::Package(
                "intent architecture mismatches this host".into(),
            )
            .into());
        }
        let manager = if self.root.join("var/lib/dpkg/status").is_file() {
            NativePackage::Deb
        } else {
            NativePackage::Rpm
        };
        let spool = self.spool_dir();
        ensure_operations_root(&spool)?;
        let previous_file = operation_dir.join("previous-version");
        let previous_version = if let Some(version) = optional_text_record(&previous_file, 128)? {
            version
        } else {
            let version = self.installed_version(manager)?;
            write_atomic(&previous_file, version.as_bytes(), TempOwner::Run)?;
            version
        };
        if !ocservia_contracts::agent_upgrade::valid_target_version(&previous_version) {
            return Err(UpgradeStoreError::Unsafe("invalid cached package version").into());
        }
        let previous = spool.join(manager.filename(&previous_version, &intent.architecture));
        open_spool_file(&previous)?;
        let rollback_file = operation_dir.join("rollback-reason");
        let failure = if let Some(reason) = optional_text_record(&rollback_file, MAX_DETAIL_BYTES)?
        {
            reason
        } else {
            let target = self.authorized_package(manager, intent)?;
            write_atomic(&operation_dir.join("state"), b"running\n", TempOwner::Run)?;
            // Native package replacement is replayable. Preserve the original baseline
            // even if dpkg or RPM was interrupted halfway through the transaction.
            match self.install_and_probe(manager, &target, &intent.target_version) {
                Ok(()) => {
                    return Ok((
                        OperationState::Succeeded,
                        "native package installed and services active".into(),
                    ));
                }
                Err(error) => {
                    let detail = bounded_detail(&error.to_string());
                    write_atomic(&rollback_file, detail.as_bytes(), TempOwner::Run)?;
                    detail
                }
            }
        };
        self.install_and_probe(manager, &previous, &previous_version)
            .map_err(|error| {
                UpgradeStoreError::Lifecycle(format!(
                    "rollback failed: {error}; upgrade failure: {failure}"
                ))
            })?;
        Ok((OperationState::RolledBack, failure))
    }

    fn effective_base_url(&self) -> Result<String, UpgradeStoreError> {
        match self.base_url_override.as_deref() {
            Some(raw) if !raw.trim().is_empty() => validate_upgrade_base_url(raw),
            _ => Ok(DEFAULT_UPGRADE_BASE_URL.to_owned()),
        }
    }

    fn authorized_package(
        &self,
        manager: NativePackage,
        intent: &UpgradeIntent,
    ) -> Result<PathBuf, NativeApplyError> {
        let name = manager.filename(&intent.target_version, &intent.architecture);
        let target = self.spool_dir().join(&name);
        if target.try_exists()? {
            verify_digest(&target, intent)?;
        } else {
            let base_url = self.effective_base_url()?;
            let url = format!("{base_url}/v{}/{}", intent.target_version, name);
            let temporary = self
                .spool_dir()
                .join(format!(".download-{}", intent.operation_id));
            let curl_res = checked(
                Command::new(self.root.join("usr/bin/curl"))
                    .args([
                        "--fail",
                        "--silent",
                        "--show-error",
                        "--location",
                        "--proto",
                        "=https",
                        "--proto-redir",
                        "=https",
                        "--max-time",
                        "600",
                        "--max-filesize",
                        &MAX_SPOOL_PACKAGE_BYTES.to_string(),
                        "--output",
                    ])
                    .arg(&temporary)
                    .arg(url),
            );
            if let Err(err) = curl_res {
                if temporary.try_exists()? {
                    let _ = fs::remove_file(&temporary);
                }
                return Err(NativeApplyError::TransientDownload(err));
            }
            let verify_and_rename = (|| {
                verify_digest(&temporary, intent)?.sync_all()?;
                fs::rename(&temporary, &target)?;
                sync_directory(&self.spool_dir())
            })();
            if verify_and_rename.is_err() && temporary.try_exists()? {
                fs::remove_file(temporary)?;
            }
            verify_and_rename?;
        }
        Ok(target)
    }

    fn installed_version(&self, manager: NativePackage) -> Result<String, UpgradeStoreError> {
        let record = match manager {
            NativePackage::Deb => {
                checked(Command::new(self.root.join("usr/bin/dpkg-query")).args([
                    "-W",
                    "-f=${Status} ${Version}",
                    "ocservia-agent",
                ]))?
            }
            NativePackage::Rpm => checked(Command::new(self.root.join("usr/bin/rpm")).args([
                "-q",
                "--qf",
                "%{VERSION}-%{RELEASE}",
                "ocservia-agent",
            ]))?,
        };
        let version = match manager {
            NativePackage::Deb => record.strip_prefix("install ok installed "),
            NativePackage::Rpm => Some(record.as_str()),
        }
        .and_then(|s| s.strip_suffix("-1"))
        .map(|s| s.replace("~rc.", "-rc."));
        version
            .filter(|s| ocservia_contracts::agent_upgrade::valid_target_version(s))
            .ok_or_else(|| {
                UpgradeStoreError::Package(
                    "installed Agent does not have a supported native package version".into(),
                )
            })
    }

    fn install_and_probe(
        &self,
        manager: NativePackage,
        package: &Path,
        version: &str,
    ) -> Result<(), UpgradeStoreError> {
        match manager {
            NativePackage::Deb => checked(
                Command::new(self.root.join("usr/bin/dpkg"))
                    .arg("-i")
                    .arg(package),
            )?,
            NativePackage::Rpm => checked(
                Command::new(self.root.join("usr/bin/rpm"))
                    .args(["-U", "--oldpackage", "--replacepkgs"])
                    .arg(package),
            )?,
        };
        if self.installed_version(manager)? != version {
            return Err(UpgradeStoreError::Lifecycle(
                "installed native package version mismatches intent".into(),
            ));
        }
        let systemctl = self.root.join("usr/bin/systemctl");
        let units = ["ocservia-privd.service", "ocservia-agent.service"];
        checked(Command::new(&systemctl).arg("start").args(units))?;
        // Verify both daemons survive a sustained startup window and confirm
        // privd is accepting connections on its UDS socket. Query individually:
        // systemctl is-active with multiple arguments succeeds if *any* is active.
        for _ in 0..self.probe_checks {
            std::thread::sleep(self.probe_interval);
            for unit in units {
                checked(Command::new(&systemctl).args(["is-active", "--quiet", unit]))?;
            }
        }
        if matches!(
            Command::new(&systemctl)
                .args(["is-enabled", "--quiet", "ocservia-network-audit.service"])
                .status(),
            Ok(status) if status.success()
        ) {
            checked(Command::new(&systemctl).args([
                "is-active",
                "--quiet",
                "ocservia-network-audit.service",
            ]))?;
        }
        if let Some(socket_path) = &self.privd_socket {
            UnixStream::connect(socket_path).map_err(|err| {
                UpgradeStoreError::Lifecycle(format!(
                    "privd socket {} unreachable: {err}",
                    socket_path.display()
                ))
            })?;
        }
        Ok(())
    }
}

pub(crate) fn validate_upgrade_base_url(raw: &str) -> Result<String, UpgradeStoreError> {
    if raw
        .bytes()
        .any(|b| b.is_ascii_control() || b.is_ascii_whitespace())
    {
        return Err(UpgradeStoreError::Unsafe(
            "upgrade base URL contains whitespace or control characters",
        ));
    }
    let trimmed = raw.trim_end_matches('/');
    let Some(rest) = trimmed.strip_prefix("https://") else {
        return Err(UpgradeStoreError::Unsafe(
            "upgrade base URL must use https://",
        ));
    };
    if rest.is_empty()
        || trimmed.contains('@')
        || trimmed.contains('?')
        || trimmed.contains('#')
        || trimmed.contains('\\')
    {
        return Err(UpgradeStoreError::Unsafe(
            "upgrade base URL contains forbidden characters",
        ));
    }
    let mut parts = rest.split('/');
    let Some(host) = parts.next() else {
        return Err(UpgradeStoreError::Unsafe("upgrade base URL host missing"));
    };
    if host.is_empty() {
        return Err(UpgradeStoreError::Unsafe("upgrade base URL host missing"));
    }
    if parts.any(|segment| segment.is_empty() || segment == "." || segment == "..") {
        return Err(UpgradeStoreError::Unsafe(
            "upgrade base URL path segment invalid",
        ));
    }
    Ok(trimmed.to_owned())
}

fn cleanup_stale_downloads(spool_dir: &Path) -> Result<(), UpgradeStoreError> {
    let entries = match fs::read_dir(spool_dir) {
        Ok(entries) => entries,
        Err(error) if error.kind() == io::ErrorKind::NotFound => return Ok(()),
        Err(error) => return Err(UpgradeStoreError::Io(error)),
    };
    for entry in entries {
        let entry = entry.map_err(UpgradeStoreError::Io)?;
        let Some(name) = entry.file_name().to_str().map(str::to_owned) else {
            continue;
        };
        if !name.starts_with(".download-") {
            continue;
        }
        let path = entry.path();
        if fs::symlink_metadata(&path)?.file_type().is_file() {
            fs::remove_file(&path)?;
        }
    }
    Ok(())
}

#[derive(Clone, Copy)]
pub(crate) enum NativePackage {
    Deb,
    Rpm,
}

impl NativePackage {
    pub(crate) fn filename(self, version: &str, arch: &str) -> String {
        match self {
            Self::Deb => format!("ocservia-agent_{version}-1_{arch}.deb"),
            Self::Rpm => {
                let rpm_arch = if arch == "amd64" { "x86_64" } else { "aarch64" };
                format!("ocservia-agent-{version}-1.{rpm_arch}.rpm")
            }
        }
    }
}

fn verify_digest(path: &Path, intent: &UpgradeIntent) -> Result<File, UpgradeStoreError> {
    let mut file = open_spool_file(path)?;
    if sha256_reader(&mut file)? != intent.package_sha256 {
        return Err(UpgradeStoreError::Package(
            "native package digest mismatches authorized intent".into(),
        ));
    }
    Ok(file)
}

fn checked(command: &mut Command) -> Result<String, UpgradeStoreError> {
    let output = command.output()?;
    if !output.status.success() {
        return Err(UpgradeStoreError::Lifecycle(format!(
            "{} exited with {}: {}",
            command.get_program().to_string_lossy(),
            output.status,
            bounded_detail(&String::from_utf8_lossy(&output.stderr))
        )));
    }
    Ok(String::from_utf8_lossy(&output.stdout)
        .trim_end()
        .to_owned())
}

pub(crate) fn write_terminal(
    operation_dir: &Path,
    intent: &UpgradeIntent,
    state: OperationState,
    detail: &str,
) -> Result<(), UpgradeStoreError> {
    let completed = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_err(|_| UpgradeStoreError::Unsafe("system clock unavailable"))?;
    let mut record = String::new();
    let _ = writeln!(record, "schema={RECORD_SCHEMA_VERSION}");
    let _ = writeln!(record, "state={}", state.as_str());
    let _ = writeln!(record, "operation_id={}", intent.operation_id);
    let _ = writeln!(record, "target_version={}", intent.target_version);
    let _ = writeln!(
        record,
        "package_sha256={}",
        hex::encode(intent.package_sha256)
    );
    let _ = writeln!(record, "completed_unix={}", completed.as_secs());
    let _ = writeln!(record, "detail={}", bounded_detail(detail));
    write_atomic(
        &operation_dir.join("result"),
        record.as_bytes(),
        TempOwner::Run,
    )?;
    write_atomic(
        &operation_dir.join("state"),
        format!("{}\n", state.as_str()).as_bytes(),
        TempOwner::Run,
    )?;
    Ok(())
}

/// Persists the durable failure evidence first, then surfaces the same
/// refusal to the caller so exit status and local evidence agree.
fn refuse(
    operation_dir: &Path,
    intent: &UpgradeIntent,
    failure: UpgradeStoreError,
) -> Result<OperationState, UpgradeStoreError> {
    if let Err(persisted) = write_terminal(
        operation_dir,
        intent,
        OperationState::Failed,
        &failure.to_string(),
    ) {
        tracing::warn!(error = %persisted, "durable failure result could not be persisted");
    }
    Err(failure)
}
