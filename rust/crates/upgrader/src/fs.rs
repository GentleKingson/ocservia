use std::fs::{self, File, OpenOptions};
use std::io::{self, Read, Write as _};
use std::os::unix::fs::{DirBuilderExt, MetadataExt, OpenOptionsExt, PermissionsExt};
use std::path::{Component, Path};

use rustix::fs::{FlockOperation, Mode, OFlags};
use rustix::io::Errno;
use sha2::{Digest as _, Sha256};
use uuid::Uuid;

use crate::scheduler::path_error;
use crate::{
    EXECUTION_LOCK_FILE, MAX_DETAIL_BYTES, MAX_SPOOL_PACKAGE_BYTES, SCHEDULE_LOCK_FILE,
    UpgradeStoreError,
};

pub(crate) struct UpgradeLock {
    _file: File,
}

impl UpgradeLock {
    fn open(operations_dir: &Path, name: &str) -> Result<File, UpgradeStoreError> {
        let path = operations_dir.join(name);
        let file = match OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(&path)
        {
            Ok(file) => file,
            Err(error) if error.kind() == io::ErrorKind::AlreadyExists => {
                validate_record_file(&path)?;
                OpenOptions::new()
                    .write(true)
                    .open(&path)
                    .map_err(UpgradeStoreError::Io)?
            }
            Err(error) => return Err(UpgradeStoreError::Io(error)),
        };
        Ok(file)
    }

    /// The scheduling critical section covers only the durable read, active
    /// scan, and intent commit - never the unit trigger - so a concurrent
    /// scheduler for the same command waits briefly here and then observes
    /// the committed intent as an exact replay instead of being rejected.
    pub(crate) fn acquire_schedule(operations_dir: &Path) -> Result<Self, UpgradeStoreError> {
        let file = Self::open(operations_dir, SCHEDULE_LOCK_FILE)?;
        rustix::fs::flock(&file, FlockOperation::LockExclusive)
            .map_err(|failure| UpgradeStoreError::Io(io::Error::from(failure)))?;
        Ok(Self { _file: file })
    }

    /// The execution lock stays non-blocking: a second runner instance for
    /// one operation is genuinely conflicting, and systemd's own restart
    /// convergence owns recovery after a crashed runner.
    pub(crate) fn acquire_execution(operations_dir: &Path) -> Result<Self, UpgradeStoreError> {
        let file = Self::open(operations_dir, EXECUTION_LOCK_FILE)?;
        rustix::fs::flock(&file, FlockOperation::NonBlockingLockExclusive).map_err(|error| {
            if error == Errno::WOULDBLOCK {
                UpgradeStoreError::ActiveConflict
            } else {
                UpgradeStoreError::Io(io::Error::from(error))
            }
        })?;
        Ok(Self { _file: file })
    }
}

pub(crate) fn parse_canonical_operation(value: &str) -> Result<Uuid, UpgradeStoreError> {
    if value.len() != 36
        || !value
            .bytes()
            .all(|byte| byte.is_ascii_hexdigit() || byte == b'-')
    {
        return Err(UpgradeStoreError::Invalid("operation identity malformed"));
    }
    let parsed = Uuid::parse_str(value)
        .map_err(|_| UpgradeStoreError::Invalid("operation identity malformed"))?;
    if parsed.get_version_num() != 7 || parsed.to_string() != value {
        return Err(UpgradeStoreError::Invalid(
            "operation identity must be canonical UUIDv7",
        ));
    }
    Ok(parsed)
}

pub(crate) fn require_uuid_v7(
    value: [u8; 16],
    detail: &'static str,
) -> Result<(), UpgradeStoreError> {
    if Uuid::from_bytes(value).get_version_num() == 7 {
        Ok(())
    } else {
        Err(UpgradeStoreError::Invalid(detail))
    }
}

pub(crate) fn decode_record_lines(
    bytes: &[u8],
    expected: usize,
) -> Result<Vec<String>, UpgradeStoreError> {
    let text = std::str::from_utf8(bytes)
        .map_err(|_| UpgradeStoreError::Unsafe("durable record is not UTF-8"))?;
    let mut lines: Vec<String> = text.split('\n').map(str::to_owned).collect();
    if lines.last().is_some_and(String::is_empty) {
        lines.pop();
    }
    if lines.len() != expected {
        return Err(UpgradeStoreError::Unsafe(
            "durable record line count malformed",
        ));
    }
    Ok(lines)
}

pub(crate) fn parse_line_value(line: &str, key: &str) -> Result<String, UpgradeStoreError> {
    let value = line
        .strip_prefix(&format!("{key}="))
        .ok_or(UpgradeStoreError::Unsafe(
            "durable record key order malformed",
        ))?;
    if value.is_empty() || value.contains('\n') {
        return Err(UpgradeStoreError::Unsafe("durable record value malformed"));
    }
    Ok(value.to_owned())
}

pub(crate) fn expect_line(line: &str, key: &str, expected: &str) -> Result<(), UpgradeStoreError> {
    if parse_line_value(line, key)? == expected {
        Ok(())
    } else {
        Err(UpgradeStoreError::Unsafe("durable record schema mismatch"))
    }
}

pub(crate) fn parse_line_uuid(line: &str, key: &str) -> Result<Uuid, UpgradeStoreError> {
    let value = parse_line_value(line, key)?;
    let parsed = Uuid::parse_str(&value)
        .map_err(|_| UpgradeStoreError::Unsafe("durable record identity malformed"))?;
    if parsed.get_version_num() != 7 || parsed.to_string() != value {
        return Err(UpgradeStoreError::Unsafe(
            "durable record identity malformed",
        ));
    }
    Ok(parsed)
}

pub(crate) fn parse_line_digest(line: &str, key: &str) -> Result<[u8; 32], UpgradeStoreError> {
    let value = parse_line_value(line, key)?;
    if value.len() != 64
        || !value
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
    {
        return Err(UpgradeStoreError::Unsafe("durable record digest malformed"));
    }
    let decoded = hex::decode(value)
        .map_err(|_| UpgradeStoreError::Unsafe("durable record digest malformed"))?;
    decoded
        .try_into()
        .map_err(|_| UpgradeStoreError::Unsafe("durable record digest malformed"))
}

pub(crate) fn bounded_detail(detail: &str) -> String {
    let mut bounded = String::new();
    for byte in detail.bytes().take(MAX_DETAIL_BYTES) {
        if (0x20..0x7f).contains(&byte) {
            bounded.push(byte as char);
        } else {
            bounded.push('.');
        }
    }
    bounded
}

fn trusted_euid() -> u32 {
    rustix::process::geteuid().as_raw()
}

pub(crate) fn validate_directory_strict(directory: &Path) -> Result<(), UpgradeStoreError> {
    let metadata = fs::symlink_metadata(directory).map_err(UpgradeStoreError::Io)?;
    if !metadata.is_dir()
        || metadata.file_type().is_symlink()
        || metadata.uid() != trusted_euid()
        || metadata.permissions().mode() & 0o022 != 0
    {
        return Err(UpgradeStoreError::Unsafe(
            "trusted upgrade path ancestry must be operator-owned real directories",
        ));
    }
    Ok(())
}

pub(crate) fn validate_record_file(path: &Path) -> Result<(), UpgradeStoreError> {
    validate_record_metadata(&fs::symlink_metadata(path)?)
}

pub(crate) fn validate_record_metadata(metadata: &fs::Metadata) -> Result<(), UpgradeStoreError> {
    if !metadata.is_file()
        || metadata.file_type().is_symlink()
        || metadata.uid() != trusted_euid()
        || metadata.nlink() != 1
        || metadata.permissions().mode() & 0o777 != 0o600
    {
        return Err(UpgradeStoreError::Unsafe(
            "durable upgrade record must be an operator-owned one-link mode 0600 regular file",
        ));
    }
    Ok(())
}

pub(crate) fn open_spool_file(path: &Path) -> Result<File, UpgradeStoreError> {
    let fd = rustix::fs::open(
        path,
        OFlags::RDONLY | OFlags::NOFOLLOW | OFlags::NONBLOCK | OFlags::CLOEXEC,
        Mode::empty(),
    )
    .map_err(|error| {
        if error == Errno::NOENT {
            UpgradeStoreError::Package(
                "trusted package spool does not provide the authorized release".to_owned(),
            )
        } else {
            path_error(error)
        }
    })?;
    let file = File::from(fd);
    let metadata = file.metadata()?;
    if !metadata.is_file()
        || metadata.file_type().is_symlink()
        || metadata.uid() != trusted_euid()
        || metadata.nlink() != 1
        || metadata.permissions().mode() & 0o022 != 0
        || metadata.len() == 0
        || metadata.len() > MAX_SPOOL_PACKAGE_BYTES
    {
        return Err(UpgradeStoreError::Package(
            "trusted package spool input has unsafe metadata".to_owned(),
        ));
    }
    Ok(file)
}

pub(crate) fn ensure_operations_root(operations_dir: &Path) -> Result<(), UpgradeStoreError> {
    open_operations_root(operations_dir, true)
}

pub(crate) fn open_operations_root(
    operations_dir: &Path,
    create: bool,
) -> Result<(), UpgradeStoreError> {
    if !operations_dir.is_absolute()
        || operations_dir
            .components()
            .any(|component| !matches!(component, Component::Normal(_) | Component::RootDir))
    {
        return Err(UpgradeStoreError::Unsafe(
            "durable operations path must be absolute and canonical",
        ));
    }
    let flags = OFlags::RDONLY | OFlags::DIRECTORY | OFlags::NOFOLLOW | OFlags::CLOEXEC;
    let mut dir = rustix::fs::open("/", flags, Mode::empty()).map_err(path_error)?;
    let mut components = operations_dir.components().skip(1).peekable();
    while let Some(Component::Normal(name)) = components.next() {
        let next = match rustix::fs::openat(&dir, name, flags, Mode::empty()) {
            Err(Errno::NOENT) if create => {
                match rustix::fs::mkdirat(&dir, name, Mode::RWXU) {
                    Ok(()) | Err(Errno::EXIST) => {}
                    Err(error) => return Err(path_error(error)),
                }
                rustix::fs::openat(&dir, name, flags, Mode::empty()).map_err(path_error)?
            }
            result => result.map_err(path_error)?,
        };
        let metadata = rustix::fs::fstat(&next).map_err(path_error)?;
        let last = components.peek().is_none();
        let owner = metadata.st_uid;
        let mode = metadata.st_mode;
        let protected_ancestor = !last && owner == 0 && mode & 0o1000 != 0;
        if (owner != trusted_euid() && (last || owner != 0))
            || (mode & 0o022 != 0 && !protected_ancestor)
        {
            return Err(UpgradeStoreError::Unsafe(
                "trusted upgrade path ancestry must be operator-owned real directories",
            ));
        }
        dir = next;
    }
    Ok(())
}

pub(crate) fn create_operation_dir(operation_dir: &Path) -> Result<(), UpgradeStoreError> {
    match fs::DirBuilder::new()
        .recursive(false)
        .mode(0o700)
        .create(operation_dir)
    {
        Ok(()) => {}
        Err(error) if error.kind() == io::ErrorKind::AlreadyExists => {
            return Err(UpgradeStoreError::Unsafe(
                "durable operation directory appeared concurrently",
            ));
        }
        Err(error) => return Err(UpgradeStoreError::Io(error)),
    }
    validate_directory_strict(operation_dir)
}

/// Which lock owner an atomic temporary belongs to. The scheduler and the
/// runner may hold their own locks on the same operation directory at the
/// same time, so each side names - and therefore only ever cleans up - its
/// own temporaries.
#[derive(Clone, Copy, Debug)]
pub(crate) enum TempOwner {
    Schedule,
    Run,
}

impl TempOwner {
    const fn as_str(self) -> &'static str {
        match self {
            Self::Schedule => "schedule",
            Self::Run => "run",
        }
    }
}

pub(crate) fn write_atomic(
    path: &Path,
    bytes: &[u8],
    owner: TempOwner,
) -> Result<(), UpgradeStoreError> {
    let directory = path.parent().ok_or(UpgradeStoreError::Unsafe(
        "durable record path needs a parent",
    ))?;
    let name = path
        .file_name()
        .and_then(|name| name.to_str())
        .ok_or(UpgradeStoreError::Unsafe(
            "durable record name must be UTF-8",
        ))?;
    let temporary = directory.join(format!(".{name}.{}.tmp.{}", owner.as_str(), Uuid::now_v7()));
    let mut file = OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(&temporary)
        .map_err(UpgradeStoreError::Io)?;
    file.write_all(bytes).map_err(UpgradeStoreError::Io)?;
    file.sync_all().map_err(UpgradeStoreError::Io)?;
    drop(file);
    fs::rename(&temporary, path).map_err(UpgradeStoreError::Io)?;
    sync_directory(directory)
}

pub(crate) fn cleanup_stale_temporaries(
    operation_dir: &Path,
    owner: TempOwner,
) -> Result<(), UpgradeStoreError> {
    match fs::symlink_metadata(operation_dir) {
        Ok(_) => validate_directory_strict(operation_dir)?,
        Err(error) if error.kind() == io::ErrorKind::NotFound => return Ok(()),
        Err(error) => return Err(UpgradeStoreError::Io(error)),
    }
    let entries = fs::read_dir(operation_dir).map_err(UpgradeStoreError::Io)?;
    let mut removed = false;
    for entry in entries {
        let entry = entry.map_err(UpgradeStoreError::Io)?;
        let Some(name) = entry.file_name().to_str().map(str::to_owned) else {
            continue;
        };
        let stale = [
            "intent",
            "state",
            "result",
            "previous-version",
            "rollback-reason",
            "download-attempts",
        ]
        .iter()
        .any(|record| {
            let prefix = format!(".{record}.{}.tmp", owner.as_str());
            if name == prefix {
                return true;
            }
            name.strip_prefix(&format!("{prefix}."))
                .is_some_and(|suffix| {
                    Uuid::parse_str(suffix)
                        .is_ok_and(|id| id.get_version_num() == 7 && id.to_string() == suffix)
                })
        });
        if !stale {
            continue;
        }
        validate_record_file(&entry.path())?;
        fs::remove_file(entry.path()).map_err(UpgradeStoreError::Io)?;
        removed = true;
    }
    if removed {
        sync_directory(operation_dir)?;
    }
    Ok(())
}

pub(crate) fn sync_directory(directory: &Path) -> Result<(), UpgradeStoreError> {
    File::open(directory)
        .and_then(|handle| handle.sync_all())
        .map_err(UpgradeStoreError::Io)
}

pub(crate) fn sha256_reader(file: &mut File) -> Result<[u8; 32], UpgradeStoreError> {
    let mut hasher = Sha256::new();
    let mut buffer = vec![0_u8; 64 * 1024];
    loop {
        let read = file.read(&mut buffer).map_err(UpgradeStoreError::Io)?;
        if read == 0 {
            break;
        }
        hasher.update(&buffer[..read]);
    }
    Ok(hasher.finalize().into())
}
